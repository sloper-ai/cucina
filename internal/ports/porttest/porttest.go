// SPDX-License-Identifier: FSL-1.1-ALv2

// Package porttest holds one conformance suite per port of internal/ports
// (R-TEST-8b), in the style of testing/fstest, nettest, csi-sanity and CRI
// critest: porttest.RunCompute(t, newCompute), RunVMRuntime, RunBuildQueue,
// RunHostFleet, RunClock, RunFS and RunSecretStore.
//
// The suites run against the fakes in internal/fakes (integration tier) and
// against the real adapters in acceptance (tiny EC2 instances, Tart on the dev
// Mac, the pinned bb_scheduler binary). A harness struct per port supplies the
// adapter plus optional capabilities; a nil capability skips the checks a real
// backend cannot perform deterministically (for example forcing an
// InsufficientInstanceCapacity error) with an explicit t.Skip reason.
//
// When a real adapter fails a suite, fix the fake so that it behaves like the
// real thing — the suites define the contract both must meet.
package porttest

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Await waits until cond holds. Fake harnesses advance their clock between
// polls; real harnesses poll on wall-clock time. Implementations must fail the
// test (t.Fatalf) when cond does not hold before their deadline.
type Await func(t *testing.T, what string, cond func() bool)

// FakeAwait returns an Await for fakes: it calls advance(step) and tick()
// between checks, for at most limit of simulated time.
func FakeAwait(advance func(time.Duration), tick func(), step, limit time.Duration) Await {
	return func(t *testing.T, what string, cond func() bool) {
		t.Helper()
		for waited := time.Duration(0); ; waited += step {
			if tick != nil {
				tick()
			}
			if cond() {
				return
			}
			if waited >= limit {
				t.Fatalf("timed out after %s (simulated) waiting for %s", limit, what)
			}
			advance(step)
		}
	}
}

var uniqSeq atomic.Uint64

// uniq returns a name unique across runs (real backends are shared).
func uniq(_ *testing.T, prefix string) string {
	return fmt.Sprintf("%s-%x-%d", prefix, time.Now().UnixNano()&0xffffffffff, uniqSeq.Add(1))
}
