// SPDX-License-Identifier: FSL-1.1-ALv2

package fakes

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// ErrHostOffline is returned for commands to a host that is not connected.
var ErrHostOffline = errors.New("host offline")

// HostFleetConfig shapes the simulated Mac fleet.
type HostFleetConfig struct {
	// BootMin/Max: StartVM of an existing VM → worker ready to register.
	BootMin, BootMax time.Duration
	// CloneLatency is added when the VM is (re-)cloned from the golden image.
	CloneLatency time.Duration
	// MaxVMsPerHost is the hard limit (Apple licence: 2).
	MaxVMsPerHost int
}

// DefaultHostFleetConfig: macOS VM boot ≈ 25–40 s (NFR-P1 target ≤ 45 s p50).
func DefaultHostFleetConfig() HostFleetConfig {
	return HostFleetConfig{BootMin: 25 * time.Second, BootMax: 40 * time.Second, CloneLatency: 10 * time.Second, MaxVMsPerHost: 2}
}

type fHostVM struct {
	name, pool, generation, image string
	running, ready                bool
	startedAt, readyAt, clonedAt  time.Time
}

type fHost struct {
	state  ports.HostState
	vms    map[string]*fHostVM
	images map[string]bool
}

// HostFleet is a fake fleet of Mac hosts (the controller side of the hostd
// stream) implementing ports.HostFleet: VM start/stop latency, the 2-VM cap,
// offline hosts, cordons, re-clone on generation change.
type HostFleet struct {
	*Faults
	mu        sync.Mutex
	clock     ports.Clock
	rnd       *Rand
	cfg       HostFleetConfig
	hosts     map[string]*fHost
	onReady   []func(host string, vm domain.VM)
	onStopped []func(host string, vm domain.VM)
	events    []func()
}

var _ ports.HostFleet = (*HostFleet)(nil)

// NewHostFleet returns a fleet without hosts.
func NewHostFleet(clock ports.Clock, rnd *Rand, cfg HostFleetConfig) *HostFleet {
	if cfg.MaxVMsPerHost <= 0 {
		cfg.MaxVMsPerHost = 2
	}
	return &HostFleet{Faults: newFaults(clock, rnd.Child("hostfleet-faults")), clock: clock, rnd: rnd.Child("hostfleet"), cfg: cfg,
		hosts: map[string]*fHost{}}
}

// AddHost adds (or replaces) a host. Slots defaults to 2.
func (f *HostFleet) AddHost(h ports.HostState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h.Slots <= 0 {
		h.Slots = 2
	}
	h.Labels = maps.Clone(h.Labels)
	h.VMs, h.RunningVMs = nil, 0
	f.hosts[h.Serial] = &fHost{state: h, vms: map[string]*fHostVM{}, images: map[string]bool{}}
}

// OnVMReady registers a hook called when a started VM's worker can register.
func (f *HostFleet) OnVMReady(h func(host string, vm domain.VM)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onReady = append(f.onReady, h)
}

// OnVMStopped registers a hook called when a VM stops or becomes unreachable.
func (f *HostFleet) OnVMStopped(h func(host string, vm domain.VM)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onStopped = append(f.onStopped, h)
}

// SetOnline connects or disconnects a host. A disconnected host's VMs keep
// running but their workers lose the scheduler (OnVMStopped); on reconnect
// running VMs register again.
func (f *HostFleet) SetOnline(serial string, online bool) {
	f.mu.Lock()
	h := f.hosts[serial]
	if h == nil || h.state.Online == online {
		f.mu.Unlock()
		return
	}
	now := f.clock.Now()
	f.advanceLocked(now)
	h.state.Online = online
	for _, name := range sortedKeys(h.vms) {
		vm := h.vms[name]
		if !vm.running {
			continue
		}
		if online {
			vm.ready, vm.readyAt = false, now
		} else if vm.ready {
			f.fire(f.onStopped, h, vm)
			vm.ready = false
		}
	}
	f.advanceLocked(now)
	ev := f.takeEvents()
	f.mu.Unlock()
	runEvents(ev)
}

// Reboot stops every VM of a host (power loss, macOS update).
func (f *HostFleet) Reboot(serial string) {
	f.mu.Lock()
	if h := f.hosts[serial]; h != nil {
		for _, name := range sortedKeys(h.vms) {
			f.stopLocked(h, h.vms[name])
		}
	}
	ev := f.takeEvents()
	f.mu.Unlock()
	runEvents(ev)
}

