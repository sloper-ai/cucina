// SPDX-License-Identifier: FSL-1.1-ALv2

// Package lifecycle is the pure core of hostd's VM management (R-MAC-3,
// R-POOL-7): a level-triggered planner over the host's VMs plus the transition
// functions that record what happened. It has no I/O, no goroutines and no
// clock; callers pass `now`. The VM manager (internal/hostd/vmm) executes the
// planned actions against ports.VMRuntime and feeds outcomes back.
//
// Invariants (checked by the property test and kept in production, R-TEST-7):
//   - at most 2 macOS VMs are active (starting, running or stopping) per host,
//     counting VMs hostd does not manage, and never more than the slot count
//     because of a start hostd planned;
//   - no VM is started while the host is cordoned;
//   - a VM is started only from a clone of its desired image and generation that
//     is younger than its maximum age and has not failed repeatedly (otherwise
//     it is re-cloned first);
//   - VMs are persistent: stopping keeps the disk (and the L1 cache);
//   - the dead-man limits (idle, scheduler unreachable, uptime) stop running VMs
//     whether or not the controller is connected.
package lifecycle

import (
	"sort"
	"time"
)

// HardMaxRunning is the Apple licence / Virtualization.framework limit.
const HardMaxRunning = 2

// Phase is hostd's view of one VM.
type Phase string

// Phases. "starting" covers `tart run` through worker start; "running" means the
// VM is configured and its Buildbarn services were started.
const (
	Absent   Phase = "absent"
	Cloning  Phase = "cloning"
	Stopped  Phase = "stopped"
	Starting Phase = "starting"
	Running  Phase = "running"
	Stopping Phase = "stopping"
	Deleting Phase = "deleting"
	Failed   Phase = "failed"
)

// Active reports whether a VM in this phase occupies a Virtualization.framework slot.
func (p Phase) Active() bool { return p == Starting || p == Running || p == Stopping }

// Intent is the desired state, set by controller commands or by the dead-man switch.
type Intent string

// Intents.
const (
	WantRunning Intent = "running"
	WantStopped Intent = "stopped"
	WantDeleted Intent = "deleted"
)

// Stop reasons (match cucina_vm_stops_total{reason} where they overlap).
const (
	ReasonIdle           = "idle"
	ReasonDrain          = "drain"
	ReasonRollout        = "rollout"
	ReasonMaintenance    = "maintenance"
	ReasonDelete         = "delete"
	ReasonDeadmanIdle    = "deadman-idle"
	ReasonDeadmanUptime  = "deadman-uptime"
	ReasonDeadmanNoSched = "deadman-unreachable"
	ReasonRequested      = "requested"
	ReasonReimage        = "reimage"
	ReasonMaxAge         = "max-age"
	ReasonUnhealthy      = "unhealthy"
	ReasonImageChanged   = "image-changed"
)

// VM is one VM's desired and observed state.
type VM struct {
	Name string // controller vm_name (the Tart VM is <prefix><Name>)
	Pool string
	Node string

	// Desired.
	Intent      Intent
	Image       string
	Generation  string
	CPU         int
	MemoryGiB   int
	DiskGiB     int
	MaxAge      time.Duration
	Reimage     bool
	StopTimeout time.Duration
	StopReason  string

	// Observed.
	Phase            Phase
	ClonedImage      string
	ClonedGeneration string
	ClonedAt         time.Time
	StartedAt        time.Time
	LastActive       time.Time // last observed worker activity
	LastUpstreamOK   time.Time // last time the VM's scheduler path worked
	Busy             bool      // worker executing an action at the last probe
	Failures         int       // consecutive start failures / crashes
	RetryAt          time.Time
	LastError        string
}

// Limits are the host-wide policy knobs.
type Limits struct {
	IdleLimit        time.Duration // dead-man idle (default 30m)
	UnreachableLimit time.Duration // dead-man scheduler unreachable (default 10m)
	MaxUptime        time.Duration // dead-man uptime (default 12h)
	DefaultMaxAge    time.Duration // re-clone age (default 7d)
	MaxFailures      int           // consecutive failures before a re-clone (default 2)
	StopTimeout      time.Duration // default `tart stop --timeout` (default 2m)
}

// DefaultLimits are R-POOL-7 / R-MAC-3 defaults.
func DefaultLimits() Limits {
	return Limits{IdleLimit: 30 * time.Minute, UnreachableLimit: 10 * time.Minute, MaxUptime: 12 * time.Hour,
		DefaultMaxAge: 7 * 24 * time.Hour, MaxFailures: 2, StopTimeout: 2 * time.Minute}
}

// Host is the planner input.
type Host struct {
	Slots    int  // VMs per host (1..2)
	Cordoned bool // no new VMs; idle VMs stop
	// ExternalActive counts running VMs on the host that hostd does not manage
	// (they still use Virtualization.framework slots).
	ExternalActive int
	VMs            []VM
}

