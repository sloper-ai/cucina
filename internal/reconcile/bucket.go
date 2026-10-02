// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"sync"
	"time"
)

// TokenBucket is the shared EC2 RunInstances budget (R-SCALE-6: burst 5,
// refill 2/s by default). It reads time from the caller, so it is
// deterministic under a fake clock.
type TokenBucket struct {
	mu     sync.Mutex
	burst  float64
	refill float64 // tokens per second
	tokens float64
	last   time.Time
}

// NewTokenBucket returns a full bucket.
func NewTokenBucket(burst int, refillPerSecond float64) *TokenBucket {
	return &TokenBucket{burst: float64(burst), refill: refillPerSecond, tokens: float64(burst)}
}

func (b *TokenBucket) advance(now time.Time) {
	if !b.last.IsZero() && now.After(b.last) {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.refill)
	}
	if b.last.IsZero() || now.After(b.last) {
		b.last = now
	}
}

// Available returns the whole tokens available at now.
func (b *TokenBucket) Available(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance(now)
	return int(b.tokens)
}

// Take consumes one token if available.
func (b *TokenBucket) Take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advance(now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
