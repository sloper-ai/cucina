// SPDX-License-Identifier: FSL-1.1-ALv2

package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/auth"
)

// TestProviderClaimShapes guards the shipped generic-OIDC samples (R-AUTH-2, R-AUTH-5
// "other OIDC providers"): each sample accepts its provider's real claim shape and
// rejects the shape that must not pass.
func TestProviderClaimShapes(t *testing.T) {
	const (
		entraIss = "https://login.microsoftonline.com/7d1c5d4e-0000-4000-8000-00000000c0de/v2.0"
		entraAud = "0b8f1a52-0000-4000-8000-00000000a99e"
		devGroup = "5e1a0d3c-0000-4000-8000-0000000000de"
		dexIss   = "https://dex.example.com"
		oktaIss  = "https://example.okta.com/oauth2/default"
		oktaAud  = "0oa1cucinaexample0h7"
	)
	base := func(iss, aud, sub string, kv ...any) map[string]any {
		m := map[string]any{"iss": iss, "aud": aud, "sub": sub, "iat": t0.Unix(), "exp": t0.Add(time.Hour).Unix()}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	entra := func(kv ...any) map[string]any {
		return base(entraIss, entraAud, "pairwise-sub", append([]any{"tid", "7d1c5d4e-0000-4000-8000-00000000c0de",
			"oid", "1f2e3d4c-0000-4000-8000-000000000001", "preferred_username", "alice@contoso.example", "groups", []any{devGroup}}, kv...)...)
	}
	keycloak := func(kv ...any) map[string]any {
		return base(keycloakIssuer, "cucina", "5b1c0d2e-7f00-4a00-9000-000000000001", append([]any{"email_verified", true,
			"preferred_username", "alice", "groups", []any{"/cucina/users"}}, kv...)...)
	}
	dex := func(kv ...any) map[string]any {
		return base(dexIss, "cucina", "CgYxMjM0NTYSBmdpdGh1Yg", append([]any{"email_verified", true, "name", "Alice",
			"federated_claims", map[string]any{"connector_id": "github", "user_id": "123456"},
			"groups", []any{"example-org:platform"}}, kv...)...)
	}
	okta := func(kv ...any) map[string]any {
		return base(oktaIss, oktaAud, "00u1a2b3c4d5e6f7g8h9", append([]any{"email", "alice@example.com", "groups", []any{"Cucina Users"}}, kv...)...)
	}
	rows := []struct {
		name    string
		iss     string
		claims  map[string]any
		wantSub string // "" = rejected with access_denied
	}{
		{"entra member of the developer group", entraIss, entra(), "entra:7d1c5d4e-0000-4000-8000-00000000c0de:1f2e3d4c-0000-4000-8000-000000000001"},
		{"entra other tenant", entraIss, entra("tid", "00000000-0000-4000-8000-000000000bad"), ""},
		{"entra without groups claim", entraIss, entra("groups", []any{}), ""},
		{"keycloak member of /cucina/users", keycloakIssuer, keycloak(), "keycloak:5b1c0d2e-7f00-4a00-9000-000000000001"},
		{"keycloak unverified e-mail", keycloakIssuer, keycloak("email_verified", false), ""},
		{"dex github connector and team", dexIss, dex(), "dex:CgYxMjM0NTYSBmdpdGh1Yg"},
		{"dex other connector", dexIss, dex("federated_claims", map[string]any{"connector_id": "ldap", "user_id": "x"}), ""},
		{"okta Cucina Users group", oktaIss, okta(), "okta:00u1a2b3c4d5e6f7g8h9"},
		{"okta without the group", oktaIss, okta("groups", []any{"Everyone"}), ""},
	}
	f := newFixture(t, auth.CELLimits{}, loadPolicy(t, "entra.yaml"), loadPolicy(t, "keycloak.yaml"), loadPolicy(t, "dex.yaml"), loadPolicy(t, "okta.yaml"))
	for _, s := range f.statuses {
		require.True(t, s.Valid, "%s: %s", s.Name, s.Message)
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			p, err := f.engine.ExchangeToken(context.Background(), auth.TokenTypeIDToken, f.issuer(r.iss).Token(r.claims))
			if r.wantSub == "" {
				require.Error(t, err)
				assert.Equal(t, auth.CodeAccessDenied, auth.AsError(err).Code, "%v", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, r.wantSub, p.Subject)
			assert.Equal(t, []string{"main"}, p.Grants[auth.VerbExecute])
			assert.Empty(t, p.Grants[auth.VerbACWrite])
		})
	}
}