// Kind is an action kind.
type Kind string

// Action kinds.
const (
	Clone  Kind = "clone"
	Start  Kind = "start"
	Stop   Kind = "stop"
	Delete Kind = "delete"
)

// Action is one planned operation on one VM.
type Action struct {
	Kind    Kind
	VM      string
	Image   string        // clone
	Reason  string        // stop/delete
	Timeout time.Duration // stop
}

// ActiveCount is the number of slot-occupying VMs, including external ones.
func (h Host) ActiveCount() int {
	n := h.ExternalActive
	for _, vm := range h.VMs {
		if vm.Phase.Active() {
			n++
		}
	}
	return n
}

// capacity is how many VMs may be active given slots and the hard limit.
func (h Host) capacity() int {
	c := h.Slots
	if c < 1 {
		c = 1
	}
	if c > HardMaxRunning {
		c = HardMaxRunning
	}
	return c
}

func (l Limits) maxAge(vm VM) time.Duration {
	if vm.MaxAge > 0 {
		return vm.MaxAge
	}
	return l.DefaultMaxAge
}

func (l Limits) stopTimeout(vm VM) time.Duration {
	if vm.StopTimeout > 0 {
		return vm.StopTimeout
	}
	if l.StopTimeout > 0 {
		return l.StopTimeout
	}
	return 2 * time.Minute
}

// NeedsReclone reports why a stopped VM must be re-cloned before it may start
// ("" if it may start as is): image or generation changed, re-image requested,
// maximum age exceeded or repeated failures (R-MAC-3).
func (l Limits) NeedsReclone(vm VM, now time.Time) string {
	switch {
	case vm.Reimage:
		return ReasonReimage
	case vm.ClonedImage != vm.Image || vm.ClonedGeneration != vm.Generation:
		return ReasonImageChanged
	case now.Sub(vm.ClonedAt) >= l.maxAge(vm):
		return ReasonMaxAge
	case l.MaxFailures > 0 && vm.Failures >= l.MaxFailures:
		return ReasonUnhealthy
	}
	return ""
}

// Deadman returns the dead-man reason for stopping a running VM, or "" (R-POOL-7).
func (l Limits) Deadman(vm VM, now time.Time) string {
	if vm.Phase != Running {
		return ""
	}
	switch {
	case l.MaxUptime > 0 && now.Sub(vm.StartedAt) >= l.MaxUptime:
		return ReasonDeadmanUptime
	case l.IdleLimit > 0 && !vm.Busy && now.Sub(vm.LastActive) >= l.IdleLimit:
		return ReasonDeadmanIdle
	case l.UnreachableLimit > 0 && now.Sub(vm.LastUpstreamOK) >= l.UnreachableLimit:
		return ReasonDeadmanNoSched
	}
	return ""
}

// Plan returns the actions that move the host toward the desired state. It is
// deterministic (VMs are visited in name order) and level-triggered: calling it
// again before any outcome is recorded yields no conflicting actions because
// in-flight phases (cloning, starting, stopping, deleting) are left alone.
func Plan(h Host, l Limits, now time.Time) []Action {
	vms := append([]VM(nil), h.VMs...)
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	var out []Action
	active := h.ActiveCount()
	for _, vm := range vms {
		switch vm.Phase {
		case Cloning, Starting, Stopping, Deleting:
			continue // in flight
		}
		if vm.Phase == Running {
			if r := l.Deadman(vm, now); r != "" {
				out = append(out, Action{Kind: Stop, VM: vm.Name, Reason: r, Timeout: l.stopTimeout(vm)})
				continue
			}
			switch {
			case vm.Intent == WantDeleted:
				out = append(out, Action{Kind: Stop, VM: vm.Name, Reason: ReasonDelete, Timeout: l.stopTimeout(vm)})
			case vm.Intent == WantStopped:
				out = append(out, Action{Kind: Stop, VM: vm.Name, Reason: stopReason(vm), Timeout: l.stopTimeout(vm)})
			case h.Cordoned && !vm.Busy:
				out = append(out, Action{Kind: Stop, VM: vm.Name, Reason: ReasonMaintenance, Timeout: l.stopTimeout(vm)})
			}
			continue
		}
		// Absent, Stopped or Failed: nothing runs.
		switch vm.Intent {
		case WantDeleted:
			if vm.Phase != Absent {
				out = append(out, Action{Kind: Delete, VM: vm.Name, Reason: ReasonDelete})
			}
			continue
		case WantStopped:
			continue
		}
		// WantRunning.
		if now.Before(vm.RetryAt) {
			continue
		}
		if vm.Phase == Absent {
			out = append(out, Action{Kind: Clone, VM: vm.Name, Image: vm.Image})
			continue
		}
		if r := l.NeedsReclone(vm, now); r != "" {
			out = append(out, Action{Kind: Delete, VM: vm.Name, Reason: r})
			continue
		}
		if h.Cordoned || active >= h.capacity() {
			continue
		}
		out = append(out, Action{Kind: Start, VM: vm.Name})
		active++
	}
	return out
}

