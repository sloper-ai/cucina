// SPDX-License-Identifier: FSL-1.1-ALv2

package lifecycle_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/internal/hostd/lifecycle"
)

var t0 = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

// model drives the lifecycle core the way the VM manager does: plan, begin
// every action, complete them later in any order with any outcome.
type model struct {
	h       lifecycle.Host
	l       lifecycle.Limits
	now     time.Time
	pending []lifecycle.Action
}

func (m *model) checkPlan(t *rapid.T, plan []lifecycle.Action) {
	seen := map[string]bool{}
	starts := 0
	for _, a := range plan {
		if seen[a.VM] {
			t.Fatalf("two actions for %s in one plan: %v", a.VM, plan)
		}
		seen[a.VM] = true
		i := m.h.Find(a.VM)
		if i < 0 {
			t.Fatalf("action for unknown vm %v", a)
		}
		vm := m.h.VMs[i]
		switch vm.Phase {
		case lifecycle.Cloning, lifecycle.Starting, lifecycle.Stopping, lifecycle.Deleting:
			t.Fatalf("action %v for in-flight vm in phase %s", a, vm.Phase)
		}
		if a.Kind != lifecycle.Start {
			continue
		}
		starts++
		slots := m.h.Slots
		if slots > lifecycle.HardMaxRunning {
			slots = lifecycle.HardMaxRunning
		}
		if m.h.ActiveCount()+starts > slots {
			t.Fatalf("start of %s exceeds capacity: active=%d starts=%d slots=%d", a.VM, m.h.ActiveCount(), starts, m.h.Slots)
		}
		if m.h.Cordoned {
			t.Fatalf("start of %s while cordoned", a.VM)
		}
		if vm.Intent != lifecycle.WantRunning {
			t.Fatalf("start of %s with intent %s", a.VM, vm.Intent)
		}
		if r := m.l.NeedsReclone(vm, m.now); r != "" {
			t.Fatalf("start of %s that needs a re-clone (%s): %+v", a.VM, r, vm)
		}
	}
	// Dead-man: every running VM over a limit is stopped in this plan (R-POOL-7).
	for _, vm := range m.h.VMs {
		if r := m.l.Deadman(vm, m.now); r != "" {
			found := false
			for _, a := range plan {
				if a.VM == vm.Name && a.Kind == lifecycle.Stop {
					found = true
				}
			}
			if !found {
				t.Fatalf("vm %s qualifies for dead-man stop (%s) but plan is %v", vm.Name, r, plan)
			}
		}
	}
}

func (m *model) checkInvariants(t *rapid.T) {
	managed := 0
	for _, vm := range m.h.VMs {
		if vm.Phase.Active() {
			managed++
		}
	}
	if managed > lifecycle.HardMaxRunning {
		t.Fatalf("%d managed VMs active (hard limit %d)", managed, lifecycle.HardMaxRunning)
	}
}

var names = []string{"a", "b", "c", "d"}

