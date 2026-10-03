// SPDX-License-Identifier: FSL-1.1-ALv2

// Package oidctest is a minimal in-process OIDC issuer for tests (ADR 0602): one or
// more issuers, each with its own path on one HTTPS test server, serving discovery and
// a JWKS, and minting RS256 tokens with arbitrary claims — plus deliberately broken
// ones (alg none, unknown kid, tampered signature). Issuers also work offline through
// KeySet, so unit tests verify signatures without any I/O. Claim builders produce
// Google- and GitHub-shaped tokens with valid defaults (R-TEST-8e).
package oidctest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/sloper-ai/cucina/api/v1alpha1"
)

var (
	keyPoolMu sync.Mutex
	keyPool   []*rsa.PrivateKey
)

// testKey returns the n-th RSA test key of this process. Keys are generated once per
// test binary (RSA generation is slow) and never written anywhere.
func testKey(n int) *rsa.PrivateKey {
	keyPoolMu.Lock()
	defer keyPoolMu.Unlock()
	for len(keyPool) <= n {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		keyPool = append(keyPool, k)
	}
	return keyPool[n]
}

var keyCounter struct {
	sync.Mutex
	n int
}

func nextKey() *rsa.PrivateKey {
	keyCounter.Lock()
	n := keyCounter.n
	keyCounter.n++
	keyCounter.Unlock()
	return testKey(n % 4)
}

// Issuer is one test identity provider.
type Issuer struct {
	URL string
	mu  sync.Mutex
	// keys[0] signs; all are published.
	keys            []*rsa.PrivateKey
	discoveryIssuer string
	jwksURI         string
}

// NewIssuer creates an issuer with one RS256 key.
func NewIssuer(url string) *Issuer {
	return &Issuer{URL: url, keys: []*rsa.PrivateKey{nextKey()}, discoveryIssuer: url, jwksURI: url + "/jwks"}
}

// SetDiscovery changes the advertised issuer and JWKS URI without changing the
// issuer's identity or signing keys, for testing discovery trust boundaries.
func (i *Issuer) SetDiscovery(issuer, jwksURI string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.discoveryIssuer, i.jwksURI = issuer, jwksURI
}

// ServeJWKSOverHTTP serves the issuer's real public keys on a separate loopback
// HTTP endpoint, closed with t.Cleanup. It returns the URL to advertise in discovery.
func (i *Issuer) ServeJWKSOverHTTP(t testing.TB) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(i.JWKS())
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func kidOf(k *rsa.PrivateKey) string {
	j := jose.JSONWebKey{Key: &k.PublicKey}
	tp, _ := j.Thumbprint(crypto.SHA256)
	return base64.RawURLEncoding.EncodeToString(tp)[:16]
}

// RotateKey publishes a new signing key (the old one stays published).
func (i *Issuer) RotateKey() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keys = append([]*rsa.PrivateKey{nextKey()}, i.keys...)
}

// JWKS is the issuer's public key set.
func (i *Issuer) JWKS() jose.JSONWebKeySet {
	i.mu.Lock()
	defer i.mu.Unlock()
	var s jose.JSONWebKeySet
	for _, k := range i.keys {
		s.Keys = append(s.Keys, jose.JSONWebKey{Key: &k.PublicKey, KeyID: kidOf(k), Algorithm: "RS256", Use: "sig"})
	}
	return s
}

// Spec returns the TrustPolicy issuer block for this issuer.
func (i *Issuer) Spec(audiences ...string) v1alpha1.IssuerSpec {
	return v1alpha1.IssuerSpec{URL: i.URL, Audiences: audiences}
}

type mintOpts struct {
	alg     string
	kid     *string
	tamper  bool
	foreign bool
}

// Option changes how a token is produced.
type Option func(*mintOpts)

// AlgNone produces an unsigned token with "alg":"none".
func AlgNone() Option { return func(o *mintOpts) { o.alg = "none" } }

// WithKID overrides the kid header ("" removes it).
func WithKID(kid string) Option { return func(o *mintOpts) { o.kid = &kid } }

// Tampered flips a bit of the signature.
func Tampered() Option { return func(o *mintOpts) { o.tamper = true } }

// SignedByUnknownKey signs with a key the issuer never published (and an unknown kid).
func SignedByUnknownKey() Option { return func(o *mintOpts) { o.foreign = true } }

