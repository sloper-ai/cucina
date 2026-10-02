// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Options are the injectable dependencies of a Manager.
type Options struct {
	Clock ports.Clock // default: wall clock
	Rand  io.Reader   // default: crypto/rand
	Log   *slog.Logger
	// LoadVerifier and Restarter are the rotation hooks wired by the controller.
	LoadVerifier LoadVerifier
	Restarter    FrontendRestarter
	// RotateEvery starts scheduled rotations (0 = only on demand).
	RotateEvery time.Duration
	// KeyRingInterval and DenyListRefresh are the replica refresh periods.
	KeyRingInterval time.Duration
	DenyListRefresh time.Duration
}

// Manager bundles the key components of one process and exposes the methods the
// management API (internal/mgmt) calls: CreateServiceKey, ListServiceKeys,
// RevokeServiceKey, Revoke and ListRevocations.
type Manager struct {
	Ring        *KeyRing
	Rotator     *Rotator
	Revocations *RevocationStore
	ServiceKeys *ServiceKeys
	Minter      *Minter
	Verifier    *Verifier
	TokenTTL    time.Duration
}

// ValidateAuthConfig fails fast on an unusable config.Auth (R-TEST-7).
func ValidateAuthConfig(cfg config.Auth) error {
	var errs []error
	for name, v := range map[string]string{
		"signingKeySecret": cfg.SigningKeySecret, "jwksConfigMap": cfg.JWKSConfigMap,
		"denyListConfigMap": cfg.DenyListConfigMap, "serviceKeysSecret": cfg.ServiceKeysSecret,
	} {
		if v == "" {
			errs = append(errs, fmt.Errorf("auth.%s is required", name))
		}
	}
	if cfg.TokenTTL.Duration < 0 || cfg.TokenTTL.Duration > MaxTokenTTL {
		errs = append(errs, fmt.Errorf("auth.tokenTTL must be at most %s", MaxTokenTTL))
	}
	if cfg.Audience != "" && cfg.Audience != Audience {
		errs = append(errs, fmt.Errorf("auth.audience must be %q (the Buildbarn jwt policy checks it)", Audience))
	}
	if d := cfg.KeyRotationPublishLead.Duration; d != 0 && d < MinPublishLead {
		errs = append(errs, fmt.Errorf("auth.keyRotationPublishLead must be at least %s", MinPublishLead))
	}
	if cfg.RateLimitPerMinute < 0 {
		errs = append(errs, errors.New("auth.rateLimitPerMinute must not be negative"))
	}
	return errors.Join(errs...)
}

// NewManager assembles the components from the controller configuration. issuer is the
// STS URL (`endpoints.stsUrl`); a trailing slash is removed.
func NewManager(o Objects, cfg config.Auth, issuer string, opts Options) (*Manager, error) {
	if err := ValidateAuthConfig(cfg); err != nil {
		return nil, err
	}
	issuer = strings.TrimRight(issuer, "/")
	if !strings.HasPrefix(issuer, "https://") {
		return nil, fmt.Errorf("endpoints.stsUrl must be an https:// URL")
	}
	clock := opts.Clock
	if clock == nil {
		clock = SystemClock
	}
	ttl := cfg.TokenTTL.Duration
	if ttl == 0 {
		ttl = MaxTokenTTL
	}
	ring := &KeyRing{Objects: o, SecretName: cfg.SigningKeySecret, Clock: clock, Interval: opts.KeyRingInterval, Log: opts.Log}
	revs := &RevocationStore{
		Objects: o, ConfigMap: cfg.DenyListConfigMap, Clock: clock,
		SessionRetention: ttl + ClockSkew, RefreshInterval: opts.DenyListRefresh, Log: opts.Log,
	}
	m := &Manager{
		Ring: ring,
		Rotator: &Rotator{
			Objects: o, Clock: clock, Rand: opts.Rand, Loaded: opts.LoadVerifier, Restarter: opts.Restarter, Log: opts.Log,
			Config: RotatorConfig{
				SigningKeySecret: cfg.SigningKeySecret, JWKSConfigMap: cfg.JWKSConfigMap,
				PublishLead: cfg.KeyRotationPublishLead.Duration, TokenTTL: ttl,
				SignerReload: opts.KeyRingInterval, RotateEvery: opts.RotateEvery,
			},
		},
		Revocations: revs,
		ServiceKeys: &ServiceKeys{
			Objects: o, StoreSecret: cfg.ServiceKeysSecret, PepperSecret: cfg.SigningKeySecret,
			Clock: clock, Rand: opts.Rand, Revocations: revs,
		},
		Minter:   &Minter{Issuer: issuer, Keys: ring, Clock: clock, Rand: opts.Rand},
		Verifier: &Verifier{Issuer: issuer, Keys: ring, Clock: clock, Deny: revs},
		TokenTTL: ttl,
	}
	return m, nil
}

// Start loads the key set and the deny-list once (call before serving).
func (m *Manager) Start(ctx context.Context) error {
	if err := m.Ring.Refresh(ctx); err != nil {
		return fmt.Errorf("loading signing keys: %w", err)
	}
	if err := m.Revocations.Refresh(ctx); err != nil {
		return fmt.Errorf("loading deny-list: %w", err)
	}
	return nil
}

// Run keeps the replica state fresh until ctx ends: the key ring and the deny-list
// snapshot on every replica; with leader set also the rotation state machine and
// deny-list pruning (every reconcileEvery, default 30 s). Call Start first.
func (m *Manager) Run(ctx context.Context, leader bool, reconcileEvery time.Duration) error {
	errc := make(chan error, 3)
	go func() { errc <- m.Ring.Run(ctx) }()
	go func() { errc <- m.Revocations.Run(ctx, leader) }()
	if leader {
		if reconcileEvery <= 0 {
			reconcileEvery = 30 * time.Second
		}
		go func() {
			for {
				if _, err := m.Rotator.Reconcile(ctx); err != nil && ctx.Err() == nil {
					m.Rotator.log().Warn("signing key reconcile failed", "error", err)
				}
				select {
				case <-ctx.Done():
					errc <- nil
					return
				case <-m.Rotator.Clock.After(reconcileEvery):
				}
			}
		}()
	}
	<-ctx.Done()
	return nil
}

// CreateServiceKey creates a key for a service account; the key is returned once.
func (m *Manager) CreateServiceKey(ctx context.Context, account, description string, ttl time.Duration, actor string) (string, ServiceKey, error) {
	return m.ServiceKeys.Create(ctx, CreateKeyRequest{Account: account, Description: description, TTL: ttl, Actor: actor})
}

// ListServiceKeys lists the keys of account ("" = all).
func (m *Manager) ListServiceKeys(ctx context.Context, account string) ([]ServiceKey, error) {
	return m.ServiceKeys.List(ctx, account)
}

// RevokeServiceKey revokes a key at once and deny-lists its outstanding tokens.
func (m *Manager) RevokeServiceKey(ctx context.Context, id, actor string) error {
	return m.ServiceKeys.Revoke(ctx, id, actor)
}

// Revoke deny-lists a subject or a session; it returns the record and the time by which
// it is effective everywhere (≤ 3 min, R-AUTH-9).
func (m *Manager) Revoke(ctx context.Context, req RevokeRequest) (Revocation, time.Time, error) {
	return m.Revocations.Revoke(ctx, req)
}

// ListRevocations lists the active deny-list records.
func (m *Manager) ListRevocations(ctx context.Context) ([]Revocation, error) {
	return m.Revocations.List(ctx)
}
