// SPDX-License-Identifier: FSL-1.1-ALv2

// Package fakes holds one maintained, stateful, seedable and fault-injectable
// fake per port in internal/ports (R-TEST-8a): Clock, Rand, Compute (EC2),
// VMRuntime (Tart), BuildQueue (Buildbarn BuildQueueState), HostFleet (Mac
// hosts), IdentityProvider, SecretStore, Exec and FS.
//
// The fakes are temporal simulators, not interaction mocks (precedent:
// Karpenter's fake EC2 API): instances go pending → running → ready after a
// latency, Describe is eventually consistent, capacity runs out per (type, AZ),
// API calls are throttled by token buckets, launches are idempotent per
// client token, the scheduler executes operations for their duration, hosts
// enforce the 2-VM limit. Every fake
//
//   - draws randomness only from a *Rand derived from one seed,
//   - reads time only from a ports.Clock (use *Clock and Advance, or SystemClock
//     inside a testing/synctest bubble),
//   - exposes FailNext(op, err), FailRate(op, p, err) and SetLatency(op, d), where
//     op is the port method name ("Launch", "Describe", …).
//
// Each fake passes the conformance suite of its port in internal/ports/porttest
// (internal/fakes/*_test.go). When a real adapter fails a suite, the fake is
// fixed so that it behaves like the real thing.
//
// Time-driven state changes are applied lazily: every call first advances the
// fake to clock.Now(); simulations call Tick after advancing the clock so that
// hooks (instance ready → worker registers) fire even without API calls.
package fakes