// CrashVM stops one VM unexpectedly.
func (f *HostFleet) CrashVM(serial, vm string) {
	f.mu.Lock()
	if h := f.hosts[serial]; h != nil && h.vms[vm] != nil {
		f.stopLocked(h, h.vms[vm])
	}
	ev := f.takeEvents()
	f.mu.Unlock()
	runEvents(ev)
}

// Tick advances the fleet to the clock's time and fires due hooks.
func (f *HostFleet) Tick() {
	f.mu.Lock()
	f.advanceLocked(f.clock.Now())
	ev := f.takeEvents()
	f.mu.Unlock()
	runEvents(ev)
}

func (f *HostFleet) advanceLocked(now time.Time) {
	for _, serial := range sortedKeys(f.hosts) {
		h := f.hosts[serial]
		if !h.state.Online {
			continue
		}
		for _, name := range sortedKeys(h.vms) {
			vm := h.vms[name]
			if vm.running && !vm.ready && !now.Before(vm.readyAt) {
				vm.ready = true
				f.fire(f.onReady, h, vm)
			}
		}
	}
}

func (f *HostFleet) fire(hooks []func(string, domain.VM), h *fHost, vm *fHostVM) {
	v := f.view(h, vm)
	serial := h.state.Serial
	for _, hk := range hooks {
		hk := hk
		f.events = append(f.events, func() { hk(serial, v) })
	}
}

func (f *HostFleet) stopLocked(h *fHost, vm *fHostVM) {
	if !vm.running {
		return
	}
	if vm.ready {
		f.fire(f.onStopped, h, vm)
	}
	vm.running, vm.ready = false, false
}

func (f *HostFleet) takeEvents() []func() {
	ev := f.events
	f.events = nil
	return ev
}

func (f *HostFleet) view(h *fHost, vm *fHostVM) domain.VM {
	st := domain.VMStopped
	switch {
	case vm.running && vm.ready:
		st = domain.VMRegistered
	case vm.running:
		st = domain.VMLaunching
	}
	if !h.state.Online && vm.running {
		st = domain.VMUnavailable
	}
	return domain.VM{ID: h.state.Serial + "/" + vm.name, Pool: domain.PoolName(vm.pool), Generation: vm.generation, State: st,
		LaunchedAt: vm.startedAt, Host: h.state.Serial}
}

func (h *fHost) running() int {
	n := 0
	for _, vm := range h.vms {
		if vm.running {
			n++
		}
	}
	return n
}

// ------------------------------------------------------------ port methods

func (f *HostFleet) host(serial string) (*fHost, error) {
	h := f.hosts[serial]
	if h == nil {
		return nil, fmt.Errorf("host %s: %w", serial, ports.ErrNotFound)
	}
	if !h.state.Online {
		return nil, fmt.Errorf("host %s: %w", serial, ErrHostOffline)
	}
	return h, nil
}

