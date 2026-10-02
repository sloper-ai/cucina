// SPDX-License-Identifier: FSL-1.1-ALv2

package ec2

import (
	"context"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Limit configures one client-side token bucket: up to Burst requests at once,
// refilled at RefillPerSecond (R-SCALE-6).
type Limit struct {
	Burst           int
	RefillPerSecond float64
}

// Limits are the client-side token buckets, one per EC2 API class. EC2 meters its
// own buckets per account and region, so these keep the controller well inside them
// even when other tooling shares the account.
type Limits struct {
	RunInstances Limit // RunInstances (default burst 5, refill 2/s)
	Describe     Limit // Describe* (default burst 50, refill 10/s)
	Terminate    Limit // TerminateInstances (default burst 20, refill 5/s)
	Mutate       Limit // DeleteVolume, DeleteNetworkInterface, Enable/DisableFastLaunch (default 20, 5/s)
	Pricing      Limit // pricing:GetProducts (default burst 5, refill 2/s)
}

func (l Limits) withDefaults() Limits {
	def := func(v Limit, burst int, refill float64) Limit {
		if v.Burst <= 0 {
			v.Burst = burst
		}
		if v.RefillPerSecond <= 0 {
			v.RefillPerSecond = refill
		}
		return v
	}
	l.RunInstances = def(l.RunInstances, 5, 2)
	l.Describe = def(l.Describe, 50, 10)
	l.Terminate = def(l.Terminate, 20, 5)
	l.Mutate = def(l.Mutate, 20, 5)
	l.Pricing = def(l.Pricing, 5, 2)
	return l
}

// bucket is a small token bucket driven by a ports.Clock, so tests advance time
// instead of sleeping. It is safe for concurrent use.
type bucket struct {
	clock ports.Clock
	burst float64
	rate  float64 // tokens per second

	mu           sync.Mutex
	tokens       float64
	last         time.Time
	blockedUntil time.Time // set from Retry-After / throttling responses
}

func newBucket(clock ports.Clock, l Limit) *bucket {
	return &bucket{clock: clock, burst: float64(l.Burst), rate: l.RefillPerSecond, tokens: float64(l.Burst), last: clock.Now()}
}

// Wait blocks until a token is available or ctx is done.
func (b *bucket) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wait := b.take()
		if wait <= 0 {
			return nil
		}
		if err := b.clock.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// take consumes a token and returns 0, or returns how long to wait for one.
func (b *bucket) take() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if now.Before(b.blockedUntil) {
		return b.blockedUntil.Sub(now)
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	return time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

// Throttled drains the bucket after EC2 throttled a request despite the SDK's
// retries, and blocks it for at least retryAfter (the server's Retry-After hint).
func (b *bucket) Throttled(retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = 0
	if retryAfter > 0 {
		if until := b.clock.Now().Add(retryAfter); until.After(b.blockedUntil) {
			b.blockedUntil = until
		}
	}
}

// systemClock is the production ports.Clock.
type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// otterClock adapts a ports.Clock to otter's Clock so cache TTLs follow the
// injected (possibly fake) clock.
type otterClock struct{ c ports.Clock }

func (o otterClock) NowNano() int64                        { return o.c.Now().UnixNano() }
func (o otterClock) Tick(d time.Duration) <-chan time.Time { return time.Tick(d) } //nolint:staticcheck // otter stops the goroutine on Close
