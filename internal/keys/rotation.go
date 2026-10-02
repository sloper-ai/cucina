// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sloper-ai/cucina/internal/ports"
)

// LoadVerifier reports whether every Buildbarn component that validates Cucina JWTs
// (frontends, scheduler) has loaded the key kid. The controller wires it (for example
// by presenting a probe token minted with Minter.MintWithKey to each frontend replica).
type LoadVerifier interface {
	KeyLoaded(ctx context.Context, kid string) (bool, error)
}

// FrontendRestarter restarts every Buildbarn component that validates Cucina JWTs so
// that they drop their token cache and re-read the JWKS (compromised-key path).
type FrontendRestarter interface {
	RestartFrontends(ctx context.Context, reason string) error
}

// RotatorConfig configures the leader-side key manager.
type RotatorConfig struct {
	SigningKeySecret string
	JWKSConfigMap    string
	// PublishLead is raised to MinPublishLead when smaller.
	PublishLead time.Duration
	// TokenTTL is the maximum lifetime of issued tokens.
	TokenTTL time.Duration
	// SignerReload bounds how long an STS replica keeps signing with a key after it was
	// retired (KeyRing refresh interval).
	SignerReload time.Duration
	// RotateEvery starts a rotation automatically when the active key is older (0 = off).
	RotateEvery time.Duration
	// AllowUnverifiedPromotion promotes a pending key after PublishLead even without a
	// LoadVerifier. Off by default: without verification a rotation waits.
	AllowUnverifiedPromotion bool
}

// Rotator drives the signing-key life cycle (R-AUTH-9). It runs on the leader only; every
// replica reads the result through a KeyRing. All methods are level-triggered and safe
// to retry: the Secret is the source of truth and the JWKS ConfigMap converges to it.
type Rotator struct {
	Objects   Objects
	Config    RotatorConfig
	Clock     ports.Clock
	Rand      io.Reader
	Loaded    LoadVerifier
	Restarter FrontendRestarter
	Log       *slog.Logger
}

// Params returns the effective state-machine bounds.
func (r *Rotator) Params() RotationParams {
	ttl := r.Config.TokenTTL
	if ttl <= 0 || ttl > MaxTokenTTL {
		ttl = MaxTokenTTL
	}
	reload := r.Config.SignerReload
	if reload <= 0 {
		reload = DefaultKeyRingInterval
	}
	return RotationParams{
		PublishLead: max(r.Config.PublishLead, MinPublishLead),
		RetireGrace: ttl + ClockSkew + reload,
	}
}

func (r *Rotator) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// loadSigning reads and parses the signing Secret.
func (r *Rotator) loadSigning(ctx context.Context) (*corev1.Secret, *KeySet, error) {
	s, err := r.Objects.GetSecret(ctx, r.Config.SigningKeySecret)
	if err != nil {
		return nil, nil, err
	}
	ks, err := keySetFromSecretData(s.Data)
	if err != nil {
		return nil, nil, fmt.Errorf("signing secret %s: %w", r.Config.SigningKeySecret, err)
	}
	return s, ks, nil
}

// writeState persists state plus the private keys it needs (from ks and extra), dropping
// PEM entries of removed keys, with optimistic concurrency on base's ResourceVersion.
func (r *Rotator) writeState(ctx context.Context, base *corev1.Secret, state RotationState, ks *KeySet, extra map[string]*ecdsa.PrivateKey) (*KeySet, error) {
	priv := map[string]*ecdsa.PrivateKey{}
	for _, k := range state.Keys {
		if p, ok := extra[k.KID]; ok {
			priv[k.KID] = p
		} else if p, ok := ks.privateKey(k.KID); ok {
			priv[k.KID] = p
		}
	}
	next, err := NewKeySet(state, priv)
	if err != nil {
		return nil, err
	}
	s := base.DeepCopy()
	data := map[string][]byte{}
	for name, v := range s.Data {
		if _, isPEM := cutPEM(name); !isPEM && name != stateEntry {
			data[name] = v // keep unrelated entries (pepper)
		}
	}
	for kid, p := range priv {
		b, err := EncodePrivateKey(p)
		if err != nil {
			return nil, err
		}
		data[kid+pemSuffix] = b
	}
	st, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	data[stateEntry] = st
	s.Data = data
	if _, err := r.Objects.UpdateSecret(ctx, s); err != nil {
		return nil, err
	}
	return next, nil
}

