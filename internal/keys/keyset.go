// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import (
	"cmp"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// KeyState is the life-cycle state of one signing key (R-AUTH-9).
type KeyState string

const (
	// KeyPending keys are published in the JWKS but never sign.
	KeyPending KeyState = "pending"
	// KeyActive is the single key that signs new tokens.
	KeyActive KeyState = "active"
	// KeyRetiring keys stay published (so tokens they signed keep validating) until RemoveAfter.
	KeyRetiring KeyState = "retiring"
)

// KeyRecord is the persisted, non-secret state of one signing key.
type KeyRecord struct {
	KID         string    `json:"kid"`
	State       KeyState  `json:"state"`
	CreatedAt   time.Time `json:"createdAt"`
	PublishedAt time.Time `json:"publishedAt"`
	ActivatedAt time.Time `json:"activatedAt,omitzero"`
	RetiredAt   time.Time `json:"retiredAt,omitzero"`
	RemoveAfter time.Time `json:"removeAfter,omitzero"`
}

// RotationState is the persisted rotation state (Secret entry "state.json"). Its
// methods are pure: the Rotator applies them and persists the result.
type RotationState struct {
	Version int         `json:"version"`
	Keys    []KeyRecord `json:"keys"`
}

// RotationParams bounds the state machine.
type RotationParams struct {
	// PublishLead is the minimum time a new key is published before it may sign
	// (kubelet sync + Buildbarn's 300 s JWKS reload; >= 10 min).
	PublishLead time.Duration
	// RetireGrace is how long a retired key stays published: >= max token TTL + skew
	// + the time STS replicas need to notice the promotion.
	RetireGrace time.Duration
}

// MinPublishLead is the lower bound for RotationParams.PublishLead (R-AUTH-9).
const MinPublishLead = 10 * time.Minute

// Errors of the rotation state machine.
var (
	ErrRotationInProgress = errors.New("a key rotation is already in progress")
	ErrNoActiveKey        = errors.New("no active signing key")
	ErrUnknownKey         = errors.New("unknown key id")
)

func (s RotationState) find(state KeyState) (KeyRecord, bool) {
	for _, k := range s.Keys {
		if k.State == state {
			return k, true
		}
	}
	return KeyRecord{}, false
}

// Active returns the signing key record.
func (s RotationState) Active() (KeyRecord, bool) { return s.find(KeyActive) }

// Pending returns the published-but-not-yet-signing key record.
func (s RotationState) Pending() (KeyRecord, bool) { return s.find(KeyPending) }

func (s RotationState) clone() RotationState {
	return RotationState{Version: 1, Keys: slices.Clone(s.Keys)}
}

// Bootstrap returns a state whose only key is kid, active at once. It is used when no
// Buildbarn component has loaded any key yet (first install) or after all keys were
// declared compromised.
func Bootstrap(kid string, now time.Time) RotationState {
	return RotationState{Version: 1, Keys: []KeyRecord{{
		KID: kid, State: KeyActive, CreatedAt: now, PublishedAt: now, ActivatedAt: now,
	}}}
}

// StartRotation publishes kid as the pending successor of the active key.
func (s RotationState) StartRotation(kid string, now time.Time) (RotationState, error) {
	if _, ok := s.Active(); !ok {
		return s, ErrNoActiveKey
	}
	if _, ok := s.Pending(); ok {
		return s, ErrRotationInProgress
	}
	n := s.clone()
	n.Keys = append(n.Keys, KeyRecord{KID: kid, State: KeyPending, CreatedAt: now, PublishedAt: now})
	return n, nil
}

// PromotionDue reports whether the pending key has been published for at least lead.
func (s RotationState) PromotionDue(now time.Time, p RotationParams) (KeyRecord, bool) {
	k, ok := s.Pending()
	if !ok || now.Before(k.PublishedAt.Add(p.PublishLead)) {
		return KeyRecord{}, false
	}
	return k, true
}

