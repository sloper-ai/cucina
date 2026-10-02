// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"slices"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/keys"
)

// Verb is a grantable permission (R-AUTH-2). The values are the TrustPolicy spelling.
type Verb string

// The six verbs; they map onto Buildbarn authorizers and the management API.
const (
	VerbCASRead  Verb = v1alpha1.VerbCASRead
	VerbCASWrite Verb = v1alpha1.VerbCASWrite
	VerbACRead   Verb = v1alpha1.VerbACRead
	VerbACWrite  Verb = v1alpha1.VerbACWrite
	VerbExecute  Verb = v1alpha1.VerbExecute
	VerbAdmin    Verb = v1alpha1.VerbAdmin
)

// AllVerbs lists every verb in canonical order.
var AllVerbs = []Verb{VerbCASRead, VerbCASWrite, VerbACRead, VerbACWrite, VerbExecute, VerbAdmin}

// ParseVerb validates a verb name.
func ParseVerb(s string) (Verb, bool) {
	v := Verb(s)
	return v, slices.Contains(AllVerbs, v)
}

// Grants maps a verb to the instance names it applies to.
type Grants map[Verb][]string

// add records verb on names (kept sorted and unique).
func (g Grants) add(v Verb, names ...string) {
	l := append(g[v], names...)
	slices.Sort(l)
	g[v] = slices.Compact(l)
}

// Has reports whether verb is granted on instance.
func (g Grants) Has(v Verb, instance string) bool { return slices.Contains(g[v], instance) }

// Empty reports whether nothing is granted.
func (g Grants) Empty() bool {
	for _, l := range g {
		if len(l) > 0 {
			return false
		}
	}
	return true
}

// Restrict keeps only the grants on instance (RFC 8693 `audience` down-scoping).
func (g Grants) Restrict(instance string) Grants {
	out := Grants{}
	for v, l := range g {
		if slices.Contains(l, instance) {
			out[v] = []string{instance}
		}
	}
	return out
}

// Scopes renders the JWT `cucina` claim.
func (g Grants) Scopes() keys.Scopes {
	return keys.Scopes{
		CASRead: g[VerbCASRead], CASWrite: g[VerbCASWrite], ACRead: g[VerbACRead],
		ACWrite: g[VerbACWrite], Execute: g[VerbExecute], Admin: g[VerbAdmin],
	}.Normalized()
}

// GrantsFromScopes is the inverse of Scopes (token verification).
func GrantsFromScopes(s keys.Scopes) Grants {
	g := Grants{}
	for v, l := range map[Verb][]string{
		VerbCASRead: s.CASRead, VerbCASWrite: s.CASWrite, VerbACRead: s.ACRead,
		VerbACWrite: s.ACWrite, VerbExecute: s.Execute, VerbAdmin: s.Admin,
	} {
		if len(l) > 0 {
			g.add(v, l...)
		}
	}
	return g
}