func cutPEM(name string) (string, bool) {
	if len(name) > len(pemSuffix) && name[len(name)-len(pemSuffix):] == pemSuffix {
		return name[:len(name)-len(pemSuffix)], true
	}
	return "", false
}

// publishJWKS makes the JWKS ConfigMap equal to ks (creating it when missing).
func (r *Rotator) publishJWKS(ctx context.Context, ks *KeySet) error {
	doc, err := ks.JWKSJSON()
	if err != nil {
		return err
	}
	_, err = updateConfigMap(ctx, r.Objects, r.Config.JWKSConfigMap, func(c *corev1.ConfigMap) (bool, error) {
		if c.Data[jwksEntry] == string(doc) {
			return false, nil
		}
		c.Data[jwksEntry] = string(doc)
		return true, nil
	})
	if errors.Is(err, ErrNotFound) {
		_, err = r.Objects.CreateConfigMap(ctx, newConfigMap(r.Config.JWKSConfigMap, map[string]string{jwksEntry: string(doc)}))
	}
	return err
}

// Reconcile advances the state machine one step and converges the JWKS ConfigMap:
// prune retired keys past their grace, promote a pending key that has been published
// for PublishLead and is verified loaded, start a scheduled rotation. It returns the
// resulting state.
func (r *Rotator) Reconcile(ctx context.Context) (RotationState, error) {
	base, ks, err := r.loadSigning(ctx)
	if err != nil {
		return RotationState{}, err
	}
	now := r.Clock.Now()
	p := r.Params()
	state := ks.State()
	changed := false

	if pruned, removed := state.Prune(now); len(removed) > 0 {
		state, changed = pruned, true
		r.log().Info("signing keys removed from JWKS", "kids", removed)
	}
	if pending, due := state.PromotionDue(now, p); due {
		ok, why := r.verified(ctx, pending.KID)
		if ok {
			if state, err = state.Promote(now, p); err != nil {
				return RotationState{}, err
			}
			changed = true
			r.log().Info("signing key promoted", "kid", pending.KID)
		} else {
			r.log().Info("signing key promotion waiting", "kid", pending.KID, "reason", why)
		}
	}
	if changed {
		if ks, err = r.writeState(ctx, base, state, ks, nil); err != nil {
			return RotationState{}, err
		}
	}
	if err := r.publishJWKS(ctx, ks); err != nil {
		return RotationState{}, err
	}
	if active, ok := state.Active(); ok && r.Config.RotateEvery > 0 {
		if _, inProgress := state.Pending(); !inProgress && !now.Before(active.ActivatedAt.Add(r.Config.RotateEvery)) {
			if _, err := r.StartRotation(ctx); err != nil && !errors.Is(err, ErrRotationInProgress) {
				return RotationState{}, err
			}
			_, ks, err = r.loadSigning(ctx)
			if err != nil {
				return RotationState{}, err
			}
			state = ks.State()
		}
	}
	return state, nil
}

func (r *Rotator) verified(ctx context.Context, kid string) (bool, string) {
	if r.Loaded == nil {
		if r.Config.AllowUnverifiedPromotion {
			return true, ""
		}
		return false, "no load verifier configured"
	}
	ok, err := r.Loaded.KeyLoaded(ctx, kid)
	switch {
	case err != nil:
		return false, "load verification failed: " + err.Error()
	case !ok:
		return false, "not loaded by every frontend yet"
	}
	return true, ""
}

