// SPDX-License-Identifier: FSL-1.1-ALv2

package fakes

import (
	"context"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Faults is the fault-injection knob set embedded in every fake. Operations
// are named after the port method ("Launch", "Describe", "ListWorkers", …).
type Faults struct {
	mu      sync.Mutex
	next    map[string][]error
	rate    map[string]rateFault
	latency map[string]time.Duration
	calls   map[string]int
	rnd     *Rand
	clock   ports.Clock
}

type rateFault struct {
	p   float64
	err error
}

func newFaults(clock ports.Clock, rnd *Rand) *Faults {
	return &Faults{next: map[string][]error{}, rate: map[string]rateFault{}, latency: map[string]time.Duration{},
		calls: map[string]int{}, rnd: rnd, clock: clock}
}

// FailNext makes the next call of op fail with err (queued: several calls
// queue several failures).
func (f *Faults) FailNext(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next[op] = append(f.next[op], err)
}

// FailRate makes every call of op fail with err with probability p (drawn
// from the fake's seeded Rand). p <= 0 removes the fault.
func (f *Faults) FailRate(op string, p float64, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p <= 0 {
		delete(f.rate, op)
		return
	}
	f.rate[op] = rateFault{p: p, err: err}
}

// SetLatency makes every call of op take d on the fake's clock (the call
// blocks in Clock.Sleep: advance the clock, or run inside synctest).
func (f *Faults) SetLatency(op string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d <= 0 {
		delete(f.latency, op)
		return
	}
	f.latency[op] = d
}

// Calls returns how many times op was called (including failed calls).
func (f *Faults) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// enter applies latency and injected faults for one call of op.
func (f *Faults) enter(ctx context.Context, op string) error {
	f.mu.Lock()
	f.calls[op]++
	d := f.latency[op]
	var err error
	if q := f.next[op]; len(q) > 0 {
		err = q[0]
		f.next[op] = q[1:]
	} else if rf, ok := f.rate[op]; ok && f.rnd.Float64() < rf.p {
		err = rf.err
	}
	f.mu.Unlock()
	if d > 0 {
		if serr := f.clock.Sleep(ctx, d); serr != nil {
			return serr
		}
	}
	if ctx != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
	}
	return err
}