func stopReason(vm VM) string {
	if vm.StopReason != "" {
		return vm.StopReason
	}
	return ReasonRequested
}

// Find returns the index of the named VM or -1.
func (h *Host) Find(name string) int {
	for i := range h.VMs {
		if h.VMs[i].Name == name {
			return i
		}
	}
	return -1
}

// Begin records that an action was started (phases become in-flight). A
// dead-man stop also flips the intent to stopped so the VM is not restarted
// until the controller asks again.
func Begin(h *Host, a Action) {
	i := h.Find(a.VM)
	if i < 0 {
		return
	}
	vm := &h.VMs[i]
	switch a.Kind {
	case Clone:
		vm.Phase = Cloning
	case Start:
		vm.Phase = Starting
	case Stop:
		vm.Phase = Stopping
		switch a.Reason {
		case ReasonDeadmanIdle, ReasonDeadmanUptime, ReasonDeadmanNoSched, ReasonMaintenance:
			vm.Intent = WantStopped
			vm.StopReason = a.Reason
		}
	case Delete:
		vm.Phase = Deleting
	}
}

// Backoff is the retry delay after the n-th consecutive failure (capped at 5 min).
func Backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := 5 * time.Second
	for i := 1; i < n && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// Complete records the outcome of an action begun with Begin.
func Complete(h *Host, a Action, err error, now time.Time) {
	i := h.Find(a.VM)
	if i < 0 {
		return
	}
	vm := &h.VMs[i]
	if err != nil {
		vm.LastError = err.Error()
	}
	switch a.Kind {
	case Clone:
		if err != nil {
			vm.Phase = Absent
			vm.Failures++
			vm.RetryAt = now.Add(Backoff(vm.Failures))
			return
		}
		vm.Phase = Stopped
		vm.ClonedImage, vm.ClonedGeneration, vm.ClonedAt = vm.Image, vm.Generation, now
		vm.Reimage = false
		vm.Failures = 0
		vm.RetryAt = time.Time{}
		vm.LastError = ""
	case Start:
		if err != nil {
			// The VM manager stops a half-started VM before reporting the failure.
			vm.Phase = Failed
			vm.Failures++
			vm.RetryAt = now.Add(Backoff(vm.Failures))
			return
		}
		vm.Phase = Running
		vm.StartedAt, vm.LastActive, vm.LastUpstreamOK = now, now, now
		vm.Busy = false
		vm.Failures = 0
		vm.LastError = ""
	case Stop:
		if err != nil {
			vm.Phase = Running // still running; the next plan retries
			return
		}
		vm.Phase = Stopped
		vm.Busy = false
	case Delete:
		if err != nil {
			vm.Phase = Stopped
			vm.RetryAt = now.Add(Backoff(1))
			return
		}
		if vm.Intent == WantDeleted {
			h.VMs = append(h.VMs[:i], h.VMs[i+1:]...)
			return
		}
		vm.Phase = Absent
		vm.ClonedImage, vm.ClonedGeneration, vm.ClonedAt = "", "", time.Time{}
		vm.Failures = 0
		vm.RetryAt = time.Time{}
	}
}

// Crashed records that a running or starting VM stopped on its own (its
// `tart run` exited or Tart reports it stopped). Crashes count as health
// failures: after Limits.MaxFailures the VM is re-cloned.
func Crashed(h *Host, name, reason string, now time.Time) {
	i := h.Find(name)
	if i < 0 {
		return
	}
	vm := &h.VMs[i]
	if vm.Phase != Running && vm.Phase != Starting {
		return
	}
	vm.Phase = Stopped
	vm.Busy = false
	vm.Failures++
	vm.LastError = reason
	vm.RetryAt = now.Add(Backoff(vm.Failures))
}

// Activity records a worker activity probe: busy=true refreshes LastActive.
func Activity(h *Host, name string, busy bool, now time.Time) {
	if i := h.Find(name); i >= 0 {
		h.VMs[i].Busy = busy
		if busy {
			h.VMs[i].LastActive = now
		}
	}
}

// Active refreshes LastActive (e.g. the worker completed actions since the last probe).
func Active(h *Host, name string, now time.Time) {
	if i := h.Find(name); i >= 0 {
		h.VMs[i].LastActive = now
	}
}

// UpstreamOK records that the VM's scheduler path worked at now.
func UpstreamOK(h *Host, name string, now time.Time) {
	if i := h.Find(name); i >= 0 && now.After(h.VMs[i].LastUpstreamOK) {
		h.VMs[i].LastUpstreamOK = now
	}
}
