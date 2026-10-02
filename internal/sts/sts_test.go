// SPDX-License-Identifier: FSL-1.1-ALv2

package sts_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/auth/oidctest"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
	"github.com/sloper-ai/cucina/internal/sts"
)

const stsURL = "https://cucina.example.com"

func samplePolicy(t testing.TB, file, issuerURL, caPEM string) v1alpha1.TrustPolicy {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "auth", "testdata", "policies", file))
	require.NoError(t, err)
	var p v1alpha1.TrustPolicy
	require.NoError(t, yaml.UnmarshalStrict(b, &p))
	p.Spec.Issuer.URL, p.Spec.Issuer.AdditionalIssuers, p.Spec.Issuer.CertificateAuthority = issuerURL, nil, caPEM
	return p
}

type env struct {
	t        *testing.T
	clock    *keystest.Clock
	mgr      *keys.Manager
	srv      *sts.Server
	http     *httptest.Server
	reg      *prometheus.Registry
	audit    *bytes.Buffer
	logs     *bytes.Buffer
	google   *oidctest.Issuer
	github   *oidctest.Issuer
	idps     *oidctest.Server
	engine   *auth.Engine
	breakKey string
}

func newEnv(t *testing.T, ratePerMinute int, configure ...func(*config.Controller)) *env {
	ctx := context.Background()
	e := &env{t: t, clock: keystest.NewClock(time.Now().Truncate(time.Second)), audit: &bytes.Buffer{}, logs: &bytes.Buffer{}}
	e.idps = oidctest.NewServer(t)
	e.google, e.github = e.idps.AddIssuer("google"), e.idps.AddIssuer("github")

	objs := keystest.NewObjects()
	cfg := config.Controller{
		InstanceNames: []string{"main"},
		Endpoints:     config.Endpoints{STSURL: stsURL + "/", ClientEndpoint: "grpcs://cucina.example.com:443", ManagementURL: "cucina.example.com:8444"},
		Auth: config.Auth{
			SigningKeySecret: "sk", JWKSConfigMap: "jwks", DenyListConfigMap: "deny", ServiceKeysSecret: "svc",
			BreakGlassKeySecret: "bg", TokenTTL: config.Duration{Duration: 15 * time.Minute}, Audience: "buildbarn",
			KeyRotationPublishLead: config.Duration{Duration: 10 * time.Minute}, RateLimitPerMinute: ratePerMinute,
		},
	}
	for _, apply := range configure {
		apply(&cfg)
	}
	require.NoError(t, keys.EnsureSigningKeysIn(ctx, objs, cfg.Auth, e.clock, nil))
	require.NoError(t, keys.EnsureBreakGlassIn(ctx, objs, cfg.Auth, e.clock, nil))
	bg, err := objs.GetSecret(ctx, "bg")
	require.NoError(t, err)
	e.breakKey = string(bg.Data["key"])
	e.mgr, err = keys.NewManager(objs, cfg.Auth, cfg.Endpoints.STSURL, keys.Options{Clock: e.clock})
	require.NoError(t, err)
	require.NoError(t, e.mgr.Start(ctx))

	logger := slog.New(slog.NewJSONHandler(e.logs, nil))
	engine, err := auth.NewEngine(auth.EngineOptions{IdentityProvider: auth.NewOIDCVerifier(e.clock), Clock: e.clock, InstanceNames: cfg.InstanceNames, Log: logger})
	require.NoError(t, err)
	google := samplePolicy(t, "google-workspace.yaml", e.google.URL, e.idps.CAPEM())
	github := samplePolicy(t, "github-sloper-ai-cucina.yaml", e.github.URL, e.idps.CAPEM())
	sa := v1alpha1.TrustPolicy{}
	sab, err := os.ReadFile(filepath.Join("..", "auth", "testdata", "policies", "service-account.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.UnmarshalStrict(sab, &sa))
	for _, s := range engine.Update([]v1alpha1.TrustPolicy{google, github, sa}) {
		require.True(t, s.Valid, s.Message)
	}
	e.engine = engine
	e.reg = prometheus.NewRegistry()
	e.srv, err = sts.New(sts.Deps{Config: cfg, Engine: engine, Keys: e.mgr, Clock: e.clock,
		Log: logger, Audit: slog.New(slog.NewJSONHandler(e.audit, nil)), Registerer: e.reg})
	require.NoError(t, err)
	e.http = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.http.Close)
	return e
}

type result struct {
	code int
	body map[string]any
	hdr  http.Header
}

func (e *env) post(body string, contentType string) result {
	e.t.Helper()
	resp, err := http.Post(e.http.URL+"/token", contentType, strings.NewReader(body))
	require.NoError(e.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return result{resp.StatusCode, m, resp.Header}
}

func (e *env) exchange(token, tokenType string, extra ...string) result {
	f := url.Values{"grant_type": {sts.GrantTypeTokenExchange}, "subject_token": {token}, "subject_token_type": {tokenType}}
	for i := 0; i+1 < len(extra); i += 2 {
		f.Add(extra[i], extra[i+1])
	}
	return e.post(f.Encode(), "application/x-www-form-urlencoded")
}

func (e *env) googleClaims(kv ...any) map[string]any {
	return oidctest.With(oidctest.GoogleClaims(e.google.URL, "000000000000-cucinaexample.apps.googleusercontent.com", e.clock.Now()), kv...)
}

func (e *env) githubClaims(kv ...any) map[string]any {
	return oidctest.With(oidctest.GitHubClaims(e.github.URL, "cucina", e.clock.Now()), kv...)
}

// TestTokenExchangeEndToEnd is the R-TEST-6 "STS/auth" integration test: Google- and
// GitHub-shaped issuers served over HTTPS with discovery and JWKS, verified by the
// go-oidc adapter, exchanged at POST /token (RFC 8693 response and RFC 6749 errors,
// R-AUTH-1), minted as Cucina JWTs (R-AUTH-3), plus service keys and break-glass
// (R-AUTH-10/-12), renewal refusal for revoked principals (R-AUTH-9), metrics and an
// audit log that never contains token material.
func TestTokenExchangeEndToEnd(t *testing.T) {
	e := newEnv(t, 1000)
	var presented []string
	ex := func(token, typ string, extra ...string) result {
		presented = append(presented, token)
		return e.exchange(token, typ, extra...)
	}
	idTok, jwtTok := auth.TokenTypeIDToken, auth.TokenTypeJWT
	var minted []string
	accept := func(t *testing.T, r result) *keys.Claims {
		t.Helper()
		code, body, hdr := r.code, r.body, r.hdr
		require.Equal(t, http.StatusOK, code, "%v", body)
		assert.Equal(t, []string{"access_token", "expires_in", "issued_token_type", "token_type"}, sortedKeys(body))
		assert.Equal(t, auth.TokenTypeAccessToken, body["issued_token_type"])
		assert.Equal(t, "Bearer", body["token_type"])
		assert.Equal(t, "no-store", hdr.Get("Cache-Control"))
		raw := body["access_token"].(string)
		minted = append(minted, raw)
		c, err := e.mgr.Verifier.Verify(raw)
		require.NoError(t, err)
		assert.InDelta(t, float64(c.Expiry-c.IssuedAt), body["expires_in"], 0)
		return c
	}
	reject := func(t *testing.T, wantStatus int, wantCode string, r result) {
		t.Helper()
		code, body := r.code, r.body
		assert.Equal(t, wantStatus, code, "%v", body)
		assert.Equal(t, wantCode, body["error"])
		assert.NotEmpty(t, body["error_description"])
		for _, tok := range presented {
			assert.NotContains(t, body["error_description"], tok)
		}
	}

	t.Run("google ID token", func(t *testing.T) {
		c := accept(t, ex(e.google.Token(e.googleClaims()), idTok))
		assert.Equal(t, "google:110248495921238986420", c.Subject)
		assert.Equal(t, "alice@example.com", c.Name, "display name from claimMappings.displayName")
		assert.Equal(t, int64(900), c.Expiry-c.IssuedAt)
		assert.Equal(t, []string{"main"}, c.Cucina.Execute)
		assert.Empty(t, c.Cucina.ACWrite)
	})
	t.Run("github push to main", func(t *testing.T) {
		c := accept(t, ex(e.github.Token(e.githubClaims()), jwtTok))
		assert.Equal(t, "github:1401027334:CI", c.Subject)
		assert.Equal(t, []string{"main"}, c.Cucina.ACWrite)
	})
	t.Run("github fork pull request is read-only", func(t *testing.T) {
		c := accept(t, ex(e.github.Token(e.githubClaims("jti", "pr-1", "event_name", "pull_request", "ref", "refs/pull/1/merge")), jwtTok))
		assert.Empty(t, c.Cucina.ACWrite)
		assert.Empty(t, c.Cucina.CASWrite)
		assert.Equal(t, []string{"main"}, c.Cucina.ACRead)
	})
	t.Run("issuer key rotation is picked up from the JWKS", func(t *testing.T) {
		before := e.idps.JWKSFetches("google")
		e.google.RotateKey()
		accept(t, ex(e.google.Token(e.googleClaims()), idTok))
		assert.Greater(t, e.idps.JWKSFetches("google"), before)
	})
	t.Run("audience down-scopes to one instance name", func(t *testing.T) {
		accept(t, ex(e.google.Token(e.googleClaims()), idTok, "audience", "main"))
		reject(t, http.StatusBadRequest, "invalid_request", ex(e.google.Token(e.googleClaims()), idTok, "audience", "nope"))
	})
	t.Run("break-glass key", func(t *testing.T) {
		c := accept(t, ex(e.breakKey, auth.TokenTypeServiceKey))
		assert.Equal(t, "sa:break-glass", c.Subject)
		assert.Equal(t, []string{"main"}, c.Cucina.Admin)
		assert.Equal(t, keys.SessionForKey(strings.Split(e.breakKey, "_")[2]), c.Session)
	})
	t.Run("service key lifecycle", func(t *testing.T) {
		key, info, err := e.mgr.CreateServiceKey(context.Background(), "nightly-cache", "", 0, "test")
		require.NoError(t, err)
		c := accept(t, ex(key, auth.TokenTypeAccessToken))
		assert.Equal(t, "sa:nightly-cache", c.Subject)
		assert.Equal(t, int64(600), c.Expiry-c.IssuedAt, "the policy's maxTTL caps the token")
		require.NoError(t, e.mgr.RevokeServiceKey(context.Background(), info.ID, "test"))
		reject(t, http.StatusBadRequest, "invalid_grant", ex(key, auth.TokenTypeAccessToken))
		_, err = e.mgr.Verifier.Verify(minted[len(minted)-1])
		assert.ErrorIs(t, err, keys.ErrTokenRevoked, "outstanding tokens of a revoked key are deny-listed")
	})
	t.Run("revoked principal cannot renew", func(t *testing.T) {
		_, _, err := e.mgr.Revoke(context.Background(), keys.RevokeRequest{Kind: keys.RevokeSubject, Value: "google:110248495921238986420", Actor: "test"})
		require.NoError(t, err)
		reject(t, http.StatusForbidden, "access_denied", ex(e.google.Token(e.googleClaims()), idTok))
		require.NoError(t, e.mgr.Revocations.Remove(context.Background(), keys.RevokeSubject, "google:110248495921238986420"))
	})

	for _, r := range []struct {
		name       string
		do         func() result
		wantStatus int
		wantCode   string
	}{
		{"wrong hd", func() result {
			return ex(e.google.Token(e.googleClaims("hd", "evil.example")), idTok)
		}, http.StatusForbidden, "access_denied"},
		{"unverified e-mail", func() result {
			return ex(e.google.Token(e.googleClaims("email_verified", "false")), idTok)
		}, http.StatusForbidden, "access_denied"},
		{"wrong audience", func() result {
			return ex(e.google.Token(e.googleClaims("aud", "someone-else")), idTok)
		}, http.StatusBadRequest, "invalid_grant"},
		{"expired", func() result {
			return ex(e.google.Token(e.googleClaims("exp", e.clock.Now().Add(-time.Minute).Unix())), idTok)
		}, http.StatusBadRequest, "invalid_grant"},
		{"foreign repository id", func() result {
			return ex(e.github.Token(e.githubClaims("jti", "x1", "repository_id", "1")), jwtTok)
		}, http.StatusForbidden, "access_denied"},
		{"pull_request_target", func() result {
			return ex(e.github.Token(e.githubClaims("jti", "x2", "event_name", "pull_request_target")), jwtTok)
		}, http.StatusForbidden, "access_denied"},
		{"tampered signature", func() result {
			return ex(e.google.Token(e.googleClaims(), oidctest.Tampered()), idTok)
		}, http.StatusBadRequest, "invalid_grant"},
		{"alg none", func() result {
			return ex(e.google.Token(e.googleClaims(), oidctest.AlgNone()), idTok)
		}, http.StatusBadRequest, "invalid_grant"},
		{"unknown kid", func() result {
			return ex(e.google.Token(e.googleClaims(), oidctest.SignedByUnknownKey()), idTok)
		}, http.StatusBadRequest, "invalid_grant"},
		{"untrusted issuer", func() result {
			other := e.idps.AddIssuer("other")
			return ex(other.Token(oidctest.GoogleClaims(other.URL, "x", e.clock.Now())), idTok)
		}, http.StatusBadRequest, "invalid_grant"},
		{"wrong service key", func() result {
			return ex(e.breakKey[:len(e.breakKey)-2]+"zz", auth.TokenTypeServiceKey)
		}, http.StatusBadRequest, "invalid_grant"},
		{"service key with an id_token type", func() result {
			return ex(e.breakKey, idTok)
		}, http.StatusBadRequest, "invalid_request"},
		{"missing subject_token", func() result {
			return e.post(url.Values{"grant_type": {sts.GrantTypeTokenExchange}, "subject_token_type": {idTok}}.Encode(), "application/x-www-form-urlencoded")
		}, http.StatusBadRequest, "invalid_request"},
		{"unsupported subject_token_type", func() result {
			return ex(e.google.Token(e.googleClaims()), "urn:example:saml")
		}, http.StatusBadRequest, "invalid_request"},
		{"wrong grant_type", func() result {
			return e.post(url.Values{"grant_type": {"client_credentials"}}.Encode(), "application/x-www-form-urlencoded")
		}, http.StatusBadRequest, "unsupported_grant_type"},
		{"duplicate parameter", func() result {
			return e.post("grant_type="+url.QueryEscape(sts.GrantTypeTokenExchange)+"&subject_token=a&subject_token=b&subject_token_type=x", "application/x-www-form-urlencoded")
		}, http.StatusBadRequest, "invalid_request"},
		{"JSON body", func() result {
			return e.post(`{"grant_type":"`+sts.GrantTypeTokenExchange+`"}`, "application/json")
		}, http.StatusBadRequest, "invalid_request"},
		{"actor token", func() result {
			return ex(e.google.Token(e.googleClaims()), idTok, "actor_token", "x", "actor_token_type", idTok)
		}, http.StatusBadRequest, "invalid_request"},
	} {
		t.Run(r.name, func(t *testing.T) {
			reject(t, r.wantStatus, r.wantCode, r.do())
		})
	}

	t.Run("parameters in the URL are refused", func(t *testing.T) {
		resp, err := http.Post(e.http.URL+"/token?subject_token=abc", "application/x-www-form-urlencoded", strings.NewReader("grant_type=x"))
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		resp, err = http.Get(e.http.URL + "/token")
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})

	t.Run("discovery, JWKS and probes", func(t *testing.T) {
		var doc map[string]any
		getJSON(t, e.http.URL+"/.well-known/cucina-configuration", &doc)
		assert.InDelta(t, float64(1), doc["version"], 0)
		assert.Equal(t, stsURL, doc["issuer"])
		assert.Equal(t, stsURL+"/token", doc["token_endpoint"])
		assert.Equal(t, stsURL+"/jwks.json", doc["jwks_uri"])
		assert.Equal(t, map[string]any{"remote_execution": "grpcs://cucina.example.com:443", "instance_name": "main", "management": "cucina.example.com:8444"}, doc["endpoints"])
		idps := doc["identity_providers"].([]any)
		require.Len(t, idps, 1)
		g := idps[0].(map[string]any)
		assert.Equal(t, "google", g["name"])
		assert.Equal(t, e.google.URL, g["issuer"])
		assert.Equal(t, "000000000000-cucinaexample.apps.googleusercontent.com", g["client_id"])
		assert.Equal(t, []any{"openid", "email", "profile", "offline_access"}, g["scopes"], "non-Google issuer URL in this test")
		assert.Equal(t, []any{}, g["redirect_ports"])

		resp, err := http.Get(e.http.URL + "/jwks.json")
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		want, _ := e.mgr.Ring.KeySet().JWKSJSON()
		assert.JSONEq(t, string(want), string(body))
		assert.NotContains(t, string(body), `"d":`, "no private key material")
		for _, p := range []string{"/-/healthy", "/-/ready"} {
			resp, err := http.Get(e.http.URL + p)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode, p)
		}
	})

	t.Run("metrics", func(t *testing.T) {
		problems, err := testutil.GatherAndLint(e.reg)
		require.NoError(t, err)
		assert.Empty(t, problems)
		mfs, err := e.reg.Gather()
		require.NoError(t, err)
		labels := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != "cucina_sts_exchanges_total" {
				continue
			}
			for _, m := range mf.GetMetric() {
				var issuer, result string
				for _, l := range m.GetLabel() {
					switch l.GetName() {
					case "issuer":
						issuer = l.GetValue()
					case "result":
						result = l.GetValue()
					}
				}
				labels[issuer+"|"+result] += m.GetCounter().GetValue()
			}
		}
		assert.InDelta(t, float64(3), labels[e.google.URL+"|ok"], 0, "%v", labels)
		assert.InDelta(t, float64(2), labels[e.github.URL+"|ok"], 0, "%v", labels)
		assert.InDelta(t, float64(2), labels["service-account|ok"], 0, "%v", labels)
		assert.Positive(t, labels[e.google.URL+"|access_denied"], "%v", labels)
		for k := range labels {
			issuer, _, _ := strings.Cut(k, "|")
			assert.Contains(t, []string{e.google.URL, e.github.URL, "service-account", "unknown"}, issuer, "issuer label must be bounded")
		}
		var ttl *dto.Histogram
		for _, mf := range mfs {
			if mf.GetName() == "cucina_sts_token_ttl_seconds" {
				ttl = mf.GetMetric()[0].GetHistogram()
			}
		}
		require.NotNil(t, ttl)
		assert.Equal(t, uint64(7), ttl.GetSampleCount())
	})

	t.Run("audit log has every exchange and no token material", func(t *testing.T) {
		assert.Contains(t, e.audit.String(), `"detail":"service key revoked"`, "internal cause of a refused service key is audited")
		lines := strings.Split(strings.TrimSpace(e.audit.String()), "\n")
		assert.GreaterOrEqual(t, len(lines), len(presented))
		for _, l := range lines {
			var rec map[string]any
			require.NoError(t, json.Unmarshal([]byte(l), &rec))
			assert.Equal(t, "sts.exchange", rec["event"])
			assert.NotEmpty(t, rec["result"])
		}
		all := e.audit.String() + e.logs.String()
		for _, tok := range append(presented, minted...) {
			assert.NotContains(t, all, tok)
			if len(tok) > 40 {
				assert.NotContains(t, all, tok[len(tok)-40:], "no token fragment (signature)")
			}
		}
	})
}