// TestLifecycleProperties is a rapid state-machine test of the VM lifecycle
// (R-MAC-3, R-POOL-7, R-TEST-6 "VM lifecycle state machine incl. the 2-VM cap"):
// arbitrary interleavings of controller commands, cordons, slot changes,
// external VMs, action outcomes (incl. failures), crashes, activity and time
// never start more VMs than the slots allow (never more than 2), never start
// while cordoned or from a stale/old/unhealthy clone, and always stop VMs that
// trip a dead-man limit.
func TestLifecycleProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := &model{h: lifecycle.Host{Slots: 2}, l: lifecycle.DefaultLimits(), now: t0}
		t.Repeat(map[string]func(*rapid.T){
			"start": func(t *rapid.T) {
				name := rapid.SampledFrom(names).Draw(t, "name")
				lifecycle.RequestStart(&m.h, lifecycle.StartRequest{
					Name: name, Pool: "macos", Node: "host/" + name,
					Image:      rapid.SampledFrom([]string{"img:1", "img:2"}).Draw(t, "image"),
					Generation: rapid.SampledFrom([]string{"g1", "g2"}).Draw(t, "gen"),
				})
			},
			"stop": func(t *rapid.T) {
				_ = lifecycle.RequestStop(&m.h, rapid.SampledFrom(names).Draw(t, "name"), time.Minute, lifecycle.ReasonIdle)
			},
			"delete": func(t *rapid.T) { _ = lifecycle.RequestDelete(&m.h, rapid.SampledFrom(names).Draw(t, "name")) },
			"reimage": func(t *rapid.T) {
				_ = lifecycle.RequestReimage(&m.h, rapid.SampledFrom(names).Draw(t, "name"), "")
			},
			"cordon": func(t *rapid.T) { m.h.Cordoned = rapid.Bool().Draw(t, "cordoned") },
			"slots":  func(t *rapid.T) { m.h.Slots = rapid.IntRange(1, 2).Draw(t, "slots") },
			"external": func(t *rapid.T) {
				// Another (unmanaged) VM may only appear if Virtualization.framework had room.
				if m.h.ActiveCount() < lifecycle.HardMaxRunning {
					m.h.ExternalActive = rapid.IntRange(0, 1).Draw(t, "external")
				} else {
					m.h.ExternalActive = 0
				}
			},
			"plan": func(t *rapid.T) {
				plan := lifecycle.Plan(m.h, m.l, m.now)
				m.checkPlan(t, plan)
				for _, a := range plan {
					lifecycle.Begin(&m.h, a)
					m.pending = append(m.pending, a)
				}
			},
			"complete": func(t *rapid.T) {
				if len(m.pending) == 0 {
					t.Skip("nothing pending")
				}
				i := rapid.IntRange(0, len(m.pending)-1).Draw(t, "i")
				a := m.pending[i]
				m.pending = append(m.pending[:i], m.pending[i+1:]...)
				var err error
				if rapid.IntRange(0, 4).Draw(t, "fail") == 0 {
					err = errors.New("injected")
				}
				lifecycle.Complete(&m.h, a, err, m.now)
			},
			"crash": func(t *rapid.T) {
				lifecycle.Crashed(&m.h, rapid.SampledFrom(names).Draw(t, "name"), "crashed", m.now)
			},
			"activity": func(t *rapid.T) {
				lifecycle.Activity(&m.h, rapid.SampledFrom(names).Draw(t, "name"), rapid.Bool().Draw(t, "busy"), m.now)
			},
			"upstream": func(t *rapid.T) { lifecycle.UpstreamOK(&m.h, rapid.SampledFrom(names).Draw(t, "name"), m.now) },
			"tick": func(t *rapid.T) {
				m.now = m.now.Add(time.Duration(rapid.Int64Range(1, int64(3*time.Hour)).Draw(t, "dt")))
			},
			"": func(t *rapid.T) { m.checkInvariants(t) },
		})
	})
}

