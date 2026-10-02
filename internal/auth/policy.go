// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/keys"
)

// RFC 8693 token type URIs.
const (
	TokenTypeIDToken     = "urn:ietf:params:oauth:token-type:id_token"
	TokenTypeJWT         = "urn:ietf:params:oauth:token-type:jwt"
	TokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
	// TokenTypeServiceKey marks a Cucina service-account key (R-AUTH-10). The STS also
	// accepts a service key sent as TokenTypeAccessToken.
	TokenTypeServiceKey = "urn:cucina:params:oauth:token-type:service-key"
)

// Policy types.
const (
	PolicyTypeOIDC           = "oidc"
	PolicyTypeServiceAccount = "serviceAccount"
)

type compiledRule struct {
	prg     *program
	message string
}

type compiledGrant struct {
	name      string
	cond      *program
	verbs     []Verb
	instances []string
	maxTTL    time.Duration
}

// compiledPolicy is a TrustPolicy with every CEL expression compiled.
type compiledPolicy struct {
	key         string // namespace/name
	typ         string
	issuer      *v1alpha1.IssuerSpec
	issuers     []string // url + additionalIssuers
	tokenType   string
	requireJTI  bool
	rules       []compiledRule
	subject     *program
	displayName *program
	groups      *program
	grants      []compiledGrant
	groupLookup *v1alpha1.GroupLookupSpec
	account     string
	breakGlass  bool
	login       *v1alpha1.LoginSpec
}

func policyKey(p v1alpha1.TrustPolicy) string {
	if p.Namespace == "" {
		return p.Name
	}
	return p.Namespace + "/" + p.Name
}

