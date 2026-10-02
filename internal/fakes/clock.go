// SPDX-License-Identifier: FSL-1.1-ALv2

package fakes

import (
	"context"
	"encoding/hex"
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// ------------------------------------------------------------------ Clock

// Clock is a manual clock: time moves only through Advance/Set. Timers created
// by After and Sleep fire, in deadline order, when the clock passes them.
type Clock struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Time
	timers []*timer
	seq    int
}

type timer struct {
	at  time.Time
	seq int
	ch  chan time.Time
}

var _ ports.Clock = (*Clock)(nil)

// NewClock returns a clock reading start.
func NewClock(start time.Time) *Clock {
	c := &Clock{now: start}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Now implements ports.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After implements ports.Clock.
func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.seq++
	c.timers = append(c.timers, &timer{at: c.now.Add(d), seq: c.seq, ch: ch})
	sort.Slice(c.timers, func(i, j int) bool {
		if !c.timers[i].at.Equal(c.timers[j].at) {
			return c.timers[i].at.Before(c.timers[j].at)
		}
		return c.timers[i].seq < c.timers[j].seq
	})
	c.cond.Broadcast()
	return ch
}

// Sleep implements ports.Clock.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Advance moves the clock forward by d, firing due timers in order.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(c.now.Add(d))
}

// Set moves the clock to t (never backwards).
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(t)
}

func (c *Clock) setLocked(t time.Time) {
	if t.Before(c.now) {
		return
	}
	for len(c.timers) > 0 && !c.timers[0].at.After(t) {
		tm := c.timers[0]
		c.timers = c.timers[1:]
		c.now = tm.at
		tm.ch <- tm.at
	}
	c.now = t
}

// Pending returns the number of timers not yet fired.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// BlockUntilTimers blocks until at least n timers are pending (for tests that
// drive goroutines blocked in Sleep/After; waits on a condition, never sleeps).
func (c *Clock) BlockUntilTimers(ctx context.Context, n int) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.cond.Broadcast()
			c.mu.Unlock()
		case <-done:
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.timers) < n {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.cond.Wait()
	}
	return nil
}

// SystemClock implements ports.Clock with package time. Inside a
// testing/synctest bubble package time is virtual, so SystemClock is the
// deterministic fake clock for goroutine-based tests (time advances when every
// goroutine in the bubble is blocked).
type SystemClock struct{}

var _ ports.Clock = SystemClock{}

// Now implements ports.Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// After implements ports.Clock.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Sleep implements ports.Clock.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ------------------------------------------------------------------- Rand

// Rand is a seeded PCG source implementing ports.Rand. It is safe for
// concurrent use; Child derives independent deterministic streams so that
// adding draws in one fake never shifts another fake's sequence.
type Rand struct {
	mu   sync.Mutex
	r    *rand.Rand
	seed uint64
}

var _ ports.Rand = (*Rand)(nil)

// NewRand returns a generator for seed.
func NewRand(seed uint64) *Rand {
	return &Rand{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), seed: seed}
}

// Seed returns the seed this generator was created from.
func (r *Rand) Seed() uint64 { return r.seed }

// Child returns an independent generator derived from this one's seed and label.
func (r *Rand) Child(label string) *Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(label))
	return NewRand(r.seed ^ h.Sum64())
}

// Float64 implements ports.Rand: a value in [0, 1).
func (r *Rand) Float64() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.Float64()
}

// Int63n implements ports.Rand: a value in [0, n); n <= 0 returns 0.
func (r *Rand) Int63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.r.Int64N(n)
}

// Token implements ports.Rand: 2n lowercase hex characters.
func (r *Rand) Token(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	r.mu.Lock()
	for i := range b {
		b[i] = byte(r.r.Uint32())
	}
	r.mu.Unlock()
	return hex.EncodeToString(b)
}

// Duration returns a duration uniformly drawn from [lo, hi].
func (r *Rand) Duration(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + time.Duration(r.Int63n(int64(hi-lo)+1))
}
