// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/ports"
)

// GroupResolver resolves group membership out of band (TrustPolicy groupLookup), e.g.
// Google Cloud Identity searchDirectGroups. Implementations cache results.
type GroupResolver interface {
	Groups(ctx context.Context, provider string, claims ports.Claims, cacheTTL time.Duration) ([]string, error)
}

// IssuerRegistry is implemented by IdentityProvider adapters that need the per-issuer
// configuration (discovery URL, CA bundle, additional issuers). The engine pushes the
// issuers of all valid policies on every Update.
type IssuerRegistry interface {
	SetIssuers(issuers []v1alpha1.IssuerSpec)
}

// EngineOptions configures the trust-policy engine.
type EngineOptions struct {
	IdentityProvider ports.IdentityProvider
	// Groups is optional; policies with groupLookup are invalid without it.
	Groups GroupResolver
	Clock  ports.Clock
	// InstanceNames are the configured Buildbarn instance names ("*" expands to them).
	InstanceNames []string
	// TokenTTL caps every token (default and maximum 15 min).
	TokenTTL time.Duration
	Limits   CELLimits
	// ReplayCacheSize bounds the jti replay cache (default 100,000 entries).
	ReplayCacheSize int
	Log             *slog.Logger
}

// Engine evaluates TrustPolicies (R-AUTH-2). It is safe for concurrent use; Update
// swaps the compiled policy set atomically.
type Engine struct {
	opts   EngineOptions
	envs   *celEnvs
	snap   atomic.Pointer[policySet]
	replay *replayCache
}

type policySet struct {
	oidc       []*compiledPolicy
	sa         []*compiledPolicy
	breakGlass []*compiledPolicy
}

// NewEngine creates an engine with no policies (only the built-in break-glass policy).
func NewEngine(opts EngineOptions) (*Engine, error) {
	if opts.IdentityProvider == nil || opts.Clock == nil {
		return nil, errors.New("auth: IdentityProvider and Clock are required")
	}
	if opts.TokenTTL <= 0 || opts.TokenTTL > keys.MaxTokenTTL {
		opts.TokenTTL = keys.MaxTokenTTL
	}
	if opts.ReplayCacheSize <= 0 {
		opts.ReplayCacheSize = 100_000
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	envs, err := newCELEnvs(opts.Limits)
	if err != nil {
		return nil, err
	}
	e := &Engine{opts: opts, envs: envs, replay: newReplayCache(opts.ReplayCacheSize)}
	e.snap.Store(&policySet{})
	return e, nil
}

// PolicyStatus is the outcome of compiling one TrustPolicy.
type PolicyStatus struct {
	Namespace, Name string
	Generation      int64
	Valid           bool
	Message         string
}

// Condition renders the `Valid` status condition.
func (s PolicyStatus) Condition(now time.Time) metav1.Condition {
	c := metav1.Condition{Type: "Valid", ObservedGeneration: s.Generation, LastTransitionTime: metav1.NewTime(now)}
	if s.Valid {
		c.Status, c.Reason, c.Message = metav1.ConditionTrue, "Compiled", "all expressions compiled"
	} else {
		msg := s.Message
		if len(msg) > 4096 {
			msg = msg[:4096]
		}
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Invalid", msg
	}
	return c
}

// Update compiles policies and atomically replaces the active set. Invalid policies are
// ignored (their status says why); disabled policies are compiled for their status
// but never match.
func (e *Engine) Update(policies []v1alpha1.TrustPolicy) []PolicyStatus {
	set := &policySet{}
	var statuses []PolicyStatus
	var issuers []v1alpha1.IssuerSpec
	for _, p := range policies {
		st := PolicyStatus{Namespace: p.Namespace, Name: p.Name, Generation: p.Generation}
		cp, err := e.compilePolicy(p)
		if err != nil {
			st.Message = err.Error()
			statuses = append(statuses, st)
			e.opts.Log.Warn("trust policy invalid", "policy", policyKey(p), "error", err)
			continue
		}
		st.Valid = true
		statuses = append(statuses, st)
		if p.Spec.Disabled {
			continue
		}
		switch {
		case cp.typ == PolicyTypeOIDC:
			set.oidc = append(set.oidc, cp)
			issuers = append(issuers, *cp.issuer)
		case cp.breakGlass:
			set.breakGlass = append(set.breakGlass, cp)
		default:
			set.sa = append(set.sa, cp)
		}
	}
	byKey := func(a, b *compiledPolicy) int { return cmp.Compare(a.key, b.key) }
	slices.SortFunc(set.oidc, byKey)
	slices.SortFunc(set.sa, byKey)
	slices.SortFunc(set.breakGlass, byKey)
	if reg, ok := e.opts.IdentityProvider.(IssuerRegistry); ok {
		reg.SetIssuers(issuers)
	}
	e.snap.Store(set)
	return statuses
}

// Principal is the result of a successful exchange.
type Principal struct {
	Subject     string
	DisplayName string
	Groups      []string
	// Issuer is the bounded issuer label (configured issuer URL or "service-account").
	Issuer string
	// Policies are the TrustPolicies that matched; GrantNames the grants that applied.
	Policies   []string
	GrantNames []string
	Grants     Grants
	TTL        time.Duration
	// Session is the `sid` to mint ("" = a fresh random session).
	Session string
}

// allowedAlgs are the asymmetric JWS algorithms accepted from external issuers; `none`
// and HMAC are never accepted.
var allowedAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512, jose.EdDSA,
}

