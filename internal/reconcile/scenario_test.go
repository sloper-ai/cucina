// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/reconcile"
)

// Guards the full autoscaler loop through the executor (R-SCALE-1..3, R-RE-4,
// NFR-C1, NFR-P1 on the fake latencies, R-TEST-7 invariants): a queue that
// appears while the pool is at zero launches exactly the VMs the demand needs,
// they register and run the work, stay while idle for less than idleTimeout,
// and are then drained and terminated — leaving zero instances and no orphans.
func TestScenarioScaleFromZeroToZero(t *testing.T) {
	h := newHarness(t, 7)
	rt := h.addPool(linuxPool("linux", 4))
	pool := rt.Spec.Name
	native := rt.Queues[0] // instance "main" × native runner (32 slots per c8i.8xlarge)
	require.Equal(t, "ISA=x86-64;OSFamily=linux", native.PlatformKey)

	for range 10 {
		h.step()
	}
	assert.Empty(t, h.compute.All(), "an idle pool at zero launches nothing")

	submitted := h.clock.Now()
	var ops []string
	for range 40 { // 40 actions of 90 s: two 32-slot VMs
		name, err := h.bq.Submit(native, 90*time.Second, "build")
		require.NoError(t, err)
		ops = append(ops, name)
	}

	qt := &reconcile.QueueTimer{BuildQueue: h.bq, Clock: h.clock, Metrics: h.metrics}
	var maxAlive int
	var maxOldest time.Duration
	lastDone := time.Time{}
	for i := 0; i < 20*60; i++ {
		h.step()
		qt.Poll(h.ctx)
		maxOldest = max(maxOldest, qt.Stats()[native].OldestQueuedAge)
		maxAlive = max(maxAlive, len(h.alive(pool)))
		done := 0
		for _, op := range h.bq.Ops() {
			if op.Stage == fakes.StageCompleted {
				done++
				require.False(t, op.Failed, "operation %s failed: %s", op.Name, op.Message)
				if op.CompletedAt.After(lastDone) {
					lastDone = op.CompletedAt
				}
			}
		}
		if done == len(ops) && len(h.alive(pool)) == 0 {
			break
		}
	}
	h.noViolations()

	// Scale-out: exactly ceil(40/32) = 2 VMs, never more (cost runaway guard).
	assert.Equal(t, 2, maxAlive)
	for _, op := range h.bq.Ops() {
		assert.LessOrEqual(t, op.Wait(h.clock.Now()), 90*time.Second, "cold start of %s (queued at %s)", op.Name, submitted)
	}
	// Scale-in only after idleTimeout (5 m for Linux) with empty queues, then terminate.
	var terminated []ports.Instance
	for _, in := range h.compute.All() {
		if in.Pool == pool {
			terminated = append(terminated, in)
		}
	}
	require.Len(t, terminated, 2)
	for _, in := range terminated {
		assert.Contains(t, []ports.InstanceState{ports.InstanceShuttingDown, ports.InstanceTerminated}, in.State)
	}
	assert.InDelta(t, 2.0, testutil.ToFloat64(h.metrics.VMStops.WithLabelValues(string(pool), "idle")), 0)
	assert.GreaterOrEqual(t, h.clock.Now().Sub(lastDone), rt.Spec.IdleTimeout, "terminated before the idle timeout")
	for tok, n := range h.compute.LaunchesPerToken() {
		assert.Equal(t, 1, n, "token %s launched %d instances", tok, n)
	}
	orphans, err := h.compute.ListOrphans(h.ctx, h.cfg.ClusterID)
	require.NoError(t, err)
	assert.Empty(t, orphans)

	// Once the instances finished shutting down the pool reports empty (the
	// signal the WorkerPool finalizer waits for).
	for i := 0; i < 120; i++ {
		if snap, _ := h.comps.Fleet.Snapshot(pool); snap.Status.Empty {
			break
		}
		h.step()
	}
	snap, ok := h.comps.Fleet.Snapshot(pool)
	require.True(t, ok)
	assert.Equal(t, 0, snap.Desired)
	assert.True(t, snap.Status.Empty)
	h.noViolations()

	// UC16 queue timing: the oldest queued action aged through the cold start,
	// and the queue-time p95 is a cold-start sample, not zero.
	assert.Greater(t, maxOldest, 20*time.Second)
	assert.LessOrEqual(t, maxOldest, 90*time.Second)
	assert.Greater(t, qt.Stats()[native].QueueTimeP95, 20*time.Second)

	// R-OBS-5: the recorded launches are priced; the compute cost matches what
	// the (fake) region billed for the same instance-seconds within 10 %.
	usage, err := h.comps.Fleet.Usage(h.ctx)
	require.NoError(t, err)
	require.Len(t, usage.Launches, 2)
	for _, l := range usage.Launches {
		assert.False(t, l.End.IsZero(), "termination recorded")
	}
	store := &memUsage{}
	h.comps.Cost.Store = store
	h.comps.Cost.Export(h.ctx)
	_, billed := h.compute.InstanceSeconds()
	got := testutil.ToFloat64(h.metrics.CostUSD.WithLabelValues(string(pool), "compute"))
	assert.InEpsilon(t, billed[pool], got, 0.1, "compute cost %f vs billed %f", got, billed[pool])
	assert.Greater(t, testutil.ToFloat64(h.metrics.StandingCostUSDPerMonth.WithLabelValues("ami-storage")), 0.0, "the pool's AMI is standing cost (NFR-C1)")

	// A new leader resumes the month's usage from the stored record.
	today, ok := h.comps.Cost.TodayUSD(pool)
	require.True(t, ok)
	next := newHarness(t, 8)
	next.clock.Set(h.clock.Now()) // the new leader takes over now
	next.comps.Cost.Store = store
	next.comps.Cost.Export(next.ctx)
	resumed, ok := next.comps.Cost.TodayUSD(pool)
	require.True(t, ok)
	assert.InDelta(t, today, resumed, 1e-6)
}

