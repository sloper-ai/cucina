// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sloper-ai/cucina/internal/ports"
)

// RevocationKind selects what a deny-list entry matches.
type RevocationKind string

const (
	// RevokeSession matches the JWT `sid` (one session or one service key's tokens).
	RevokeSession RevocationKind = "sid"
	// RevokeSubject matches the JWT `sub` or a workload certificate's URI SAN.
	RevokeSubject RevocationKind = "sub"
)

// Revocation is one deny-list record (ConfigMap entry revocations.json).
type Revocation struct {
	Kind      RevocationKind `json:"kind"`
	Value     string         `json:"value"`
	Reason    string         `json:"reason,omitempty"`
	CreatedBy string         `json:"createdBy,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
	// ExpiresAt is when the entry is pruned; zero = never (subjects only).
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

func (r Revocation) active(now time.Time) bool {
	return r.ExpiresAt.IsZero() || now.Before(r.ExpiresAt)
}

// PropagationBound is the documented worst case from a Revoke call until every Buildbarn
// authorizer denies (kubelet ConfigMap sync + Buildbarn's 60 s file reload), R-AUTH-9.
const PropagationBound = 3 * time.Minute

// MaxDenyListBytes caps denylist.json: every authorizer scans the whole file per request.
const MaxDenyListBytes = 64 << 10

// ErrDenyListFull is returned when an entry would push denylist.json over MaxDenyListBytes.
var ErrDenyListFull = errors.New("deny-list is full (64 KiB); remove or let entries expire first")

// denyFile is the shape of denylist.json (docs/security.md §Deny-list).
type denyFile struct {
	Version int      `json:"version"`
	Sids    []string `json:"sids"`
	Subs    []string `json:"subs"`
}

// MatchToken is the string a deny-list entry contributes to denylist.json and that the
// Buildbarn authorizer searches for (with the surrounding quotes).
func MatchToken(kind RevocationKind, value string) string { return string(kind) + ":" + value }

func validRevocationValue(kind RevocationKind, v string) error {
	switch kind {
	case RevokeSession:
		if !ValidSessionID(v) {
			return errors.New("sid must be 22 characters of [A-Za-z0-9_-]")
		}
	case RevokeSubject:
		if !ValidSubject(v) {
			return errors.New("sub must have the form <scheme>:<id> with characters [A-Za-z0-9._~:@/+=%-] (max 512 bytes)")
		}
	default:
		return fmt.Errorf("unknown revocation kind %q", kind)
	}
	return nil
}

// EncodeDenyList renders denylist.json from the records active at now. Each entry is
// verified to appear byte-for-byte as its quoted match token, so the Buildbarn substring
// test is an exact match; the result is capped at MaxDenyListBytes.
func EncodeDenyList(revs []Revocation, now time.Time) ([]byte, error) {
	f := denyFile{Version: 1, Sids: []string{}, Subs: []string{}}
	for _, r := range revs {
		if !r.active(now) {
			continue
		}
		if err := validRevocationValue(r.Kind, r.Value); err != nil {
			return nil, err
		}
		if r.Kind == RevokeSession {
			f.Sids = append(f.Sids, MatchToken(r.Kind, r.Value))
		} else {
			f.Subs = append(f.Subs, MatchToken(r.Kind, r.Value))
		}
	}
	slices.Sort(f.Sids)
	f.Sids = slices.Compact(f.Sids)
	slices.Sort(f.Subs)
	f.Subs = slices.Compact(f.Subs)
	// Each token must encode as itself in quotes; then the array encoding contains it
	// verbatim and the authorizer's substring test is exact.
	for _, tok := range append(slices.Clone(f.Sids), f.Subs...) {
		var one bytes.Buffer
		enc := json.NewEncoder(&one)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(tok); err != nil || one.String() != `"`+tok+"\"\n" {
			return nil, fmt.Errorf("deny-list entry would be escaped in JSON")
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	if len(out) > MaxDenyListBytes {
		return nil, ErrDenyListFull
	}
	return out, nil
}

// EmptyDenyList is the content bootstrap writes.
const EmptyDenyList = `{"version":1,"sids":[],"subs":[]}`

// RevokeRequest asks for one deny-list entry.
type RevokeRequest struct {
	Kind   RevocationKind
	Value  string
	Reason string
	Actor  string
	// ExpiresAt is optional for subjects; for sessions it defaults to (and may not be
	// earlier than) now + SessionRetention.
	ExpiresAt time.Time
}

// RevocationStore manages the deny-list ConfigMap (source of truth for revocations) and
// keeps an in-memory snapshot for the STS and the management API.
type RevocationStore struct {
	Objects   Objects
	ConfigMap string
	Clock     ports.Clock
	// SessionRetention is how long sid entries stay: max token TTL + skew (default 17 min).
	SessionRetention time.Duration
	// RefreshInterval is the snapshot refresh period of Run (default 10 s).
	RefreshInterval time.Duration
	Log             *slog.Logger

	snap atomic.Pointer[[]Revocation]
}

func (s *RevocationStore) retention() time.Duration {
	if s.SessionRetention > 0 {
		return s.SessionRetention
	}
	return MaxTokenTTL + ClockSkew
}

func parseRevocations(c *corev1.ConfigMap) ([]Revocation, error) {
	raw := c.Data[revocEntry]
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var revs []Revocation
	if err := json.Unmarshal([]byte(raw), &revs); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", revocEntry, err)
	}
	return revs, nil
}