// compilePolicy validates a TrustPolicy and compiles its expressions (R-AUTH-2: compile
// at load time, reject non-bool rules). Errors become the policy's Valid=False condition.
func (e *Engine) compilePolicy(p v1alpha1.TrustPolicy) (*compiledPolicy, error) {
	s := p.Spec
	c := &compiledPolicy{key: policyKey(p), typ: s.Type, requireJTI: s.RequireUniqueJTI, groupLookup: s.GroupLookup, login: s.Login}
	if c.typ == "" {
		c.typ = PolicyTypeOIDC
	}
	var errs []error
	switch c.typ {
	case PolicyTypeOIDC:
		errs = append(errs, e.compileOIDC(c, s)...)
	case PolicyTypeServiceAccount:
		errs = append(errs, compileServiceAccount(c, s)...)
	default:
		errs = append(errs, fmt.Errorf("unknown type %q", s.Type))
	}
	for i, r := range s.ClaimValidationRules {
		prg, err := e.envs.compile(e.envs.claims, r.Expression, wantBool)
		if err != nil {
			errs = append(errs, fmt.Errorf("claimValidationRules[%d]: %w", i, err))
			continue
		}
		msg := r.Message
		if msg == "" {
			msg = fmt.Sprintf("claim validation rule %d failed", i)
		}
		c.rules = append(c.rules, compiledRule{prg: prg, message: msg})
	}
	if len(s.Grants) == 0 {
		errs = append(errs, errors.New("at least one grant is required"))
	}
	for i, g := range s.Grants {
		cg, err := e.compileGrant(g)
		if err != nil {
			errs = append(errs, fmt.Errorf("grants[%d]: %w", i, err))
			continue
		}
		if cg.name == "" {
			cg.name = fmt.Sprintf("grant-%d", i)
		}
		c.grants = append(c.grants, cg)
	}
	if s.GroupLookup != nil {
		if s.GroupLookup.Provider != "cloud-identity" {
			errs = append(errs, fmt.Errorf("groupLookup.provider %q is not supported", s.GroupLookup.Provider))
		} else if e.opts.Groups == nil {
			errs = append(errs, errors.New("groupLookup is configured but the STS has no group resolver (auth.groupLookup)"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

func (e *Engine) compileOIDC(c *compiledPolicy, s v1alpha1.TrustPolicySpec) []error {
	var errs []error
	if s.ServiceAccount != nil {
		errs = append(errs, errors.New("serviceAccount is only valid for type serviceAccount"))
	}
	switch s.SubjectTokenType {
	case "", TokenTypeIDToken:
		c.tokenType = TokenTypeIDToken
	case TokenTypeJWT:
		c.tokenType = TokenTypeJWT
	default:
		errs = append(errs, fmt.Errorf("subjectTokenType %q is not supported", s.SubjectTokenType))
	}
	if s.Issuer == nil {
		return append(errs, errors.New("issuer is required for type oidc"))
	}
	c.issuer = s.Issuer
	if err := httpsURL(s.Issuer.URL); err != nil {
		errs = append(errs, fmt.Errorf("issuer.url: %w", err))
	}
	if s.Issuer.DiscoveryURL != "" {
		if err := httpsURL(s.Issuer.DiscoveryURL); err != nil {
			errs = append(errs, fmt.Errorf("issuer.discoveryURL: %w", err))
		}
	}
	c.issuers = append([]string{s.Issuer.URL}, s.Issuer.AdditionalIssuers...)
	if len(s.Issuer.Audiences) == 0 || slices.Contains(s.Issuer.Audiences, "") {
		errs = append(errs, errors.New("issuer.audiences must list at least one non-empty audience"))
	}
	if s.ClaimMappings == nil || s.ClaimMappings.Subject.Expression == "" {
		return append(errs, errors.New("claimMappings.subject is required for type oidc"))
	}
	var err error
	if c.subject, err = e.envs.compile(e.envs.claims, s.ClaimMappings.Subject.Expression, wantString); err != nil {
		errs = append(errs, fmt.Errorf("claimMappings.subject: %w", err))
	}
	if d := s.ClaimMappings.DisplayName; d != nil && d.Expression != "" {
		if c.displayName, err = e.envs.compile(e.envs.claims, d.Expression, wantString); err != nil {
			errs = append(errs, fmt.Errorf("claimMappings.displayName: %w", err))
		}
	}
	if g := s.ClaimMappings.Groups; g != nil && g.Expression != "" {
		if c.groups, err = e.envs.compile(e.envs.claims, g.Expression, wantStringList); err != nil {
			errs = append(errs, fmt.Errorf("claimMappings.groups: %w", err))
		}
	}
	if s.Login != nil && (s.Login.Name == "" || s.Login.ClientID == "") {
		errs = append(errs, errors.New("login.name and login.clientID are required"))
	}
	return errs
}

func compileServiceAccount(c *compiledPolicy, s v1alpha1.TrustPolicySpec) []error {
	var errs []error
	if s.ServiceAccount == nil {
		return []error{errors.New("serviceAccount is required for type serviceAccount")}
	}
	c.account, c.breakGlass = s.ServiceAccount.Name, s.ServiceAccount.BreakGlass
	if !keys.ValidAccount(c.account) {
		errs = append(errs, errors.New("serviceAccount.name must be a DNS label"))
	}
	if c.breakGlass != (c.account == keys.BreakGlassAccount) {
		errs = append(errs, fmt.Errorf("serviceAccount.breakGlass must be set exactly for the account %q", keys.BreakGlassAccount))
	}
	if s.Issuer != nil || s.ClaimMappings != nil || s.Login != nil || s.SubjectTokenType != "" || s.RequireUniqueJTI || s.GroupLookup != nil {
		errs = append(errs, errors.New("issuer, claimMappings, login, subjectTokenType, requireUniqueJTI and groupLookup are only valid for type oidc"))
	}
	return errs
}

func (e *Engine) compileGrant(g v1alpha1.Grant) (compiledGrant, error) {
	cg := compiledGrant{name: g.Name}
	if len(g.Verbs) == 0 || len(g.InstanceNames) == 0 {
		return cg, errors.New("verbs and instanceNames must not be empty")
	}
	for _, v := range g.Verbs {
		verb, ok := ParseVerb(v)
		if !ok {
			return cg, fmt.Errorf("unknown verb %q", v)
		}
		cg.verbs = append(cg.verbs, verb)
	}
	for _, n := range g.InstanceNames {
		if n == "" {
			return cg, errors.New("instance names must not be empty (use \"*\" for all)")
		}
	}
	cg.instances = slices.Clone(g.InstanceNames)
	if g.MaxTTL != nil {
		if g.MaxTTL.Duration <= 0 || g.MaxTTL.Duration > keys.MaxTokenTTL {
			return cg, fmt.Errorf("maxTTL must be in (0, %s]", keys.MaxTokenTTL)
		}
		cg.maxTTL = g.MaxTTL.Duration
	}
	if g.Condition != "" {
		prg, err := e.envs.compile(e.envs.grant, g.Condition, wantBool)
		if err != nil {
			return cg, fmt.Errorf("condition: %w", err)
		}
		cg.cond = prg
	}
	return cg, nil
}

func httpsURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must be an https URL without user info, query or fragment")
	}
	return nil
}

// builtinBreakGlass is used for the break-glass key when no valid TrustPolicy with
// serviceAccount.breakGlass exists: every verb on every instance name (R-AUTH-12).
func builtinBreakGlass() *compiledPolicy {
	return &compiledPolicy{
		key: "builtin/break-glass", typ: PolicyTypeServiceAccount, account: keys.BreakGlassAccount, breakGlass: true,
		grants: []compiledGrant{{name: "break-glass", verbs: slices.Clone(AllVerbs), instances: []string{"*"}}},
	}
}
