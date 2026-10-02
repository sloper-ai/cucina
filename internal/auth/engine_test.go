// SPDX-License-Identifier: FSL-1.1-ALv2

package auth_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/auth/oidctest"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

const (
	githubIssuer = "https://token.actions.githubusercontent.com"
	googleAud    = "000000000000-cucinaexample.apps.googleusercontent.com"
)

func loadPolicy(t testing.TB, file string) v1alpha1.TrustPolicy {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "policies", file))
	require.NoError(t, err)
	var p v1alpha1.TrustPolicy
	require.NoError(t, yaml.UnmarshalStrict(b, &p), file)
	return p
}

// celPolicy is a minimal OIDC policy for an issuer with one rule and the given grants.
func celPolicy(name, issuer, rule string, grants ...v1alpha1.Grant) v1alpha1.TrustPolicy {
	if len(grants) == 0 {
		grants = []v1alpha1.Grant{{Name: "base", InstanceNames: []string{"main"}, Verbs: []string{"cas-read"}}}
	}
	spec := v1alpha1.TrustPolicySpec{
		Type:          "oidc",
		Issuer:        &v1alpha1.IssuerSpec{URL: issuer, Audiences: []string{"cucina"}},
		ClaimMappings: &v1alpha1.ClaimMappings{Subject: v1alpha1.CELExpression{Expression: "'test:' + claims.sub"}},
		Grants:        grants,
	}
	if rule != "" {
		spec.ClaimValidationRules = []v1alpha1.ClaimValidationRule{{Expression: rule, Message: name + " rule failed"}}
	}
	return v1alpha1.TrustPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "cucina", Name: name}, Spec: spec}
}

// fixture is an engine with static (offline) issuers: no network, no sleeps.
type fixture struct {
	clock    *keystest.Clock
	idp      *auth.OIDCVerifier
	engine   *auth.Engine
	issuers  map[string]*oidctest.Issuer
	statuses []auth.PolicyStatus
}

func (f *fixture) issuer(url string) *oidctest.Issuer {
	if i, ok := f.issuers[url]; ok {
		return i
	}
	i := oidctest.NewIssuer(url)
	f.issuers[url] = i
	f.idp.AddStaticIssuer(v1alpha1.IssuerSpec{URL: url}, i.KeySet(), []string{"RS256"})
	return i
}