// TestReclone guards the R-MAC-3 re-clone triggers: image/generation change,
// explicit re-image, maximum age (default 7 days) and repeated failures.
func TestReclone(t *testing.T) {
	l := lifecycle.DefaultLimits()
	fresh := lifecycle.VM{Name: "a", Intent: lifecycle.WantRunning, Phase: lifecycle.Stopped,
		Image: "img:2", Generation: "g2", ClonedImage: "img:2", ClonedGeneration: "g2", ClonedAt: t0}
	tests := []struct {
		name string
		mut  func(vm *lifecycle.VM)
		now  time.Time
		want lifecycle.Action
	}{
		{"fresh clone starts", func(*lifecycle.VM) {}, t0.Add(time.Hour), lifecycle.Action{Kind: lifecycle.Start, VM: "a"}},
		{"new image", func(vm *lifecycle.VM) { vm.Image = "img:3" }, t0, lifecycle.Action{Kind: lifecycle.Delete, VM: "a", Reason: lifecycle.ReasonImageChanged}},
		{"new generation", func(vm *lifecycle.VM) { vm.Generation = "g3" }, t0, lifecycle.Action{Kind: lifecycle.Delete, VM: "a", Reason: lifecycle.ReasonImageChanged}},
		{"reimage", func(vm *lifecycle.VM) { vm.Reimage = true }, t0, lifecycle.Action{Kind: lifecycle.Delete, VM: "a", Reason: lifecycle.ReasonReimage}},
		{"older than 7 days", func(*lifecycle.VM) {}, t0.Add(7 * 24 * time.Hour), lifecycle.Action{Kind: lifecycle.Delete, VM: "a", Reason: lifecycle.ReasonMaxAge}},
		{"pool max age", func(vm *lifecycle.VM) { vm.MaxAge = time.Hour }, t0.Add(time.Hour), lifecycle.Action{Kind: lifecycle.Delete, VM: "a", Reason: lifecycle.ReasonMaxAge}},
		{"failed twice", func(vm *lifecycle.VM) { vm.Failures = 2 }, t0, lifecycle.Action{Kind: lifecycle.Delete, VM: "a", Reason: lifecycle.ReasonUnhealthy}},
		{"absent clones", func(vm *lifecycle.VM) { vm.Phase = lifecycle.Absent }, t0, lifecycle.Action{Kind: lifecycle.Clone, VM: "a", Image: "img:2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vm := fresh
			tc.mut(&vm)
			require.Equal(t, []lifecycle.Action{tc.want}, lifecycle.Plan(lifecycle.Host{Slots: 2, VMs: []lifecycle.VM{vm}}, l, tc.now))
		})
	}
}

// Guards: real VZ startup failure regression — successful delete/clone recovery
// must not reset launch backoff into an endless five/ten-second retry cycle.
func TestStartupBackoffSurvivesReclone(t *testing.T) {
	limits := lifecycle.DefaultLimits()
	h := lifecycle.Host{Slots: 1}
	request := lifecycle.StartRequest{Name: "a", Pool: "macos", Image: "image:1", Generation: "g1"}
	lifecycle.RequestStart(&h, request)
	now := t0
	start := func() lifecycle.Action {
		t.Helper()
		for range 3 { // at most delete, clone, start
			plan := lifecycle.Plan(h, limits, now)
			require.Len(t, plan, 1)
			lifecycle.Begin(&h, plan[0])
			if plan[0].Kind == lifecycle.Start {
				return plan[0]
			}
			lifecycle.Complete(&h, plan[0], nil, now)
		}
		t.Fatal("recovery did not produce a start")
		return lifecycle.Action{}
	}
	for _, delay := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute} {
		a := start()
		lifecycle.Complete(&h, a, errors.New("hypervisor rejected startup"), now)
		lifecycle.RequestStart(&h, request) // an unchanged desired state is not an operator retry
		require.Empty(t, lifecycle.Plan(h, limits, now.Add(delay-time.Nanosecond)), "failed starts must keep their exponential delay across successful reclones")
		now = now.Add(delay)
	}
	// Only a genuinely healthy start resets the failure streak.
	a := start()
	lifecycle.Complete(&h, a, nil, now)
	lifecycle.Crashed(&h, "a", "later crash", now)
	require.Empty(t, lifecycle.Plan(h, limits, now.Add(5*time.Second-time.Nanosecond)))
	require.NotEmpty(t, lifecycle.Plan(h, limits, now.Add(5*time.Second)))
}