func getJSON(t *testing.T, u string, v any) {
	t.Helper()
	resp, err := http.Get(u)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(v))
}

func sortedKeys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestDiscoveryTransportAliases guards R-DATA-4/R-AUTH-1: discovery reached through an
// explicitly configured transport authority must not send the client's token back
// over the public route. Neither Host injection nor forwarded headers may invent an
// endpoint or change the canonical issuer or the other discovery fields.
func TestDiscoveryTransportAliases(t *testing.T) {
	const private = "https://sts.private.example.com:8443"
	longestDNS := strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("z", 61)
	e := newEnv(t, 1000, func(c *config.Controller) {
		c.Endpoints.STSAliases = []string{
			private + "/", "https://DEFAULT.example.com:443", "https://[2001:db8::1]:443",
			"https://[2001:db8::2]:8443", "https://a0-z9.private.example.com:1",
			"https://z9-a0.private.example.com:65535", "https://" + longestDNS,
		}
	})
	discover := func(host, forwardedHost string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, stsURL+"/.well-known/cucina-configuration", nil)
		req.Host = host
		if forwardedHost != "" {
			req.Header.Set("X-Forwarded-Host", forwardedHost)
			req.Header.Set("X-Forwarded-Proto", "http")
			req.Header.Set("Forwarded", "host="+forwardedHost+";proto=http")
		}
		rr := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc))
		return doc
	}
	public := discover("cucina.example.com", "")
	for _, tc := range []struct {
		name, host, forwardedHost, origin string
	}{
		{"public unchanged", "cucina.example.com", "", stsURL},
		{"private origin", "sts.private.example.com:8443", "", private},
		{"host casing", "STS.PRIVATE.EXAMPLE.COM:8443", "", private},
		{"default HTTPS port omitted", "default.example.com", "", "https://default.example.com"},
		{"explicit default HTTPS port", "default.example.com:443", "", "https://default.example.com"},
		{"IPv6 normalized", "[2001:0db8:0:0:0:0:0:1]:443", "", "https://[2001:db8::1]"},
		{"IPv6 nondefault port", "[2001:db8::2]:8443", "", "https://[2001:db8::2]:8443"},
		{"DNS digits and hyphen with minimum port", "A0-Z9.private.example.com:1", "", "https://a0-z9.private.example.com:1"},
		{"maximum port", "z9-a0.private.example.com:65535", "", "https://z9-a0.private.example.com:65535"},
		{"maximum DNS lengths", longestDNS, "", "https://" + longestDNS},
		{"foreign Host", "attacker.example.com", "", stsURL},
		{"alias-looking suffix", "sts.private.example.com.attacker.example.com:8443", "", stsURL},
		{"wrong port", "sts.private.example.com:9443", "", stsURL},
		{"forwarded alias on foreign Host", "attacker.example.com", "sts.private.example.com:8443", stsURL},
		{"forwarded alias on public Host", "cucina.example.com", "sts.private.example.com:8443", stsURL},
		{"forwarded foreign Host on alias", "sts.private.example.com:8443", "attacker.example.com", private},
		{"Host is not a URL", "https://sts.private.example.com:8443", "", stsURL},
		{"Host cannot contain userinfo", "user@sts.private.example.com:8443", "", stsURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := discover(tc.host, tc.forwardedHost)
			assert.Equal(t, stsURL, doc["issuer"])
			assert.Equal(t, tc.origin+"/token", doc["token_endpoint"])
			assert.Equal(t, tc.origin+"/jwks.json", doc["jwks_uri"])
			assert.Equal(t, public["endpoints"], doc["endpoints"])
			assert.Equal(t, public["identity_providers"], doc["identity_providers"])
		})
	}

	t.Run("no aliases preserves canonical prefix", func(t *testing.T) {
		e := newEnv(t, 1000, func(c *config.Controller) { c.Endpoints.STSURL = stsURL + "/sts/" })
		req := httptest.NewRequest(http.MethodGet, private+"/.well-known/cucina-configuration", nil)
		rr := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc))
		assert.Equal(t, stsURL+"/sts", doc["issuer"])
		assert.Equal(t, stsURL+"/sts/token", doc["token_endpoint"])
		assert.Equal(t, stsURL+"/sts/jwks.json", doc["jwks_uri"])
	})

	t.Run("public authority alias cannot strip canonical prefix", func(t *testing.T) {
		e := newEnv(t, 1000, func(c *config.Controller) {
			c.Endpoints.STSURL = stsURL + "/sts/"
			c.Endpoints.STSAliases = []string{stsURL + ":443/", private}
		})
		for host, want := range map[string]string{
			"cucina.example.com:443":       stsURL + "/sts",
			"sts.private.example.com:8443": private,
		} {
			req := httptest.NewRequest(http.MethodGet, stsURL+"/.well-known/cucina-configuration", nil)
			req.Host = host
			rr := httptest.NewRecorder()
			e.srv.Handler().ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code)
			var doc map[string]any
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc))
			assert.Equal(t, stsURL+"/sts", doc["issuer"])
			assert.Equal(t, want+"/token", doc["token_endpoint"])
			assert.Equal(t, want+"/jwks.json", doc["jwks_uri"])
		}
	})
}