func newFixture(t testing.TB, limits auth.CELLimits, policies ...v1alpha1.TrustPolicy) *fixture {
	t.Helper()
	clock := keystest.NewClock(t0)
	f := &fixture{clock: clock, idp: auth.NewOIDCVerifier(clock), issuers: map[string]*oidctest.Issuer{}}
	for _, p := range policies {
		if p.Spec.Issuer != nil {
			f.issuer(p.Spec.Issuer.URL)
		}
	}
	e, err := auth.NewEngine(auth.EngineOptions{
		IdentityProvider: f.idp, Clock: clock, InstanceNames: []string{"main"}, Limits: limits,
		Log: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	f.engine = e
	f.statuses = e.Update(policies)
	return f
}

const (
	celCostIssuer    = "https://cel-cost.example.com"
	celDynIssuer     = "https://cel-dyn.example.com"
	celNumericIssuer = "https://cel-numeric.example.com"
	celBadIssuer     = "https://cel-compile.example.com"
	celGrantIssuer   = "https://cel-grant.example.com"
	celSplitIssuer   = "https://cel-split.example.com"
	disabledIssuer   = "https://disabled.example.com"
	keycloakIssuer   = "https://keycloak.example.com/realms/engineering"
	untrustedIssuer  = "https://evil.example.com"
)

func standardPolicies(t testing.TB) []v1alpha1.TrustPolicy {
	disabled := celPolicy("disabled", disabledIssuer, "")
	disabled.Spec.Disabled = true
	split1 := celPolicy("split-a", celSplitIssuer, "")
	split2 := celPolicy("split-b", celSplitIssuer, "")
	split2.Spec.ClaimMappings.Subject.Expression = "'other:' + claims.sub"
	return []v1alpha1.TrustPolicy{
		loadPolicy(t, "google-workspace.yaml"),
		loadPolicy(t, "github-sloper-ai-cucina.yaml"),
		loadPolicy(t, "keycloak.yaml"),
		loadPolicy(t, "service-account.yaml"),
		// Nested comprehension: quadratic in len(claims.big).
		celPolicy("cel-cost", celCostIssuer, "claims.big.all(x, claims.big.all(y, x <= y || x > y))"),
		// Type-checks as dyn; the run-time type decides.
		celPolicy("cel-dyn", celDynIssuer, "claims.flag"),
		// Numeric identity claims must not round through float64 before CEL comparison.
		celPolicy("cel-numeric", celNumericIssuer, "claims.repository_id == 9007199254740992"),
		// A string literal is not a bool: rejected at load time.
		celPolicy("cel-compile", celBadIssuer, "'yes'"),
		celPolicy("cel-grant", celGrantIssuer, "",
			v1alpha1.Grant{Name: "base", InstanceNames: []string{"main"}, Verbs: []string{"cas-read"}},
			v1alpha1.Grant{Name: "broken", Condition: "claims.extra.level == 'high'", InstanceNames: []string{"main"}, Verbs: []string{"cas-write"}},
		),
		split1, split2, disabled,
	}
}

type exchange func(f *fixture) (*auth.Principal, error)

func token(issuer, typ string, claims map[string]any, opts ...oidctest.Option) exchange {
	return func(f *fixture) (*auth.Principal, error) {
		return f.engine.ExchangeToken(context.Background(), typ, f.issuer(issuer).Token(claims, opts...))
	}
}

func google(kv ...any) map[string]any {
	return oidctest.With(oidctest.GoogleClaims(auth.GoogleIssuer, googleAud, t0), kv...)
}

func github(kv ...any) map[string]any {
	return oidctest.With(oidctest.GitHubClaims(githubIssuer, "cucina", t0), kv...)
}

func simple(iss string, kv ...any) map[string]any {
	return oidctest.With(map[string]any{"iss": iss, "aud": "cucina", "sub": "u1", "iat": t0.Unix(), "exp": t0.Add(time.Hour).Unix()}, kv...)
}

func verbs(p *auth.Principal) map[auth.Verb][]string {
	out := map[auth.Verb][]string{}
	for v, l := range p.Grants {
		if len(l) > 0 {
			out[v] = l
		}
	}
	return out
}

var mainOnly = []string{"main"}

// TestExchangeTable is the R-TEST-6 "STS/auth" unit table: every R-AUTH acceptance and
// rejection case of token verification and trust-policy evaluation (R-AUTH-2, -5, -7,
// -10, -12), through the public Engine API with offline signature verification.
func TestExchangeTable(t *testing.T) {
	const idTok, jwtTok = auth.TokenTypeIDToken, auth.TokenTypeJWT
	// 1000² iterations exceed the default cost limit while the token stays < 16 KiB.
	big := make([]any, 1000)
	for i := range big {
		big[i] = i
	}
	developer := map[auth.Verb][]string{auth.VerbCASRead: mainOnly, auth.VerbCASWrite: mainOnly, auth.VerbACRead: mainOnly, auth.VerbExecute: mainOnly}
	ciBranch := map[auth.Verb][]string{auth.VerbCASRead: mainOnly, auth.VerbCASWrite: mainOnly, auth.VerbACRead: mainOnly, auth.VerbExecute: mainOnly}
	ciMain := map[auth.Verb][]string{auth.VerbCASRead: mainOnly, auth.VerbCASWrite: mainOnly, auth.VerbACRead: mainOnly, auth.VerbACWrite: mainOnly, auth.VerbExecute: mainOnly}
	readOnly := map[auth.Verb][]string{auth.VerbCASRead: mainOnly, auth.VerbACRead: mainOnly}
	all := map[auth.Verb][]string{}
	for _, v := range auth.AllVerbs {
		all[v] = mainOnly
	}
	replayed := github("jti", "replayed-jti-1")

	rows := []struct {
		name     string
		before   exchange // optional preceding exchange (must succeed)
		do       exchange
		wantCode auth.Code // "" = success
		wantSub  string
		want     map[auth.Verb][]string
		wantTTL  time.Duration
	}{
		// --- Google Workspace (R-AUTH-5) ---
		{name: "google developer", do: token(auth.GoogleIssuer, idTok, google()), wantSub: "google:110248495921238986420", want: developer, wantTTL: 15 * time.Minute},
		{name: "google admin by verified hd-pinned email", do: token(auth.GoogleIssuer, idTok, google("email", "admin@example.com")),
			want: map[auth.Verb][]string{auth.VerbCASRead: mainOnly, auth.VerbCASWrite: mainOnly, auth.VerbACRead: mainOnly, auth.VerbExecute: mainOnly, auth.VerbAdmin: mainOnly}},
		{name: "google email_verified as string true", do: token(auth.GoogleIssuer, idTok, google("email_verified", "true")), want: developer},
		{name: "google scheme-less issuer", do: token(auth.GoogleIssuer, idTok, google("iss", "accounts.google.com")), want: developer},
		{name: "google wrong hd", do: token(auth.GoogleIssuer, idTok, google("hd", "evil.example")), wantCode: auth.CodeAccessDenied},
		{name: "google email domain without hd is not trusted", do: token(auth.GoogleIssuer, idTok, google("hd", nil, "email", "mallory@example.com")), wantCode: auth.CodeAccessDenied},
		{name: "google email_verified false", do: token(auth.GoogleIssuer, idTok, google("email_verified", false)), wantCode: auth.CodeAccessDenied},
		{name: "google email_verified string false", do: token(auth.GoogleIssuer, idTok, google("email_verified", "false")), wantCode: auth.CodeAccessDenied},
		{name: "google email_verified missing (CEL error fails closed)", do: token(auth.GoogleIssuer, idTok, google("email_verified", nil)), wantCode: auth.CodeAccessDenied},
		{name: "google wrong audience", do: token(auth.GoogleIssuer, idTok, google("aud", "other-client.apps.googleusercontent.com")), wantCode: auth.CodeInvalidGrant},
		{name: "google id token sent as jwt type", do: token(auth.GoogleIssuer, jwtTok, google()), wantCode: auth.CodeInvalidGrant},
		// --- generic token verification (R-AUTH-2) ---
		{name: "expired", do: token(auth.GoogleIssuer, idTok, google("exp", t0.Add(-time.Second).Unix())), wantCode: auth.CodeInvalidGrant},
		{name: "missing exp", do: token(auth.GoogleIssuer, idTok, google("exp", nil)), wantCode: auth.CodeInvalidGrant},
		{name: "not yet valid (nbf beyond skew)", do: token(auth.GoogleIssuer, idTok, google("nbf", t0.Add(10*time.Minute).Unix())), wantCode: auth.CodeInvalidGrant},
		{name: "issued in the future (iat beyond skew)", do: token(auth.GoogleIssuer, idTok, google("iat", t0.Add(10*time.Minute).Unix())), wantCode: auth.CodeInvalidGrant},
		{name: "tampered signature", do: token(auth.GoogleIssuer, idTok, google(), oidctest.Tampered()), wantCode: auth.CodeInvalidGrant},
		{name: "alg none", do: token(auth.GoogleIssuer, idTok, google(), oidctest.AlgNone()), wantCode: auth.CodeInvalidGrant},
		{name: "unknown signing key and kid", do: token(auth.GoogleIssuer, idTok, google(), oidctest.SignedByUnknownKey()), wantCode: auth.CodeInvalidGrant},
		{name: "kid not in the issuer's JWKS", do: token(auth.GoogleIssuer, idTok, google(), oidctest.WithKID("not-a-published-kid")), wantCode: auth.CodeInvalidGrant},
		{name: "claims google but signed by another issuer's key", do: token(githubIssuer, idTok, google()), wantCode: auth.CodeInvalidGrant},
		{name: "untrusted issuer (no policy)", do: token(untrustedIssuer, idTok, simple(untrustedIssuer)), wantCode: auth.CodeInvalidGrant},
		{name: "disabled policy is ignored", do: token(disabledIssuer, idTok, simple(disabledIssuer)), wantCode: auth.CodeInvalidGrant},
		// --- GitHub Actions (R-AUTH-7) ---
		{name: "github push to protected main", do: token(githubIssuer, jwtTok, github()), wantSub: "github:1401027334:CI", want: ciMain},
		{name: "github push to a branch: no ac-write", do: token(githubIssuer, jwtTok, github("jti", "j-branch", "ref", "refs/heads/feature")), want: ciBranch},
		{name: "github push to unprotected mainOnly: no ac-write", do: token(githubIssuer, jwtTok, github("jti", "j-unprot", "ref_protected", "false")), want: ciBranch},
		{name: "github workflow_dispatch on mainOnly: no ac-write", do: token(githubIssuer, jwtTok, github("jti", "j-wd", "event_name", "workflow_dispatch")), want: ciBranch},
		{name: "github fork pull request is read-only", do: token(githubIssuer, jwtTok, github("jti", "j-pr", "event_name", "pull_request", "ref", "refs/pull/7/merge", "ref_protected", "false")), want: readOnly},
		{name: "github pull_request_target denied", do: token(githubIssuer, jwtTok, github("jti", "j-prt", "event_name", "pull_request_target")), wantCode: auth.CodeAccessDenied},
		{name: "github event not allow-listed", do: token(githubIssuer, jwtTok, github("jti", "j-ic", "event_name", "issue_comment")), wantCode: auth.CodeAccessDenied},
		{name: "github foreign repository id", do: token(githubIssuer, jwtTok, github("jti", "j-repo", "repository_id", "999999")), wantCode: auth.CodeAccessDenied},
		{name: "github foreign owner id", do: token(githubIssuer, jwtTok, github("jti", "j-own", "repository_owner_id", "1")), wantCode: auth.CodeAccessDenied},
		{name: "github sub is never parsed", do: token(githubIssuer, jwtTok, github("jti", "j-sub", "sub", "repo:sloper-ai/cucina:ref:refs/heads/main", "repository_id", "42", "repository", "evil/cucina")), wantCode: auth.CodeAccessDenied},
		{name: "github reusable workflow from another repository", do: token(githubIssuer, jwtTok, github("jti", "j-wf", "job_workflow_ref", "evil/repo/.github/workflows/x.yml@refs/heads/main")), wantCode: auth.CodeAccessDenied},
		{name: "github self-hosted runner", do: token(githubIssuer, jwtTok, github("jti", "j-sh", "runner_environment", "self-hosted")), wantCode: auth.CodeAccessDenied},
		{name: "github missing claim fails closed", do: token(githubIssuer, jwtTok, github("jti", "j-miss", "repository_id", nil)), wantCode: auth.CodeAccessDenied},
		{name: "github wrong audience", do: token(githubIssuer, jwtTok, github("jti", "j-aud", "aud", "sts.amazonaws.com")), wantCode: auth.CodeInvalidGrant},
		{name: "github replayed jti", before: token(githubIssuer, jwtTok, replayed), do: token(githubIssuer, jwtTok, replayed), wantCode: auth.CodeInvalidGrant},
		{name: "github token without jti", do: token(githubIssuer, jwtTok, github("jti", nil)), wantCode: auth.CodeInvalidGrant},
		{name: "github workflow name is percent-encoded into the subject", do: token(githubIssuer, jwtTok, github("jti", "j-name", "workflow", "Build & test")),
			wantSub: "github:1401027334:Build%20%26%20test", want: ciMain},
		// --- CEL safety (R-AUTH-2) ---
		{name: "CEL within cost limit", do: token(celCostIssuer, idTok, simple(celCostIssuer, "big", big[:3])), want: map[auth.Verb][]string{auth.VerbCASRead: mainOnly}},
		{name: "CEL cost limit exceeded fails closed", do: token(celCostIssuer, idTok, simple(celCostIssuer, "big", big)), wantCode: auth.CodeAccessDenied},
		{name: "CEL dyn rule with bool result", do: token(celDynIssuer, idTok, simple(celDynIssuer, "flag", true)), want: map[auth.Verb][]string{auth.VerbCASRead: mainOnly}},
		{name: "CEL non-bool result at run time fails closed", do: token(celDynIssuer, idTok, simple(celDynIssuer, "flag", "yes")), wantCode: auth.CodeAccessDenied},
		{name: "CEL non-bool rule rejected at load time", do: token(celBadIssuer, idTok, simple(celBadIssuer)), wantCode: auth.CodeInvalidGrant},
		{name: "CEL error in a grant condition skips that grant", do: token(celGrantIssuer, idTok, simple(celGrantIssuer)), want: map[auth.Verb][]string{auth.VerbCASRead: mainOnly}},
		{name: "CEL exact large integer claim matches", do: token(celNumericIssuer, idTok, simple(celNumericIssuer, "repository_id", int64(9007199254740992))), want: map[auth.Verb][]string{auth.VerbCASRead: mainOnly}},
		{name: "CEL large integer claim never rounds into another identity", do: token(celNumericIssuer, idTok, simple(celNumericIssuer, "repository_id", int64(9007199254740993))), wantCode: auth.CodeAccessDenied},
		// --- policy combination ---
		{name: "no grant applies", do: token(keycloakIssuer, idTok, simple(keycloakIssuer, "email_verified", true, "groups", []any{"/other"})), wantCode: auth.CodeAccessDenied},
		{name: "policies mapping different subjects", do: token(celSplitIssuer, idTok, simple(celSplitIssuer)), wantCode: auth.CodeAccessDenied},
		// --- service accounts and break-glass (R-AUTH-10, R-AUTH-12) ---
		{name: "service account key with policy (maxTTL)", do: func(f *fixture) (*auth.Principal, error) {
			return f.engine.ExchangeServiceAccount(context.Background(), auth.ServiceAccountKey{ID: "aaaaaaaaaaaaaaaa", Account: "nightly-cache"})
		}, wantSub: "sa:nightly-cache", wantTTL: 10 * time.Minute,
			want: map[auth.Verb][]string{auth.VerbCASRead: mainOnly, auth.VerbCASWrite: mainOnly, auth.VerbACRead: mainOnly, auth.VerbACWrite: mainOnly}},
		{name: "service account without policy", do: func(f *fixture) (*auth.Principal, error) {
			return f.engine.ExchangeServiceAccount(context.Background(), auth.ServiceAccountKey{ID: "bbbbbbbbbbbbbbbb", Account: "unknown"})
		}, wantCode: auth.CodeAccessDenied},
		{name: "break-glass key uses the built-in all-verbs policy", do: func(f *fixture) (*auth.Principal, error) {
			return f.engine.ExchangeServiceAccount(context.Background(), auth.ServiceAccountKey{ID: "cccccccccccccccc", Account: keys.BreakGlassAccount, BreakGlass: true})
		}, wantSub: "sa:break-glass", want: all},
		{name: "regular key for the break-glass account gets nothing", do: func(f *fixture) (*auth.Principal, error) {
			return f.engine.ExchangeServiceAccount(context.Background(), auth.ServiceAccountKey{ID: "dddddddddddddddd", Account: keys.BreakGlassAccount})
		}, wantCode: auth.CodeAccessDenied},
	}

	f := newFixture(t, auth.CELLimits{}, standardPolicies(t)...)
	for _, s := range f.statuses {
		assert.Equal(t, s.Name != "cel-compile", s.Valid, "policy %s: %s", s.Name, s.Message)
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if r.before != nil {
				_, err := r.before(f)
				require.NoError(t, err)
			}
			p, err := r.do(f)
			if r.wantCode != "" {
				require.Error(t, err)
				assert.Equal(t, r.wantCode, auth.AsError(err).Code, "%v", err)
				assert.Nil(t, p)
				return
			}
			require.NoError(t, err)
			if r.wantSub != "" {
				assert.Equal(t, r.wantSub, p.Subject)
			}
			if r.want != nil {
				assert.Equal(t, r.want, verbs(p))
			}
			if r.wantTTL != 0 {
				assert.Equal(t, r.wantTTL, p.TTL)
			}
			assert.True(t, keys.ValidSubject(p.Subject), p.Subject)
		})
	}
}