// Promote makes the pending key active and retires the previously active key. The
// caller must have verified that the frontends loaded the pending key.
func (s RotationState) Promote(now time.Time, p RotationParams) (RotationState, error) {
	pending, ok := s.PromotionDue(now, p)
	if !ok {
		return s, fmt.Errorf("no pending key published for at least %s", p.PublishLead)
	}
	n := s.clone()
	for i := range n.Keys {
		switch {
		case n.Keys[i].KID == pending.KID:
			n.Keys[i].State = KeyActive
			n.Keys[i].ActivatedAt = now
		case n.Keys[i].State == KeyActive:
			n.Keys[i].State = KeyRetiring
			n.Keys[i].RetiredAt = now
			n.Keys[i].RemoveAfter = now.Add(p.RetireGrace)
		}
	}
	return n, nil
}

// Prune removes retiring keys whose grace period has ended and returns their ids.
func (s RotationState) Prune(now time.Time) (RotationState, []string) {
	n := RotationState{Version: 1}
	var removed []string
	for _, k := range s.Keys {
		if k.State == KeyRetiring && !now.Before(k.RemoveAfter) {
			removed = append(removed, k.KID)
			continue
		}
		n.Keys = append(n.Keys, k)
	}
	return n, removed
}

// Compromise removes kid ("*" = every key) from the state at once. If the active key is
// removed, replacement becomes active immediately: the frontends must be restarted so
// that they drop cached tokens and load the new JWKS (R-AUTH-9 compromised-key path).
func (s RotationState) Compromise(kid, replacement string, now time.Time) (RotationState, bool, error) {
	n := RotationState{Version: 1}
	found, activeRemoved := false, false
	for _, k := range s.Keys {
		if kid == "*" || k.KID == kid {
			found = true
			activeRemoved = activeRemoved || k.State == KeyActive
			continue
		}
		n.Keys = append(n.Keys, k)
	}
	if !found {
		return s, false, ErrUnknownKey
	}
	if activeRemoved {
		n.Keys = append(n.Keys, KeyRecord{
			KID: replacement, State: KeyActive, CreatedAt: now, PublishedAt: now, ActivatedAt: now,
		})
	}
	return n, activeRemoved, nil
}

// Validate checks the structural invariants: unique kids, exactly one active key, at
// most one pending key.
func (s RotationState) Validate() error {
	seen := map[string]bool{}
	active, pending := 0, 0
	for _, k := range s.Keys {
		if k.KID == "" || seen[k.KID] {
			return fmt.Errorf("duplicate or empty kid %q", k.KID)
		}
		seen[k.KID] = true
		switch k.State {
		case KeyActive:
			active++
		case KeyPending:
			pending++
		case KeyRetiring:
		default:
			return fmt.Errorf("key %s has unknown state %q", k.KID, k.State)
		}
	}
	if active != 1 {
		return fmt.Errorf("%d active keys, want exactly 1", active)
	}
	if pending > 1 {
		return fmt.Errorf("%d pending keys, want at most 1", pending)
	}
	return nil
}

// KeySet is an immutable snapshot of the signing keys: the rotation state plus the
// private keys. It never leaves the process.
type KeySet struct {
	state RotationState
	priv  map[string]*ecdsa.PrivateKey
}

// NewKeySet assembles a key set; every key of the state needs its private key.
func NewKeySet(state RotationState, priv map[string]*ecdsa.PrivateKey) (*KeySet, error) {
	if err := state.Validate(); err != nil {
		return nil, err
	}
	ks := &KeySet{state: state.clone(), priv: map[string]*ecdsa.PrivateKey{}}
	for _, k := range state.Keys {
		p, ok := priv[k.KID]
		if !ok {
			return nil, fmt.Errorf("private key for kid %s missing", k.KID)
		}
		if want, err := KeyID(&p.PublicKey); err != nil || want != k.KID {
			return nil, fmt.Errorf("private key does not match kid %s", k.KID)
		}
		ks.priv[k.KID] = p
	}
	return ks, nil
}

// State returns the rotation state of the snapshot.
func (k *KeySet) State() RotationState { return k.state.clone() }

