// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// reference is the naive model of the scale-out policy for a one-runner pool
// (contracts §3): clamp(ceil((queued + executing) / slots), minRunning, max).
func reference(queued, executing, slots, minRunning, maxVMs int) int {
	d := (queued + executing + slots - 1) / slots
	return min(max(d, minRunning), maxVMs)
}

// TestPlanAgainstReference guards R-TEST-6 / R-SCALE-2/3 with random
// workloads (bursts, trickles, oscillation) against a ground-truth world:
// desired equals the naive reference model whenever the planner can see; the
// pool never exceeds max; no VM with running operations is terminated; desired
// is ≥ 1 whenever work is queued and max > 0; scale-in happens only after
// idleTimeout; oscillating input shorter than idleTimeout never scales in
// (no flapping); and the pool returns to its floor once the input stops.
func TestPlanAgainstReference(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		maxVMs := rapid.IntRange(0, 5).Draw(t, "max")
		slots := rapid.IntRange(1, 8).Draw(t, "slots")
		idle := time.Duration(rapid.IntRange(2, 6).Draw(t, "idleMinutes")) * time.Minute
		w := newWorld(rapid.Uint64().Draw(t, "seed"), maxVMs, slots, idle)
		w.spec.MinRunning = rapid.IntRange(0, maxVMs).Draw(t, "minRunning")
		oscillate := rapid.Bool().Draw(t, "oscillate")
		const poll = 5 * time.Second
		period := rapid.IntRange(4, int(idle/poll)-4).Draw(t, "periodPolls")
		steps := rapid.IntRange(10, 120).Draw(t, "polls")
		dur := time.Duration(rapid.IntRange(1, 60).Draw(t, "workSeconds")) * time.Second
		burst := rapid.IntRange(1, 2*slots).Draw(t, "burst") // constant amplitude: strictly periodic input
		for i := 0; i < steps; i++ {
			switch {
			case oscillate && i%period == 0:
				w.arrive(burst, dur)
			case !oscillate && rapid.IntRange(0, 15).Draw(t, "arrival") == 0:
				w.arrive(rapid.IntRange(1, 12).Draw(t, "n"), dur)
			}
			d := w.step()
			queued, executing := w.lastObs.Queues[0].Queued, 0
			for _, wk := range w.lastObs.Workers {
				if wk.Executing {
					executing++
				}
			}
			require.Equal(t, reference(queued, executing, slots, w.spec.MinRunning, maxVMs), d.Desired,
				"desired at poll %d (queued %d, executing %d)", i, queued, executing)
			w.advance(poll)
		}
		if oscillate {
			// No flapping: under periodic input shorter than idleTimeout a VM is
			// launched at most once; only VMs that never got work are scaled in
			// (checked against ground truth at every idle drain).
			require.LessOrEqual(t, w.launches, maxVMs, "relaunch churn under oscillating input (period %d polls < idleTimeout %s)", period, idle)
		}
		// Input stops: once the work is done and idleTimeout passed, the pool is back at its floor.
		w.settle(poll, 24*time.Hour)
		require.Empty(t, w.violations)
		require.Equal(t, w.spec.MinRunning, w.live(), "pool returns to minRunning when idle")
	})
}