// TestCELTimeoutFailsClosed guards R-AUTH-2: no grants after cancellation or budget
// expiry, even for expressions too short to reach a cooperative interrupt. Virtual
// time replaces the old assumption that a three-element rule must exceed 1ns of wall
// time; the 1ns budget is unchanged, but its expiry is now known rather than raced.
func TestCELTimeoutFailsClosed(t *testing.T) {
	for name, expression := range map[string]string{
		"constant":      "true",
		"comprehension": "claims.big.all(x, x >= 0)",
	} {
		t.Run(name, func(t *testing.T) {
			for _, state := range []string{"live", "canceled", "expired parent", "deadline before cancellation delivery", "configured rule deadline"} {
				t.Run(state, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						limits := auth.CELLimits{Timeout: time.Nanosecond, InterruptCheckFrequency: 1}
						f := newFixture(t, limits, celPolicy("bounded", celCostIssuer, expression))
						raw := f.issuer(celCostIssuer).Token(simple(celCostIssuer, "big", []any{1, 2, 3}))
						ctx := context.Background()
						switch state {
						case "canceled":
							canceled, cancel := context.WithCancel(ctx)
							cancel()
							ctx = canceled
							require.ErrorIs(t, ctx.Err(), context.Canceled)
						case "expired parent":
							expired, cancel := context.WithTimeout(ctx, limits.Timeout)
							defer cancel()
							<-expired.Done() // advances virtual time, not a wall-clock sleep
							ctx = expired
							require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
						case "deadline before cancellation delivery":
							pending, cancel := context.WithCancel(ctx)
							defer cancel()
							ctx = pendingDeadlineContext{Context: pending, at: time.Now().Add(-limits.Timeout)}
							require.NoError(t, ctx.Err(), "the cancellation callback has not run")
						case "configured rule deadline":
							// Pause after WithTimeout chooses the rule's deadline. The parent
							// has no deadline and remains live: only the configured rule budget
							// can deny this exchange (removing WithTimeout must fail this row).
							ctx = delayedDeadlineContext{Context: ctx, delay: 2 * limits.Timeout}
						}
						p, err := f.engine.ExchangeToken(ctx, auth.TokenTypeIDToken, raw)
						if state == "live" {
							require.NoError(t, err)
							assert.Equal(t, mainOnly, p.Grants[auth.VerbCASRead])
							return
						}
						if state == "configured rule deadline" {
							require.NoError(t, ctx.Err(), "parent cancellation is not the cause")
						}
						require.Error(t, err)
						assert.Nil(t, p)
						assert.Equal(t, auth.CodeAccessDenied, auth.AsError(err).Code)
					})
				})
			}
		})
	}
}

