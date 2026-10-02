// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/ports"
)

// OIDCVerifier is the production ports.IdentityProvider: go-oidc v3 with discovery and
// JWKS caching per issuer (R-AUTH-2). It checks the signature with an asymmetric
// algorithm advertised by the issuer (never `none` or HMAC), the exact `iss` (the
// configured URL or one of its additionalIssuers), that `aud` contains an accepted
// audience, `exp` (no leeway) and that `nbf`/`iat` are not in the future by more than
// MaxClockSkew.
type OIDCVerifier struct {
	Clock ports.Clock
	// MaxClockSkew bounds how far nbf/iat may lie in the future (default 1 min).
	MaxClockSkew time.Duration
	// RetryAfter is the back-off after a failed discovery (default 10 s).
	RetryAfter time.Duration
	// HTTPTimeout bounds discovery and JWKS requests (default 10 s).
	HTTPTimeout time.Duration

	mu      sync.Mutex
	issuers map[string]*issuerEntry
}

type issuerEntry struct {
	spec        v1alpha1.IssuerSpec
	accepted    []string // url + additional issuers (union over policies)
	verifier    *oidc.IDTokenVerifier
	static      bool
	lastErr     error
	lastAttempt time.Time
}

// NewOIDCVerifier returns an adapter without issuers; the engine registers them.
func NewOIDCVerifier(clock ports.Clock) *OIDCVerifier {
	return &OIDCVerifier{Clock: clock, issuers: map[string]*issuerEntry{}}
}

func sameSpec(a, b v1alpha1.IssuerSpec) bool {
	return a.URL == b.URL && a.DiscoveryURL == b.DiscoveryURL && a.CertificateAuthority == b.CertificateAuthority
}

// SetIssuers implements IssuerRegistry. Unchanged issuers keep their verifier (and so
// their JWKS cache). When several policies name the same issuer URL, the first spec's
// discovery URL and CA win and the additional issuers are united (the engine still
// routes tokens per policy).
func (v *OIDCVerifier) SetIssuers(specs []v1alpha1.IssuerSpec) {
	v.mu.Lock()
	defer v.mu.Unlock()
	next := map[string]*issuerEntry{}
	for _, s := range specs {
		if e, ok := next[s.URL]; ok {
			e.accepted = unite(e.accepted, s.AdditionalIssuers)
			continue
		}
		e := &issuerEntry{spec: s, accepted: unite([]string{s.URL}, s.AdditionalIssuers)}
		if old, ok := v.issuers[s.URL]; ok && (old.static || sameSpec(old.spec, s)) {
			e.verifier, e.static, e.lastErr, e.lastAttempt = old.verifier, old.static, old.lastErr, old.lastAttempt
		}
		next[s.URL] = e
	}
	for url, e := range v.issuers {
		if _, ok := next[url]; !ok && e.static {
			next[url] = e
		}
	}
	v.issuers = next
}

