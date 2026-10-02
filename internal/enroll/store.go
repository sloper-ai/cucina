// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"
)

// Store errors.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// ------------------------------------------------------------------ tokens

// TokenRecord is a site enrollment token as stored: never the secret, only its
// SHA-256 (R-SEC-3: stored hashed).
type TokenRecord struct {
	ID          string     `json:"id"`
	Site        string     `json:"site"`
	SecretHash  string     `json:"secretSha256"`
	MaxHosts    int        `json:"maxHosts"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	CreatedBy   string     `json:"createdBy,omitempty"`
	Description string     `json:"description,omitempty"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
	RevokedBy   string     `json:"revokedBy,omitempty"`
	// Hosts are the serials bound to the token (pending or enrolled); they count
	// towards MaxHosts. Removing a host unbinds it.
	Hosts []string `json:"hosts,omitempty"`
}

// TokenStore persists token records. Update applies fn atomically (the
// Kubernetes implementation retries on write conflicts, so fn may run more
// than once and must not have side effects beyond the record).
type TokenStore interface {
	Get(ctx context.Context, id string) (TokenRecord, error)
	List(ctx context.Context) ([]TokenRecord, error)
	Create(ctx context.Context, rec TokenRecord) error
	Update(ctx context.Context, id string, fn func(*TokenRecord) error) (TokenRecord, error)
}

// ------------------------------------------------------------------- hosts

// HostRecord is the enrollment view of one MacHost (R-SEC-3).
type HostRecord struct {
	Serial string // canonical upper case
	// Name is the MacHost object name (lower-case serial for objects Cucina creates).
	Name     string
	Site     string
	Labels   map[string]string
	Approved bool
	// TokenID is the site token that bound the host (empty for hosts that never
	// presented one).
	TokenID string
	// KeySHA256 is the identity key bound at first contact (pending) or at
	// enrollment; hex SHA-256 of the DER SubjectPublicKeyInfo.
	KeySHA256    string
	PendingSince time.Time // first unapproved contact
	EnrolledAt   time.Time // zero until a host certificate was issued
	CertExpiry   time.Time
	Hostname     string
}

// Enrolled reports whether a host certificate was issued.
func (h HostRecord) Enrolled() bool { return !h.EnrolledAt.IsZero() }

// HostStore persists host records (production: the MacHost CR).
type HostStore interface {
	Get(ctx context.Context, serial string) (HostRecord, error)
	List(ctx context.Context) ([]HostRecord, error)
	Create(ctx context.Context, rec HostRecord) error
	Update(ctx context.Context, serial string, fn func(*HostRecord) error) (HostRecord, error)
	Delete(ctx context.Context, serial string) error
}

// ---------------------------------------------------------- worker launches

// LaunchRecord remembers the certificates issued for one EC2 instance launch
// (one-per-boot semantics with a short crash-recovery window).
type LaunchRecord struct {
	InstanceID  string    `json:"instanceId"`
	PendingTime time.Time `json:"pendingTime"`
	KeySHA256   string    `json:"keySha256"`
	FirstIssued time.Time `json:"firstIssued"`
	Issued      int       `json:"issued"`
}

// ReplayStore persists launch records shared by every controller replica.
// Claim applies fn to the current record (found=false: zero record) and stores
// the result atomically if fn returns nil.
type ReplayStore interface {
	Claim(ctx context.Context, instanceID string, fn func(rec *LaunchRecord, found bool) error) error
	// Prune deletes records whose first issuance is before cutoff.
	Prune(ctx context.Context, cutoff time.Time) (int, error)
}

// ------------------------------------------------------- memory implementations

// MemoryTokens is an in-memory TokenStore (tests, single replica).
type MemoryTokens struct {
	mu sync.Mutex
	m  map[string]TokenRecord
}

// NewMemoryTokens returns an empty store.
func NewMemoryTokens() *MemoryTokens { return &MemoryTokens{m: map[string]TokenRecord{}} }

