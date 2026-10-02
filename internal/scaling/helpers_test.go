// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// TestFloorWindows guards R-SCALE-7: scheduled minRunning overrides, including
// windows that cross midnight, and minRunning' = max(minRunning, active windows).
func TestFloorWindows(t *testing.T) {
	office, err := scaling.ParseFloorWindow("office", []string{"Mon", "Tue", "Wed", "Thu", "Fri"}, "09:00", "18:00", 2)
	require.NoError(t, err)
	night, err := scaling.ParseFloorWindow("nightly", []string{"Friday"}, "22:00", "06:00", 3)
	require.NoError(t, err)
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		require.NoError(t, err)
		return v
	}
	for _, tc := range []struct {
		now  string
		min  int
		want int
	}{
		{"2026-10-05T08:59:00Z", 0, 0}, // Monday before the window
		{"2026-10-05T09:00:00Z", 0, 2}, // Monday window start
		{"2026-10-05T17:59:59Z", 1, 2}, // max(minRunning, window)
		{"2026-10-05T18:00:00Z", 1, 1}, // window end is exclusive
		{"2026-10-04T10:00:00Z", 0, 0}, // Sunday
		{"2026-10-09T22:00:00Z", 0, 3}, // Friday night window starts (inclusive)
		{"2026-10-09T23:00:00Z", 0, 3}, // Friday night window
		{"2026-10-10T05:59:00Z", 0, 3}, // ...continues into Saturday morning
		{"2026-10-10T06:00:00Z", 0, 0}, // ...and ends at 06:00
		{"2026-10-09T10:00:00Z", 4, 4}, // minRunning above every window
	} {
		require.Equal(t, tc.want, scaling.FloorAt(tc.min, []scaling.FloorWindow{office, night}, at(tc.now)), tc.now)
	}
	_, err = scaling.ParseFloorWindow("bad", []string{"Funday"}, "09:00", "10:00", 1)
	require.Error(t, err)
	_, err = scaling.ParseFloorWindow("bad", []string{"Mon"}, "9am", "10:00", 1)
	require.Error(t, err)
	_, err = scaling.ParseFloorWindow("bad", []string{"Mon"}, "09:00", "10:00", 0) // CRD: minimum 1
	require.Error(t, err)
}

func validSpec() scaling.Spec {
	return scaling.Spec{PoolSpec: domain.PoolSpec{Name: "p", Provider: domain.ProviderEC2, Max: 4, Generation: "g1",
		Runners:     []domain.Runner{{Name: "native", Properties: map[string]string{"OSFamily": "linux"}, Concurrency: 8}},
		IdleTimeout: 5 * time.Minute, DrainTimeout: 30 * time.Minute, StartupTimeout: 5 * time.Minute},
		InstanceTypes: []string{"c8i.2xlarge"}}
}

// TestValidateSpec guards R-POOL-7 dead-man consistency and the fail-fast
// configuration checks of R-TEST-7: the controller's idle policy must be
// strictly tighter than the worker's own dead-man switch.
func TestValidateSpec(t *testing.T) {
	cfg := scaling.DefaultConfig()
	for _, tc := range []struct {
		name string
		mod  func(*scaling.Spec, *scaling.Config)
		ok   bool
	}{
		{"defaults", func(*scaling.Spec, *scaling.Config) {}, true},
		{"windows timers", func(s *scaling.Spec, _ *scaling.Config) {
			s.IdleTimeout, s.StartupTimeout = 10*time.Minute, 15*time.Minute
		}, true},
		{"idle not below dead-man idle limit", func(s *scaling.Spec, _ *scaling.Config) { s.IdleTimeout = 29 * time.Minute }, false},
		{"dead-man idle limit lowered", func(_ *scaling.Spec, c *scaling.Config) { c.Deadman.IdleLimit = 5 * time.Minute }, false},
		{"recycle margin shorter than drain", func(s *scaling.Spec, _ *scaling.Config) { s.DrainTimeout = time.Hour }, false},
		{"minRunning above max", func(s *scaling.Spec, _ *scaling.Config) { s.MinRunning = 5 }, false},
		{"runner without slots", func(s *scaling.Spec, _ *scaling.Config) { s.Runners[0].Concurrency = 0 }, false},
		{"ec2 without instance types", func(s *scaling.Spec, _ *scaling.Config) { s.InstanceTypes = nil }, false},
		{"floor above max", func(s *scaling.Spec, _ *scaling.Config) {
			s.Floors = []scaling.FloorWindow{{Name: "f", MinRunning: 9}}
		}, false},
	} {
		s, c := validSpec(), cfg
		s.Runners = append([]domain.Runner(nil), s.Runners...)
		tc.mod(&s, &c)
		err := scaling.ValidateSpec(s, c)
		if tc.ok {
			require.NoError(t, err, tc.name)
		} else {
			require.Error(t, err, tc.name)
		}
	}
}

