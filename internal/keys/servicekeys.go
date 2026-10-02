// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sloper-ai/cucina/internal/ports"
)

// ServiceKeyPrefix starts every service-account key: cuc_sk_<id>_<secret>.
const ServiceKeyPrefix = "cuc_sk_"

// BreakGlassAccount is the reserved account of the Helm-install admin key (R-AUTH-12).
const BreakGlassAccount = "break-glass"

const (
	keyIDBytes     = 10 // 16 base32 characters
	keySecretBytes = 32 // 256 bits, 52 base32 characters
)

var (
	lowerB32       = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
	keyPattern     = regexp.MustCompile(`^cuc_sk_([a-z2-7]{16})_([a-z2-7]{52})$`)
	keyIDPattern   = regexp.MustCompile(`^[a-z2-7]{16}$`)
	accountPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ParseServiceKey splits a presented key. It is strict: any other shape is not a key.
func ParseServiceKey(s string) (id, secret string, ok bool) {
	m := keyPattern.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// LooksLikeServiceKey reports whether s claims to be a service key (prefix only).
func LooksLikeServiceKey(s string) bool { return strings.HasPrefix(s, ServiceKeyPrefix) }

// SessionForKey is the `sid` of every token minted from service key id: revoking the key
// deny-lists this sid, which kills its outstanding tokens within PropagationBound.
func SessionForKey(id string) string {
	sum := sha256.Sum256([]byte("cucina-sid-v1|service-key|" + id))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// ServiceKey is the stored record of one key. The secret is never stored, only
// HMAC-SHA-256(pepper, id, secret) (ADR 0601).
type ServiceKey struct {
	ID          string    `json:"id"`
	Account     string    `json:"account"`
	Description string    `json:"description,omitempty"`
	BreakGlass  bool      `json:"breakGlass,omitempty"`
	Hash        string    `json:"hash,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	CreatedBy   string    `json:"createdBy,omitempty"`
	ExpiresAt   time.Time `json:"expiresAt,omitzero"`
	LastUsedAt  time.Time `json:"lastUsedAt,omitzero"`
	RevokedAt   time.Time `json:"revokedAt,omitzero"`
	RevokedBy   string    `json:"revokedBy,omitempty"`
}

// Revoked reports whether the key was revoked.
func (k ServiceKey) Revoked() bool { return !k.RevokedAt.IsZero() }

// KeyError is the error of a failed service-key authentication. Error() is the same for
// every cause (clients learn nothing); Reason is for the audit log.
type KeyError struct{ Reason string }

func (e *KeyError) Error() string { return "invalid service key" }

// ErrUnknownServiceKey is returned for operations on a key id that does not exist.
var ErrUnknownServiceKey = errors.New("unknown service key")

// CreateKeyRequest describes a new key.
type CreateKeyRequest struct {
	Account     string
	Description string
	TTL         time.Duration // 0 = no expiry
	Actor       string
}

// ServiceKeys stores opaque service-account keys (R-AUTH-10) in one Secret (an entry
// per key id holding the JSON record) and authenticates presented keys. Reads go to
// the API server on every authentication, so a revocation is effective at once for new
// exchanges; Revocations (optional) deny-lists the key's sid for outstanding tokens.
type ServiceKeys struct {
	Objects Objects
	// StoreSecret is config.Auth.ServiceKeysSecret.
	StoreSecret string
	// PepperSecret is the Secret holding the pepper (config.Auth.SigningKeySecret).
	PepperSecret string
	Clock        ports.Clock
	Rand         io.Reader
	Revocations  *RevocationStore
	// LastUsedEvery throttles last-used writes per key and replica (default 5 min).
	LastUsedEvery time.Duration

	mu        sync.Mutex
	lastWrite map[string]time.Time
}

func (s *ServiceKeys) rand() io.Reader {
	if s.Rand != nil {
		return s.Rand
	}
	return rand.Reader
}

func (s *ServiceKeys) pepper(ctx context.Context) ([]byte, error) {
	sec, err := s.Objects.GetSecret(ctx, s.PepperSecret)
	if err != nil {
		return nil, fmt.Errorf("reading service-key pepper: %w", err)
	}
	p := sec.Data[pepperEntry]
	if len(p) < 32 {
		return nil, errors.New("service-key pepper missing or shorter than 32 bytes")
	}
	return p, nil
}

func keyHash(pepper []byte, id, secret string) string {
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte("cucina-service-key-v1\x00" + id + "\x00" + secret))
	return base64.RawStdEncoding.EncodeToString(m.Sum(nil))
}

// GenerateServiceKey returns a fresh key string and its id.
func GenerateServiceKey(r io.Reader) (key, id string, err error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, keyIDBytes+keySecretBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", "", fmt.Errorf("reading randomness: %w", err)
	}
	id = lowerB32.EncodeToString(b[:keyIDBytes])
	return ServiceKeyPrefix + id + "_" + lowerB32.EncodeToString(b[keyIDBytes:]), id, nil
}

func decodeRecord(b []byte) (ServiceKey, error) {
	var k ServiceKey
	err := json.Unmarshal(b, &k)
	return k, err
}

// ValidAccount reports whether name is a valid service-account name (DNS label).
func ValidAccount(name string) bool { return accountPattern.MatchString(name) }

// Create generates a key for an account. The key string is returned exactly once.
func (s *ServiceKeys) Create(ctx context.Context, req CreateKeyRequest) (string, ServiceKey, error) {
	if !ValidAccount(req.Account) {
		return "", ServiceKey{}, errors.New("account must be a DNS label ([a-z0-9-], max 63)")
	}
	if req.Account == BreakGlassAccount {
		return "", ServiceKey{}, errors.New("account name break-glass is reserved")
	}
	if req.TTL < 0 {
		return "", ServiceKey{}, errors.New("ttl must not be negative")
	}
	key, _, err := GenerateServiceKey(s.rand())
	if err != nil {
		return "", ServiceKey{}, err
	}
	rec, err := s.register(ctx, key, req.Account, req.Description, req.Actor, req.TTL, false)
	if err != nil {
		return "", ServiceKey{}, err
	}
	return key, rec, nil
}

// register stores the hash of an existing key string (Create, break-glass bootstrap).
func (s *ServiceKeys) register(ctx context.Context, key, account, description, actor string, ttl time.Duration, breakGlass bool) (ServiceKey, error) {
	id, secret, ok := ParseServiceKey(key)
	if !ok {
		return ServiceKey{}, errors.New("malformed service key")
	}
	pepper, err := s.pepper(ctx)
	if err != nil {
		return ServiceKey{}, err
	}
	now := s.Clock.Now()
	rec := ServiceKey{
		ID: id, Account: account, Description: description, BreakGlass: breakGlass,
		Hash: keyHash(pepper, id, secret), CreatedAt: now, CreatedBy: actor,
	}
	if ttl > 0 {
		rec.ExpiresAt = now.Add(ttl)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return ServiceKey{}, err
	}
	_, err = updateSecret(ctx, s.Objects, s.StoreSecret, func(sec *corev1.Secret) (bool, error) {
		if _, exists := sec.Data[id]; exists {
			return false, errors.New("service key id collision")
		}
		sec.Data[id] = b
		return true, nil
	})
	if errors.Is(err, ErrNotFound) {
		_, err = s.Objects.CreateSecret(ctx, newSecret(s.StoreSecret, map[string][]byte{id: b}))
	}
	if err != nil {
		return ServiceKey{}, err
	}
	rec.Hash = ""
	return rec, nil
}

// Get returns one record (without its hash).
func (s *ServiceKeys) Get(ctx context.Context, id string) (ServiceKey, error) {
	sec, err := s.Objects.GetSecret(ctx, s.StoreSecret)
	if errors.Is(err, ErrNotFound) {
		return ServiceKey{}, ErrUnknownServiceKey
	}
	if err != nil {
		return ServiceKey{}, err
	}
	b, ok := sec.Data[id]
	if !ok {
		return ServiceKey{}, ErrUnknownServiceKey
	}
	rec, err := decodeRecord(b)
	rec.Hash = ""
	return rec, err
}

// List returns the keys of account ("" = all), ordered by creation, without hashes.
func (s *ServiceKeys) List(ctx context.Context, account string) ([]ServiceKey, error) {
	sec, err := s.Objects.GetSecret(ctx, s.StoreSecret)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ServiceKey
	for _, b := range sec.Data {
		rec, err := decodeRecord(b)
		if err != nil {
			return nil, err
		}
		if account != "" && rec.Account != account {
			continue
		}
		rec.Hash = ""
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b ServiceKey) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Revoke disables a key at once for new exchanges and deny-lists its session so that
// tokens already minted from it stop working within PropagationBound.
func (s *ServiceKeys) Revoke(ctx context.Context, id, actor string) error {
	if !keyIDPattern.MatchString(id) {
		return ErrUnknownServiceKey
	}
	_, err := updateSecret(ctx, s.Objects, s.StoreSecret, func(sec *corev1.Secret) (bool, error) {
		b, ok := sec.Data[id]
		if !ok {
			return false, ErrUnknownServiceKey
		}
		rec, err := decodeRecord(b)
		if err != nil {
			return false, err
		}
		if rec.Revoked() {
			return false, nil
		}
		rec.RevokedAt, rec.RevokedBy = s.Clock.Now(), actor
		if sec.Data[id], err = json.Marshal(rec); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, ErrNotFound) {
		return ErrUnknownServiceKey
	}
	if err != nil {
		return err
	}
	if s.Revocations != nil {
		_, _, err = s.Revocations.Revoke(ctx, RevokeRequest{
			Kind: RevokeSession, Value: SessionForKey(id), Reason: "service key " + id + " revoked", Actor: actor,
		})
	}
	return err
}

// Authenticate verifies a presented key and returns its record. Every failure is a
// *KeyError. A successful use updates LastUsedAt (throttled; best effort).
func (s *ServiceKeys) Authenticate(ctx context.Context, presented string) (ServiceKey, error) {
	id, secret, ok := ParseServiceKey(presented)
	if !ok {
		return ServiceKey{}, &KeyError{Reason: "malformed service key"}
	}
	sec, err := s.Objects.GetSecret(ctx, s.StoreSecret)
	if errors.Is(err, ErrNotFound) {
		return ServiceKey{}, &KeyError{Reason: "unknown service key"}
	}
	if err != nil {
		return ServiceKey{}, err
	}
	b, ok := sec.Data[id]
	if !ok {
		return ServiceKey{}, &KeyError{Reason: "unknown service key"}
	}
	rec, err := decodeRecord(b)
	if err != nil {
		return ServiceKey{}, err
	}
	pepper, err := s.pepper(ctx)
	if err != nil {
		return ServiceKey{}, err
	}
	if !hmac.Equal([]byte(keyHash(pepper, id, secret)), []byte(rec.Hash)) {
		return ServiceKey{}, &KeyError{Reason: "service key secret mismatch"}
	}
	now := s.Clock.Now()
	switch {
	case rec.Revoked():
		return ServiceKey{}, &KeyError{Reason: "service key revoked"}
	case !rec.ExpiresAt.IsZero() && !now.Before(rec.ExpiresAt):
		return ServiceKey{}, &KeyError{Reason: "service key expired"}
	}
	s.touch(ctx, id, now)
	rec.Hash = ""
	return rec, nil
}

// touch records last use, at most once per LastUsedEvery per key on this replica.
func (s *ServiceKeys) touch(ctx context.Context, id string, now time.Time) {
	every := s.LastUsedEvery
	if every <= 0 {
		every = 5 * time.Minute
	}
	s.mu.Lock()
	if s.lastWrite == nil {
		s.lastWrite = map[string]time.Time{}
	}
	if last, ok := s.lastWrite[id]; ok && now.Sub(last) < every {
		s.mu.Unlock()
		return
	}
	s.lastWrite[id] = now
	s.mu.Unlock()
	_, _ = updateSecret(ctx, s.Objects, s.StoreSecret, func(sec *corev1.Secret) (bool, error) {
		b, ok := sec.Data[id]
		if !ok {
			return false, nil
		}
		rec, err := decodeRecord(b)
		if err != nil || !rec.LastUsedAt.Before(now) {
			return false, err
		}
		rec.LastUsedAt = now
		sec.Data[id], err = json.Marshal(rec)
		return err == nil, err
	})
}