// Get implements TokenStore.
func (s *MemoryTokens) Get(_ context.Context, id string) (TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[id]
	if !ok {
		return TokenRecord{}, ErrNotFound
	}
	return cloneToken(r), nil
}

// List implements TokenStore.
func (s *MemoryTokens) List(context.Context) ([]TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TokenRecord, 0, len(s.m))
	for _, r := range s.m {
		out = append(out, cloneToken(r))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt) || out[i].CreatedAt.Equal(out[j].CreatedAt) && out[i].ID < out[j].ID
	})
	return out, nil
}

// Create implements TokenStore.
func (s *MemoryTokens) Create(_ context.Context, rec TokenRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[rec.ID]; ok {
		return ErrExists
	}
	s.m[rec.ID] = cloneToken(rec)
	return nil
}

// Update implements TokenStore.
func (s *MemoryTokens) Update(_ context.Context, id string, fn func(*TokenRecord) error) (TokenRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[id]
	if !ok {
		return TokenRecord{}, ErrNotFound
	}
	r = cloneToken(r)
	if err := fn(&r); err != nil {
		return TokenRecord{}, err
	}
	s.m[id] = cloneToken(r)
	return r, nil
}

func cloneToken(r TokenRecord) TokenRecord {
	r.Hosts = slices.Clone(r.Hosts)
	if r.RevokedAt != nil {
		t := *r.RevokedAt
		r.RevokedAt = &t
	}
	return r
}

// MemoryHosts is an in-memory HostStore (tests).
type MemoryHosts struct {
	mu sync.Mutex
	m  map[string]HostRecord
}

// NewMemoryHosts returns an empty store.
func NewMemoryHosts() *MemoryHosts { return &MemoryHosts{m: map[string]HostRecord{}} }

// Get implements HostStore.
func (s *MemoryHosts) Get(_ context.Context, serial string) (HostRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[serial]
	if !ok {
		return HostRecord{}, ErrNotFound
	}
	return cloneHost(r), nil
}

// List implements HostStore.
func (s *MemoryHosts) List(context.Context) ([]HostRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HostRecord, 0, len(s.m))
	for _, r := range s.m {
		out = append(out, cloneHost(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out, nil
}

// Create implements HostStore.
func (s *MemoryHosts) Create(_ context.Context, rec HostRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[rec.Serial]; ok {
		return ErrExists
	}
	s.m[rec.Serial] = cloneHost(rec)
	return nil
}

// Update implements HostStore.
func (s *MemoryHosts) Update(_ context.Context, serial string, fn func(*HostRecord) error) (HostRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[serial]
	if !ok {
		return HostRecord{}, ErrNotFound
	}
	r = cloneHost(r)
	if err := fn(&r); err != nil {
		return HostRecord{}, err
	}
	s.m[serial] = cloneHost(r)
	return r, nil
}

// Delete implements HostStore.
func (s *MemoryHosts) Delete(_ context.Context, serial string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[serial]; !ok {
		return ErrNotFound
	}
	delete(s.m, serial)
	return nil
}

func cloneHost(r HostRecord) HostRecord {
	r.Labels = maps.Clone(r.Labels)
	return r
}

// MemoryReplay is an in-memory ReplayStore (tests, single replica).
type MemoryReplay struct {
	mu sync.Mutex
	m  map[string]LaunchRecord
}

// NewMemoryReplay returns an empty store.
func NewMemoryReplay() *MemoryReplay { return &MemoryReplay{m: map[string]LaunchRecord{}} }

// Claim implements ReplayStore.
func (s *MemoryReplay) Claim(_ context.Context, id string, fn func(*LaunchRecord, bool) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, found := s.m[id]
	if err := fn(&r, found); err != nil {
		return err
	}
	s.m[id] = r
	return nil
}

// Prune implements ReplayStore.
func (s *MemoryReplay) Prune(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.m {
		if r.FirstIssued.Before(cutoff) {
			delete(s.m, id)
			n++
		}
	}
	return n, nil
}