// TestDiscoveryRejectsMalformedAliases guards R-TEST-7/R-DATA-4: unsafe or ambiguous
// transport origins fail startup, including bad entries after a valid one.
func TestDiscoveryRejectsMalformedAliases(t *testing.T) {
	e := newEnv(t, 1000)
	for _, alias := range []string{
		"", "sts.private.example.com", "http://sts.private.example.com", "https:///",
		"https://user@sts.private.example.com", "https://user:synthetic@sts.private.example.com",
		"https://sts.private.example.com/prefix", "https://sts.private.example.com//",
		"https://sts.private.example.com?query=x", "https://sts.private.example.com?",
		"https://sts.private.example.com#fragment", "https://sts.private.example.com#",
		"https://sts.private.example.com:", "https://sts.private.example.com:0",
		"https://sts.private.example.com:65536", "https://sts.private.example.com:port",
		"https://*.private.example.com", "https://[not-an-ip]", "https://2001:db8::1",
	} {
		t.Run(alias, func(t *testing.T) {
			_, err := sts.New(sts.Deps{
				Engine: e.engine, Keys: e.mgr, Clock: e.clock,
				Config: config.Controller{
					InstanceNames: []string{"main"},
					Endpoints:     config.Endpoints{STSURL: stsURL, STSAliases: []string{"https://valid.example.com", alias}},
				},
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "endpoints.stsAliases[1]")
		})
	}
}

// TestPrivateSTSDiscoveryExchange guards the real R-DATA-4 edge: a TLS-verified
// private alias keeps discovery, exchange and JWKS on that transport; the issued JWT
// still verifies against the canonical issuer. The IdP is the local OIDC fixture.
func TestPrivateSTSDiscoveryExchange(t *testing.T) {
	private := httptest.NewUnstartedServer(nil)
	t.Cleanup(private.Close)
	origin := "https://" + private.Listener.Addr().String()
	e := newEnv(t, 1000, func(c *config.Controller) { c.Endpoints.STSAliases = []string{origin} })
	private.Config.Handler = e.srv.Handler()
	private.StartTLS()
	client := private.Client() // verifies the certificate's loopback-IP SAN, never skips TLS verification

	resp, err := client.Get(origin + "/.well-known/cucina-configuration")
	require.NoError(t, err)
	var doc struct {
		Issuer        string `json:"issuer"`
		TokenEndpoint string `json:"token_endpoint"`
		JWKSURI       string `json:"jwks_uri"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, origin+"/token", doc.TokenEndpoint)
	require.Equal(t, origin+"/jwks.json", doc.JWKSURI)
	assert.Equal(t, stsURL, doc.Issuer)

	resp, err = client.PostForm(doc.TokenEndpoint, url.Values{
		"grant_type": {sts.GrantTypeTokenExchange}, "subject_token_type": {auth.TokenTypeIDToken},
		"subject_token": {e.google.Token(e.googleClaims())},
	})
	require.NoError(t, err)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&token))
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	claims, err := e.mgr.Verifier.Verify(token.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, stsURL, claims.Issuer)
	assert.Equal(t, "buildbarn", claims.Audience)

	resp, err = client.Get(doc.JWKSURI)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	want, err := e.mgr.Ring.KeySet().JWKSJSON()
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(body))
}

// TestRateLimits guards R-AUTH-1 rate limiting: per client IP, with 429 and Retry-After,
// and exactly one request refilled every 20 seconds when the limit is three per minute.
func TestRateLimits(t *testing.T) {
	e := newEnv(t, 3)
	for i := 0; i < 3; i++ {
		require.Equal(t, http.StatusBadRequest, e.exchange("not-a-token", auth.TokenTypeIDToken).code)
	}
	r := e.exchange("not-a-token", auth.TokenTypeIDToken)
	assert.Equal(t, http.StatusTooManyRequests, r.code)
	assert.Equal(t, "slow_down", r.body["error"])
	assert.NotEmpty(t, r.hdr.Get("Retry-After"))
	for _, step := range []struct {
		name       string
		advance    time.Duration
		wantStatus int
	}{
		{"19 seconds cannot refill a full request", 19 * time.Second, http.StatusTooManyRequests},
		// 400 means the rate limiter admitted the request and token validation rejected it.
		{"20 seconds refill exactly one request", time.Second, http.StatusBadRequest},
		{"the refilled request is consumed", 0, http.StatusTooManyRequests},
	} {
		t.Run(step.name, func(t *testing.T) {
			e.clock.Advance(step.advance)
			assert.Equal(t, step.wantStatus, e.exchange("not-a-token", auth.TokenTypeIDToken).code)
		})
	}
}

// TestServeTLS guards the STS transport (R-AUTH-1, UC22): HTTPS with HTTP/2, and a
// certificate replaced on disk is served without a restart.
func TestServeTLS(t *testing.T) {
	e := newEnv(t, 1000)
	dir := t.TempDir()
	certA, poolA := writeCert(t, dir, "a")
	reloader, err := sts.NewCertReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), e.clock)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- e.srv.Serve(ctx, ln, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: reloader.GetCertificate})
	}()
	t.Cleanup(func() { cancel(); <-done })

	get := func(pool *x509.CertPool) (*http.Response, error) {
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true}
		defer tr.CloseIdleConnections()
		return (&http.Client{Transport: tr}).Get("https://" + ln.Addr().String() + "/-/healthy")
	}
	resp, err := get(poolA)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 2, resp.ProtoMajor, "HTTP/2")
	assert.Equal(t, certA, resp.TLS.PeerCertificates[0].SerialNumber.String())

	certB, poolB := writeCert(t, dir, "b")
	e.clock.Advance(11 * time.Second)
	resp, err = get(poolB)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, certB, resp.TLS.PeerCertificates[0].SerialNumber.String(), "rotated certificate served")
}

// writeCert writes a fresh self-signed certificate for 127.0.0.1 and returns its serial.
func writeCert(t *testing.T, dir, name string) (string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "sts-" + name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	// Write to temp names and rename, as kubelet's atomic writer does.
	for file, blk := range map[string]*pem.Block{"tls.crt": {Type: "CERTIFICATE", Bytes: der}, "tls.key": {Type: "PRIVATE KEY", Bytes: keyDER}} {
		tmp := filepath.Join(dir, file+".tmp")
		require.NoError(t, os.WriteFile(tmp, pem.EncodeToMemory(blk), 0o600))
		require.NoError(t, os.Rename(tmp, filepath.Join(dir, file)))
	}
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return serial.String(), pool
}

// TestZeroTrustPolicies guards the bootstrap addendum (R-AUTH-12): with no TrustPolicy at
// all the STS still serves discovery and the JWKS, the break-glass key works, and every
// external token is refused.
func TestZeroTrustPolicies(t *testing.T) {
	e := newEnv(t, 1000)
	e.engine.Update(nil)
	var doc map[string]any
	getJSON(t, e.http.URL+"/.well-known/cucina-configuration", &doc)
	assert.Equal(t, []any{}, doc["identity_providers"])
	resp, err := http.Get(e.http.URL + "/jwks.json")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	r := e.exchange(e.breakKey, auth.TokenTypeServiceKey)
	require.Equal(t, http.StatusOK, r.code, "%v", r.body)
	c, err := e.mgr.Verifier.Verify(r.body["access_token"].(string))
	require.NoError(t, err)
	assert.Equal(t, []string{"main"}, c.Cucina.Admin)
	assert.Equal(t, http.StatusBadRequest, e.exchange(e.google.Token(e.googleClaims()), auth.TokenTypeIDToken).code)
}
