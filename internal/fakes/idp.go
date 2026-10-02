// SPDX-License-Identifier: FSL-1.1-ALv2

package fakes

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Errors returned by the fake IdentityProvider's Verify (all fail closed).
var (
	ErrTokenMalformed = errors.New("token malformed")
	ErrTokenSignature = errors.New("token signature invalid")
	ErrTokenIssuer    = errors.New("token issuer mismatch")
	ErrTokenAudience  = errors.New("token audience mismatch")
	ErrTokenExpired   = errors.New("token expired or not yet valid")
)

// Well-known issuers.
const (
	GoogleIssuer = "https://accounts.google.com"
	GitHubIssuer = "https://token.actions.githubusercontent.com"
)

type idpKey struct {
	kid  string
	priv ed25519.PrivateKey
}

// IdentityProvider is a fake OIDC world implementing ports.IdentityProvider:
// it signs tokens (EdDSA, deterministic keys from the seed) for arbitrary
// issuers and claims, and verifies them like go-oidc would (signature by kid,
// exact iss, aud, exp/nbf against the clock).
type IdentityProvider struct {
	*Faults
	mu    sync.Mutex
	clock ports.Clock
	rnd   *Rand
	keys  map[string][]idpKey // issuer → keys, newest last (all verify; newest signs)
}

var _ ports.IdentityProvider = (*IdentityProvider)(nil)

// NewIdentityProvider returns an IdP world with no issuers (they are created on first use).
func NewIdentityProvider(clock ports.Clock, rnd *Rand) *IdentityProvider {
	return &IdentityProvider{Faults: newFaults(clock, rnd.Child("idp-faults")), clock: clock, rnd: rnd.Child("idp"), keys: map[string][]idpKey{}}
}

func (p *IdentityProvider) keyLocked(issuer string) idpKey {
	ks := p.keys[issuer]
	if len(ks) == 0 {
		return p.rotateLocked(issuer)
	}
	return ks[len(ks)-1]
}

func (p *IdentityProvider) rotateLocked(issuer string) idpKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(p.rnd.Int63n(256))
	}
	k := idpKey{kid: p.rnd.Token(8), priv: ed25519.NewKeyFromSeed(seed)}
	p.keys[issuer] = append(p.keys[issuer], k)
	return k
}

// RotateKey adds a new signing key for issuer; older keys keep verifying
// until RetireOldKeys.
func (p *IdentityProvider) RotateKey(issuer string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rotateLocked(issuer).kid
}

// RetireOldKeys removes every key of issuer except the newest.
func (p *IdentityProvider) RetireOldKeys(issuer string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ks := p.keys[issuer]; len(ks) > 1 {
		p.keys[issuer] = ks[len(ks)-1:]
	}
}

// JWKS returns the issuer's public keys as a JSON Web Key Set (OKP/Ed25519).
func (p *IdentityProvider) JWKS(issuer string) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keyLocked(issuer)
	type jwk struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		Kid string `json:"kid"`
		Alg string `json:"alg"`
		Use string `json:"use"`
		X   string `json:"x"`
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	for _, k := range p.keys[issuer] {
		pub := k.priv.Public().(ed25519.PublicKey)
		set.Keys = append(set.Keys, jwk{Kty: "OKP", Crv: "Ed25519", Kid: k.kid, Alg: "EdDSA", Use: "sig", X: b64(pub)})
	}
	out, _ := json.Marshal(set)
	return out
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Issue signs a token for issuer with the given claims ("iss" is set to
// issuer; "iat" defaults to now). It never fails.
func (p *IdentityProvider) Issue(issuer string, claims ports.Claims) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := p.keyLocked(issuer)
	c := maps.Clone(claims)
	if c == nil {
		c = ports.Claims{}
	}
	c["iss"] = issuer
	if _, ok := c["iat"]; !ok {
		c["iat"] = p.clock.Now().Unix()
	}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": k.kid})
	payload, _ := json.Marshal(c)
	input := b64(header) + "." + b64(payload)
	return input + "." + b64(ed25519.Sign(k.priv, []byte(input)))
}

// Verify implements ports.IdentityProvider.
func (p *IdentityProvider) Verify(ctx context.Context, issuerURL string, audiences []string, raw string) (ports.Claims, error) {
	if err := p.enter(ctx, "Verify"); err != nil {
		return nil, err
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrTokenMalformed
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	pb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, ErrTokenMalformed
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "EdDSA" {
		return nil, fmt.Errorf("%w: alg %q", ErrTokenMalformed, hdr.Alg)
	}
	p.mu.Lock()
	var pub ed25519.PublicKey
	for _, k := range p.keys[issuerURL] {
		if k.kid == hdr.Kid {
			pub = k.priv.Public().(ed25519.PublicKey)
		}
	}
	now := p.clock.Now()
	p.mu.Unlock()
	if pub == nil || !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, ErrTokenSignature
	}
	var claims ports.Claims
	if err := json.Unmarshal(pb, &claims); err != nil {
		return nil, ErrTokenMalformed
	}
	if iss, _ := claims["iss"].(string); iss != issuerURL {
		return nil, fmt.Errorf("%w: %q", ErrTokenIssuer, iss)
	}
	if !audienceOK(claims["aud"], audiences) {
		return nil, ErrTokenAudience
	}
	exp, ok := claims["exp"].(float64)
	if !ok || now.Unix() >= int64(exp) {
		return nil, ErrTokenExpired
	}
	if nbf, ok := claims["nbf"].(float64); ok && now.Unix() < int64(nbf) {
		return nil, ErrTokenExpired
	}
	return claims, nil
}

func audienceOK(aud any, want []string) bool {
	switch a := aud.(type) {
	case string:
		return slices.Contains(want, a)
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && slices.Contains(want, s) {
				return true
			}
		}
	}
	return false
}

// GoogleClaims returns Google-shaped ID-token claims (Desktop-app login).
func GoogleClaims(sub, email, hostedDomain, audience string, exp time.Time) ports.Claims {
	c := ports.Claims{"sub": sub, "email": email, "email_verified": true, "aud": audience, "azp": audience,
		"exp": exp.Unix(), "name": strings.Split(email, "@")[0]}
	if hostedDomain != "" {
		c["hd"] = hostedDomain
	}
	return c
}

// GitHubClaims returns GitHub Actions OIDC claims for a workflow run in repo at ref.
func GitHubClaims(repo, ref, workflow, event, audience string, exp time.Time) ports.Claims {
	owner := strings.SplitN(repo, "/", 2)[0]
	return ports.Claims{
		"sub":                   "repo:" + repo + ":ref:" + ref,
		"aud":                   audience,
		"exp":                   exp.Unix(),
		"repository":            repo,
		"repository_owner":      owner,
		"ref":                   ref,
		"ref_type":              "branch",
		"workflow":              workflow,
		"job_workflow_ref":      repo + "/.github/workflows/" + workflow + ".yml@" + ref,
		"event_name":            event,
		"repository_id":         "100",
		"repository_visibility": "public",
		"runner_environment":    "github-hosted",
	}
}
