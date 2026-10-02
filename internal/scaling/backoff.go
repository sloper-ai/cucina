// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling

import (
	"slices"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Backoff returns the jittered exponential delay before attempt n (n ≥ 1):
// base = min(hi, lo·2^(n-1)), uniformly drawn from [base/2, base] ("equal
// jitter": never shorter than half the base, so retries cannot stampede, and
// decorrelated across pools through the shared seeded Rand).
func Backoff(n int, lo, hi time.Duration, rnd ports.Rand) time.Duration {
	if n < 1 {
		n = 1
	}
	base := lo
	for i := 1; i < n && base < hi; i++ {
		base *= 2
	}
	base = min(base, hi)
	half := base / 2
	if half <= 0 {
		return base
	}
	return half + time.Duration(rnd.Float64()*float64(base-half))
}

// RotateTypes returns the preference list (instance types or subnets) with
// the entries still cooling down after a capacity failure moved to the end
// (stable within both groups).
func RotateTypes(types []string, cooldown map[string]time.Time, now time.Time) []string {
	out := make([]string, 0, len(types))
	var cooling []string
	for _, t := range types {
		if until, ok := cooldown[t]; ok && now.Before(until) {
			cooling = append(cooling, t)
			continue
		}
		out = append(out, t)
	}
	return append(out, cooling...)
}

// coolBefore marks every entry tried before the one that succeeded as
// recently out of capacity (the adapter walks the lists in order).
func coolBefore(cooldown map[string]time.Time, tried []string, got string, until time.Time) {
	i := slices.Index(tried, got)
	if i <= 0 {
		return
	}
	for _, t := range tried[:i] {
		cooldown[t] = until
	}
}

func (s *PoolState) expireCooldowns(now time.Time) {
	for _, m := range []map[string]time.Time{s.typeCooldown, s.subnetCooldown} {
		for k, until := range m {
			if !now.Before(until) {
				delete(m, k)
			}
		}
	}
}