// TestDeadman guards R-POOL-7 for Mac VMs: idle 30 min, scheduler unreachable
// 10 min and uptime 12 h stop a running VM, independent of the controller.
func TestDeadman(t *testing.T) {
	l := lifecycle.DefaultLimits()
	running := lifecycle.VM{Name: "a", Intent: lifecycle.WantRunning, Phase: lifecycle.Running,
		StartedAt: t0, LastActive: t0, LastUpstreamOK: t0}
	tests := []struct {
		name string
		mut  func(vm *lifecycle.VM)
		at   time.Duration
		want string
	}{
		{"recently active", func(vm *lifecycle.VM) { vm.LastUpstreamOK = t0.Add(25 * time.Minute) }, 29 * time.Minute, ""},
		{"idle 30m", func(vm *lifecycle.VM) { vm.LastUpstreamOK = t0.Add(25 * time.Minute) }, 30 * time.Minute, lifecycle.ReasonDeadmanIdle},
		{"busy is never idle", func(vm *lifecycle.VM) { vm.Busy = true; vm.LastUpstreamOK = t0.Add(time.Hour) }, time.Hour, ""},
		{"scheduler unreachable 10m", func(vm *lifecycle.VM) { vm.Busy = true }, 10 * time.Minute, lifecycle.ReasonDeadmanNoSched},
		{"uptime 12h even when busy", func(vm *lifecycle.VM) {
			vm.Busy = true
			vm.LastUpstreamOK = t0.Add(12 * time.Hour)
		}, 12 * time.Hour, lifecycle.ReasonDeadmanUptime},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vm := running
			tc.mut(&vm)
			require.Equal(t, tc.want, l.Deadman(vm, t0.Add(tc.at)))
			h := lifecycle.Host{Slots: 2, VMs: []lifecycle.VM{vm}}
			plan := lifecycle.Plan(h, l, t0.Add(tc.at))
			if tc.want == "" {
				require.Empty(t, plan)
				return
			}
			require.Equal(t, []lifecycle.Action{{Kind: lifecycle.Stop, VM: "a", Reason: tc.want, Timeout: l.StopTimeout}}, plan)
			lifecycle.Begin(&h, plan[0])
			lifecycle.Complete(&h, plan[0], nil, t0.Add(tc.at))
			require.Empty(t, lifecycle.Plan(h, l, t0.Add(tc.at+time.Hour)), "a dead-man stop is not undone without a new StartVM")
		})
	}
}

// TestSize guards the R-MAC-3 sizing formula: (cores−2)/slots vCPUs and
// (RAM−8 GiB)/slots memory per VM, overridable, never exceeding the host.
func TestSize(t *testing.T) {
	tests := []struct {
		c                     lifecycle.HostCapacity
		slots, rc, rm, oc, om int
		wantCPU, wantMem      int
	}{
		{lifecycle.HostCapacity{Cores: 16, MemoryGiB: 48}, 2, 0, 0, 0, 0, 7, 20},    // dev Mac M4 Max
		{lifecycle.HostCapacity{Cores: 18, MemoryGiB: 64}, 2, 0, 0, 0, 0, 8, 28},    // Mac mini M5 Pro (guide)
		{lifecycle.HostCapacity{Cores: 16, MemoryGiB: 48}, 1, 0, 0, 0, 0, 14, 40},   // one slot
		{lifecycle.HostCapacity{Cores: 16, MemoryGiB: 48}, 2, 0, 0, 6, 16, 6, 16},   // preference/controller override
		{lifecycle.HostCapacity{Cores: 16, MemoryGiB: 48}, 2, 4, 12, 6, 16, 4, 12},  // pool request wins
		{lifecycle.HostCapacity{Cores: 16, MemoryGiB: 48}, 2, 64, 64, 0, 0, 16, 20}, // capped at the host
		{lifecycle.HostCapacity{Cores: 4, MemoryGiB: 8}, 2, 0, 0, 0, 0, 1, 4},       // tiny host floors
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%dc-%dg-%ds", tc.c.Cores, tc.c.MemoryGiB, tc.slots), func(t *testing.T) {
			cpu, mem := lifecycle.Size(tc.c, tc.slots, tc.rc, tc.rm, tc.oc, tc.om)
			require.Equal(t, tc.wantCPU, cpu)
			require.Equal(t, tc.wantMem, mem)
		})
	}
}