// StartRotation generates a successor key and publishes it (pending). The JWKS
// ConfigMap is written before the state records the publication time, so the lead time
// is never counted from before the key was actually published.
func (r *Rotator) StartRotation(ctx context.Context) (string, error) {
	base, ks, err := r.loadSigning(ctx)
	if err != nil {
		return "", err
	}
	kid, priv, err := GenerateKey(r.Rand)
	if err != nil {
		return "", err
	}
	state, err := ks.State().StartRotation(kid, r.Clock.Now())
	if err != nil {
		return "", err
	}
	next, err := NewKeySet(state, mergeKeys(ks, state, map[string]*ecdsa.PrivateKey{kid: priv}))
	if err != nil {
		return "", err
	}
	if err := r.publishJWKS(ctx, next); err != nil {
		return "", err
	}
	// Re-stamp the publication time after the ConfigMap write.
	state, _ = ks.State().StartRotation(kid, r.Clock.Now())
	if _, err := r.writeState(ctx, base, state, ks, map[string]*ecdsa.PrivateKey{kid: priv}); err != nil {
		return "", err
	}
	r.log().Info("signing key rotation started", "kid", kid)
	return kid, nil
}

func mergeKeys(ks *KeySet, state RotationState, extra map[string]*ecdsa.PrivateKey) map[string]*ecdsa.PrivateKey {
	out := map[string]*ecdsa.PrivateKey{}
	for _, k := range state.Keys {
		if p, ok := extra[k.KID]; ok {
			out[k.KID] = p
		} else if p, ok := ks.privateKey(k.KID); ok {
			out[k.KID] = p
		}
	}
	return out
}

// Compromise removes kid ("*" = every key) from the key set and the JWKS at once. When
// the active key is removed a fresh key becomes active immediately. Afterwards the
// frontends are restarted: Buildbarn caches validation results per token string and
// would otherwise keep accepting tokens signed by the removed key.
func (r *Rotator) Compromise(ctx context.Context, kid, reason string) error {
	base, ks, err := r.loadSigning(ctx)
	if err != nil {
		return err
	}
	newKID, priv, err := GenerateKey(r.Rand)
	if err != nil {
		return err
	}
	state, _, err := ks.State().Compromise(kid, newKID, r.Clock.Now())
	if err != nil {
		return err
	}
	next, err := r.writeState(ctx, base, state, ks, map[string]*ecdsa.PrivateKey{newKID: priv})
	if err != nil {
		return err
	}
	if err := r.publishJWKS(ctx, next); err != nil {
		return err
	}
	r.log().Warn("signing key declared compromised; restarting frontends", "kid", kid, "reason", reason)
	if r.Restarter == nil {
		return errors.New("compromised key removed from the JWKS, but no frontend restarter is configured: restart the Buildbarn frontends and scheduler now")
	}
	return r.Restarter.RestartFrontends(ctx, "signing key compromised: "+reason)
}

// DefaultKeyRingInterval is how often replicas re-read the signing Secret.
const DefaultKeyRingInterval = 30 * time.Second

// KeyRing keeps every replica's view of the signing keys current (read-only).
type KeyRing struct {
	Objects    Objects
	SecretName string
	Clock      ports.Clock
	Interval   time.Duration
	Log        *slog.Logger
	cur        atomic.Pointer[KeySet]
}

// KeySet implements KeySource; nil until the first successful Refresh.
func (k *KeyRing) KeySet() *KeySet { return k.cur.Load() }

// Refresh re-reads the signing Secret. On error the previous key set stays in use.
func (k *KeyRing) Refresh(ctx context.Context) error {
	s, err := k.Objects.GetSecret(ctx, k.SecretName)
	if err != nil {
		return err
	}
	ks, err := keySetFromSecretData(s.Data)
	if err != nil {
		return err
	}
	k.cur.Store(ks)
	return nil
}

// Run refreshes every Interval until ctx ends.
func (k *KeyRing) Run(ctx context.Context) error {
	every := k.Interval
	if every <= 0 {
		every = DefaultKeyRingInterval
	}
	for {
		if err := k.Refresh(ctx); err != nil && ctx.Err() == nil {
			l := k.Log
			if l == nil {
				l = slog.Default()
			}
			l.Warn("refreshing signing keys failed; keeping the previous set", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-k.Clock.After(every):
		}
	}
}