// Guards the Tart path of the loop (R-POOL-6 placement spread, R-MAC-3 at most
// 2 VMs per host and persistent VMs, R-SCALE-3 Tart scale-in = stop with the
// disk kept, and restart of the same VM — warm L1 — when work returns).
func TestScenarioTartSpreadStopRestart(t *testing.T) {
	h := newHarness(t, 9)
	h.hosts.OnVMReady(func(_ string, vm domain.VM) {
		h.mu.Lock()
		th := h.threads[vm.Pool]
		h.mu.Unlock()
		h.bq.RegisterNode(vm.Pool, vm.ID, th)
	})
	h.hosts.OnVMStopped(func(_ string, vm domain.VM) { h.bq.RemoveNode(vm.ID) })
	for _, s := range []string{"C02HOSTAAA1", "C02HOSTBBB2"} {
		h.hosts.AddHost(ports.HostState{Serial: s, Name: s, Online: true, Approved: true, Slots: 2})
	}
	wp := &v1alpha1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "cucina"}, Spec: v1alpha1.WorkerPoolSpec{
		Platform: "macos-arm64-xcode27.0", Provider: "tart", SizeClass: "default", Capacity: v1alpha1.CapacitySpec{Max: 3},
		Image: v1alpha1.ImageSpec{Reference: "ghcr.io/sloper-ai/cucina-worker-macos:27.0-1"},
		Tart:  &v1alpha1.TartSpec{VMsPerHost: 2, CPU: ptrInt32(4)},
	}}
	rt := h.addPool(wp)
	xcode := rt.Queues[0] // 4 slots per VM

	submit := func(n int) {
		for range n {
			_, err := h.bq.Submit(xcode, 2*time.Minute, "build")
			require.NoError(t, err)
		}
	}
	running := func() map[string]int {
		hosts, err := h.hosts.Hosts(h.ctx)
		require.NoError(t, err)
		out := map[string]int{}
		for _, hs := range hosts {
			out[hs.Serial] = hs.RunningVMs
			require.LessOrEqual(t, hs.RunningVMs, 2, "at most 2 VMs per host")
		}
		return out
	}

	submit(8) // two VMs' worth: one per host, never both on one host
	for range 90 {
		h.step()
	}
	assert.Equal(t, map[string]int{"C02HOSTAAA1": 1, "C02HOSTBBB2": 1}, running())

	// Idle → stopped, disk kept (the VMs still exist on the hosts).
	for range 15 * 60 {
		h.step()
	}
	assert.Equal(t, map[string]int{"C02HOSTAAA1": 0, "C02HOSTBBB2": 0}, running())
	hosts, err := h.hosts.Hosts(h.ctx)
	require.NoError(t, err)
	var stopped []string
	for _, hs := range hosts {
		for _, vm := range hs.VMs {
			stopped = append(stopped, vm.ID)
		}
	}
	assert.Len(t, stopped, 2, "stopped VMs are kept for their L1 cache")
	assert.InDelta(t, 2.0, testutil.ToFloat64(h.metrics.VMStops.WithLabelValues("macos", "idle")), 0)

	// Work returns: the same VMs start again (no new clones).
	submit(4)
	for range 90 {
		h.step()
	}
	hosts, err = h.hosts.Hosts(h.ctx)
	require.NoError(t, err)
	var all []string
	for _, hs := range hosts {
		for _, vm := range hs.VMs {
			all = append(all, vm.ID)
		}
	}
	assert.ElementsMatch(t, stopped, all, "a stopped VM was restarted instead of cloning a new one")
	for _, op := range h.bq.Ops() {
		assert.False(t, op.Failed, op.Name)
	}
	h.noViolations()
}

func ptrInt32(v int32) *int32 { return &v }
