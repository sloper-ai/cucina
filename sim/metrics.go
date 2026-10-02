// SPDX-License-Identifier: FSL-1.1-ALv2

package sim

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[(len(sorted)-1)*p/100]
}

func (w *world) finish(end time.Time) Result {
	res := Result{Scenario: w.sc.Name, Seed: w.sc.Seed, Policy: w.opt.Policy.Name}
	m := &w.m
	var waits []time.Duration
	for _, a := range w.actions {
		m.Actions++
		switch {
		case !a.done:
			m.Unfinished++
		case a.failed:
			m.Failed++
			m.FailWaitMax = max(m.FailWaitMax, a.failedAt.Sub(a.arrival))
		default:
			m.Succeeded++
			waits = append(waits, a.started.Sub(a.arrival))
		}
	}
	m.Unfinished += len(w.arrivals)
	m.Actions += len(w.arrivals)
	slices.Sort(waits)
	m.WaitP50, m.WaitP95 = percentile(waits, 50), percentile(waits, 95)
	if len(waits) > 0 {
		m.WaitMax = waits[len(waits)-1]
	}
	secs, usd := w.compute.InstanceSeconds()
	for _, p := range w.pools {
		m.InstanceSeconds += secs[p.spec.Name]
		m.CostUSD += usd[p.spec.Name]
	}
	for _, in := range w.compute.All() {
		if in.State == ports.InstancePending || in.State == ports.InstanceRunning {
			m.EndLive++
		}
	}
	if hs, err := w.hosts.Hosts(w.ctx); err == nil {
		for _, h := range hs {
			for _, vm := range h.VMs {
				if vm.State != domain.VMStopped && vm.State != domain.VMTerminated {
					m.EndLive++
				}
			}
		}
	}

	e := w.sc.Expectations
	fail := func(format string, args ...any) { res.Failures = append(res.Failures, fmt.Sprintf(format, args...)) }
	bound := func(name string, got time.Duration, limit Duration) {
		if limit > 0 && got > limit.D() {
			fail("%s %s > %s", name, got, limit.D())
		}
	}
	bound("queue wait p50", m.WaitP50, e.QueueWaitP50)
	bound("queue wait p95", m.WaitP95, e.QueueWaitP95)
	bound("queue wait max", m.WaitMax, e.QueueWaitMax)
	bound("fail-fast wait max", m.FailWaitMax, e.MaxFailWait)
	bound("idle with empty queue", m.MaxIdleWithEmptyQueue, e.MaxIdleWithEmptyQueue)
	for _, pool := range sortedNames(e.MaxInstances) {
		if got := m.MaxLive[pool]; got > e.MaxInstances[pool] {
			fail("pool %s peaked at %d live VMs > %d", pool, got, e.MaxInstances[pool])
		}
	}
	for _, pool := range sortedNames(e.MaxLaunches) {
		if got := m.Launches[pool]; got > e.MaxLaunches[pool] {
			fail("pool %s launched %d VMs > %d", pool, got, e.MaxLaunches[pool])
		}
	}
	if e.MaxCostUSD > 0 && m.CostUSD > e.MaxCostUSD {
		fail("cost $%.2f > $%.2f", m.CostUSD, e.MaxCostUSD)
	}
	if e.AllSucceed && (m.Failed > 0 || m.Unfinished > 0) {
		fail("%d actions failed and %d unfinished (all must succeed)", m.Failed, m.Unfinished)
	}
	if e.MinFailed > 0 && m.Failed < e.MinFailed {
		fail("%d actions failed fast < %d expected", m.Failed, e.MinFailed)
	}
	if e.EndAtZero && m.EndLive > 0 {
		fail("%d VMs still live at the end (scale to zero)", m.EndLive)
	}
	res.Violations = w.violations
	res.Metrics = *m
	res.Passed = len(res.Failures) == 0 && len(res.Violations) == 0
	res.Log = w.log
	return res
}

func sortedNames(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Summary is a one-line description of a result.
func (r Result) Summary() string {
	status := "PASS"
	if !r.Passed {
		status = "FAIL"
	}
	m := r.Metrics
	return fmt.Sprintf("%s %s seed=%d policy=%s actions=%d ok=%d failed=%d unfinished=%d wait p50=%s p95=%s max=%s cost=$%.2f launches=%v",
		status, r.Scenario, r.Seed, r.Policy, m.Actions, m.Succeeded, m.Failed, m.Unfinished,
		m.WaitP50.Round(time.Second), m.WaitP95.Round(time.Second), m.WaitMax.Round(time.Second), m.CostUSD, m.Launches)
}

// Problems lists the expectation failures and invariant violations.
func (r Result) Problems() string {
	var b strings.Builder
	for _, f := range r.Failures {
		b.WriteString("  expectation: " + f + "\n")
	}
	for _, v := range r.Violations {
		b.WriteString("  invariant: " + v.String() + "\n")
	}
	return b.String()
}
