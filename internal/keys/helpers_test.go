// SPDX-License-Identifier: FSL-1.1-ALv2

package keys_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
)

func authConfig() config.Auth {
	return config.Auth{
		SigningKeySecret: "cucina-signing-keys", JWKSConfigMap: "cucina-jwks", DenyListConfigMap: "cucina-denylist",
		ServiceKeysSecret: "cucina-service-keys", BreakGlassKeySecret: "cucina-break-glass",
		TokenTTL: config.Duration{Duration: 15 * time.Minute}, Audience: "buildbarn",
		KeyRotationPublishLead: config.Duration{Duration: 10 * time.Minute}, RateLimitPerMinute: 60,
	}
}

func secretWith(name string, kv ...string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: map[string][]byte{}}
	for i := 0; i+1 < len(kv); i += 2 {
		s.Data[kv[i]] = []byte(kv[i+1])
	}
	return s
}

// conflictingUpdates adds a finite fault stream to the stateful Objects fake.
// Even an unbounded-retry mutant eventually reaches a successful write, so the
// tests fail on its observable result instead of hanging or inspecting call counts.
type conflictingUpdates struct {
	keys.Objects
	target    string
	remaining int
}

func (o *conflictingUpdates) conflict(target string) bool {
	if o.target != target || o.remaining == 0 {
		return false
	}
	o.remaining--
	return true
}

func (o *conflictingUpdates) UpdateSecret(ctx context.Context, s *corev1.Secret) (*corev1.Secret, error) {
	if o.conflict("service key") {
		return nil, keys.ErrConflict
	}
	return o.Objects.UpdateSecret(ctx, s)
}

func (o *conflictingUpdates) UpdateConfigMap(ctx context.Context, cm *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	if o.conflict("principal") {
		return nil, keys.ErrConflict
	}
	return o.Objects.UpdateConfigMap(ctx, cm)
}

// TestRevocationConflictBudget guards R-AUTH-9/-10 and the bounded-write invariant:
// a revocation tolerates transient contention, but reports ErrConflict with no
// revocation persisted after the initial attempt plus eight conflict retries.
func TestRevocationConflictBudget(t *testing.T) {
	for _, target := range []string{"service key", "principal"} {
		t.Run(target, func(t *testing.T) {
			for _, tc := range []struct {
				name       string
				conflicts  int
				wantFailed bool
			}{
				{"transient contention recovers", 1, false},
				{"last allowed retry succeeds", 8, false},
				{"retry budget exhausted", 9, true},
				{"sustained contention stays bounded", 32, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx := context.Background()
					objs := keystest.NewObjects()
					store, revocations := newServiceKeys(t, objs, keystest.NewClock(t0))
					_, key, err := store.Create(ctx, keys.CreateKeyRequest{Account: "nightly-cache"})
					require.NoError(t, err)
					faults := &conflictingUpdates{Objects: objs, target: target, remaining: tc.conflicts}
					store.Objects, revocations.Objects = faults, faults
					const subject = "google:conflict-test"
					if target == "service key" {
						err = store.Revoke(ctx, key.ID, "google:admin")
					} else {
						_, _, err = revocations.Revoke(ctx, keys.RevokeRequest{Kind: keys.RevokeSubject, Value: subject})
					}
					if tc.wantFailed {
						require.ErrorIs(t, err, keys.ErrConflict)
					} else {
						require.NoError(t, err)
					}
					if target == "service key" {
						rec, err := store.Get(ctx, key.ID)
						require.NoError(t, err)
						assert.Equal(t, !tc.wantFailed, rec.Revoked())
						assert.Equal(t, !tc.wantFailed, revocations.IsDenied(keys.SessionForKey(key.ID), ""))
					} else {
						assert.Equal(t, !tc.wantFailed, revocations.IsDenied("", subject))
						persisted, err := revocations.List(ctx)
						require.NoError(t, err)
						if tc.wantFailed {
							assert.Empty(t, persisted)
						} else {
							require.Len(t, persisted, 1)
							assert.Equal(t, subject, persisted[0].Value)
						}
					}
				})
			}
		})
	}
}

// TestValidateAuthConfig guards the fail-fast configuration check (R-TEST-7): the
// controller refuses to start with auth settings that would break R-AUTH-3/-9.
func TestValidateAuthConfig(t *testing.T) {
	require.NoError(t, keys.ValidateAuthConfig(authConfig()))
	for name, mutate := range map[string]func(*config.Auth){
		"missing signing secret":    func(c *config.Auth) { c.SigningKeySecret = "" },
		"missing deny-list":         func(c *config.Auth) { c.DenyListConfigMap = "" },
		"TTL above 15 min":          func(c *config.Auth) { c.TokenTTL.Duration = 16 * time.Minute },
		"wrong audience":            func(c *config.Auth) { c.Audience = "cucina" },
		"publish lead under 10 min": func(c *config.Auth) { c.KeyRotationPublishLead.Duration = 5 * time.Minute },
		"negative rate limit":       func(c *config.Auth) { c.RateLimitPerMinute = -1 },
	} {
		c := authConfig()
		mutate(&c)
		assert.Error(t, keys.ValidateAuthConfig(c), name)
	}
}