// Token mints a compact JWS over claims (claims are used verbatim).
func (i *Issuer) Token(claims map[string]any, opts ...Option) string {
	o := mintOpts{alg: "RS256"}
	for _, f := range opts {
		f(&o)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	i.mu.Lock()
	key := i.keys[0]
	i.mu.Unlock()
	if o.foreign {
		key = testKey(4) // never handed to an issuer (nextKey cycles 0..3)
	}
	if o.alg == "none" {
		h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		return h + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
	}
	kid := kidOf(key)
	if o.kid != nil {
		kid = *o.kid
	}
	sk := jose.SigningKey{Algorithm: jose.RS256, Key: key}
	if kid != "" {
		sk.Key = jose.JSONWebKey{Key: key, KeyID: kid}
	}
	signer, err := jose.NewSigner(sk, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		panic(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		panic(err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		panic(err)
	}
	if o.tamper {
		parts := strings.Split(raw, ".")
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sig[len(sig)/2] ^= 0x01
		parts[2] = base64.RawURLEncoding.EncodeToString(sig)
		raw = strings.Join(parts, ".")
	}
	return raw
}

// KeySet is a kid-aware static oidc.KeySet over the issuer's current JWKS (no I/O).
func (i *Issuer) KeySet() *KeySet { return &KeySet{issuer: i} }

// KeySet implements go-oidc's KeySet interface.
type KeySet struct{ issuer *Issuer }

// VerifySignature implements oidc.KeySet: the key is chosen by kid (all keys when the
// token has none), as go-oidc's remote key set does.
func (k *KeySet) VerifySignature(_ context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return nil, err
	}
	kid := jws.Signatures[0].Header.KeyID
	for _, key := range k.issuer.JWKS().Keys {
		if kid != "" && key.KeyID != kid {
			continue
		}
		if payload, err := jws.Verify(key.Key); err == nil {
			return payload, nil
		}
	}
	return nil, errors.New("no key verifies the token")
}

// Server hosts issuers over HTTPS: issuer "name" lives at <URL>/name with
// /.well-known/openid-configuration and /jwks.
type Server struct {
	*httptest.Server
	mu        sync.Mutex
	issuers   map[string]*Issuer
	jwksFetch map[string]int
}

// NewServer starts an HTTPS test server; it is closed with t.Cleanup.
func NewServer(t testing.TB) *Server {
	s := &Server{issuers: map[string]*Issuer{}, jwksFetch: map[string]int{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// AddIssuer creates the issuer <URL>/name.
func (s *Server) AddIssuer(name string) *Issuer {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := NewIssuer(s.URL + "/" + name)
	s.issuers[name] = i
	return i
}

// CAPEM is the server certificate as PEM (for IssuerSpec.CertificateAuthority).
func (s *Server) CAPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
}

// JWKSFetches counts JWKS downloads of an issuer.
func (s *Server) JWKSFetches(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jwksFetch[name]
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	s.mu.Lock()
	iss, ok := s.issuers[name]
	if ok && rest == "jwks" {
		s.jwksFetch[name]++
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch rest {
	case ".well-known/openid-configuration":
		iss.mu.Lock()
		issuer, jwksURI := iss.discoveryIssuer, iss.jwksURI
		iss.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "jwks_uri": jwksURI,
			"authorization_endpoint": iss.URL + "/authorize", "token_endpoint": iss.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	case "jwks":
		_ = json.NewEncoder(w).Encode(iss.JWKS())
	default:
		http.NotFound(w, r)
	}
}

// GoogleClaims are valid Google Workspace ID-token claims (R-AUTH-5 shape).
func GoogleClaims(iss, aud string, now time.Time) map[string]any {
	return map[string]any{
		"iss": iss, "aud": aud, "azp": aud, "sub": "110248495921238986420",
		"email": "alice@example.com", "email_verified": true, "hd": "example.com",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
}

// GitHubClaims are valid GitHub Actions OIDC claims for a push to main of
// sloper-ai/cucina (R-AUTH-7 shape; ids are strings as GitHub sends them).
func GitHubClaims(iss, aud string, now time.Time) map[string]any {
	return map[string]any{
		"iss": iss, "aud": aud, "jti": "0d4c2c8e-7d7b-4a5a-9b8a-5f2b2e0f3c11",
		"sub":                   "repo:sloper-ai/cucina:ref:refs/heads/main",
		"repository":            "sloper-ai/cucina",
		"repository_id":         "1401027334",
		"repository_owner":      "sloper-ai",
		"repository_owner_id":   "310369022",
		"repository_visibility": "public",
		"event_name":            "push",
		"ref":                   "refs/heads/main",
		"ref_type":              "branch",
		"ref_protected":         "true",
		"workflow":              "CI",
		"workflow_ref":          "sloper-ai/cucina/.github/workflows/ci.yml@refs/heads/main",
		"job_workflow_ref":      "sloper-ai/cucina/.github/workflows/ci.yml@refs/heads/main",
		"runner_environment":    "github-hosted",
		"run_id":                "1234567890", "run_number": "42", "run_attempt": "1",
		"actor": "octocat", "actor_id": "583231",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

// With returns a copy of claims with the given overrides (nil deletes a claim).
func With(claims map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(claims))
	for k, v := range claims {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		k := kv[i].(string)
		if kv[i+1] == nil {
			delete(out, k)
		} else {
			out[k] = kv[i+1]
		}
	}
	return out
}
