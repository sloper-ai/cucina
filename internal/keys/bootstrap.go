// SPDX-License-Identifier: FSL-1.1-ALv2

package keys

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/ports"
)

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SystemClock is the wall clock (for callers without a ports.Clock at hand).
var SystemClock ports.Clock = systemClock{}

// Break-glass Secret entries (read with kubectl as documented in the chart's NOTES.txt).
const (
	BreakGlassKeyEntry   = "key"
	BreakGlassKeyIDEntry = "key-id"
)

// EnsureSigningKeys is the idempotent bootstrap of the token key material, called by
// `cucina-controller bootstrap` (Helm pre-install/pre-upgrade hook) before any Buildbarn
// pod starts. It creates, only when absent, the signing-key Secret (first ES256 key,
// active, plus the service-key pepper), the JWKS ConfigMap (jwks.json) and an empty
// deny-list ConfigMap (denylist.json). It never overwrites existing content.
func EnsureSigningKeys(ctx context.Context, k8s kubernetes.Interface, ns string, cfg config.Auth) error {
	return EnsureSigningKeysIn(ctx, NewKubeObjects(k8s, ns), cfg, SystemClock, rand.Reader)
}

// EnsureSigningKeysIn is EnsureSigningKeys on an Objects implementation.
func EnsureSigningKeysIn(ctx context.Context, o Objects, cfg config.Auth, clock ports.Clock, r io.Reader) error {
	if cfg.SigningKeySecret == "" || cfg.JWKSConfigMap == "" || cfg.DenyListConfigMap == "" {
		return errors.New("auth.signingKeySecret, auth.jwksConfigMap and auth.denyListConfigMap are required")
	}
	ks, err := ensureSigningSecret(ctx, o, cfg.SigningKeySecret, clock, r)
	if err != nil {
		return fmt.Errorf("signing-key secret: %w", err)
	}
	if _, err := o.GetConfigMap(ctx, cfg.JWKSConfigMap); errors.Is(err, ErrNotFound) {
		doc, err := ks.JWKSJSON()
		if err != nil {
			return err
		}
		if _, err := o.CreateConfigMap(ctx, newConfigMap(cfg.JWKSConfigMap, map[string]string{jwksEntry: string(doc)})); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("creating JWKS configmap: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("reading JWKS configmap: %w", err)
	}
	if _, err := o.GetConfigMap(ctx, cfg.DenyListConfigMap); errors.Is(err, ErrNotFound) {
		cm := newConfigMap(cfg.DenyListConfigMap, map[string]string{denyListEntry: EmptyDenyList, revocEntry: "[]"})
		if _, err := o.CreateConfigMap(ctx, cm); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("creating deny-list configmap: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("reading deny-list configmap: %w", err)
	}
	return nil
}

// ensureSigningSecret creates the Secret or completes a partial one (missing pepper or
// missing key state) without touching existing keys.
func ensureSigningSecret(ctx context.Context, o Objects, name string, clock ports.Clock, r io.Reader) (*KeySet, error) {
	if r == nil {
		r = rand.Reader
	}
	newKeyData := func(data map[string][]byte) error {
		kid, p, err := GenerateKey(r)
		if err != nil {
			return err
		}
		st, err := json.Marshal(Bootstrap(kid, clock.Now()))
		if err != nil {
			return err
		}
		pemBytes, err := EncodePrivateKey(p)
		if err != nil {
			return err
		}
		data[stateEntry], data[kid+pemSuffix] = st, pemBytes
		return nil
	}
	newPepper := func(data map[string][]byte) error {
		p := make([]byte, 32)
		if _, err := io.ReadFull(r, p); err != nil {
			return err
		}
		data[pepperEntry] = p
		return nil
	}
	_, err := o.GetSecret(ctx, name)
	if errors.Is(err, ErrNotFound) {
		data := map[string][]byte{}
		if err := newKeyData(data); err != nil {
			return nil, err
		}
		if err := newPepper(data); err != nil {
			return nil, err
		}
		if _, err := o.CreateSecret(ctx, newSecret(name, data)); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	s, err := updateSecret(ctx, o, name, func(s *corev1.Secret) (bool, error) {
		changed := false
		if _, ok := s.Data[stateEntry]; !ok {
			if err := newKeyData(s.Data); err != nil {
				return false, err
			}
			changed = true
		}
		if len(s.Data[pepperEntry]) < 32 {
			if err := newPepper(s.Data); err != nil {
				return false, err
			}
			changed = true
		}
		return changed, nil
	})
	if err != nil {
		return nil, err
	}
	return keySetFromSecretData(s.Data)
}

// EnsureBreakGlass is the idempotent bootstrap of the break-glass admin key (R-AUTH-12),
// called after EnsureSigningKeys. If the Secret config.Auth.BreakGlassKeySecret is
// absent it generates a key, stores the plaintext there (entries "key" and "key-id")
// for the operator to read with kubectl, and registers only its hash in the service-key
// store. If the Secret exists (for example pre-created by the operator) its key is
// registered when not yet known. An existing registration, revoked or not, is kept.
func EnsureBreakGlass(ctx context.Context, k8s kubernetes.Interface, ns string, cfg config.Auth) error {
	return EnsureBreakGlassIn(ctx, NewKubeObjects(k8s, ns), cfg, SystemClock, rand.Reader)
}

// EnsureBreakGlassIn is EnsureBreakGlass on an Objects implementation.
func EnsureBreakGlassIn(ctx context.Context, o Objects, cfg config.Auth, clock ports.Clock, r io.Reader) error {
	if cfg.BreakGlassKeySecret == "" {
		return nil // break-glass disabled
	}
	if cfg.ServiceKeysSecret == "" || cfg.SigningKeySecret == "" {
		return errors.New("auth.serviceKeysSecret and auth.signingKeySecret are required for the break-glass key")
	}
	if _, err := ensureSigningSecret(ctx, o, cfg.SigningKeySecret, clock, r); err != nil {
		return fmt.Errorf("signing-key secret: %w", err)
	}
	if _, err := o.GetSecret(ctx, cfg.ServiceKeysSecret); errors.Is(err, ErrNotFound) {
		if _, err := o.CreateSecret(ctx, newSecret(cfg.ServiceKeysSecret, map[string][]byte{})); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return err
		}
	} else if err != nil {
		return err
	}
	store := &ServiceKeys{Objects: o, StoreSecret: cfg.ServiceKeysSecret, PepperSecret: cfg.SigningKeySecret, Clock: clock, Rand: r}

	sec, err := o.GetSecret(ctx, cfg.BreakGlassKeySecret)
	if errors.Is(err, ErrNotFound) {
		key, id, err := GenerateServiceKey(r)
		if err != nil {
			return err
		}
		// Plaintext first: if registering fails, the next run registers the stored key.
		bg := newSecret(cfg.BreakGlassKeySecret, map[string][]byte{BreakGlassKeyEntry: []byte(key), BreakGlassKeyIDEntry: []byte(id)})
		if _, err := o.CreateSecret(ctx, bg); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("creating break-glass secret: %w", err)
		}
		if sec, err = o.GetSecret(ctx, cfg.BreakGlassKeySecret); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	key := string(sec.Data[BreakGlassKeyEntry])
	id, _, ok := ParseServiceKey(key)
	if !ok {
		return fmt.Errorf("secret %s entry %q is not a Cucina service key (cuc_sk_<16 base32>_<52 base32>); delete the secret to let bootstrap generate one", cfg.BreakGlassKeySecret, BreakGlassKeyEntry)
	}
	if _, err := store.Get(ctx, id); err == nil {
		return nil // already registered (possibly revoked: keep the operator's decision)
	} else if !errors.Is(err, ErrUnknownServiceKey) {
		return err
	}
	_, err = store.register(ctx, key, BreakGlassAccount, "Helm-generated break-glass admin key (R-AUTH-12)", "bootstrap", 0, true)
	return err
}

// RotateBreakGlass replaces the break-glass key: a new key is registered and written to
// the Secret, then the previous one is revoked (its outstanding tokens are deny-listed).
func RotateBreakGlass(ctx context.Context, o Objects, cfg config.Auth, store *ServiceKeys, actor string) error {
	sec, err := o.GetSecret(ctx, cfg.BreakGlassKeySecret)
	if err != nil {
		return err
	}
	oldID, _, _ := ParseServiceKey(string(sec.Data[BreakGlassKeyEntry]))
	key, id, err := GenerateServiceKey(store.rand())
	if err != nil {
		return err
	}
	if _, err := store.register(ctx, key, BreakGlassAccount, "Helm-generated break-glass admin key (R-AUTH-12)", actor, 0, true); err != nil {
		return err
	}
	if _, err := updateSecret(ctx, o, cfg.BreakGlassKeySecret, func(s *corev1.Secret) (bool, error) {
		s.Data[BreakGlassKeyEntry], s.Data[BreakGlassKeyIDEntry] = []byte(key), []byte(id)
		return true, nil
	}); err != nil {
		return err
	}
	if oldID != "" {
		if err := store.Revoke(ctx, oldID, actor); err != nil && !errors.Is(err, ErrUnknownServiceKey) {
			return err
		}
	}
	return nil
}