// pendingDeadlineContext models an elapsed deadline whose asynchronous cancellation
// has not been delivered yet: Err is still nil and Done remains open until cancel.
type pendingDeadlineContext struct {
	context.Context
	at time.Time
}

func (c pendingDeadlineContext) Deadline() (time.Time, bool) { return c.at, true }

// delayedDeadlineContext models a scheduling pause during deadline lookup, after
// context.WithTimeout has selected its deadline. Used only inside synctest so the
// pause advances virtual time. It preserves the parent's cancellation semantics.
type delayedDeadlineContext struct {
	context.Context
	delay time.Duration
}

func (c delayedDeadlineContext) Deadline() (time.Time, bool) {
	<-time.After(c.delay)
	return c.Context.Deadline()
}

// TestTokenTTLCap guards R-AUTH-3: whatever the configuration says, tokens live at most
// 15 minutes.
func TestTokenTTLCap(t *testing.T) {
	clock := keystest.NewClock(t0)
	idp := auth.NewOIDCVerifier(clock)
	e, err := auth.NewEngine(auth.EngineOptions{IdentityProvider: idp, Clock: clock, InstanceNames: []string{"main"},
		TokenTTL: time.Hour, Log: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	e.Update([]v1alpha1.TrustPolicy{loadPolicy(t, "service-account.yaml")})
	p, err := e.ExchangeServiceAccount(context.Background(), auth.ServiceAccountKey{ID: "aaaaaaaaaaaaaaaa", BreakGlass: true, Account: keys.BreakGlassAccount})
	require.NoError(t, err)
	assert.Equal(t, keys.MaxTokenTTL, p.TTL)
}

// TestReplayCacheFailsClosed guards R-AUTH-7's jti replay cache: when it is full of
// unexpired entries it refuses new exchanges instead of evicting (an evicted entry would
// let that token be replayed), and frees space as tokens expire.
func TestReplayCacheFailsClosed(t *testing.T) {
	clock := keystest.NewClock(t0)
	idp := auth.NewOIDCVerifier(clock)
	gh := oidctest.NewIssuer(githubIssuer)
	idp.AddStaticIssuer(v1alpha1.IssuerSpec{URL: githubIssuer}, gh.KeySet(), []string{"RS256"})
	e, err := auth.NewEngine(auth.EngineOptions{IdentityProvider: idp, Clock: clock, InstanceNames: []string{"main"},
		ReplayCacheSize: 1, Log: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	e.Update([]v1alpha1.TrustPolicy{loadPolicy(t, "github-sloper-ai-cucina.yaml")})
	exchange := func(jti string) error {
		_, err := e.ExchangeToken(context.Background(), auth.TokenTypeJWT, gh.Token(oidctest.With(oidctest.GitHubClaims(githubIssuer, "cucina", clock.Now()), "jti", jti)))
		return err
	}
	require.NoError(t, exchange("first"))
	err = exchange("second")
	require.Error(t, err)
	assert.Equal(t, auth.CodeServerError, auth.AsError(err).Code, "full cache: refuse, never evict")
	clock.Advance(5*time.Minute + keys.ClockSkew)
	assert.NoError(t, exchange("third"), "expired entries free the cache")
}

// TestPolicyStatus guards load-time validation (R-AUTH-2): invalid policies, including
// ones exceeding default or configured CEL size/nesting ceilings, get Valid=False
// with a reason and never match.
func TestPolicyStatus(t *testing.T) {
	bad := []v1alpha1.TrustPolicy{
		celPolicy("unknown-verb", "https://a.example.com", "", v1alpha1.Grant{InstanceNames: mainOnly, Verbs: []string{"write"}}),
		celPolicy("syntax", "https://b.example.com", "claims.sub =="),
		celPolicy("subject-not-string", "https://c.example.com", ""),
		celPolicy("ttl-too-long", "https://d.example.com", "", v1alpha1.Grant{InstanceNames: mainOnly, Verbs: []string{"cas-read"}, MaxTTL: &metav1.Duration{Duration: time.Hour}}),
		celPolicy("http-issuer", "http://e.example.com", ""),
		celPolicy("rule-uses-subject", "https://f.example.com", "subject == 'x'"),
		celPolicy("default-expression-size", "https://limits.example.com", "'"+strings.Repeat("x", 4097)+"'.size() > 0"),
		celPolicy("explicit-expression-size", "https://limits.example.com", "'"+strings.Repeat("x", 64)+"'.size() > 0"),
		celPolicy("default-nesting", "https://limits.example.com", strings.Repeat("(", 80)+"true"+strings.Repeat(")", 80)),
		celPolicy("explicit-nesting", "https://limits.example.com", strings.Repeat("(", 8)+"true"+strings.Repeat(")", 8)),
	}
	bad[2].Spec.ClaimMappings.Subject.Expression = "claims.sub == 'x'"
	groupLookup := celPolicy("group-lookup-without-resolver", "https://g.example.com", "")
	groupLookup.Spec.GroupLookup = &v1alpha1.GroupLookupSpec{Provider: "cloud-identity"}
	bad = append(bad, groupLookup)
	limits := map[string]auth.CELLimits{
		"explicit-expression-size": {MaxExpressionLen: 32},
		"explicit-nesting":         {MaxNesting: 4},
	}
	for _, p := range bad {
		t.Run(p.Name, func(t *testing.T) {
			f := newFixture(t, limits[p.Name], p)
			require.Len(t, f.statuses, 1)
			s := f.statuses[0]
			c := s.Condition(t0)
			assert.False(t, s.Valid, s.Name)
			assert.Equal(t, metav1.ConditionFalse, c.Status, s.Name)
			assert.NotEmpty(t, c.Message, s.Name)
		})
	}
}

// TestSamplePoliciesCompile guards the shipped samples (R-AUTH-7, R-AUTH-12): every file
// under testdata/policies is a valid TrustPolicy.
func TestSamplePoliciesCompile(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "policies", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	var ps []v1alpha1.TrustPolicy
	for _, f := range files {
		ps = append(ps, loadPolicy(t, filepath.Base(f)))
	}
	f := newFixture(t, auth.CELLimits{}, ps...)
	for _, s := range f.statuses {
		assert.True(t, s.Valid, "%s: %s", s.Name, s.Message)
	}
	names := []string{}
	for _, l := range f.engine.LoginProviders() {
		names = append(names, l.Name)
		assert.NotNil(t, l.RedirectPorts)
	}
	slices.Sort(names)
	assert.Equal(t, []string{"dex", "entra", "google", "keycloak", "okta"}, names)
	for _, l := range f.engine.LoginProviders() {
		switch l.Name {
		case "google": // Google issues refresh tokens to installed apps without offline_access
			assert.Equal(t, []string{"openid", "email", "profile"}, l.Scopes)
		case "keycloak": // other providers need offline_access (R-AUTH-5)
			assert.Equal(t, []string{"openid", "email", "profile", "offline_access"}, l.Scopes)
		}
	}
}