// write rewrites both entries of the ConfigMap from revs (pruned at now).
func (s *RevocationStore) write(ctx context.Context, mutate func(now time.Time, revs []Revocation) ([]Revocation, error)) ([]Revocation, error) {
	var result []Revocation
	_, err := updateConfigMap(ctx, s.Objects, s.ConfigMap, func(c *corev1.ConfigMap) (bool, error) {
		cur, err := parseRevocations(c)
		if err != nil {
			return false, err
		}
		now := s.Clock.Now()
		next, err := mutate(now, cur)
		if err != nil {
			return false, err
		}
		next = slices.DeleteFunc(next, func(r Revocation) bool { return !r.active(now) })
		slices.SortFunc(next, func(a, b Revocation) int {
			return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Value, b.Value))
		})
		deny, err := EncodeDenyList(next, now)
		if err != nil {
			return false, err
		}
		recs, err := json.Marshal(next)
		if err != nil {
			return false, err
		}
		if next == nil {
			recs = []byte("[]")
		}
		result = next
		if c.Data[denyListEntry] == string(deny) && c.Data[revocEntry] == string(recs) {
			return false, nil
		}
		c.Data[denyListEntry] = string(deny)
		c.Data[revocEntry] = string(recs)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	s.snap.Store(&result)
	return result, nil
}

// Revoke adds (or extends) a deny-list entry. It returns the stored record and the time
// by which every Buildbarn authorizer is guaranteed to deny (now + PropagationBound).
// The local snapshot is updated at once, so this replica refuses renewals immediately.
func (s *RevocationStore) Revoke(ctx context.Context, req RevokeRequest) (Revocation, time.Time, error) {
	if err := validRevocationValue(req.Kind, req.Value); err != nil {
		return Revocation{}, time.Time{}, err
	}
	var stored Revocation
	_, err := s.write(ctx, func(now time.Time, revs []Revocation) ([]Revocation, error) {
		rec := Revocation{
			Kind: req.Kind, Value: req.Value, Reason: req.Reason, CreatedBy: req.Actor,
			CreatedAt: now, ExpiresAt: req.ExpiresAt,
		}
		if req.Kind == RevokeSession {
			if floor := now.Add(s.retention()); rec.ExpiresAt.Before(floor) {
				rec.ExpiresAt = floor
			}
		} else if !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(now) {
			return nil, errors.New("expiry must be in the future")
		}
		for i, r := range revs {
			if r.Kind == rec.Kind && r.Value == rec.Value {
				// Keep the earliest creation and the latest (or no) expiry.
				rec.CreatedAt = r.CreatedAt
				if r.ExpiresAt.IsZero() || (!rec.ExpiresAt.IsZero() && r.ExpiresAt.After(rec.ExpiresAt)) {
					rec.ExpiresAt = r.ExpiresAt
				}
				revs[i] = rec
				stored = rec
				return revs, nil
			}
		}
		stored = rec
		return append(revs, rec), nil
	})
	if err != nil {
		return Revocation{}, time.Time{}, err
	}
	return stored, s.Clock.Now().Add(PropagationBound), nil
}

// Remove deletes a deny-list entry (un-revoke).
func (s *RevocationStore) Remove(ctx context.Context, kind RevocationKind, value string) error {
	_, err := s.write(ctx, func(_ time.Time, revs []Revocation) ([]Revocation, error) {
		return slices.DeleteFunc(revs, func(r Revocation) bool { return r.Kind == kind && r.Value == value }), nil
	})
	return err
}

// Prune drops expired entries (leader, periodically).
func (s *RevocationStore) Prune(ctx context.Context) error {
	_, err := s.write(ctx, func(_ time.Time, revs []Revocation) ([]Revocation, error) { return revs, nil })
	return err
}

// List returns the active records, read from the API server.
func (s *RevocationStore) List(ctx context.Context) ([]Revocation, error) {
	if err := s.Refresh(ctx); err != nil {
		return nil, err
	}
	now := s.Clock.Now()
	var out []Revocation
	for _, r := range s.snapshot() {
		if r.active(now) {
			out = append(out, r)
		}
	}
	return out, nil
}

// Refresh reloads the snapshot from the ConfigMap.
func (s *RevocationStore) Refresh(ctx context.Context) error {
	c, err := s.Objects.GetConfigMap(ctx, s.ConfigMap)
	if err != nil {
		return err
	}
	revs, err := parseRevocations(c)
	if err != nil {
		return err
	}
	s.snap.Store(&revs)
	return nil
}

func (s *RevocationStore) snapshot() []Revocation {
	if p := s.snap.Load(); p != nil {
		return *p
	}
	return nil
}

// IsDenied implements DenyChecker from the in-memory snapshot (no I/O).
func (s *RevocationStore) IsDenied(sid, sub string) bool {
	now := s.Clock.Now()
	for _, r := range s.snapshot() {
		if !r.active(now) {
			continue
		}
		if (r.Kind == RevokeSession && sid != "" && r.Value == sid) || (r.Kind == RevokeSubject && sub != "" && r.Value == sub) {
			return true
		}
	}
	return false
}

// Run refreshes the snapshot every RefreshInterval; with prune set (leader only) it also
// rewrites the ConfigMap to drop expired entries.
func (s *RevocationStore) Run(ctx context.Context, prune bool) error {
	every := s.RefreshInterval
	if every <= 0 {
		every = 10 * time.Second
	}
	for {
		var err error
		if prune {
			err = s.Prune(ctx)
		} else {
			err = s.Refresh(ctx)
		}
		if err != nil && ctx.Err() == nil {
			l := s.Log
			if l == nil {
				l = slog.Default()
			}
			l.Warn("refreshing deny-list failed; keeping the previous snapshot", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.Clock.After(every):
		}
	}
}
