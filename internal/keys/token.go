// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/sloper-ai/cucina/internal/ports"
)

// KeySource yields the current key set (a KeyRing in production).
type KeySource interface {
	KeySet() *KeySet
}

// StaticKeys is a KeySource that always returns the same key set (tests, tools).
type StaticKeys struct{ Set *KeySet }

// KeySet implements KeySource.
func (s StaticKeys) KeySet() *KeySet { return s.Set }

// MintRequest describes one Cucina JWT.
type MintRequest struct {
	Subject string
	// Session is the `sid`; empty means a fresh random one.
	Session string
	// Name is the optional display name (`name` claim, audit only).
	Name   string
	Scopes Scopes
	TTL    time.Duration
}

// Minter signs Cucina JWTs with the active key (ES256, always with `kid`).
type Minter struct {
	// Issuer is the exact `iss` (the STS URL without a trailing slash).
	Issuer string
	Keys   KeySource
	Clock  ports.Clock
	// Rand feeds jti/sid generation (crypto/rand when nil).
	Rand io.Reader
}

// Mint issues a token signed by the active key.
func (m *Minter) Mint(req MintRequest) (string, Claims, error) {
	ks := m.Keys.KeySet()
	if ks == nil {
		return "", Claims{}, ErrNoActiveKey
	}
	kid, _, ok := ks.Signing()
	if !ok {
		return "", Claims{}, ErrNoActiveKey
	}
	return m.MintWithKey(kid, req)
}

// MintWithKey signs with a specific published key. It exists for the rotation probe:
// a token signed by the pending key shows whether a frontend loaded it.
func (m *Minter) MintWithKey(kid string, req MintRequest) (string, Claims, error) {
	if m.Issuer == "" || strings.HasSuffix(m.Issuer, "/") {
		return "", Claims{}, errors.New("minter issuer must be set and have no trailing slash")
	}
	if !ValidSubject(req.Subject) {
		return "", Claims{}, errors.New("invalid subject")
	}
	if req.TTL <= 0 || req.TTL > MaxTokenTTL {
		return "", Claims{}, fmt.Errorf("token TTL must be in (0, %s]", MaxTokenTTL)
	}
	ks := m.Keys.KeySet()
	if ks == nil {
		return "", Claims{}, ErrNoActiveKey
	}
	priv, ok := ks.privateKey(kid)
	if !ok {
		return "", Claims{}, ErrUnknownKey
	}
	jti, err := NewID(m.Rand)
	if err != nil {
		return "", Claims{}, err
	}
	sid := req.Session
	if sid == "" {
		if sid, err = NewID(m.Rand); err != nil {
			return "", Claims{}, err
		}
	}
	if !ValidSessionID(sid) {
		return "", Claims{}, errors.New("invalid session id")
	}
	now := m.Clock.Now()
	c := Claims{
		Issuer:   m.Issuer,
		Audience: Audience,
		Subject:  req.Subject,
		IssuedAt: now.Unix(),
		// Truncate so that exp - iat == TTL exactly in whole seconds.
		Expiry:  now.Unix() + int64(req.TTL/time.Second),
		ID:      jti,
		Session: sid,
		Name:    clipName(req.Name),
		Cucina:  req.Scopes.Normalized(),
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", Claims{}, fmt.Errorf("encoding claims: %w", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: priv, KeyID: kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", Claims{}, fmt.Errorf("creating signer: %w", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", Claims{}, fmt.Errorf("signing token: %w", err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		return "", Claims{}, fmt.Errorf("serializing token: %w", err)
	}
	return raw, c, nil
}

// Verification errors. Messages never contain token material.
var (
	ErrInvalidToken = errors.New("invalid token")
	ErrTokenExpired = errors.New("token expired")
	ErrTokenRevoked = errors.New("token revoked")
)

// DenyChecker answers deny-list lookups from an in-memory snapshot.
type DenyChecker interface {
	IsDenied(sid, sub string) bool
}

// Verifier validates Cucina JWTs locally the way Buildbarn does (signature by a published
// key selected by `kid`, exact iss and aud, exp with zero leeway) plus the deny-list. It
// is used by the management API (R-AUTH-11).
type Verifier struct {
	Issuer string
	Keys   KeySource
	Clock  ports.Clock
	// Deny is optional; when set, deny-listed sid/sub are rejected with ErrTokenRevoked.
	Deny DenyChecker
}

// Verify returns the claims of a valid token.
func (v *Verifier) Verify(raw string) (*Claims, error) {
	if len(raw) == 0 || len(raw) > 16<<10 {
		return nil, ErrInvalidToken
	}
	obj, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(obj.Signatures) != 1 {
		return nil, ErrInvalidToken
	}
	kid := obj.Signatures[0].Header.KeyID
	ks := v.Keys.KeySet()
	if kid == "" || ks == nil {
		return nil, ErrInvalidToken
	}
	pub, ok := ks.PublicKey(kid)
	if !ok {
		return nil, ErrInvalidToken
	}
	payload, err := obj.Verify(pub)
	if err != nil {
		return nil, ErrInvalidToken
	}
	var c Claims
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, ErrInvalidToken
	}
	if c.Issuer != v.Issuer || c.Audience != Audience || !ValidSubject(c.Subject) || !ValidSessionID(c.Session) ||
		c.ID == "" || c.Cucina.CASRead == nil || c.Cucina.CASWrite == nil || c.Cucina.ACRead == nil ||
		c.Cucina.ACWrite == nil || c.Cucina.Execute == nil || c.Cucina.Admin == nil {
		return nil, ErrInvalidToken
	}
	now := v.Clock.Now()
	if !now.Before(time.Unix(c.Expiry, 0)) {
		return nil, ErrTokenExpired
	}
	if time.Unix(c.IssuedAt, 0).After(now.Add(ClockSkew)) {
		return nil, ErrInvalidToken
	}
	if v.Deny != nil && v.Deny.IsDenied(c.Session, c.Subject) {
		return nil, ErrTokenRevoked
	}
	return &c, nil
}
