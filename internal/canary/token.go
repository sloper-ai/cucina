// SPDX-License-Identifier: FSL-1.1-ALv2

package canary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ServiceKeyTokenType is the RFC 8693 subject_token_type the STS expects
// for service-account keys (internal/auth.TokenTypeServiceKey).
const ServiceKeyTokenType = "urn:cucina:params:oauth:token-type:service-key"

// Discovery is the subset of /.well-known/cucina-configuration the canary
// uses (docs/contracts.md §5.1).
type Discovery struct {
	Issuer        string `json:"issuer"`
	TokenEndpoint string `json:"token_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
	Endpoints     struct {
		RemoteExecution string `json:"remote_execution"`
		InstanceName    string `json:"instance_name"`
	} `json:"endpoints"`
}

// Token is a minted, verified Cucina JWT.
type Token struct {
	Raw     string
	Subject string
	KeyID   string
	Expiry  time.Time
	// Grants are the cucina claim's verb → instance names.
	Grants map[string][]string
}

// TokenSource mints and verifies a Cucina JWT ("token mint/verify").
type TokenSource interface {
	Token(ctx context.Context) (Token, error)
}

// StaticToken is a pre-minted token (tests; never production).
type StaticToken string

// Token implements TokenSource without verification.
func (s StaticToken) Token(context.Context) (Token, error) { return Token{Raw: string(s)}, nil }

// STS mints tokens by exchanging a service-account key at the STS (RFC 8693)
// and verifies each minted token against the STS's JWKS: signature (ES256 or
// EdDSA, by kid), issuer, audience "buildbarn", expiry and the cucina claim.
type STS struct {
	// URL is the STS base URL (https://…:8443); discovery lives under it.
	URL string
	// KeyFile holds the service key (the chart mounts a Secret; Key wins if set).
	KeyFile string
	Key     string
	// Audience optionally narrows the token to an instance name.
	Audience string
	HTTP     *http.Client
	Now      func() time.Time
}

func (s *STS) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *STS) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *STS) key() (string, error) {
	if s.Key != "" {
		return s.Key, nil
	}
	if s.KeyFile == "" {
		return "", errors.New("canary: no service key configured")
	}
	b, err := os.ReadFile(s.KeyFile)
	if err != nil {
		return "", fmt.Errorf("canary: service key: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *STS) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.Unmarshal(body, v)
}

// Discover fetches the discovery document.
func (s *STS) Discover(ctx context.Context) (Discovery, error) {
	var d Discovery
	err := s.getJSON(ctx, strings.TrimSuffix(s.URL, "/")+"/.well-known/cucina-configuration", &d)
	if err == nil && (d.Issuer == "" || d.TokenEndpoint == "" || d.JWKSURI == "") {
		err = errors.New("discovery document lacks issuer, token_endpoint or jwks_uri")
	}
	return d, err
}

// Token implements TokenSource.
func (s *STS) Token(ctx context.Context) (Token, error) {
	d, err := s.Discover(ctx)
	if err != nil {
		return Token{}, fmt.Errorf("discovery: %w", err)
	}
	key, err := s.key()
	if err != nil {
		return Token{}, err
	}
	form := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {key},
		"subject_token_type": {ServiceKeyTokenType},
	}
	if s.Audience != "" {
		form.Set("audience", s.Audience)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client().Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		return Token{}, fmt.Errorf("token exchange: %s: %s %s", resp.Status, tr.Error, tr.Description)
	}
	var jwks jose.JSONWebKeySet
	if err := s.getJSON(ctx, d.JWKSURI, &jwks); err != nil {
		return Token{}, fmt.Errorf("jwks: %w", err)
	}
	return Verify(tr.AccessToken, jwks, d.Issuer, s.now())
}

// cucinaClaims is the JWT payload of docs/contracts.md §5.1.
type cucinaClaims struct {
	jwt.Claims
	SID    string              `json:"sid"`
	Cucina map[string][]string `json:"cucina"`
}

// Verify checks a Cucina JWT against a JWKS: a known kid, an ES256/EdDSA
// signature, the issuer, audience "buildbarn", expiry (no clock skew beyond
// 30 s) and a present cucina grant claim.
func Verify(raw string, jwks jose.JSONWebKeySet, issuer string, now time.Time) (Token, error) {
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256, jose.EdDSA})
	if err != nil {
		return Token{}, fmt.Errorf("verify: %w", err)
	}
	if len(tok.Headers) != 1 || tok.Headers[0].KeyID == "" {
		return Token{}, errors.New("verify: token has no kid")
	}
	kid := tok.Headers[0].KeyID
	keys := jwks.Key(kid)
	if len(keys) == 0 {
		return Token{}, fmt.Errorf("verify: kid %q not in the published JWKS", kid)
	}
	var c cucinaClaims
	if err := tok.Claims(keys[0].Key, &c); err != nil {
		return Token{}, fmt.Errorf("verify: signature: %w", err)
	}
	if err := c.ValidateWithLeeway(jwt.Expected{Issuer: issuer, AnyAudience: jwt.Audience{"buildbarn"}, Time: now}, 30*time.Second); err != nil {
		return Token{}, fmt.Errorf("verify: claims: %w", err)
	}
	if c.Expiry == nil {
		return Token{}, errors.New("verify: token has no exp")
	}
	if len(c.Cucina) == 0 {
		return Token{}, errors.New("verify: token has no cucina grants")
	}
	return Token{Raw: raw, Subject: c.Subject, KeyID: kid, Expiry: c.Expiry.Time(), Grants: c.Cucina}, nil
}