func unite(a, b []string) []string {
	out := slices.Clone(a)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// AddStaticIssuer registers an issuer whose keys are fixed (unit tests, air-gapped
// issuers); no discovery happens for it.
func (v *OIDCVerifier) AddStaticIssuer(spec v1alpha1.IssuerSpec, keySet oidc.KeySet, algs []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.issuers == nil {
		v.issuers = map[string]*issuerEntry{}
	}
	v.issuers[spec.URL] = &issuerEntry{
		spec: spec, accepted: unite([]string{spec.URL}, spec.AdditionalIssuers), static: true,
		verifier: v.newVerifier(spec.URL, keySet, algs),
	}
}

func (v *OIDCVerifier) newVerifier(issuer string, ks oidc.KeySet, algs []string) *oidc.IDTokenVerifier {
	return oidc.NewVerifier(issuer, ks, &oidc.Config{
		// Audience and issuer are checked by Verify (several audiences, additional issuers).
		SkipClientIDCheck:    true,
		SkipIssuerCheck:      true,
		SupportedSigningAlgs: algs,
		Now:                  v.Clock.Now,
	})
}

func (v *OIDCVerifier) entry(ctx context.Context, issuerURL string) (*issuerEntry, error) {
	v.mu.Lock()
	e, ok := v.issuers[issuerURL]
	if !ok {
		v.mu.Unlock()
		return nil, verifyErr("issuer", errors.New("issuer not configured"))
	}
	if e.verifier != nil {
		v.mu.Unlock()
		return e, nil
	}
	retry := v.RetryAfter
	if retry <= 0 {
		retry = 10 * time.Second
	}
	now := v.Clock.Now()
	if e.lastErr != nil && now.Sub(e.lastAttempt) < retry {
		err := e.lastErr
		v.mu.Unlock()
		return nil, verifyErr("unavailable", err)
	}
	spec := e.spec
	v.mu.Unlock()

	verifier, err := v.discover(ctx, spec)

	v.mu.Lock()
	defer v.mu.Unlock()
	e.lastAttempt, e.lastErr = now, err
	if err != nil {
		return nil, verifyErr("unavailable", err)
	}
	e.verifier = verifier
	return e, nil
}

type discoveryDoc struct {
	Issuer  string   `json:"issuer"`
	JWKSURI string   `json:"jwks_uri"`
	Algs    []string `json:"id_token_signing_alg_values_supported"`
}

// httpClientFor builds the client for discovery and JWKS fetches: optional private CA,
// TLS 1.2+, no redirects (SSRF guard).
func httpClientFor(caPEM string, timeout time.Duration) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, errors.New("issuer.certificateAuthority holds no PEM certificate")
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func (v *OIDCVerifier) discover(ctx context.Context, spec v1alpha1.IssuerSpec) (*oidc.IDTokenVerifier, error) {
	timeout := v.HTTPTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client, err := httpClientFor(spec.CertificateAuthority, timeout)
	if err != nil {
		return nil, err
	}
	discURL := spec.DiscoveryURL
	if discURL == "" {
		discURL = strings.TrimSuffix(spec.URL, "/") + "/.well-known/openid-configuration"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching discovery document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery document: HTTP %d", resp.StatusCode)
	}
	var doc discoveryDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing discovery document: %w", err)
	}
	if doc.Issuer != spec.URL {
		return nil, fmt.Errorf("discovery document issuer %q does not match %q", doc.Issuer, spec.URL)
	}
	if u, err := url.Parse(doc.JWKSURI); err != nil || u.Scheme != "https" {
		return nil, errors.New("discovery document jwks_uri must be https")
	}
	advertised := doc.Algs
	if len(advertised) == 0 {
		advertised = []string{"RS256"}
	}
	var algs []string
	for _, a := range advertised {
		for _, ok := range allowedAlgs {
			if a == string(ok) {
				algs = append(algs, a)
			}
		}
	}
	if len(algs) == 0 {
		return nil, errors.New("issuer advertises no supported asymmetric signing algorithm")
	}
	// The key set outlives the request: give it a background context carrying the client.
	ks := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), doc.JWKSURI)
	return v.newVerifier(spec.URL, ks, algs), nil
}

// Verify implements ports.IdentityProvider.
func (v *OIDCVerifier) Verify(ctx context.Context, issuerURL string, audiences []string, raw string) (ports.Claims, error) {
	e, err := v.entry(ctx, issuerURL)
	if err != nil {
		return nil, err
	}
	idt, err := e.verifier.Verify(ctx, raw)
	if err != nil {
		var te *oidc.TokenExpiredError
		msg := err.Error()
		switch {
		case errors.As(err, &te):
			return nil, verifyErr("expired", err)
		case strings.Contains(msg, "nbf"):
			return nil, verifyErr("not-yet-valid", err)
		case strings.Contains(msg, "malformed jwt"):
			return nil, verifyErr("malformed", err)
		case strings.Contains(msg, "failed to verify signature"):
			return nil, verifyErr("signature", err)
		}
		return nil, verifyErr("malformed", err)
	}
	if !slices.Contains(e.accepted, idt.Issuer) {
		return nil, verifyErr("issuer", fmt.Errorf("issuer %q not accepted", idt.Issuer))
	}
	if !slices.ContainsFunc(idt.Audience, func(a string) bool { return slices.Contains(audiences, a) }) {
		return nil, verifyErr("audience", errors.New("no accepted audience"))
	}
	var rawClaims json.RawMessage
	if err := idt.Claims(&rawClaims); err != nil {
		return nil, verifyErr("malformed", err)
	}
	claims, err := decodeClaims(rawClaims)
	if err != nil {
		return nil, verifyErr("malformed", err)
	}
	skew := v.MaxClockSkew
	if skew <= 0 {
		skew = time.Minute
	}
	horizon := v.Clock.Now().Add(skew)
	for _, name := range []string{"nbf", "iat"} {
		if t, ok := numericDate(claims[name]); ok && t.After(horizon) {
			return nil, verifyErr("not-yet-valid", fmt.Errorf("%s in the future", name))
		}
	}
	return claims, nil
}

func numericDate(v any) (time.Time, bool) {
	switch t := v.(type) {
	case int64:
		return time.Unix(t, 0), true
	case float64:
		return time.Unix(int64(t), 0), true
	}
	return time.Time{}, false
}
