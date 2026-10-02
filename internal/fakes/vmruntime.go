// SPDX-License-Identifier: FSL-1.1-ALv2

package fakes

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// VMRuntimeConfig shapes one simulated Mac host running Tart.
type VMRuntimeConfig struct {
	// MaxRunning is the concurrent-VM limit (Apple licence: 2).
	MaxRunning int
	// DiskBytes is the host's free disk for images and VM disks.
	DiskBytes uint64
	// ImageBytes is the size of a pulled image; VMDiskBytes the space a clone takes.
	ImageBytes, VMDiskBytes uint64
	// BootLatency: Run → IP available; GuestAgentDelay: IP → guest agent answers.
	BootLatency, GuestAgentDelay time.Duration
	// GuestAgentFlakiness is the probability that a GuestExec fails with ErrGuestAgent.
	GuestAgentFlakiness float64
}

// DefaultVMRuntimeConfig is a 1 TB Mac mini with ~30 GB images.
func DefaultVMRuntimeConfig() VMRuntimeConfig {
	return VMRuntimeConfig{MaxRunning: 2, DiskBytes: 1 << 40, ImageBytes: 30 << 30, VMDiskBytes: 8 << 30,
		BootLatency: 20 * time.Second, GuestAgentDelay: 5 * time.Second}
}

// GuestHandler answers GuestExec calls.
type GuestHandler func(vm string, c ports.Command) (ports.ExecResult, error)

type fVM struct {
	vm        ports.TartVM
	startedAt time.Time
	ip        netip.Addr
}

// VMRuntime is a fake `tart` on one host implementing ports.VMRuntime.
type VMRuntime struct {
	*Faults
	mu      sync.Mutex
	clock   ports.Clock
	rnd     *Rand
	cfg     VMRuntimeConfig
	vms     map[string]*fVM
	images  map[string]ports.ImageInfo
	used    uint64
	handler GuestHandler
	ipSeq   byte
	pulls   []string
}

var _ ports.VMRuntime = (*VMRuntime)(nil)

// NewVMRuntime returns a host with no images and no VMs.
func NewVMRuntime(clock ports.Clock, rnd *Rand, cfg VMRuntimeConfig) *VMRuntime {
	if cfg.MaxRunning <= 0 {
		cfg.MaxRunning = 2
	}
	return &VMRuntime{Faults: newFaults(clock, rnd.Child("vmruntime-faults")), clock: clock, rnd: rnd.Child("vmruntime"), cfg: cfg,
		vms: map[string]*fVM{}, images: map[string]ports.ImageInfo{},
		handler: func(string, ports.Command) (ports.ExecResult, error) { return ports.ExecResult{}, nil }}
}

// SetGuestHandler sets the GuestExec responder.
func (r *VMRuntime) SetGuestHandler(h GuestHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handler = h
}

// SetFreeDisk changes the host's disk budget (e.g. 0 to simulate a full disk).
func (r *VMRuntime) SetFreeDisk(bytes uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.DiskBytes = bytes
}

// Crash stops a running VM unexpectedly (guest panic, host OOM).
func (r *VMRuntime) Crash(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[name]
	if !ok || v.vm.State != ports.TartRunning {
		return ports.ErrVMNotFound
	}
	v.vm.State = ports.TartStopped
	return nil
}

// Running returns the number of running VMs (ground truth for AtMostTwoVMsPerHost).
func (r *VMRuntime) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runningLocked()
}

func (r *VMRuntime) runningLocked() int {
	n := 0
	for _, v := range r.vms {
		if v.vm.State == ports.TartRunning {
			n++
		}
	}
	return n
}

// List implements ports.VMRuntime.
func (r *VMRuntime) List(ctx context.Context) ([]ports.TartVM, error) {
	if err := r.enter(ctx, "List"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ports.TartVM, 0, len(r.vms))
	for _, name := range sortedKeys(r.vms) {
		out = append(out, r.vms[name].vm)
	}
	return out, nil
}