// Hosts implements ports.HostFleet.
func (f *HostFleet) Hosts(ctx context.Context) ([]ports.HostState, error) {
	if err := f.enter(ctx, "Hosts"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	now := f.clock.Now()
	f.advanceLocked(now)
	out := make([]ports.HostState, 0, len(f.hosts))
	for _, serial := range sortedKeys(f.hosts) {
		h := f.hosts[serial]
		st := h.state
		st.Labels = maps.Clone(st.Labels)
		st.RunningVMs = h.running()
		st.VMs = nil
		for _, name := range sortedKeys(h.vms) {
			st.VMs = append(st.VMs, f.view(h, h.vms[name]))
		}
		st.Images = slices.Sorted(maps.Keys(h.images))
		if st.Online {
			st.LastSeen = now
			h.state.LastSeen = now
		}
		out = append(out, st)
	}
	ev := f.takeEvents()
	f.mu.Unlock()
	runEvents(ev)
	return out, nil
}

// StartVM implements ports.HostFleet. Starting a running VM is a no-op; a
// stopped VM of another generation/image, or older than MaxAge, is re-cloned.
func (f *HostFleet) StartVM(ctx context.Context, serial string, req ports.StartVMRequest) error {
	if err := f.enter(ctx, "StartVM"); err != nil {
		return err
	}
	f.mu.Lock()
	defer func() {
		ev := f.takeEvents()
		f.mu.Unlock()
		runEvents(ev)
	}()
	now := f.clock.Now()
	f.advanceLocked(now)
	h, err := f.host(serial)
	if err != nil {
		return err
	}
	if h.state.Cordoned || !h.state.Approved {
		return fmt.Errorf("host %s is cordoned or not approved: %w", serial, ports.ErrInvalid)
	}
	if req.VMName == "" || req.Pool == "" {
		return fmt.Errorf("start vm needs a name and a pool: %w", ports.ErrInvalid)
	}
	vm := h.vms[req.VMName]
	if vm != nil && vm.running {
		return nil
	}
	if h.running() >= min(h.state.Slots, f.cfg.MaxVMsPerHost) {
		return fmt.Errorf("host %s: %w", serial, ports.ErrVMLimit)
	}
	boot := f.rnd.Duration(f.cfg.BootMin, f.cfg.BootMax)
	reclone := vm == nil || vm.generation != req.Generation || vm.image != req.Image ||
		(req.MaxAge > 0 && now.Sub(vm.clonedAt) > req.MaxAge)
	if reclone {
		if vm == nil {
			vm = &fHostVM{name: req.VMName}
			h.vms[req.VMName] = vm
		}
		vm.pool, vm.generation, vm.image, vm.clonedAt = string(req.Pool), req.Generation, req.Image, now
		h.images[req.Image] = true
		boot += f.cfg.CloneLatency
	}
	vm.running, vm.ready, vm.startedAt, vm.readyAt = true, false, now, now.Add(boot)
	return nil
}

// StopVM implements ports.HostFleet.
func (f *HostFleet) StopVM(ctx context.Context, serial, vmName string, timeout time.Duration, reason string) error {
	if err := f.enter(ctx, "StopVM"); err != nil {
		return err
	}
	f.mu.Lock()
	defer func() {
		ev := f.takeEvents()
		f.mu.Unlock()
		runEvents(ev)
	}()
	h, err := f.host(serial)
	if err != nil {
		return err
	}
	vm := h.vms[vmName]
	if vm == nil {
		return fmt.Errorf("vm %s/%s: %w", serial, vmName, ports.ErrVMNotFound)
	}
	f.stopLocked(h, vm)
	return nil
}

// DeleteVM implements ports.HostFleet (the VM must be stopped).
func (f *HostFleet) DeleteVM(ctx context.Context, serial, vmName string) error {
	if err := f.enter(ctx, "DeleteVM"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.host(serial)
	if err != nil {
		return err
	}
	vm := h.vms[vmName]
	if vm == nil {
		return fmt.Errorf("vm %s/%s: %w", serial, vmName, ports.ErrVMNotFound)
	}
	if vm.running {
		return fmt.Errorf("vm %s/%s is running: %w", serial, vmName, ports.ErrInvalid)
	}
	delete(h.vms, vmName)
	return nil
}

// ReimageVM implements ports.HostFleet: the next start clones image afresh.
func (f *HostFleet) ReimageVM(ctx context.Context, serial, vmName, image string) error {
	if err := f.enter(ctx, "ReimageVM"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.host(serial)
	if err != nil {
		return err
	}
	vm := h.vms[vmName]
	if vm == nil {
		return fmt.Errorf("vm %s/%s: %w", serial, vmName, ports.ErrVMNotFound)
	}
	if vm.running {
		return fmt.Errorf("vm %s/%s is running: %w", serial, vmName, ports.ErrInvalid)
	}
	vm.image, vm.clonedAt = image, f.clock.Now()
	h.images[image] = true
	return nil
}

// PullImage implements ports.HostFleet.
func (f *HostFleet) PullImage(ctx context.Context, serial, image string) error {
	if err := f.enter(ctx, "PullImage"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.host(serial)
	if err != nil {
		return err
	}
	h.images[image] = true
	return nil
}

// SetCordon implements ports.HostFleet.
func (f *HostFleet) SetCordon(ctx context.Context, serial string, cordoned bool) error {
	if err := f.enter(ctx, "SetCordon"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[serial]
	if h == nil {
		return fmt.Errorf("host %s: %w", serial, ports.ErrNotFound)
	}
	h.state.Cordoned = cordoned
	return nil
}
