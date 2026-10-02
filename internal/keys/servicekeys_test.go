// SPDX-License-Identifier: FSL-1.1-ALv2

package keys_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
)

func newServiceKeys(t testing.TB, objs keys.Objects, clock *keystest.Clock) (*keys.ServiceKeys, *keys.RevocationStore) {
	t.Helper()
	require.NoError(t, keys.EnsureSigningKeysIn(context.Background(), objs, authConfig(), clock, nil))
	revs := newStore(objs, clock)
	return &keys.ServiceKeys{Objects: objs, StoreSecret: "cucina-service-keys", PepperSecret: "cucina-signing-keys",
		Clock: clock, Revocations: revs}, revs
}

func keyReason(err error) string {
	var ke *keys.KeyError
	if errors.As(err, &ke) {
		return ke.Reason
	}
	return "not a KeyError: " + err.Error()
}

// TestServiceKeyLifecycle guards R-AUTH-10: opaque ≥ 256-bit keys with the cuc_sk_ prefix,
// stored only as a peppered hash, authenticated with the same generic error for every
// failure, last-use tracking, expiry, and revocation that is immediate for new exchanges
// on every replica and deny-lists the key's outstanding tokens.
func TestServiceKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	clock := keystest.NewClock(t0)
	objs := keystest.NewObjects()
	replicaA, revs := newServiceKeys(t, objs, clock)
	replicaB := &keys.ServiceKeys{Objects: objs, StoreSecret: "cucina-service-keys", PepperSecret: "cucina-signing-keys", Clock: clock}

	key, info, err := replicaA.Create(ctx, keys.CreateKeyRequest{Account: "nightly-cache", Description: "cache warmer", Actor: "google:admin"})
	require.NoError(t, err)
	assert.Regexp(t, `^cuc_sk_[a-z2-7]{16}_[a-z2-7]{52}$`, key, "52 base32 characters = 260 bits ≥ 256")
	id, secret, ok := keys.ParseServiceKey(key)
	require.True(t, ok)
	assert.Equal(t, id, info.ID)
	assert.Empty(t, info.Hash)

	sec, err := objs.GetSecret(ctx, "cucina-service-keys")
	require.NoError(t, err)
	for _, v := range sec.Data {
		assert.NotContains(t, string(v), secret, "the secret is never stored")
		assert.NotContains(t, string(v), key)
	}

	rec, err := replicaB.Authenticate(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, "nightly-cache", rec.Account)
	listed, err := replicaA.List(ctx, "nightly-cache")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, t0, listed[0].LastUsedAt, "last use is tracked")
	assert.Empty(t, listed[0].Hash)
	clock.Advance(time.Minute)
	_, _ = replicaB.Authenticate(ctx, key)
	listed, _ = replicaA.List(ctx, "")
	assert.Equal(t, t0, listed[0].LastUsedAt, "last-use writes are throttled")
	clock.Advance(5 * time.Minute)
	_, _ = replicaB.Authenticate(ctx, key)
	listed, _ = replicaA.List(ctx, "")
	assert.Equal(t, t0.Add(6*time.Minute), listed[0].LastUsedAt)

	wrong := key[:len(key)-1] + map[bool]string{true: "b", false: "a"}[strings.HasSuffix(key, "a")]
	for name, presented := range map[string]string{
		"wrong secret": wrong, "unknown id": "cuc_sk_aaaaaaaaaaaaaaaa_" + secret, "malformed": "cuc_sk_short", "uppercase": strings.ToUpper(key),
	} {
		_, err := replicaB.Authenticate(ctx, presented)
		require.Error(t, err, name)
		assert.Equal(t, "invalid service key", err.Error(), "%s: one generic error for every cause", name)
	}

	// Revocation: immediate on every replica; outstanding tokens' sid is deny-listed.
	require.NoError(t, replicaA.Revoke(ctx, id, "google:admin"))
	_, err = replicaB.Authenticate(ctx, key)
	assert.Equal(t, "service key revoked", keyReason(err))
	assert.True(t, revs.IsDenied(keys.SessionForKey(id), ""))
	cm, err := objs.GetConfigMap(ctx, "cucina-denylist")
	require.NoError(t, err)
	assert.Contains(t, cm.Data["denylist.json"], `"sid:`+keys.SessionForKey(id)+`"`)
	assert.ErrorIs(t, replicaA.Revoke(ctx, "aaaaaaaaaaaaaaaa", "x"), keys.ErrUnknownServiceKey)

	// Expiry.
	short, _, err := replicaA.Create(ctx, keys.CreateKeyRequest{Account: "nightly-cache", TTL: time.Hour})
	require.NoError(t, err)
	_, err = replicaB.Authenticate(ctx, short)
	require.NoError(t, err)
	clock.Advance(time.Hour)
	_, err = replicaB.Authenticate(ctx, short)
	assert.Equal(t, "service key expired", keyReason(err))

	_, _, err = replicaA.Create(ctx, keys.CreateKeyRequest{Account: keys.BreakGlassAccount})
	assert.Error(t, err, "the break-glass account is reserved")
	_, _, err = replicaA.Create(ctx, keys.CreateKeyRequest{Account: "Not_A_Label"})
	assert.Error(t, err)
}

