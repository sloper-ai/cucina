// SPDX-License-Identifier: FSL-1.1-ALv2

package keys_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/keys/keystest"
)

type verifierFunc func(ctx context.Context, kid string) (bool, error)

func (f verifierFunc) KeyLoaded(ctx context.Context, kid string) (bool, error) { return f(ctx, kid) }

type restarterFunc func(ctx context.Context, reason string) error

func (f restarterFunc) RestartFrontends(ctx context.Context, reason string) error {
	return f(ctx, reason)
}

type jwksAt struct {
	at   time.Time
	kids []string
}

type minted struct {
	kid string
	exp time.Time
}

// rotationModel drives a Rotator against a model of the cluster: an STS replica whose
// key ring refreshes at most every SignerReload, and frontends whose view of the JWKS
// ConfigMap lags by a fixed delay (kubelet sync + Buildbarn's 300 s reload) except
// right after a restart.
type rotationModel struct {
	ctx         context.Context
	clock       *keystest.Clock
	objs        *keystest.Objects
	rot         *keys.Rotator
	ring        *keys.KeyRing
	ringAt      time.Time
	minter      *keys.Minter
	lag         time.Duration
	history     []jwksAt
	restartedAt time.Time
	restarts    int
	tokens      []minted
	compromised map[string]bool
	activated   map[string]time.Time // kid -> activation observed
}

const signerReload = 30 * time.Second

// Boundary-heavy choices (rapid's integer generators favour small values): frontend lags
// below and above the 10 min publish lead, and time steps around the reload period, the
// lead, the token TTL and the retire grace.
var (
	lags  = []time.Duration{0, 30 * time.Second, 2 * time.Minute, 6 * time.Minute, 9 * time.Minute, 11 * time.Minute, 15 * time.Minute, 20 * time.Minute}
	steps = []time.Duration{time.Second, 5 * time.Second, 20 * time.Second, 31 * time.Second, time.Minute, 2 * time.Minute,
		5 * time.Minute, 9 * time.Minute, 10 * time.Minute, 11 * time.Minute, 14*time.Minute + 45*time.Second,
		15 * time.Minute, 17 * time.Minute, 18 * time.Minute}
)

