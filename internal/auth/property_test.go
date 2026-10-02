// SPDX-License-Identifier: FSL-1.1-ALv2

package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
	"github.com/sloper-ai/cucina/internal/ports"
)

// decodingIDP "verifies" a token by decoding its payload: this property is about policy
// evaluation; signature checks are covered by TestExchangeTable.
type decodingIDP struct{}

func (decodingIDP) Verify(_ context.Context, _ string, _ []string, raw string) (ports.Claims, error) {
	parts := strings.Split(raw, ".")
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c map[string]any
	return c, json.Unmarshal(b, &c)
}

func unsignedToken(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	p, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(p) + ".c2ln"
}

// A template is a CEL expression plus a plain-Go oracle of its truth; any CEL error
// (missing key, wrong type) must behave like false (fail closed).
type template struct {
	expr   string
	oracle func(claims map[string]any, subject string, groups []string) bool
}

var ruleTemplates = []template{
	{"true", func(map[string]any, string, []string) bool { return true }},
	{"claims.a == 'x'", func(c map[string]any, _ string, _ []string) bool { return c["a"] == "x" }},
	{"has(claims.b) && claims.b == true", func(c map[string]any, _ string, _ []string) bool { return c["b"] == true }},
	{"claims.c in ['p', 'q']", func(c map[string]any, _ string, _ []string) bool { return c["c"] == "p" || c["c"] == "q" }},
}

var conditionTemplates = append(slices.Clone(ruleTemplates),
	template{"'g1' in groups", func(_ map[string]any, _ string, g []string) bool { return slices.Contains(g, "g1") }},
	template{"subject == 't:u1'", func(_ map[string]any, s string, _ []string) bool { return s == "t:u1" }},
)

type genGrant struct {
	cond      int // -1 = none
	verbs     []string
	instances []string
}

type genPolicy struct {
	rules  []int
	grants []genGrant
}

// TestGrantsMatchPolicyModel is the R-TEST-6 "STS/auth" property: for random policies
// and random claim sets, the engine grants exactly what a plain model of the policies
// grants. In particular no claim set ever yields a grant that no applicable policy gave,
// and every evaluation error withholds (never adds) permissions.
func TestGrantsMatchPolicyModel(t *testing.T) {
	const iss = "https://prop.example.com"
	instances := []string{"main", "team-a"}
	verbNames := []string{"cas-read", "cas-write", "ac-read", "ac-write", "execute", "admin"}
	rapid.Check(t, func(rt *rapid.T) {
		policies := rapid.SliceOfN(rapid.Custom(func(rt *rapid.T) genPolicy {
			return genPolicy{
				rules: rapid.SliceOfN(rapid.IntRange(0, len(ruleTemplates)-1), 0, 2).Draw(rt, "rules"),
				grants: rapid.SliceOfN(rapid.Custom(func(rt *rapid.T) genGrant {
					return genGrant{
						cond:      rapid.IntRange(-1, len(conditionTemplates)-1).Draw(rt, "cond"),
						verbs:     rapid.SliceOfNDistinct(rapid.SampledFrom(verbNames), 1, 3, rapid.ID[string]).Draw(rt, "verbs"),
						instances: rapid.SliceOfNDistinct(rapid.SampledFrom([]string{"main", "team-a", "*", "other"}), 1, 2, rapid.ID[string]).Draw(rt, "instances"),
					}
				}), 1, 3).Draw(rt, "grants"),
			}
		}), 1, 3).Draw(rt, "policies")

		claims := map[string]any{"iss": iss, "aud": "cucina"}
		value := rapid.OneOf(rapid.Just[any]("x"), rapid.Just[any]("y"), rapid.Just[any]("p"), rapid.Just[any](true), rapid.Just[any](false), rapid.Just[any](7.0))
		for _, k := range []string{"a", "b", "c"} {
			if rapid.Bool().Draw(rt, "has-"+k) {
				claims[k] = value.Draw(rt, k)
			}
		}
		if rapid.Bool().Draw(rt, "has-sub") {
			claims["sub"] = rapid.SampledFrom([]string{"u1", "u2"}).Draw(rt, "sub")
		}
		var groups []string
		if rapid.Bool().Draw(rt, "has-groups") {
			groups = rapid.SliceOfNDistinct(rapid.SampledFrom([]string{"g1", "g2"}), 0, 2, rapid.ID[string]).Draw(rt, "groups")
			l := []any{}
			for _, g := range groups {
				l = append(l, g)
			}
			claims["groups"] = l
		}

		var objs []v1alpha1.TrustPolicy
		for i, gp := range policies {
			p := v1alpha1.TrustPolicy{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p%d", i)}, Spec: v1alpha1.TrustPolicySpec{
				Type:   "oidc",
				Issuer: &v1alpha1.IssuerSpec{URL: iss, Audiences: []string{"cucina"}},
				ClaimMappings: &v1alpha1.ClaimMappings{
					Subject: v1alpha1.CELExpression{Expression: "'t:' + claims.sub"},
					Groups:  &v1alpha1.CELExpression{Expression: "has(claims.groups) ? claims.groups : []"},
				},
			}}
			for _, r := range gp.rules {
				p.Spec.ClaimValidationRules = append(p.Spec.ClaimValidationRules, v1alpha1.ClaimValidationRule{Expression: ruleTemplates[r].expr})
			}
			for _, g := range gp.grants {
				vg := v1alpha1.Grant{Verbs: g.verbs, InstanceNames: g.instances}
				if g.cond >= 0 {
					vg.Condition = conditionTemplates[g.cond].expr
				}
				p.Spec.Grants = append(p.Spec.Grants, vg)
			}
			objs = append(objs, p)
		}

		// Model.
		want := map[string]bool{}
		subject := ""
		if s, ok := claims["sub"].(string); ok {
			subject = "t:" + s
		}
		for _, gp := range policies {
			if subject == "" {
				continue // subject mapping fails: policy denies
			}
			ok := true
			for _, r := range gp.rules {
				ok = ok && ruleTemplates[r].oracle(claims, subject, groups)
			}
			if !ok {
				continue
			}
			for _, g := range gp.grants {
				if g.cond >= 0 && !conditionTemplates[g.cond].oracle(claims, subject, groups) {
					continue
				}
				for _, v := range g.verbs {
					for _, in := range g.instances {
						if in == "*" {
							for _, n := range instances {
								want[v+"@"+n] = true
							}
						} else {
							want[v+"@"+in] = true
						}
					}
				}
			}
		}

		e, err := auth.NewEngine(auth.EngineOptions{
			IdentityProvider: decodingIDP{}, Clock: keystest.NewClock(t0), InstanceNames: instances,
			Log: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			rt.Fatal(err)
		}
		for _, s := range e.Update(objs) {
			if !s.Valid {
				rt.Fatalf("generated policy invalid: %s", s.Message)
			}
		}
		p, err := e.ExchangeToken(context.Background(), auth.TokenTypeIDToken, unsignedToken(claims))
		got := map[string]bool{}
		if err == nil {
			for v, l := range p.Grants {
				for _, n := range l {
					got[string(v)+"@"+n] = true
				}
			}
		} else if code := auth.AsError(err).Code; code != auth.CodeAccessDenied {
			rt.Fatalf("unexpected error code %s: %v", code, err)
		}
		if len(got) != len(want) {
			rt.Fatalf("grants %v, model %v (claims %v)", got, want, claims)
		}
		for k := range want {
			if !got[k] {
				rt.Fatalf("grants %v, model %v (claims %v)", got, want, claims)
			}
		}
	})
}
