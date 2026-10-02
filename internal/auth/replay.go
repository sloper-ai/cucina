// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"sync"
	"time"
)

// replayCache remembers (issuer, jti) of exchanged tokens until they expire (R-AUTH-7).
// It is per replica: the STS is stateless, so a token could be replayed once against each
// other replica within its lifetime (documented in docs/security.md). When full of
// unexpired entries it refuses new ones (fail closed) instead of evicting.
type replayCache struct {
	mu  sync.Mutex
	max int
	m   map[string]time.Time
}

type replayResult int

const (
	replayFresh replayResult = iota
	replaySeen
	replayFull
)

func newReplayCache(max int) *replayCache { return &replayCache{max: max, m: map[string]time.Time{}} }

// record stores key until `until` and reports whether it was fresh.
func (r *replayCache) record(key string, until, now time.Time) replayResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.m[key]; ok && now.Before(t) {
		return replaySeen
	}
	if len(r.m) >= r.max {
		for k, t := range r.m {
			if !now.Before(t) {
				delete(r.m, k)
			}
		}
		if len(r.m) >= r.max {
			return replayFull
		}
	}
	r.m[key] = until
	return replayFresh
}