// MaxSubjectTokenBytes bounds a presented subject token.
const MaxSubjectTokenBytes = 16 << 10

// peekIssuer reads `iss` from an unverified token, only to route it to candidate
// policies; nothing else from the unverified payload is used.
func peekIssuer(raw string) (string, error) {
	jws, err := jose.ParseSignedCompact(raw, allowedAlgs)
	if err != nil {
		return "", verifyErr("malformed", err)
	}
	var p struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &p); err != nil || p.Iss == "" {
		return "", verifyErr("malformed", err)
	}
	return p.Iss, nil
}

// ExchangeToken verifies an external token and evaluates the trust policies of its
// issuer. tokenType is the RFC 8693 subject_token_type.
func (e *Engine) ExchangeToken(ctx context.Context, tokenType, raw string) (*Principal, error) {
	if tokenType != TokenTypeIDToken && tokenType != TokenTypeJWT {
		return nil, errf(CodeInvalidRequest, IssuerUnknown, "unsupported subject_token_type")
	}
	if len(raw) == 0 || len(raw) > MaxSubjectTokenBytes {
		return nil, errf(CodeInvalidGrant, IssuerUnknown, "subject token is malformed")
	}
	iss, err := peekIssuer(raw)
	if err != nil {
		return nil, errf(CodeInvalidGrant, IssuerUnknown, "subject token is malformed or uses an unsupported algorithm")
	}
	set := e.snap.Load()
	var candidates []*compiledPolicy
	for _, p := range set.oidc {
		if slices.Contains(p.issuers, iss) && p.tokenType == tokenType {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return nil, errf(CodeInvalidGrant, IssuerUnknown, "no trust policy accepts tokens of this issuer and subject_token_type")
	}
	label := candidates[0].issuer.URL

	type verified struct {
		claims ports.Claims
		err    error
	}
	cache := map[string]verified{}
	var accepted []*evalResult
	var firstDenial, firstVerifyErr *Error
	for _, p := range candidates {
		ck := p.issuer.URL + "\x00" + strings.Join(p.issuer.Audiences, "\x00")
		v, ok := cache[ck]
		if !ok {
			c, err := e.opts.IdentityProvider.Verify(ctx, p.issuer.URL, p.issuer.Audiences, raw)
			v = verified{claims: c, err: err}
			cache[ck] = v
		}
		if v.err != nil {
			if firstVerifyErr == nil {
				firstVerifyErr = verificationError(label, v.err)
			}
			continue
		}
		res, denial := e.evaluate(ctx, p, v.claims, "")
		if denial != nil {
			denial.Issuer = label
			if firstDenial == nil {
				firstDenial = denial
			}
			continue
		}
		accepted = append(accepted, res)
	}
	if len(accepted) == 0 {
		if firstDenial != nil {
			return nil, firstDenial
		}
		return nil, firstVerifyErr
	}
	pr, perr := e.combine(accepted, label)
	if perr != nil {
		return nil, perr
	}
	for _, r := range accepted {
		if r.policy.requireJTI {
			if err := e.checkReplay(iss, r.claims); err != nil {
				err.Issuer, err.Policy = label, r.policy.key
				return nil, err
			}
			break
		}
	}
	return pr, nil
}

func verificationError(label string, err error) *Error {
	var ve *VerifyError
	reason := "subject token could not be verified"
	code := CodeInvalidGrant
	if errors.As(err, &ve) {
		reason = "subject token is invalid: " + ve.Category
		if ve.Category == "unavailable" {
			code, reason = CodeServerError, "issuer keys are temporarily unavailable"
		}
	}
	return &Error{Code: code, Reason: reason, Issuer: label, cause: err}
}

func (e *Engine) checkReplay(iss string, claims ports.Claims) *Error {
	jti, _ := claims["jti"].(string)
	if jti == "" {
		return &Error{Code: CodeInvalidGrant, Reason: "subject token has no jti but the trust policy requires unique jti"}
	}
	var exp time.Time
	switch v := claims["exp"].(type) {
	case int64:
		exp = time.Unix(v, 0)
	case float64:
		exp = time.Unix(int64(v), 0)
	}
	now := e.opts.Clock.Now()
	switch e.replay.record(iss+"\x00"+jti, exp.Add(keys.ClockSkew), now) {
	case replayFresh:
		return nil
	case replayFull:
		return &Error{Code: CodeServerError, Reason: "replay cache is full"}
	}
	return &Error{Code: CodeInvalidGrant, Reason: "subject token was already exchanged (jti replay)"}
}

// ServiceAccountKey is an authenticated service-account key (internal/keys).
type ServiceAccountKey struct {
	ID         string
	Account    string
	BreakGlass bool
}

// ExchangeServiceAccount evaluates the serviceAccount TrustPolicies for an already
// authenticated key (R-AUTH-10). The break-glass key uses the built-in all-verbs policy
// unless a valid TrustPolicy with serviceAccount.breakGlass replaces it (R-AUTH-12).
func (e *Engine) ExchangeServiceAccount(ctx context.Context, k ServiceAccountKey) (*Principal, error) {
	set := e.snap.Load()
	var candidates []*compiledPolicy
	if k.BreakGlass {
		candidates = set.breakGlass
		if len(candidates) == 0 {
			candidates = []*compiledPolicy{builtinBreakGlass()}
		}
	} else {
		for _, p := range set.sa {
			if p.account == k.Account {
				candidates = append(candidates, p)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, errf(CodeAccessDenied, IssuerServiceAccount, "no trust policy for this service account")
	}
	subject := "sa:" + k.Account
	if !keys.ValidSubject(subject) {
		return nil, errf(CodeAccessDenied, IssuerServiceAccount, "invalid service account name")
	}
	claims := ports.Claims{"account": k.Account, "key_id": k.ID, "break_glass": k.BreakGlass}
	var accepted []*evalResult
	var firstDenial *Error
	for _, p := range candidates {
		res, denial := e.evaluate(ctx, p, claims, subject)
		if denial != nil {
			if firstDenial == nil {
				firstDenial = denial
			}
			continue
		}
		accepted = append(accepted, res)
	}
	if len(accepted) == 0 {
		firstDenial.Issuer = IssuerServiceAccount
		return nil, firstDenial
	}
	pr, err := e.combine(accepted, IssuerServiceAccount)
	if err != nil {
		return nil, err
	}
	pr.DisplayName = k.Account
	pr.Session = keys.SessionForKey(k.ID)
	return pr, nil
}

type evalResult struct {
	policy     *compiledPolicy
	claims     ports.Claims
	subject    string
	display    string
	groups     []string
	grants     Grants
	grantNames []string
	ttl        time.Duration
}

// evaluate applies one policy to verified claims. Every CEL error fails closed: a rule
// or mapping error denies the policy, a grant-condition error skips the grant.
func (e *Engine) evaluate(ctx context.Context, p *compiledPolicy, claims ports.Claims, fixedSubject string) (*evalResult, *Error) {
	deny := func(format string, args ...any) *Error {
		return &Error{Code: CodeAccessDenied, Policy: p.key, Reason: fmt.Sprintf(format, args...)}
	}
	cv := map[string]any(claimsValue(map[string]any(claims)).(map[string]any))
	vars := map[string]any{"claims": cv}
	for _, r := range p.rules {
		ok, err := r.prg.evalBool(ctx, vars)
		if err != nil {
			e.opts.Log.Warn("trust policy rule failed to evaluate", "policy", p.key, "error", err)
			return nil, deny("%s (rule could not be evaluated)", r.message)
		}
		if !ok {
			return nil, deny("%s", r.message)
		}
	}
	res := &evalResult{policy: p, claims: claims, subject: fixedSubject, grants: Grants{}, ttl: e.opts.TokenTTL}
	if p.subject != nil {
		mapped, err := p.subject.evalString(ctx, vars)
		if err != nil {
			return nil, deny("subject mapping failed")
		}
		if res.subject, err = keys.NormalizeSubject(mapped); err != nil {
			return nil, deny("subject mapping produced an invalid subject: %v", err)
		}
	}
	if p.displayName != nil {
		d, err := p.displayName.evalString(ctx, vars)
		if err != nil {
			return nil, deny("displayName mapping failed")
		}
		res.display = d
	}
	if p.groups != nil {
		g, err := p.groups.evalStringList(ctx, vars)
		if err != nil {
			return nil, deny("groups mapping failed")
		}
		res.groups = g
	}
	if p.groupLookup != nil {
		ttl := 10 * time.Minute
		if p.groupLookup.CacheTTL != nil {
			ttl = p.groupLookup.CacheTTL.Duration
		}
		g, err := e.opts.Groups.Groups(ctx, p.groupLookup.Provider, claims, ttl)
		if err != nil {
			e.opts.Log.Warn("group lookup failed", "policy", p.key, "error", err)
			return nil, &Error{Code: CodeServerError, Policy: p.key, Reason: "group lookup failed", cause: err}
		}
		res.groups = append(res.groups, g...)
	}
	slices.Sort(res.groups)
	res.groups = slices.Compact(res.groups)

	gvars := map[string]any{"claims": cv, "subject": res.subject, "groups": res.groups}
	for _, g := range p.grants {
		if g.cond != nil {
			ok, err := g.cond.evalBool(ctx, gvars)
			if err != nil {
				e.opts.Log.Warn("grant condition failed to evaluate; grant skipped", "policy", p.key, "grant", g.name, "error", err)
				continue
			}
			if !ok {
				continue
			}
		}
		names := e.expand(g.instances)
		if len(names) == 0 {
			continue
		}
		for _, v := range g.verbs {
			res.grants.add(v, names...)
		}
		res.grantNames = append(res.grantNames, p.key+"/"+g.name)
		if g.maxTTL > 0 && g.maxTTL < res.ttl {
			res.ttl = g.maxTTL
		}
	}
	return res, nil
}

// expand replaces "*" by the configured instance names.
func (e *Engine) expand(names []string) []string {
	var out []string
	for _, n := range names {
		if n == "*" {
			out = append(out, e.opts.InstanceNames...)
		} else {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// combine unions the accepted policies' results: subjects must agree, grants are
// united, the TTL is the minimum.
func (e *Engine) combine(accepted []*evalResult, label string) (*Principal, *Error) {
	pr := &Principal{Subject: accepted[0].subject, Issuer: label, Grants: Grants{}, TTL: e.opts.TokenTTL}
	for _, r := range accepted {
		if r.subject != pr.Subject {
			return nil, &Error{Code: CodeAccessDenied, Issuer: label, Policy: r.policy.key,
				Reason: "trust policies map this token to different subjects"}
		}
		if pr.DisplayName == "" {
			pr.DisplayName = r.display
		}
		pr.Groups = append(pr.Groups, r.groups...)
		pr.Policies = append(pr.Policies, r.policy.key)
		pr.GrantNames = append(pr.GrantNames, r.grantNames...)
		for v, names := range r.grants {
			pr.Grants.add(v, names...)
		}
		if len(r.grantNames) > 0 && r.ttl < pr.TTL {
			pr.TTL = r.ttl
		}
	}
	slices.Sort(pr.Groups)
	pr.Groups = slices.Compact(pr.Groups)
	if pr.Grants.Empty() {
		return nil, &Error{Code: CodeAccessDenied, Issuer: label, Policy: accepted[0].policy.key, Reason: "no grant applies to this identity"}
	}
	return pr, nil
}

// LoginProvider is one entry of the discovery document's identity_providers.
type LoginProvider struct {
	Name             string   `json:"name"`
	Type             string   `json:"type"`
	Issuer           string   `json:"issuer"`
	ClientID         string   `json:"client_id"`
	ClientSecret     string   `json:"client_secret"`
	Scopes           []string `json:"scopes"`
	HostedDomainHint string   `json:"hosted_domain_hint"`
	RedirectPorts    []int32  `json:"redirect_ports"`
}

// GoogleIssuer is Google's OIDC issuer.
const GoogleIssuer = "https://accounts.google.com"

// LoginProviders lists the interactive identity providers of the valid, enabled OIDC
// policies (first policy per provider name, by policy key).
func (e *Engine) LoginProviders() []LoginProvider {
	var out []LoginProvider
	seen := map[string]bool{}
	for _, p := range e.snap.Load().oidc {
		l := p.login
		if l == nil || seen[l.Name] {
			continue
		}
		seen[l.Name] = true
		scopes := slices.Clone(l.Scopes)
		if len(scopes) == 0 {
			scopes = []string{"openid", "email", "profile"}
			if p.issuer.URL != GoogleIssuer {
				scopes = append(scopes, "offline_access")
			}
		}
		ports := slices.Clone(l.RedirectPorts)
		if ports == nil {
			ports = []int32{}
		}
		out = append(out, LoginProvider{
			Name: l.Name, Type: "oidc", Issuer: p.issuer.URL, ClientID: l.ClientID, ClientSecret: l.ClientSecret,
			Scopes: scopes, HostedDomainHint: l.HostedDomainHint, RedirectPorts: ports,
		})
	}
	return out
}

// decodeClaims parses a JSON payload with exact numbers.
func decodeClaims(payload []byte) (ports.Claims, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return ports.Claims(claimsValue(m).(map[string]any)), nil
}
