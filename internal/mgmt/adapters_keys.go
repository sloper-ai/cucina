// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Adapters to internal/keys (agent auth): the local Cucina JWT verifier, the
// service-account key store and the deny-list.

// TokenVerifier validates a Cucina JWT locally; *keys.Verifier implements it
// (signature by a published key selected by kid, exact iss and aud, exp, deny-list).
type TokenVerifier interface {
	Verify(raw string) (*keys.Claims, error)
}

var _ TokenVerifier = (*keys.Verifier)(nil)

// NewTokenAuthenticator adapts the Cucina JWT verifier to Authenticator.
func NewTokenAuthenticator(v TokenVerifier) Authenticator { return tokenAuthenticator{v} }

type tokenAuthenticator struct{ v TokenVerifier }

func (a tokenAuthenticator) Authenticate(_ context.Context, token string) (Principal, error) {
	c, err := a.v.Verify(token)
	if err != nil {
		return Principal{}, err
	}
	s := c.Cucina
	return Principal{
		Subject:   c.Subject,
		SessionID: c.Session,
		Grants: map[string][]string{
			v1alpha1.VerbCASRead: s.CASRead, v1alpha1.VerbCASWrite: s.CASWrite,
			v1alpha1.VerbACRead: s.ACRead, v1alpha1.VerbACWrite: s.ACWrite,
			v1alpha1.VerbExecute: s.Execute, v1alpha1.VerbAdmin: s.Admin,
		},
	}, nil
}

// KeyStore is the part of *keys.Manager behind KeyAdmin and RevocationAdmin.
type KeyStore interface {
	CreateServiceKey(ctx context.Context, account, description string, ttl time.Duration, actor string) (string, keys.ServiceKey, error)
	ListServiceKeys(ctx context.Context, account string) ([]keys.ServiceKey, error)
	RevokeServiceKey(ctx context.Context, id, actor string) error
	Revoke(ctx context.Context, req keys.RevokeRequest) (keys.Revocation, time.Time, error)
	ListRevocations(ctx context.Context) ([]keys.Revocation, error)
}

var _ KeyStore = (*keys.Manager)(nil)

// KeysAdapter implements KeyAdmin and RevocationAdmin over the key manager.
type KeysAdapter struct{ Store KeyStore }

var (
	_ KeyAdmin        = (*KeysAdapter)(nil)
	_ RevocationAdmin = (*KeysAdapter)(nil)
)

func serviceKey(k keys.ServiceKey) ServiceKey {
	return ServiceKey{
		ID: k.ID, Account: k.Account, Description: k.Description, Created: k.CreatedAt,
		ExpiresAt: k.ExpiresAt, LastUsed: k.LastUsedAt, Revoked: k.Revoked(),
	}
}

// CreateServiceKey implements KeyAdmin.
func (a *KeysAdapter) CreateServiceKey(ctx context.Context, req ServiceKeyRequest) (ServiceKey, string, error) {
	if !keys.ValidAccount(req.Account) {
		return ServiceKey{}, "", fmt.Errorf("%w: account must be a DNS label ([a-z0-9-], at most 63 characters)", ports.ErrInvalid)
	}
	if req.Account == keys.BreakGlassAccount {
		return ServiceKey{}, "", fmt.Errorf("%w: account %q is reserved for the Helm-generated key", ports.ErrInvalid, req.Account)
	}
	secret, k, err := a.Store.CreateServiceKey(ctx, req.Account, req.Description, req.TTL, req.CreatedBy)
	if err != nil {
		return ServiceKey{}, "", err
	}
	return serviceKey(k), secret, nil
}

// ListServiceKeys implements KeyAdmin.
func (a *KeysAdapter) ListServiceKeys(ctx context.Context, account string) ([]ServiceKey, error) {
	ks, err := a.Store.ListServiceKeys(ctx, account)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceKey, 0, len(ks))
	for _, k := range ks {
		out = append(out, serviceKey(k))
	}
	return out, nil
}

// RevokeServiceKey implements KeyAdmin; the caller is recorded as the actor.
func (a *KeysAdapter) RevokeServiceKey(ctx context.Context, id string) error {
	err := a.Store.RevokeServiceKey(ctx, id, caller(ctx))
	if errors.Is(err, keys.ErrUnknownServiceKey) {
		return fmt.Errorf("%w: service key %q", ports.ErrNotFound, id)
	}
	return err
}

// Revoke implements RevocationAdmin: one deny-list entry per given subject and
// session; it reports the latest enforcement time. Both values are validated before
// anything is written.
func (a *KeysAdapter) Revoke(ctx context.Context, r Revocation) (time.Time, error) {
	type entry struct {
		kind  keys.RevocationKind
		value string
	}
	var entries []entry
	if r.Subject != "" {
		if !keys.ValidSubject(r.Subject) {
			return time.Time{}, fmt.Errorf("%w: sub must have the form <scheme>:<id>", ports.ErrInvalid)
		}
		entries = append(entries, entry{keys.RevokeSubject, r.Subject})
	}
	if r.SessionID != "" {
		if !keys.ValidSessionID(r.SessionID) {
			return time.Time{}, fmt.Errorf("%w: sid must be 22 characters of [A-Za-z0-9_-]", ports.ErrInvalid)
		}
		entries = append(entries, entry{keys.RevokeSession, r.SessionID})
	}
	var latest time.Time
	for _, e := range entries {
		_, eff, err := a.Store.Revoke(ctx, keys.RevokeRequest{Kind: e.kind, Value: e.value, Reason: r.Reason, Actor: r.CreatedBy})
		if errors.Is(err, keys.ErrDenyListFull) {
			return time.Time{}, status.Error(codes.FailedPrecondition, err.Error())
		}
		if err != nil {
			return time.Time{}, err
		}
		if eff.After(latest) {
			latest = eff
		}
	}
	return latest, nil
}

// ListRevocations implements RevocationAdmin.
func (a *KeysAdapter) ListRevocations(ctx context.Context) ([]Revocation, error) {
	rs, err := a.Store.ListRevocations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Revocation, 0, len(rs))
	for _, r := range rs {
		rev := Revocation{Reason: r.Reason, Created: r.CreatedAt, CreatedBy: r.CreatedBy}
		if r.Kind == keys.RevokeSession {
			rev.SessionID = r.Value
		} else {
			rev.Subject = r.Value
		}
		out = append(out, rev)
	}
	return out, nil
}