// TestLifecycleUnderFaults is the rapid state machine of R-TEST-6 / R-SCALE-5/8:
// arbitrary interleavings of arrivals, time, ICE, throttling, ambiguous launch
// errors, failed terminate calls, controller restarts (state lost, persisted
// ledger kept) while EC2 Describe lags behind, scheduler blind periods, worker
// deaths, spot interruptions and live max changes never break the safety
// invariants, and once the faults clear the pool scales back to zero.
func TestLifecycleUnderFaults(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		seed := rapid.Uint64().Draw(t, "seed")
		w := newWorld(seed, rapid.IntRange(1, 5).Draw(t, "max"), rapid.IntRange(1, 8).Draw(t, "slots"), 5*time.Minute)
		// Describe lists new instances 0–60 s late (they may register before they are listed).
		w.visibleLag = time.Duration(rapid.SampledFrom([]int{0, 5, 20, 60}).Draw(t, "visibleLagSeconds")) * time.Second
		const poll = 5 * time.Second
		run := func(n int) {
			for i := 0; i < n; i++ {
				w.step()
				w.advance(poll)
			}
		}
		pick := func(t *rapid.T, ok func(*tvm) bool) *tvm {
			var vs []*tvm
			for _, v := range w.vms {
				if ok(v) {
					vs = append(vs, v)
				}
			}
			if len(vs) == 0 {
				return nil
			}
			return vs[rapid.IntRange(0, len(vs)-1).Draw(t, "vm")]
		}
		requeue := func(v *tvm) {
			for i, s := range v.slots {
				if s != nil {
					w.queue = append([]*work{s}, w.queue...)
					v.slots[i] = nil
				}
			}
		}
		t.Repeat(map[string]func(*rapid.T){
			"arrive": func(t *rapid.T) {
				w.arrive(rapid.IntRange(1, 24).Draw(t, "n"), time.Duration(rapid.IntRange(1, 60).Draw(t, "s"))*time.Second)
			},
			"time":      func(t *rapid.T) { run(rapid.IntRange(1, 48).Draw(t, "polls")) },
			"ice":       func(t *rapid.T) { w.ice = !w.ice },
			"throttle":  func(t *rapid.T) { w.throttle += rapid.IntRange(1, 6).Draw(t, "calls") },
			"ambiguous": func(t *rapid.T) { w.ambig++ },
			"stopFails": func(t *rapid.T) { w.stopFails += rapid.IntRange(1, 3).Draw(t, "calls") },
			"restart": func(t *rapid.T) {
				seed++
				w.restart(seed)
			},
			"blind":  func(t *rapid.T) { w.blind += rapid.IntRange(1, 36).Draw(t, "polls") },
			"setMax": func(t *rapid.T) { w.setMax(rapid.IntRange(0, 5).Draw(t, "newMax")) }, // UC11: max changes live
			"workerDeath": func(t *rapid.T) {
				if v := pick(t, func(v *tvm) bool { return w.registered(v) }); v != nil {
					requeue(v)
					v.lost = true
				}
			},
			"spot": func(t *rapid.T) {
				if v := pick(t, func(v *tvm) bool { return !v.terminated }); v != nil {
					requeue(v)
					v.terminated, v.terminatedAt = true, w.now
				}
			},
			"": func(t *rapid.T) { require.Empty(t, w.violations) },
		})
		w.ice, w.throttle, w.ambig, w.blind, w.stopFails = false, 0, 0, 0, 0
		w.spec.Max = max(w.spec.Max, 1) // so that remaining work can finish
		w.settle(poll, 24*time.Hour)
		require.Empty(t, w.violations)
		require.Zero(t, w.live(), "all VMs terminated once idle (zero idle cost)")
		require.Empty(t, w.queue, "no work left hanging")
	})
}

// TestCapacityRotation guards R-SCALE-4 / R-POOL-2: after a launch only
// succeeded on a later instance type and subnet (the earlier ones had no
// capacity), the next launches try the working ones first, until
// CapacityCooldown has passed.
func TestCapacityRotation(t *testing.T) {
	w := newWorld(1, 4, 8, 5*time.Minute)
	w.spec.StartupTimeout = time.Hour // the VMs stay launching: only rotation is under test
	w.spec.InstanceTypes = []string{"c8i.2xlarge", "c7i.2xlarge", "c7a.2xlarge"}
	w.spec.SubnetIDs = []string{"subnet-a", "subnet-b"}
	var listed []ports.Instance // what Describe returns
	obs := func(queued int, results ...scaling.Result) scaling.Observation {
		return scaling.Observation{Now: w.now, QueuesKnown: true, WorkersKnown: true, ProviderKnown: true, Results: results,
			Queues: []domain.QueueObservation{{Key: w.key, Queued: queued}}, Instances: listed}
	}
	launches := func(d scaling.Decision) []scaling.Action {
		var out []scaling.Action
		for _, a := range d.Actions {
			if a.Kind == scaling.ActLaunch {
				out = append(out, a)
			}
		}
		return out
	}
	first := launches(w.planner.Plan(w.spec, obs(8), w.st))
	require.Len(t, first, 1)
	require.Equal(t, w.spec.InstanceTypes, first[0].Launch.InstanceTypes)
	require.Equal(t, w.spec.SubnetIDs, first[0].Launch.SubnetIDs)

	w.now = w.now.Add(5 * time.Second)
	got := ports.Instance{ID: "i-1", Pool: "p", State: ports.InstancePending, Type: "c7a.2xlarge", SubnetID: "subnet-b", LaunchTime: w.now}
	next := launches(w.planner.Plan(w.spec, obs(16, scaling.Result{Action: first[0], Instance: &got, At: w.now}), w.st))
	require.Len(t, next, 1)
	require.Equal(t, []string{"c7a.2xlarge", "c8i.2xlarge", "c7i.2xlarge"}, next[0].Launch.InstanceTypes)
	require.Equal(t, []string{"subnet-b", "subnet-a"}, next[0].Launch.SubnetIDs)

	listed = append(listed, got)
	w.now = w.now.Add(scaling.DefaultConfig().CapacityCooldown + time.Second)
	got2 := ports.Instance{ID: "i-2", Pool: "p", State: ports.InstancePending, Type: "c7a.2xlarge", SubnetID: "subnet-b", LaunchTime: w.now}
	later := launches(w.planner.Plan(w.spec, obs(24, scaling.Result{Action: next[0], Instance: &got2, At: w.now}), w.st))
	require.Len(t, later, 1)
	require.Equal(t, w.spec.InstanceTypes, later[0].Launch.InstanceTypes, "original preference after the cooldown")
	require.Equal(t, w.spec.SubnetIDs, later[0].Launch.SubnetIDs)
}
