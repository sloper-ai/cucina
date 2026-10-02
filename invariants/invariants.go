// SPDX-License-Identifier: FSL-1.1-ALv2

// Package invariants holds the fleet invariants that stay on in production
// (R-TEST-7) and that the deterministic simulation checks after every step
// (R-TEST-8c). The same predicates run in both places so tests, alerts and the
// controller cannot drift apart (R-TEST-8f).
//
// Predicates are pure functions over domain/ports values and return the
// violations they find (nil when the invariant holds). The production
// controller calls them before acting (a violating action is aborted), counts
// every violation through Report (cucina_invariant_violations_total{invariant})
// and crashes when its own state is suspect; the simulation fails the run.
package invariants

import (
	"fmt"
	"sync"
)

// Name identifies an invariant. It is the value of the `invariant` label of
// cucina_invariant_violations_total, so values are API (docs/contracts.md §6).
type Name string

// The invariants (R-TEST-7, task list in docs/dev/scaling.md).
const (
	// InstancesNeverExceedMax: a pool's active VMs (launching + registered, plus
	// launches in flight) never exceed spec.max. VMs already draining or stopping
	// after a max reduction are retiring and do not count.
	InstancesNeverExceedMax Name = "instances_never_exceed_max"
	// AtMostTwoVMsPerHost: a Mac host never runs more than two macOS VMs
	// (Apple licence, Virtualization.framework limit, R-MAC-3).
	AtMostTwoVMsPerHost Name = "at_most_two_vms_per_host"
	// NeverTerminateBusyOrLeased: no VM is terminated/stopped while one of its
	// runner threads executes an operation, unless its drain timeout expired.
	NeverTerminateBusyOrLeased Name = "never_terminate_busy_or_leased"
	// EveryResourceTagged: every instance, volume and ENI the controller created
	// carries the managed-by/cluster/pool/generation/launch-token/role tags.
	EveryResourceTagged Name = "every_resource_tagged"
	// NoDuplicateLaunchPerToken: at most one instance exists per idempotency token.
	NoDuplicateLaunchPerToken Name = "no_duplicate_launch_per_token"
	// NoStoppedEC2Instances: EC2 workers are terminated, never stopped (zero idle
	// cost, R-POOL-2/D3).
	NoStoppedEC2Instances Name = "no_stopped_ec2_instances"
	// ScaleInOnlyAfterIdleTimeout: an idle scale-in only targets a VM that was
	// idle for at least idleTimeout while the pool's queues were empty.
	ScaleInOnlyAfterIdleTimeout Name = "scale_in_only_after_idle_timeout"
	// NoOrphanVolumesBeyondGrace: pool-tagged volumes/ENIs not attached to a live
	// instance are deleted within the sweep grace period.
	NoOrphanVolumesBeyondGrace Name = "no_orphan_volumes_beyond_grace"
)

// All lists every invariant (for metric pre-registration: each label value is
// exported as 0 before the first violation).
var All = []Name{
	InstancesNeverExceedMax,
	AtMostTwoVMsPerHost,
	NeverTerminateBusyOrLeased,
	EveryResourceTagged,
	NoDuplicateLaunchPerToken,
	NoStoppedEC2Instances,
	ScaleInOnlyAfterIdleTimeout,
	NoOrphanVolumesBeyondGrace,
}

// Violation is one failed invariant check.
type Violation struct {
	Invariant Name
	// Pool is the pool concerned, if any.
	Pool string
	// Subject names the offending resource (instance ID, "<host>/<vm>", host serial, token, …).
	Subject string
	// Detail is a human-readable explanation (logged, never parsed).
	Detail string
}

func (v Violation) Error() string {
	s := string(v.Invariant)
	if v.Pool != "" {
		s += " pool=" + v.Pool
	}
	if v.Subject != "" {
		s += " subject=" + v.Subject
	}
	if v.Detail != "" {
		s += ": " + v.Detail
	}
	return s
}

func violation(inv Name, pool, subject, format string, args ...any) Violation {
	return Violation{Invariant: inv, Pool: pool, Subject: subject, Detail: fmt.Sprintf(format, args...)}
}

// Reporter receives every violation. The production implementation (owned by
// internal/metrics) increments cucina_invariant_violations_total{invariant},
// logs the violation and raises the alert; the simulation records it.
type Reporter interface {
	ReportViolation(v Violation)
}

// ReporterFunc adapts a function to Reporter.
type ReporterFunc func(Violation)

// ReportViolation implements Reporter.
func (f ReporterFunc) ReportViolation(v Violation) { f(v) }

var (
	mu       sync.RWMutex
	reporter Reporter
)

// SetReporter installs the process-wide reporter (the controller does this at
// startup, wiring it to its metrics registry). nil disables reporting.
func SetReporter(r Reporter) {
	mu.Lock()
	defer mu.Unlock()
	reporter = r
}

// Report forwards violations to the installed reporter and returns how many
// were reported. It is safe for concurrent use.
func Report(vs ...Violation) int {
	mu.RLock()
	r := reporter
	mu.RUnlock()
	if r == nil {
		return len(vs)
	}
	for _, v := range vs {
		r.ReportViolation(v)
	}
	return len(vs)
}