// TestBootstrap guards the Helm-hook entry points (R-AUTH-12 and the bootstrap addendum):
// idempotent, never overwriting; the break-glass key is stored in plaintext only in its own
// Secret and authenticates as break-glass; the STS's JWKS and deny-list exist before
// Buildbarn starts.
func TestBootstrap(t *testing.T) {
	ctx := context.Background()
	clock := keystest.NewClock(t0)
	objs := keystest.NewObjects()
	cfg := authConfig()

	require.NoError(t, keys.EnsureSigningKeysIn(ctx, objs, cfg, clock, nil))
	require.NoError(t, keys.EnsureBreakGlassIn(ctx, objs, cfg, clock, nil))
	snapshot := func() map[string]string {
		out := map[string]string{}
		for _, n := range []string{"cucina-signing-keys", "cucina-service-keys", "cucina-break-glass"} {
			s, err := objs.GetSecret(ctx, n)
			require.NoError(t, err, n)
			for k, v := range s.Data {
				out[n+"/"+k] = string(v)
			}
		}
		for _, n := range []string{"cucina-jwks", "cucina-denylist"} {
			c, err := objs.GetConfigMap(ctx, n)
			require.NoError(t, err, n)
			for k, v := range c.Data {
				out[n+"/"+k] = v
			}
		}
		return out
	}
	first := snapshot()
	assert.Equal(t, keys.EmptyDenyList, first["cucina-denylist/denylist.json"])
	assert.Contains(t, first["cucina-jwks/jwks.json"], `"alg":"ES256"`)

	clock.Advance(time.Hour)
	require.NoError(t, keys.EnsureSigningKeysIn(ctx, objs, cfg, clock, nil))
	require.NoError(t, keys.EnsureBreakGlassIn(ctx, objs, cfg, clock, nil))
	assert.Equal(t, first, snapshot(), "re-running the hook changes nothing")

	key := first["cucina-break-glass/key"]
	store := &keys.ServiceKeys{Objects: objs, StoreSecret: cfg.ServiceKeysSecret, PepperSecret: cfg.SigningKeySecret, Clock: clock}
	rec, err := store.Authenticate(ctx, key)
	require.NoError(t, err)
	assert.True(t, rec.BreakGlass)
	assert.Equal(t, keys.BreakGlassAccount, rec.Account)
	for k, v := range first {
		if !strings.HasPrefix(k, "cucina-break-glass/") {
			assert.NotContains(t, v, key, "plaintext only in the break-glass Secret (%s)", k)
		}
	}

	// A revoked break-glass key stays revoked across hook runs (the operator disabled it).
	require.NoError(t, store.Revoke(ctx, rec.ID, "google:admin"))
	require.NoError(t, keys.EnsureBreakGlassIn(ctx, objs, cfg, clock, nil))
	_, err = store.Authenticate(ctx, key)
	assert.Equal(t, "service key revoked", keyReason(err))

	// Rotation issues a new working key and revokes the old (still active) one.
	require.NoError(t, keys.RotateBreakGlass(ctx, objs, cfg, store, "google:admin"))
	s, err := objs.GetSecret(ctx, cfg.BreakGlassKeySecret)
	require.NoError(t, err)
	newKey := string(s.Data["key"])
	assert.NotEqual(t, key, newKey)
	rec, err = store.Authenticate(ctx, newKey)
	require.NoError(t, err)
	assert.True(t, rec.BreakGlass)
	require.NoError(t, keys.RotateBreakGlass(ctx, objs, cfg, store, "google:admin"))
	_, err = store.Authenticate(ctx, newKey)
	assert.Equal(t, "service key revoked", keyReason(err), "rotation revokes the replaced key")

	// Partial state is completed from what exists, never regenerated: a lost JWKS ConfigMap
	// is rebuilt from the existing keys, a missing break-glass Secret is recreated.
	partial := keystest.NewObjects()
	sk, err := objs.GetSecret(ctx, cfg.SigningKeySecret)
	require.NoError(t, err)
	_, err = partial.CreateSecret(ctx, secretWith(cfg.SigningKeySecret, "state.json", string(sk.Data["state.json"])))
	require.NoError(t, err)
	for k, v := range sk.Data {
		if strings.HasSuffix(k, ".pem") {
			cur, _ := partial.GetSecret(ctx, cfg.SigningKeySecret)
			cur.Data[k] = v
			_, err = partial.UpdateSecret(ctx, cur)
			require.NoError(t, err)
		}
	}
	_, err = partial.CreateSecret(ctx, secretWith(cfg.ServiceKeysSecret))
	require.NoError(t, err)
	require.NoError(t, keys.EnsureSigningKeysIn(ctx, partial, cfg, clock, nil))
	require.NoError(t, keys.EnsureBreakGlassIn(ctx, partial, cfg, clock, nil))
	jwks, err := partial.GetConfigMap(ctx, cfg.JWKSConfigMap)
	require.NoError(t, err)
	assert.Equal(t, first["cucina-jwks/jwks.json"], jwks.Data["jwks.json"], "rebuilt from the existing keys")
	ps, err := partial.GetSecret(ctx, cfg.SigningKeySecret)
	require.NoError(t, err)
	assert.Len(t, ps.Data["service-key-pepper"], 32, "missing pepper added")
	bgs, err := partial.GetSecret(ctx, cfg.BreakGlassKeySecret)
	require.NoError(t, err)
	partialStore := &keys.ServiceKeys{Objects: partial, StoreSecret: cfg.ServiceKeysSecret, PepperSecret: cfg.SigningKeySecret, Clock: clock}
	_, err = partialStore.Authenticate(ctx, string(bgs.Data["key"]))
	require.NoError(t, err)

	// An operator-provided break-glass Secret is registered as is; garbage is refused.
	objs2 := keystest.NewObjects()
	provided, _, err := keys.GenerateServiceKey(nil)
	require.NoError(t, err)
	_, err = objs2.CreateSecret(ctx, secretWith(cfg.BreakGlassKeySecret, "key", provided))
	require.NoError(t, err)
	require.NoError(t, keys.EnsureBreakGlassIn(ctx, objs2, cfg, clock, nil))
	store2 := &keys.ServiceKeys{Objects: objs2, StoreSecret: cfg.ServiceKeysSecret, PepperSecret: cfg.SigningKeySecret, Clock: clock}
	_, err = store2.Authenticate(ctx, provided)
	require.NoError(t, err)
	objs3 := keystest.NewObjects()
	_, err = objs3.CreateSecret(ctx, secretWith(cfg.BreakGlassKeySecret, "key", "randAlphaNum-64-from-helm"))
	require.NoError(t, err)
	assert.Error(t, keys.EnsureBreakGlassIn(ctx, objs3, cfg, clock, nil))
}