// Signing returns the active key.
func (k *KeySet) Signing() (string, *ecdsa.PrivateKey, bool) {
	a, ok := k.state.Active()
	if !ok {
		return "", nil, false
	}
	return a.KID, k.priv[a.KID], true
}

// privateKey returns any published key (probes are signed with pending keys).
func (k *KeySet) privateKey(kid string) (*ecdsa.PrivateKey, bool) {
	p, ok := k.priv[kid]
	return p, ok
}

// PublicKey returns the published public key with the given kid.
func (k *KeySet) PublicKey(kid string) (*ecdsa.PublicKey, bool) {
	p, ok := k.priv[kid]
	if !ok {
		return nil, false
	}
	return &p.PublicKey, true
}

// JWKS returns the public JSON Web Key Set of every published key (pending, active and
// retiring), ordered by kid. Every key carries kid, alg ES256 and use sig.
func (k *KeySet) JWKS() jose.JSONWebKeySet {
	var set jose.JSONWebKeySet
	for _, r := range k.state.Keys {
		set.Keys = append(set.Keys, jose.JSONWebKey{
			Key: &k.priv[r.KID].PublicKey, KeyID: r.KID, Algorithm: string(jose.ES256), Use: "sig",
		})
	}
	slices.SortFunc(set.Keys, func(a, b jose.JSONWebKey) int { return cmp.Compare(a.KeyID, b.KeyID) })
	return set
}

// JWKSJSON is the deterministic `jwks.json` document (ConfigMap and GET /jwks.json).
func (k *KeySet) JWKSJSON() ([]byte, error) {
	set := k.JWKS()
	if set.Keys == nil {
		set.Keys = []jose.JSONWebKey{}
	}
	return json.Marshal(set)
}

// GenerateKey creates a new ES256 (P-256) signing key and its kid.
func GenerateKey(r io.Reader) (string, *ecdsa.PrivateKey, error) {
	if r == nil {
		r = rand.Reader
	}
	p, err := ecdsa.GenerateKey(elliptic.P256(), r)
	if err != nil {
		return "", nil, fmt.Errorf("generating ES256 key: %w", err)
	}
	kid, err := KeyID(&p.PublicKey)
	if err != nil {
		return "", nil, err
	}
	return kid, p, nil
}

// KeyID is the RFC 7638 JWK thumbprint (SHA-256, base64url) of a public key.
func KeyID(pub *ecdsa.PublicKey) (string, error) {
	jwk := jose.JSONWebKey{Key: pub}
	tp, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("computing key thumbprint: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(tp), nil
}

// EncodePrivateKey returns the PKCS#8 PEM encoding of p.
func EncodePrivateKey(p *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(p)
	if err != nil {
		return nil, fmt.Errorf("encoding private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// DecodePrivateKey parses a PKCS#8 PEM P-256 key.
func DecodePrivateKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PKCS#8 PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("signing key must be ECDSA P-256")
	}
	return ec, nil
}

// Secret layout of the signing-key Secret.
const (
	stateEntry    = "state.json"
	pemSuffix     = ".pem"
	pepperEntry   = "service-key-pepper"
	jwksEntry     = "jwks.json"
	denyListEntry = "denylist.json"
	revocEntry    = "revocations.json"
)

// keySetFromSecretData parses the signing Secret's data.
func keySetFromSecretData(data map[string][]byte) (*KeySet, error) {
	raw, ok := data[stateEntry]
	if !ok {
		return nil, fmt.Errorf("signing secret has no %s", stateEntry)
	}
	var st RotationState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", stateEntry, err)
	}
	priv := map[string]*ecdsa.PrivateKey{}
	for name, v := range data {
		kid, ok := strings.CutSuffix(name, pemSuffix)
		if !ok {
			continue
		}
		p, err := DecodePrivateKey(v)
		if err != nil {
			return nil, fmt.Errorf("signing key %s: %w", kid, err)
		}
		priv[kid] = p
	}
	return NewKeySet(st, priv)
}