func (m *rotationModel) configMapKids(t *rapid.T) []string {
	cm, err := m.objs.GetConfigMap(m.ctx, "cucina-jwks")
	if err != nil {
		t.Fatal(err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal([]byte(cm.Data["jwks.json"]), &set); err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, k := range set.Keys {
		kids = append(kids, k.KeyID)
	}
	slices.Sort(kids)
	return kids
}

func (m *rotationModel) record(t *rapid.T) {
	kids := m.configMapKids(t)
	if n := len(m.history); n == 0 || !slices.Equal(m.history[n-1].kids, kids) {
		m.history = append(m.history, jwksAt{at: m.clock.Now(), kids: kids})
	}
}

// window is the span of JWKS ConfigMap versions a frontend may be using at now: its view
// lags by at most m.lag (kubelet sync + Buildbarn's 300 s reload) and is fresh right
// after a restart. Additions are judged at the most lagged view, removals at any view
// in the window (a frontend may see a removal at once).
func (m *rotationModel) window(now time.Time) time.Time {
	from := now.Add(-m.lag)
	if m.restartedAt.After(from) && !m.restartedAt.After(now) {
		from = m.restartedAt
	}
	return from
}

func (m *rotationModel) versionAt(at time.Time) []string {
	var kids []string
	for _, h := range m.history {
		if !h.at.After(at) {
			kids = h.kids
		}
	}
	return kids
}

// loadedEverywhere: even the most lagged frontend has kid.
func (m *rotationModel) loadedEverywhere(kid string, now time.Time) bool {
	return slices.Contains(m.versionAt(m.window(now)), kid)
}

// verifiableEverywhere: every view a frontend may hold at now contains kid.
func (m *rotationModel) verifiableEverywhere(kid string, now time.Time) bool {
	from := m.window(now)
	if !slices.Contains(m.versionAt(from), kid) {
		return false
	}
	for _, h := range m.history {
		if h.at.After(from) && !h.at.After(now) && !slices.Contains(h.kids, kid) {
			return false
		}
	}
	return true
}

func (m *rotationModel) state(t *rapid.T) keys.RotationState {
	s, err := m.objs.GetSecret(m.ctx, "cucina-signing-keys")
	if err != nil {
		t.Fatal(err)
	}
	var st keys.RotationState
	if err := json.Unmarshal(s.Data["state.json"], &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestRotationStateMachine guards R-AUTH-9's rotation sequence and compromised-key path:
// a key signs only after it was published for PublishLead and the frontends were seen
// to load it; every unexpired token signed by a non-compromised key stays verifiable by
// the frontends through any interleaving of rotations; retired keys disappear only after
// max TTL + skew + signer reload; a compromised key leaves the JWKS at once and the
// frontends are restarted.
func TestRotationStateMachine(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		clock := keystest.NewClock(t0)
		objs := keystest.NewObjects()
		if err := keys.EnsureSigningKeysIn(ctx, objs, authConfig(), clock, nil); err != nil {
			rt.Fatal(err)
		}
		m := &rotationModel{
			ctx: ctx, clock: clock, objs: objs, lag: rapid.SampledFrom(lags).Draw(rt, "lag"),
			compromised: map[string]bool{}, activated: map[string]time.Time{},
		}
		m.ring = &keys.KeyRing{Objects: objs, SecretName: "cucina-signing-keys", Clock: clock}
		if err := m.ring.Refresh(ctx); err != nil {
			rt.Fatal(err)
		}
		m.ringAt = t0
		m.minter = &keys.Minter{Issuer: issuer, Keys: m.ring, Clock: clock}
		m.rot = &keys.Rotator{
			Objects: objs, Clock: clock,
			Config: keys.RotatorConfig{SigningKeySecret: "cucina-signing-keys", JWKSConfigMap: "cucina-jwks",
				PublishLead: 10 * time.Minute, TokenTTL: keys.MaxTokenTTL, SignerReload: signerReload},
			Loaded: verifierFunc(func(_ context.Context, kid string) (bool, error) {
				return m.loadedEverywhere(kid, clock.Now()), nil
			}),
			Restarter: restarterFunc(func(context.Context, string) error { m.restartedAt = clock.Now(); m.restarts++; return nil }),
		}
		m.record(rt)
		// Buildbarn starts after the bootstrap hook wrote the JWKS: its first view is fresh.
		m.restartedAt = t0
		first, _ := m.state(rt).Active()
		m.activated[first.KID] = t0

		rt.Repeat(map[string]func(*rapid.T){
			"advance": func(t *rapid.T) {
				clock.Advance(rapid.SampledFrom(steps).Draw(t, "step"))
				// The replica re-reads the Secret every signerReload; state only changes in
				// other actions, so refreshing at the end of a long jump is exact.
				if clock.Now().Sub(m.ringAt) >= signerReload {
					if err := m.ring.Refresh(ctx); err != nil {
						t.Fatal(err)
					}
					m.ringAt = clock.Now()
				}
			},
			"start-rotation": func(t *rapid.T) {
				before := m.state(t)
				_, err := m.rot.StartRotation(ctx)
				if _, pending := before.Pending(); pending != errors.Is(err, keys.ErrRotationInProgress) {
					t.Fatalf("StartRotation: %v (pending before: %v)", err, pending)
				}
				m.record(t)
			},
			"reconcile": func(t *rapid.T) {
				st, err := m.rot.Reconcile(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if a, ok := st.Active(); ok {
					if _, seen := m.activated[a.KID]; !seen {
						m.activated[a.KID] = clock.Now()
						// Promotion: published for the lead and seen loaded by the frontends.
						if clock.Now().Sub(a.PublishedAt) < 10*time.Minute {
							t.Fatalf("key %s promoted %s after publication", a.KID, clock.Now().Sub(a.PublishedAt))
						}
						if !m.loadedEverywhere(a.KID, clock.Now()) {
							t.Fatalf("key %s promoted before the frontends loaded it", a.KID)
						}
					}
				}
				m.record(t)
			},
			"mint": func(t *rapid.T) {
				raw, c, err := m.minter.Mint(keys.MintRequest{Subject: "google:1", TTL: keys.MaxTokenTTL})
				if err != nil {
					t.Fatal(err)
				}
				var h struct {
					Kid string `json:"kid"`
				}
				b, _ := base64.RawURLEncoding.DecodeString(strings.Split(raw, ".")[0])
				_ = json.Unmarshal(b, &h)
				m.tokens = append(m.tokens, minted{kid: h.Kid, exp: time.Unix(c.Expiry, 0)})
			},
			"compromise": func(t *rapid.T) {
				st := m.state(t)
				kid := "*"
				if rapid.Bool().Draw(t, "single") {
					kid = rapid.SampledFrom(st.Keys).Draw(t, "key").KID
				}
				restartsBefore := m.restarts
				if err := m.rot.Compromise(ctx, kid, "test"); err != nil {
					t.Fatal(err)
				}
				for _, k := range st.Keys {
					if kid == "*" || k.KID == kid {
						m.compromised[k.KID] = true
					}
				}
				m.record(t)
				for _, k := range m.configMapKids(t) {
					if m.compromised[k] {
						t.Fatalf("compromised key %s still published", k)
					}
				}
				if m.restarts != restartsBefore+1 {
					t.Fatalf("frontends not restarted after a compromise")
				}
				if a, ok := m.state(t).Active(); ok {
					m.activated[a.KID] = clock.Now()
				}
			},
			"": func(t *rapid.T) {
				st := m.state(t)
				if err := st.Validate(); err != nil {
					t.Fatal(err)
				}
				var published []string
				for _, k := range st.Keys {
					published = append(published, k.KID)
				}
				slices.Sort(published)
				if got := m.configMapKids(t); !slices.Equal(got, published) {
					t.Fatalf("JWKS %v != published %v", got, published)
				}
				now := clock.Now()
				for _, tok := range m.tokens {
					if now.Before(tok.exp) && !m.compromised[tok.kid] && !m.verifiableEverywhere(tok.kid, now) {
						t.Fatalf("token signed by %s (exp %s) not verifiable by every frontend at %s", tok.kid, tok.exp, now)
					}
				}
			},
		})
	})
}

// TestRotationRejectsMultiplePending guards R-AUTH-9's persisted-state invariant:
// there may be only one pending successor, including when loading stored state.
func TestRotationRejectsMultiplePending(t *testing.T) {
	state, err := keys.Bootstrap("active", t0).StartRotation("pending-a", t0)
	require.NoError(t, err)
	require.NoError(t, state.Validate())
	state.Keys = append(state.Keys, keys.KeyRecord{KID: "pending-b", State: keys.KeyPending, CreatedAt: t0, PublishedAt: t0})
	require.Error(t, state.Validate(), "multiple pending successors must not be admitted")
}

// TestRotationWaitsForVerification guards the R-AUTH-9 rule that promotion needs proof:
// without a load verifier a pending key never signs (unless explicitly allowed).
func TestRotationWaitsForVerification(t *testing.T) {
	ctx := context.Background()
	clock := keystest.NewClock(t0)
	objs := keystest.NewObjects()
	require.NoError(t, keys.EnsureSigningKeysIn(ctx, objs, authConfig(), clock, nil))
	rot := &keys.Rotator{Objects: objs, Clock: clock, Config: keys.RotatorConfig{
		SigningKeySecret: "cucina-signing-keys", JWKSConfigMap: "cucina-jwks", PublishLead: time.Minute, TokenTTL: keys.MaxTokenTTL}}
	pepper := func() []byte {
		s, err := objs.GetSecret(ctx, "cucina-signing-keys")
		require.NoError(t, err)
		return s.Data["service-key-pepper"]
	}
	before := pepper()
	require.Len(t, before, 32)
	kid, err := rot.StartRotation(ctx)
	require.NoError(t, err)
	clock.Advance(time.Hour)
	st, err := rot.Reconcile(ctx)
	require.NoError(t, err)
	pending, ok := st.Pending()
	require.True(t, ok)
	require.Equal(t, kid, pending.KID)

	require.Equal(t, before, pepper(), "starting a rotation keeps the service-key pepper")
	rot.Config.AllowUnverifiedPromotion = true
	st, err = rot.Reconcile(ctx)
	require.NoError(t, err)
	active, _ := st.Active()
	require.Equal(t, kid, active.KID)
	require.Equal(t, before, pepper(), "rotation keeps the service-key pepper (losing it would invalidate every service key)")
	// PublishLead below the R-AUTH-9 minimum (10 min) is raised to it.
	require.Equal(t, keys.MinPublishLead, rot.Params().PublishLead)

	// Scheduled rotation starts once the active key reaches RotateEvery.
	rot.Config.RotateEvery = 30 * 24 * time.Hour
	clock.Advance(30*24*time.Hour - time.Minute)
	st, err = rot.Reconcile(ctx)
	require.NoError(t, err)
	_, rotating := st.Pending()
	require.False(t, rotating)
	clock.Advance(time.Minute)
	st, err = rot.Reconcile(ctx)
	require.NoError(t, err)
	_, rotating = st.Pending()
	require.True(t, rotating, "scheduled rotation started")
	require.Equal(t, before, pepper())
}

// TestRetiredKeyOutlivesItsTokens guards R-AUTH-9 step 4 at its boundary: a replica that
// has not yet re-read the signing Secret keeps signing with the old key for up to one
// refresh period after the promotion; that key stays in the JWKS until such a token has
// expired, and is removed afterwards.
func TestRetiredKeyOutlivesItsTokens(t *testing.T) {
	ctx := context.Background()
	clock := keystest.NewClock(t0)
	objs := keystest.NewObjects()
	require.NoError(t, keys.EnsureSigningKeysIn(ctx, objs, authConfig(), clock, nil))
	rot := &keys.Rotator{Objects: objs, Clock: clock,
		Loaded: verifierFunc(func(context.Context, string) (bool, error) { return true, nil }),
		Config: keys.RotatorConfig{SigningKeySecret: "cucina-signing-keys", JWKSConfigMap: "cucina-jwks",
			PublishLead: 10 * time.Minute, TokenTTL: keys.MaxTokenTTL, SignerReload: signerReload}}
	ring := &keys.KeyRing{Objects: objs, SecretName: "cucina-signing-keys", Clock: clock}
	jwksKids := func() string {
		cm, err := objs.GetConfigMap(ctx, "cucina-jwks")
		require.NoError(t, err)
		return cm.Data["jwks.json"]
	}

	_, err := rot.StartRotation(ctx)
	require.NoError(t, err)
	clock.Advance(10 * time.Minute)
	require.NoError(t, ring.Refresh(ctx)) // the replica's last refresh before the promotion
	oldKID, _, _ := ring.KeySet().Signing()
	_, err = rot.Reconcile(ctx)
	require.NoError(t, err)
	promoted := clock.Now()

	clock.Advance(signerReload - time.Second)
	_, c, err := (&keys.Minter{Issuer: issuer, Keys: ring, Clock: clock}).Mint(keys.MintRequest{Subject: "google:1", TTL: keys.MaxTokenTTL})
	require.NoError(t, err)
	exp := time.Unix(c.Expiry, 0)

	clock.Set(exp.Add(-time.Second))
	_, err = rot.Reconcile(ctx)
	require.NoError(t, err)
	require.Contains(t, jwksKids(), oldKID, "the old key is still needed by an unexpired token")

	// The grace (max TTL + skew + replica refresh) is counted from the promotion.
	removeAt := promoted.Add(rot.Params().RetireGrace)
	require.False(t, removeAt.Before(exp.Add(keys.ClockSkew)), "grace covers the stale replica's token plus skew")
	clock.Set(removeAt)
	_, err = rot.Reconcile(ctx)
	require.NoError(t, err)
	require.NotContains(t, jwksKids(), oldKID, "removed once every token it signed has expired")
}