// Clone implements ports.VMRuntime (pulling the image if it is not local, like tart clone).
func (r *VMRuntime) Clone(ctx context.Context, image, name string, diskGiB int) error {
	if err := r.enter(ctx, "Clone"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.vms[name]; ok {
		return fmt.Errorf("vm %s: %w", name, ports.ErrVMExists)
	}
	if _, ok := r.images[image]; !ok {
		if err := r.pullLocked(image); err != nil {
			return err
		}
	}
	if r.used+r.cfg.VMDiskBytes > r.cfg.DiskBytes {
		return fmt.Errorf("clone %s: %w", name, ports.ErrDiskFull)
	}
	r.used += r.cfg.VMDiskBytes
	r.vms[name] = &fVM{vm: ports.TartVM{Name: name, Image: image, State: ports.TartStopped, DiskGiB: diskGiB,
		SizeOnDiskBytes: r.cfg.VMDiskBytes, CPU: 4, MemoryMiB: 8192}}
	return nil
}

// Run implements ports.VMRuntime.
func (r *VMRuntime) Run(ctx context.Context, name string, opts ports.RunOptions) error {
	if err := r.enter(ctx, "Run"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[name]
	if !ok {
		return fmt.Errorf("vm %s: %w", name, ports.ErrVMNotFound)
	}
	if v.vm.State == ports.TartRunning {
		return nil
	}
	if r.runningLocked() >= r.cfg.MaxRunning {
		return ports.ErrVMLimit
	}
	if opts.CPU > 0 {
		v.vm.CPU = opts.CPU
	}
	if opts.MemoryMiB > 0 {
		v.vm.MemoryMiB = opts.MemoryMiB
	}
	r.ipSeq++
	v.vm.State, v.startedAt, v.ip = ports.TartRunning, r.clock.Now(), netip.AddrFrom4([4]byte{192, 168, 64, 1 + r.ipSeq})
	return nil
}

// Stop implements ports.VMRuntime.
func (r *VMRuntime) Stop(ctx context.Context, name string, timeout time.Duration) error {
	if err := r.enter(ctx, "Stop"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[name]
	if !ok {
		return fmt.Errorf("vm %s: %w", name, ports.ErrVMNotFound)
	}
	v.vm.State = ports.TartStopped
	return nil
}

// Delete implements ports.VMRuntime (a running VM must be stopped first).
func (r *VMRuntime) Delete(ctx context.Context, name string) error {
	if err := r.enter(ctx, "Delete"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.vms[name]
	if !ok {
		return fmt.Errorf("vm %s: %w", name, ports.ErrVMNotFound)
	}
	if v.vm.State == ports.TartRunning {
		return fmt.Errorf("vm %s is running: %w", name, ports.ErrInvalid)
	}
	r.used -= min(r.used, v.vm.SizeOnDiskBytes)
	delete(r.vms, name)
	return nil
}

// IP implements ports.VMRuntime: it waits (on the clock) up to wait for the address.
func (r *VMRuntime) IP(ctx context.Context, name string, wait time.Duration) (netip.Addr, error) {
	if err := r.enter(ctx, "IP"); err != nil {
		return netip.Addr{}, err
	}
	r.mu.Lock()
	v, ok := r.vms[name]
	if !ok {
		r.mu.Unlock()
		return netip.Addr{}, fmt.Errorf("vm %s: %w", name, ports.ErrVMNotFound)
	}
	if v.vm.State != ports.TartRunning {
		r.mu.Unlock()
		return netip.Addr{}, fmt.Errorf("vm %s is not running: %w", name, ports.ErrInvalid)
	}
	readyAt := v.startedAt.Add(r.cfg.BootLatency)
	now := r.clock.Now()
	ip := v.ip
	r.mu.Unlock()
	if now.Before(readyAt) {
		if readyAt.Sub(now) > wait {
			if err := r.clock.Sleep(ctx, wait); err != nil {
				return netip.Addr{}, err
			}
			return netip.Addr{}, fmt.Errorf("vm %s: no IP after %s: %w", name, wait, ports.ErrGuestAgent)
		}
		if err := r.clock.Sleep(ctx, readyAt.Sub(now)); err != nil {
			return netip.Addr{}, err
		}
	}
	return ip, nil
}

// GuestExec implements ports.VMRuntime.
func (r *VMRuntime) GuestExec(ctx context.Context, name string, c ports.Command) (ports.ExecResult, error) {
	if err := r.enter(ctx, "GuestExec"); err != nil {
		return ports.ExecResult{}, err
	}
	r.mu.Lock()
	v, ok := r.vms[name]
	if !ok {
		r.mu.Unlock()
		return ports.ExecResult{}, fmt.Errorf("vm %s: %w", name, ports.ErrVMNotFound)
	}
	now := r.clock.Now()
	if v.vm.State != ports.TartRunning || now.Before(v.startedAt.Add(r.cfg.BootLatency+r.cfg.GuestAgentDelay)) ||
		(r.cfg.GuestAgentFlakiness > 0 && r.rnd.Float64() < r.cfg.GuestAgentFlakiness) {
		r.mu.Unlock()
		return ports.ExecResult{}, fmt.Errorf("vm %s: %w", name, ports.ErrGuestAgent)
	}
	h := r.handler
	r.mu.Unlock()
	return h(name, c)
}

// Pull implements ports.VMRuntime.
func (r *VMRuntime) Pull(ctx context.Context, image string, creds *ports.RegistryCreds) error {
	if err := r.enter(ctx, "Pull"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.images[image]; ok {
		return nil
	}
	return r.pullLocked(image)
}

func (r *VMRuntime) pullLocked(image string) error {
	if r.used+r.cfg.ImageBytes > r.cfg.DiskBytes {
		return fmt.Errorf("pull %s: %w", image, ports.ErrDiskFull)
	}
	r.used += r.cfg.ImageBytes
	r.images[image] = ports.ImageInfo{Reference: image, SizeBytes: r.cfg.ImageBytes, Pulled: r.clock.Now()}
	r.pulls = append(r.pulls, image)
	return nil
}

// Images implements ports.VMRuntime.
func (r *VMRuntime) Images(ctx context.Context) ([]ports.ImageInfo, error) {
	if err := r.enter(ctx, "Images"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ports.ImageInfo, 0, len(r.images))
	for _, ref := range sortedKeys(r.images) {
		out = append(out, r.images[ref])
	}
	return out, nil
}

// Prune implements ports.VMRuntime: oldest images first until the cache fits the budget.
func (r *VMRuntime) Prune(ctx context.Context, budget uint64) error {
	if err := r.enter(ctx, "Prune"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	imgs := make([]ports.ImageInfo, 0, len(r.images))
	var total uint64
	for _, img := range r.images {
		imgs = append(imgs, img)
		total += img.SizeBytes
	}
	sort.Slice(imgs, func(i, j int) bool {
		if !imgs[i].Pulled.Equal(imgs[j].Pulled) {
			return imgs[i].Pulled.Before(imgs[j].Pulled)
		}
		return imgs[i].Reference < imgs[j].Reference
	})
	for _, img := range imgs {
		if total <= budget {
			break
		}
		delete(r.images, img.Reference)
		total -= img.SizeBytes
		r.used -= min(r.used, img.SizeBytes)
	}
	return nil
}