// TestLaunchTokens guards R-SCALE-5: tokens are within the EC2 ClientToken
// limit, parse back to their ledger position, and differ across pools.
func TestLaunchTokens(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cluster := rapid.StringMatching(`[a-z0-9-]{1,40}`).Draw(t, "cluster")
		pool := domain.PoolName(rapid.StringMatching(`[a-z0-9.-]{1,63}`).Draw(t, "pool"))
		epoch := rapid.StringMatching(`[0-9a-f]{8}`).Draw(t, "epoch")
		seq := rapid.Uint64().Draw(t, "seq")
		prefix := scaling.TokenPrefix(cluster, pool)
		tok := scaling.MakeToken(prefix, epoch, seq)
		require.LessOrEqual(t, len(tok), 64)
		p, e, s, ok := scaling.ParseToken(tok)
		require.True(t, ok)
		require.Equal(t, []any{prefix, epoch, seq}, []any{p, e, s})
		require.NotEqual(t, prefix, scaling.TokenPrefix(cluster, pool+"x"))
	})
}

// TestBackoff guards R-SCALE-4: jittered exponential backoff stays within
// [base/2, base] with base = min(max, min·2^(n-1)).
func TestBackoff(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		lo := time.Duration(rapid.IntRange(1, 60).Draw(t, "lo")) * time.Second
		hi := lo * time.Duration(rapid.IntRange(1, 64).Draw(t, "factor"))
		n := rapid.IntRange(1, 40).Draw(t, "attempt")
		base := lo
		for i := 1; i < n && base < hi; i++ {
			base *= 2
		}
		base = min(base, hi)
		d := scaling.Backoff(n, lo, hi, fakes.NewRand(rapid.Uint64().Draw(t, "seed")))
		require.GreaterOrEqual(t, d, base/2)
		require.LessOrEqual(t, d, base)
	})
	// Jitter decorrelates pools: different random streams pick different delays.
	seen := map[time.Duration]bool{}
	for seed := uint64(1); seed <= 16; seed++ {
		seen[scaling.Backoff(3, 10*time.Second, 5*time.Minute, fakes.NewRand(seed))] = true
	}
	require.Greater(t, len(seen), 8, "backoff must be jittered")
}

// TestAttributeShared guards contracts §3: queued work of a queue served by
// several pools is split without loss or duplication, eligible pools first in
// lexical order up to their headroom, ineligible pools get nothing unless no
// pool is eligible (then the first pool takes it and fails it fast).
func TestAttributeShared(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		q := domain.QueueKey{InstanceNamePrefix: "main", PlatformKey: "ISA=arm-a64;OSFamily=macos", SizeClass: 1}
		queued := rapid.IntRange(0, 500).Draw(t, "queued")
		n := rapid.IntRange(2, 4).Draw(t, "pools")
		var in []scaling.ShareInput
		anyEligible := false
		for i := 0; i < n; i++ {
			e := rapid.Bool().Draw(t, "eligible")
			anyEligible = anyEligible || e
			in = append(in, scaling.ShareInput{Pool: domain.PoolName(fmt.Sprintf("macos-%d", n-i)), Queues: []domain.QueueKey{q}, Eligible: e,
				Headroom: map[string]int{q.PlatformKey: rapid.IntRange(0, 100).Draw(t, "headroom")}})
		}
		out := scaling.AttributeShared([]domain.QueueObservation{{Key: q, Queued: queued}}, in)
		sum := 0
		for _, p := range in {
			share := out[p.Pool][q]
			require.GreaterOrEqual(t, share, 0)
			sum += share
			if anyEligible && !p.Eligible {
				require.Zero(t, share, "ineligible pool %s", p.Pool)
			}
		}
		require.Equal(t, queued, sum, "shares must add up to the queued work")
		if !anyEligible {
			require.Equal(t, queued, out["macos-1"][q], "with no eligible pool the lexically first one takes everything")
		}
		// Eligible pools fill in lexical order: every eligible pool but the last
		// takes up to its headroom before the next one gets anything.
		var eligible []scaling.ShareInput
		for i := 1; i <= n; i++ { // macos-1 … macos-n (n < 10: lexical = numeric)
			for _, p := range in {
				if p.Pool == domain.PoolName(fmt.Sprintf("macos-%d", i)) && p.Eligible {
					eligible = append(eligible, p)
				}
			}
		}
		left := queued
		for k, p := range eligible {
			want := left
			if k < len(eligible)-1 {
				want = min(left, p.Headroom[q.PlatformKey])
			}
			require.Equal(t, want, out[p.Pool][q], "share of %s", p.Pool)
			left -= want
		}
	})
}

// TestTokenBucket guards R-SCALE-6: RunInstances burst 5, refill 2/s.
func TestTokenBucket(t *testing.T) {
	t0 := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	b := scaling.NewTokenBucket(5, 2, t0)
	require.Equal(t, scaling.Budget{Limited: true, Launches: 5}, b.Budget(t0))
	require.Equal(t, 5, b.Take(7, t0))
	require.Zero(t, b.Take(1, t0))
	require.Equal(t, 1, b.Available(t0.Add(500*time.Millisecond)))
	require.Equal(t, 5, b.Available(t0.Add(time.Hour)), "never more than the burst")
}
