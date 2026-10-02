// SPDX-License-Identifier: FSL-1.1-ALv2

// Package vmm is hostd's VM manager: it owns the lifecycle model
// (internal/hostd/lifecycle), persists the desired state in a journal,
// reconciles it with `tart list` (crash-only: state is rebuilt from Tart plus
// the journal after a restart), executes planned actions against
// ports.VMRuntime and configures each VM through the in-VM contract
// (internal/hostd/guest) with credentials from the controller (R-MAC-3/-4).
package vmm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/hostd/guest"
	"github.com/sloper-ai/cucina/internal/hostd/identity"
	"github.com/sloper-ai/cucina/internal/hostd/lifecycle"
	"github.com/sloper-ai/cucina/internal/hostd/render"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Controller is what the VM manager needs from the controller link.
type Controller interface {
	IssueVMIdentity(ctx context.Context, req *cucinav1.IssueVMIdentityRequest) (*cucinav1.IssueVMIdentityResponse, error)
	RegistryCreds(ctx context.Context, image string) (*ports.RegistryCreds, error)
}

// Tunables are the controller/preference knobs the manager applies.
type Tunables struct {
	Slots       int
	VMCPU       int
	VMMemoryGiB int
}

// Options configure a Manager.
type Options struct {
	Runtime    ports.VMRuntime
	Clock      ports.Clock
	FS         ports.FS
	Render     render.Renderer
	Controller Controller
	// HostCAPEM returns the host-local L2 CA VMs must trust.
	HostCAPEM func(context.Context) ([]byte, error)
	// Prefix is prepended to controller VM names to form Tart VM names.
	Prefix string
	// Host is the host part of the node label ("<host>/<vm>"), the serial number.
	Host        string
	Capacity    lifecycle.HostCapacity
	Limits      lifecycle.Limits
	JournalPath string
	// StoragePort and SchedulerPort are the relay ports on the gateway.
	StoragePort, SchedulerPort int
	// Gateway returns the host address a VM reaches hostd at.
	Gateway func(vmIP netip.Addr) (netip.Addr, bool)
	// ScrapeActivity returns bb_worker's completed-operation counter of a VM.
	ScrapeActivity func(ctx context.Context, ip netip.Addr, port uint32) (uint64, error)
	// Events receives VM events for the controller.
	Events func(*cucinav1.VMEvent)
	Log    *slog.Logger

	TickInterval  time.Duration // reconcile period (default 5s)
	ProbeInterval time.Duration // activity probe period (default 30s)
	StartTimeout  time.Duration // whole start sequence (default 5m)
}

// Manager runs the VMs of one host.
type Manager struct {
	o Options

	mu        sync.Mutex
	host      lifecycle.Host
	tun       Tunables
	ips       map[string]netip.Addr
	ports     map[string]uint32
	lastCount map[string]uint64
	inflight  map[string]bool
	adopting  map[string]bool
	kick      chan struct{}
	wg        sync.WaitGroup
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// Errors returned to controller commands.
var (
	ErrCordoned = errors.New("host is cordoned")
	ErrNoSlot   = errors.New("host has no free VM slot")
	ErrBadName  = errors.New("invalid vm name (want [a-z0-9][a-z0-9-]{0,40})")
)

// New returns a manager; call Load then Run.
func New(o Options) *Manager {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.TickInterval <= 0 {
		o.TickInterval = 5 * time.Second
	}
	if o.ProbeInterval <= 0 {
		o.ProbeInterval = 30 * time.Second
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = 5 * time.Minute
	}
	if o.StoragePort == 0 {
		o.StoragePort = 8981
	}
	if o.SchedulerPort == 0 {
		o.SchedulerPort = 8983
	}
	if o.ScrapeActivity == nil {
		o.ScrapeActivity = ScrapeBuildExecutorCount
	}
	return &Manager{
		o: o, host: lifecycle.Host{Slots: 2}, tun: Tunables{Slots: 2},
		ips: map[string]netip.Addr{}, ports: map[string]uint32{}, lastCount: map[string]uint64{},
		inflight: map[string]bool{}, adopting: map[string]bool{}, kick: make(chan struct{}, 1),
	}
}

func (m *Manager) tartName(vm string) string { return m.o.Prefix + vm }

// Kick asks for a reconcile soon.
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// ------------------------------------------------------------------ journal

type journalVM struct {
	Name             string           `json:"name"`
	Pool             string           `json:"pool"`
	Node             string           `json:"node"`
	Intent           lifecycle.Intent `json:"intent"`
	Image            string           `json:"image"`
	Generation       string           `json:"generation"`
	CPU              int              `json:"cpu,omitempty"`
	MemoryGiB        int              `json:"memoryGiB,omitempty"`
	DiskGiB          int              `json:"diskGiB,omitempty"`
	MaxAge           time.Duration    `json:"maxAge,omitempty"`
	Reimage          bool             `json:"reimage,omitempty"`
	StopReason       string           `json:"stopReason,omitempty"`
	ClonedImage      string           `json:"clonedImage,omitempty"`
	ClonedGeneration string           `json:"clonedGeneration,omitempty"`
	ClonedAt         time.Time        `json:"clonedAt,omitempty"`
	MetricsPort      uint32           `json:"metricsPort,omitempty"`
	Failures         int              `json:"failures,omitempty"`
	FailuresAtClone  int              `json:"failuresAtClone,omitempty"`
	RetryAt          time.Time        `json:"retryAt,omitempty"`
	LastError        string           `json:"lastError,omitempty"`
}

type journal struct {
	Version  int         `json:"version"`
	Cordoned bool        `json:"cordoned"`
	VMs      []journalVM `json:"vms"`
}

// Load reads the journal (missing = empty). Phases are learned from Tart.
func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.o.JournalPath == "" {
		return nil
	}
	ok, err := m.o.FS.Exists(m.o.JournalPath)
	if err != nil || !ok {
		return err
	}
	b, err := m.o.FS.ReadFile(m.o.JournalPath)
	if err != nil {
		return err
	}
	var j journal
	if err := json.Unmarshal(b, &j); err != nil {
		m.o.Log.Error("vm journal is corrupt; starting empty (VMs are rediscovered from tart)", "err", err)
		return nil
	}
	m.host.Cordoned = j.Cordoned
	for _, v := range j.VMs {
		m.ports[v.Name] = v.MetricsPort
		m.host.VMs = append(m.host.VMs, lifecycle.VM{
			Name: v.Name, Pool: v.Pool, Node: v.Node, Intent: v.Intent, Image: v.Image, Generation: v.Generation,
			CPU: v.CPU, MemoryGiB: v.MemoryGiB, DiskGiB: v.DiskGiB, MaxAge: v.MaxAge, Reimage: v.Reimage,
			StopReason: v.StopReason, ClonedImage: v.ClonedImage, ClonedGeneration: v.ClonedGeneration,
			ClonedAt: v.ClonedAt, Phase: lifecycle.Absent,
			Failures: v.Failures, FailuresAtClone: v.FailuresAtClone, RetryAt: v.RetryAt, LastError: v.LastError,
		})
	}
	return nil
}

func (m *Manager) saveLocked() {
	if m.o.JournalPath == "" {
		return
	}
	j := journal{Version: 1, Cordoned: m.host.Cordoned}
	for _, v := range m.host.VMs {
		j.VMs = append(j.VMs, journalVM{Name: v.Name, Pool: v.Pool, Node: v.Node, Intent: v.Intent, Image: v.Image,
			Generation: v.Generation, CPU: v.CPU, MemoryGiB: v.MemoryGiB, DiskGiB: v.DiskGiB, MaxAge: v.MaxAge,
			Reimage: v.Reimage, StopReason: v.StopReason, ClonedImage: v.ClonedImage,
			ClonedGeneration: v.ClonedGeneration, ClonedAt: v.ClonedAt, MetricsPort: m.ports[v.Name],
			Failures: v.Failures, FailuresAtClone: v.FailuresAtClone, RetryAt: v.RetryAt, LastError: v.LastError})
	}
	b, _ := json.MarshalIndent(j, "", "  ")
	if err := m.o.FS.WriteFileAtomic(m.o.JournalPath, b, 0o600); err != nil {
		m.o.Log.Error("writing vm journal", "err", err)
	}
}

// ------------------------------------------------------------------ commands

// StartVM records the desired state of a StartVM command (R-MAC-6).
func (m *Manager) StartVM(r lifecycle.StartRequest) error {
	if !nameRe.MatchString(r.Name) {
		return ErrBadName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.host.Cordoned {
		return ErrCordoned
	}
	wanted := 0
	for _, vm := range m.host.VMs {
		if vm.Name != r.Name && vm.Intent == lifecycle.WantRunning {
			wanted++
		}
	}
	if wanted >= min(m.host.Slots, lifecycle.HardMaxRunning) {
		return fmt.Errorf("%w (slots=%d, VMs wanted running=%d)", ErrNoSlot, m.host.Slots, wanted)
	}
	if r.Node == "" {
		r.Node = m.o.Host + "/" + r.Name
	}
	lifecycle.RequestStart(&m.host, r)
	m.saveLocked()
	m.Kick()
	return nil
}

// StopVM records a StopVM command.
func (m *Manager) StopVM(name string, timeout time.Duration, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := lifecycle.RequestStop(&m.host, name, timeout, reason); err != nil {
		return fmt.Errorf("%w: %s", err, name)
	}
	m.saveLocked()
	m.Kick()
	return nil
}

// DeleteVM records a DeleteVM command.
func (m *Manager) DeleteVM(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := lifecycle.RequestDelete(&m.host, name); err != nil {
		return fmt.Errorf("%w: %s", err, name)
	}
	m.saveLocked()
	m.Kick()
	return nil
}

// ReimageVM records a ReimageVM command.
func (m *Manager) ReimageVM(name, image string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := lifecycle.RequestReimage(&m.host, name, image); err != nil {
		return fmt.Errorf("%w: %s", err, name)
	}
	m.saveLocked()
	m.Kick()
	return nil
}

// SetCordon cordons (no new VMs; idle VMs stop) or uncordons the host.
func (m *Manager) SetCordon(c bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.host.Cordoned = c
	m.saveLocked()
	m.Kick()
}

// Cordoned reports the cordon state.
func (m *Manager) Cordoned() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.host.Cordoned
}

// SetTunables applies slots and VM size overrides.
func (m *Manager) SetTunables(t Tunables) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.Slots < 1 {
		t.Slots = 1
	}
	if t.Slots > lifecycle.HardMaxRunning {
		t.Slots = lifecycle.HardMaxRunning
	}
	m.tun = t
	m.host.Slots = t.Slots
	m.Kick()
}

// VMForIP implements relay.Admission: only running/starting managed VMs.
func (m *Manager) VMForIP(ip netip.Addr) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, a := range m.ips {
		if a == ip {
			return name, true
		}
	}
	return "", false
}

// UpstreamOK records scheduler reachability for a VM (relay callback).
func (m *Manager) UpstreamOK(vm string, ok bool) {
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lifecycle.UpstreamOK(&m.host, vm, m.o.Clock.Now())
}

// RunningCount is the number of active managed VMs.
func (m *Manager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, vm := range m.host.VMs {
		if vm.Phase.Active() {
			n++
		}
	}
	return n
}

// Snapshot returns a copy of the model (tests, metrics).
func (m *Manager) Snapshot() lifecycle.Host {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.host
	h.VMs = append([]lifecycle.VM(nil), m.host.VMs...)
	return h
}

func stateString(p lifecycle.Phase) string {
	switch p {
	case lifecycle.Cloning:
		return "cloning"
	case lifecycle.Starting:
		return "starting"
	case lifecycle.Running:
		return "running"
	case lifecycle.Stopping, lifecycle.Deleting:
		return "stopping"
	case lifecycle.Failed:
		return "failed"
	}
	return "stopped"
}

func (m *Manager) infoLocked(vm lifecycle.VM) *cucinav1.VMInfo {
	info := &cucinav1.VMInfo{Name: vm.Name, Pool: vm.Pool, State: stateString(vm.Phase), Image: vm.ClonedImage,
		Generation: vm.ClonedGeneration, LastError: vm.LastError}
	if info.Image == "" {
		info.Image, info.Generation = vm.Image, vm.Generation
	}
	if ip, ok := m.ips[vm.Name]; ok {
		info.Ip = ip.String()
	}
	if !vm.ClonedAt.IsZero() {
		info.Created = timestamppb.New(vm.ClonedAt)
	}
	if !vm.StartedAt.IsZero() && vm.Phase.Active() {
		info.Started = timestamppb.New(vm.StartedAt)
	}
	return info
}

// MetricsTarget is a running, managed VM's locally reachable worker endpoint.
// It is resolved from runtime state, never from controller-provided scrape URLs.
type MetricsTarget struct {
	Name string
	IP   netip.Addr
	Port uint32
}

// MetricsTargets returns a snapshot for hostd's outbound metrics relay.
func (m *Manager) MetricsTargets() []MetricsTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MetricsTarget
	for _, vm := range m.host.VMs {
		if ip, ok := m.ips[vm.Name]; vm.Phase == lifecycle.Running && ok && m.ports[vm.Name] > 0 {
			out = append(out, MetricsTarget{Name: vm.Name, IP: ip, Port: m.ports[vm.Name]})
		}
	}
	return out
}

// Inventory returns the VMs for Hello/Heartbeat.
func (m *Manager) Inventory() []*cucinav1.VMInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*cucinav1.VMInfo, 0, len(m.host.VMs))
	for _, vm := range m.host.VMs {
		out = append(out, m.infoLocked(vm))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) emit(name, event, msg string) {
	if m.o.Events == nil {
		return
	}
	m.mu.Lock()
	i := m.host.Find(name)
	var info *cucinav1.VMInfo
	if i >= 0 {
		info = m.infoLocked(m.host.VMs[i])
	} else {
		info = &cucinav1.VMInfo{Name: name, State: "stopped"}
	}
	m.mu.Unlock()
	m.o.Events(&cucinav1.VMEvent{Vm: info, Event: event, Message: msg, Time: timestamppb.New(m.o.Clock.Now())})
}

// ------------------------------------------------------------------ loop

// Run reconciles until ctx is done, then waits for in-flight actions.
func (m *Manager) Run(ctx context.Context) error {
	defer m.wg.Wait()
	m.reconcile(ctx)
	tick := m.o.Clock.After(m.o.TickInterval)
	probe := m.o.Clock.After(m.o.ProbeInterval)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-m.kick:
			m.reconcile(ctx)
		case <-tick:
			m.reconcile(ctx)
			tick = m.o.Clock.After(m.o.TickInterval)
		case <-probe:
			m.probe(ctx)
			probe = m.o.Clock.After(m.o.ProbeInterval)
		}
	}
}

// ReconcileOnce runs one reconcile pass (tests drive the manager step by step).
func (m *Manager) ReconcileOnce(ctx context.Context) { m.reconcile(ctx) }

// ProbeOnce runs one activity probe pass.
func (m *Manager) ProbeOnce(ctx context.Context) { m.probe(ctx) }

// Wait blocks until in-flight actions finish.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) reconcile(ctx context.Context) {
	tvms, err := m.o.Runtime.List(ctx)
	if err != nil {
		m.o.Log.Warn("tart list failed", "err", err)
		return
	}
	now := m.o.Clock.Now()
	m.mu.Lock()
	byName := map[string]ports.TartVM{}
	external := 0
	for _, t := range tvms {
		if !strings.HasPrefix(t.Name, m.o.Prefix) {
			if t.State == ports.TartRunning {
				external++
			}
			continue
		}
		byName[strings.TrimPrefix(t.Name, m.o.Prefix)] = t
	}
	m.host.ExternalActive = external
	var adopt []string
	for name, t := range byName {
		if m.host.Find(name) < 0 {
			// Unknown to the journal (lost state): keep running VMs running, keep stopped ones stopped.
			intent := lifecycle.WantStopped
			if t.State == ports.TartRunning {
				intent = lifecycle.WantRunning
			}
			m.host.VMs = append(m.host.VMs, lifecycle.VM{Name: name, Node: m.o.Host + "/" + name, Intent: intent, Phase: lifecycle.Absent})
		}
	}
	for i := range m.host.VMs {
		vm := &m.host.VMs[i]
		if m.inflight[vm.Name] || m.adopting[vm.Name] {
			continue
		}
		t, exists := byName[vm.Name]
		switch {
		case !exists:
			if vm.Phase == lifecycle.Running {
				lifecycle.Crashed(&m.host, vm.Name, "vm disappeared from tart", now)
				delete(m.ips, vm.Name)
			}
			vm = &m.host.VMs[i]
			vm.Phase = lifecycle.Absent
		case t.State == ports.TartRunning:
			if vm.Phase != lifecycle.Running {
				adopt = append(adopt, vm.Name)
				m.adopting[vm.Name] = true
			}
		default:
			switch vm.Phase {
			case lifecycle.Running:
				lifecycle.Crashed(&m.host, vm.Name, "vm stopped unexpectedly", now)
				delete(m.ips, vm.Name)
				go m.emit(vm.Name, "failed", "vm stopped unexpectedly")
			case lifecycle.Absent:
				vm.Phase = lifecycle.Stopped
			}
		}
	}
	plan := lifecycle.Plan(m.host, m.o.Limits, now)
	for _, a := range plan {
		lifecycle.Begin(&m.host, a)
		m.inflight[a.VM] = true
	}
	if len(plan) > 0 {
		m.saveLocked()
	}
	m.mu.Unlock()
	for _, name := range adopt {
		m.wg.Add(1)
		go func(name string) {
			defer m.wg.Done()
			m.adopt(ctx, name)
		}(name)
	}
	for _, a := range plan {
		m.wg.Add(1)
		go func(a lifecycle.Action) {
			defer m.wg.Done()
			err := m.execute(ctx, a)
			m.mu.Lock()
			lifecycle.Complete(&m.host, a, err, m.o.Clock.Now())
			delete(m.inflight, a.VM)
			m.saveLocked()
			m.mu.Unlock()
			// Report after the model reflects the outcome, so the event carries the new state.
			if err != nil {
				m.emit(a.VM, "failed", string(a.Kind)+": "+err.Error())
			} else {
				m.emit(a.VM, map[lifecycle.Kind]string{lifecycle.Clone: "cloned", lifecycle.Start: "ready",
					lifecycle.Stop: "stopped", lifecycle.Delete: "deleted"}[a.Kind], a.Reason)
			}
			m.Kick()
		}(a)
	}
}

// adopt takes over a VM that runs although the model does not know it as
// running (hostd restarted): a configured worker is adopted, anything else is stopped.
func (m *Manager) adopt(ctx context.Context, name string) {
	defer func() {
		m.mu.Lock()
		delete(m.adopting, name)
		m.mu.Unlock()
	}()
	tn := m.tartName(name)
	res, err := m.o.Runtime.GuestExec(ctx, tn, guest.WorkerStatusCmd())
	ip, ipErr := m.o.Runtime.IP(ctx, tn, 5*time.Second)
	m.mu.Lock()
	metricsPort := m.ports[name]
	m.mu.Unlock()
	if metricsPort == 0 && err == nil && guest.ParseRunning(res) {
		metricsPort = m.adoptMetricsPort(ctx, tn)
	}
	now := m.o.Clock.Now()
	m.mu.Lock()
	i := m.host.Find(name)
	if i < 0 {
		m.mu.Unlock()
		return
	}
	if err == nil && ipErr == nil && guest.ParseRunning(res) {
		vm := &m.host.VMs[i]
		vm.Phase = lifecycle.Running
		vm.StartedAt, vm.LastActive, vm.LastUpstreamOK = now, now, now
		m.ips[name] = ip
		m.ports[name] = metricsPort
		m.saveLocked()
		m.mu.Unlock()
		m.o.Log.Info("adopted running vm", "vm", name, "ip", ip)
		m.emit(name, "ready", "adopted after hostd restart")
		return
	}
	m.host.VMs[i].Phase = lifecycle.Stopping
	m.inflight[name] = true
	m.mu.Unlock()
	m.o.Log.Warn("running vm has no healthy worker; stopping it", "vm", name, "err", err)
	stopErr := m.o.Runtime.Stop(ctx, tn, 30*time.Second)
	m.mu.Lock()
	delete(m.inflight, name)
	if i := m.host.Find(name); i >= 0 {
		if stopErr == nil {
			m.host.VMs[i].Phase = lifecycle.Stopped
		} else {
			m.host.VMs[i].Phase = lifecycle.Running
		}
	}
	m.mu.Unlock()
	m.Kick()
}

// Legacy journals lack the metrics port. Recover only the port from the
// root-owned worker configuration; the scrape address remains the runtime's VM
// IP, never a hostname supplied by guest data. Missing evidence stays absent.
func (m *Manager) adoptMetricsPort(ctx context.Context, tartName string) uint32 {
	res, err := m.o.Runtime.GuestExec(ctx, tartName, ports.Command{Path: "/bin/cat", Args: []string{guest.ConfigRoot + "/bb/worker.json"}})
	if err != nil || res.ExitCode != 0 || len(res.Stdout) > 128<<10 {
		return 0
	}
	var cfg struct {
		Global struct {
			Diagnostics struct {
				Servers []struct {
					Addresses []string `json:"listenAddresses"`
				} `json:"httpServers"`
			} `json:"diagnosticsHttpServer"`
		} `json:"global"`
	}
	if json.Unmarshal(res.Stdout, &cfg) != nil {
		return 0
	}
	for _, server := range cfg.Global.Diagnostics.Servers {
		for _, addr := range server.Addresses {
			if _, port, err := net.SplitHostPort(addr); err == nil {
				if n, err := strconv.ParseUint(port, 10, 16); err == nil && n > 0 {
					return uint32(n)
				}
			}
		}
	}
	return 0
}

func (m *Manager) vm(name string) (lifecycle.VM, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.host.Find(name)
	if i < 0 {
		return lifecycle.VM{}, false
	}
	return m.host.VMs[i], true
}

func (m *Manager) execute(ctx context.Context, a lifecycle.Action) error {
	vm, ok := m.vm(a.VM)
	if !ok {
		return lifecycle.ErrUnknownVM
	}
	tn := m.tartName(a.VM)
	log := m.o.Log.With("vm", a.VM, "action", string(a.Kind))
	switch a.Kind {
	case lifecycle.Clone:
		if err := m.ensureImage(ctx, vm.Image); err != nil {
			log.Error("image pull failed", "image", vm.Image, "err", err)
			return fmt.Errorf("pull %s: %w", vm.Image, err)
		}
		if err := m.o.Runtime.Clone(ctx, vm.Image, tn, vm.DiskGiB); err != nil {
			if errors.Is(err, ports.ErrVMExists) {
				_ = m.o.Runtime.Delete(ctx, tn)
				err = m.o.Runtime.Clone(ctx, vm.Image, tn, vm.DiskGiB)
			}
			if err != nil {
				log.Error("clone failed", "err", err)
				return err
			}
		}
		log.Info("cloned", "image", vm.Image)
		return nil
	case lifecycle.Start:
		sctx, cancel := context.WithTimeout(ctx, m.o.StartTimeout)
		defer cancel()
		if err := m.start(sctx, vm); err != nil {
			log.Error("start failed", "err", err)
			m.mu.Lock()
			delete(m.ips, a.VM)
			m.mu.Unlock()
			if stopErr := m.o.Runtime.Stop(context.WithoutCancel(ctx), tn, 30*time.Second); stopErr != nil && !errors.Is(stopErr, ports.ErrVMNotFound) {
				log.Error("stopping half-started vm failed", "err", stopErr)
			}
			return err
		}
		log.Info("ready")
		return nil
	case lifecycle.Stop:
		m.mu.Lock()
		delete(m.ips, a.VM)
		m.mu.Unlock()
		if err := m.o.Runtime.Stop(ctx, tn, a.Timeout); err != nil && !errors.Is(err, ports.ErrVMNotFound) {
			log.Error("stop failed", "err", err)
			return err
		}
		log.Info("stopped", "reason", a.Reason)
		return nil
	case lifecycle.Delete:
		if err := m.o.Runtime.Delete(ctx, tn); err != nil && !errors.Is(err, ports.ErrVMNotFound) {
			log.Error("delete failed", "err", err)
			return err
		}
		log.Info("deleted", "reason", a.Reason)
		return nil
	}
	return fmt.Errorf("unknown action %q", a.Kind)
}

func (m *Manager) ensureImage(ctx context.Context, image string) error {
	imgs, err := m.o.Runtime.Images(ctx)
	if err != nil {
		return err
	}
	for _, i := range imgs {
		if i.Reference == image {
			return nil
		}
	}
	var creds *ports.RegistryCreds
	if m.o.Controller != nil {
		c, err := m.o.Controller.RegistryCreds(ctx, image)
		if err != nil {
			m.o.Log.Warn("no registry credentials; pulling anonymously", "image", image, "err", err)
		} else {
			creds = c
		}
	}
	return m.o.Runtime.Pull(ctx, image, creds)
}

// Pull pre-pulls an image (PullImage command, R-MAC-5).
func (m *Manager) Pull(ctx context.Context, image string) error { return m.ensureImage(ctx, image) }

func (m *Manager) retryGuest(ctx context.Context, tn string, c ports.Command, ok func(ports.ExecResult) error) (ports.ExecResult, error) {
	delay := 500 * time.Millisecond
	for {
		res, err := m.o.Runtime.GuestExec(ctx, tn, c)
		if err == nil {
			if err = ok(res); err == nil {
				return res, nil
			}
		}
		if ctx.Err() != nil {
			return res, fmt.Errorf("%w (last error: %w)", ctx.Err(), err)
		}
		if err := m.o.Clock.Sleep(ctx, delay); err != nil {
			return res, err
		}
		if delay < 5*time.Second {
			delay *= 2
		}
	}
}

func exit0(res ports.ExecResult) error {
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// start is the §1.2 sequence.
func (m *Manager) start(ctx context.Context, vm lifecycle.VM) error {
	tn := m.tartName(vm.Name)
	m.mu.Lock()
	tun, slots := m.tun, m.host.Slots
	m.mu.Unlock()
	cpu, mem := lifecycle.Size(m.o.Capacity, slots, vm.CPU, vm.MemoryGiB, tun.VMCPU, tun.VMMemoryGiB)
	if err := m.o.Runtime.Run(ctx, tn, ports.RunOptions{NoGraphics: true, RootDiskOpts: "caching=cached,sync=none",
		CPU: cpu, MemoryMiB: mem * 1024}); err != nil {
		return fmt.Errorf("tart run: %w", err)
	}
	ip, err := m.o.Runtime.IP(ctx, tn, 2*time.Minute)
	if err != nil {
		return fmt.Errorf("tart ip: %w", err)
	}
	if _, err := m.retryGuest(ctx, tn, guest.ProbeCmd(), exit0); err != nil {
		return fmt.Errorf("guest agent: %w", err)
	}
	res, err := m.o.Runtime.GuestExec(ctx, tn, guest.ManifestCmd())
	if err != nil {
		return err
	}
	if err := exit0(res); err != nil {
		return fmt.Errorf("image manifest: %w", err)
	}
	man, err := guest.ParseManifest(res.Stdout)
	if err != nil {
		return err
	}
	var console guest.Console
	if _, err := m.retryGuest(ctx, tn, guest.ConsoleCmd(), func(r ports.ExecResult) error {
		if err := exit0(r); err != nil {
			return err
		}
		c, err := guest.ParseConsole(r.Stdout)
		if err != nil {
			return err
		}
		if c.User != man.BuildUser {
			return fmt.Errorf("%w (console owner %q, build user %q)", guest.ErrNotLoggedIn, c.User, man.BuildUser)
		}
		console = c
		return nil
	}); err != nil {
		return fmt.Errorf("gui session: %w", err)
	}
	worker := console
	switch wu := man.Worker(); {
	case wu == "root":
		worker = guest.Console{User: "root"}
	case wu != console.User:
		r, err := m.o.Runtime.GuestExec(ctx, tn, guest.IDCmd(wu))
		if err != nil {
			return err
		}
		if err := exit0(r); err != nil {
			return fmt.Errorf("worker user %q: %w", wu, err)
		}
		if worker, err = guest.ParseID(r.Stdout); err != nil {
			return err
		}
	}
	if m.o.Controller == nil {
		return errors.New("controller not connected: cannot issue a VM identity")
	}
	key, keyPEM, err := identity.NewVMKey()
	if err != nil {
		return err
	}
	csr, err := identity.CSR(key, vm.Node)
	if err != nil {
		return err
	}
	idr, err := m.o.Controller.IssueVMIdentity(ctx, &cucinav1.IssueVMIdentityRequest{VmName: vm.Name, Pool: vm.Pool, CsrPem: csr})
	if err != nil {
		return fmt.Errorf("IssueVMIdentity: %w", err)
	}
	if idr.GetSettings() == nil {
		return errors.New("IssueVMIdentity returned no worker settings")
	}
	ws := proto.Clone(idr.GetSettings()).(*cucinav1.WorkerSettings)
	gw, ok := m.o.Gateway(ip)
	if !ok {
		return fmt.Errorf("no host interface on the network of vm address %s", ip)
	}
	ws.StorageEndpoint = net.JoinHostPort(gw.String(), strconv.Itoa(m.o.StoragePort))
	ws.SchedulerEndpoint = net.JoinHostPort(gw.String(), strconv.Itoa(m.o.SchedulerPort))
	if ws.Node == "" {
		ws.Node = vm.Node
	}
	if ws.Pool == "" {
		ws.Pool = vm.Pool
	}
	caBundle := append([]byte(nil), idr.GetCaPem()...)
	if m.o.HostCAPEM != nil {
		hc, err := m.o.HostCAPEM(ctx)
		if err != nil {
			return fmt.Errorf("host L2 CA: %w", err)
		}
		caBundle = append(append(caBundle, '\n'), hc...)
	}
	rendered, err := m.o.Render.Worker(ws, render.VM{VCPUs: cpu, MemoryBytes: uint64(mem) << 30,
		CABundlePEM: string(caBundle), Console: console, Manifest: man})
	if err != nil {
		return fmt.Errorf("rendering worker config: %w", err)
	}
	vmJSON, _ := json.Marshal(map[string]any{"pool": vm.Pool, "node": ws.Node, "generation": vm.Generation,
		"image": vm.Image, "imageVersion": man.ImageVersion, "cpu": cpu, "memoryGiB": mem,
		"configuredAt": m.o.Clock.Now().UTC().Format(time.RFC3339)})
	files := guest.Bundle{WorkerJSON: rendered.WorkerJSON, RunnerJSON: rendered.RunnerJSON, VMJSON: vmJSON,
		KeyPEM: keyPEM, CertPEM: idr.GetCertificatePem(), CAPEM: caBundle}.Files(worker)
	tarStream, err := guest.Tar(files, m.o.Clock.Now())
	if err != nil {
		return err
	}
	if res, err := m.o.Runtime.GuestExec(ctx, tn, guest.ExtractCmd(tarStream)); err != nil {
		return fmt.Errorf("pushing config: %w", err)
	} else if err := exit0(res); err != nil {
		return fmt.Errorf("pushing config: %w", err)
	}
	dirs := make([]guest.Dir, 0, len(rendered.Dirs))
	for _, d := range rendered.Dirs {
		dirs = append(dirs, guest.Dir{Path: d.Path, Mode: uint32(d.Mode.Perm()), BuildUserOwned: d.BuildUserOwned})
	}
	act, err := guest.ActivateCmd(console, worker, dirs)
	if err != nil {
		return err
	}
	// Admit the VM to the relays before its worker dials them.
	m.mu.Lock()
	m.ips[vm.Name] = ip
	m.ports[vm.Name] = ws.GetMetricsPort()
	delete(m.lastCount, vm.Name)
	m.mu.Unlock()
	if res, err := m.o.Runtime.GuestExec(ctx, tn, act); err != nil {
		return fmt.Errorf("activating services: %w", err)
	} else if err := exit0(res); err != nil {
		return fmt.Errorf("activating services: %w", err)
	}
	if _, err := m.retryGuest(ctx, tn, guest.WorkerStatusCmd(), func(r ports.ExecResult) error {
		if !guest.ParseRunning(r) {
			return errors.New("bb_worker job is not running")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("worker health: %w", err)
	}
	return nil
}

// probe refreshes activity (dead-man idle input) for running VMs and tells
// long-idle ones apart from VMs running one long action.
func (m *Manager) probe(ctx context.Context) {
	type target struct {
		name string
		ip   netip.Addr
		port uint32
		idle time.Duration
	}
	now := m.o.Clock.Now()
	var targets []target
	m.mu.Lock()
	for _, vm := range m.host.VMs {
		if vm.Phase != lifecycle.Running {
			continue
		}
		ip, ok := m.ips[vm.Name]
		if !ok {
			continue
		}
		targets = append(targets, target{vm.Name, ip, m.ports[vm.Name], now.Sub(vm.LastActive)})
	}
	m.mu.Unlock()
	for _, t := range targets {
		if t.port != 0 {
			if n, err := m.o.ScrapeActivity(ctx, t.ip, t.port); err == nil {
				m.mu.Lock()
				last, seen := m.lastCount[t.name]
				m.lastCount[t.name] = n
				if seen && n != last {
					lifecycle.Active(&m.host, t.name, now)
					t.idle = 0
				}
				m.mu.Unlock()
			}
		}
		if t.idle >= m.o.Limits.IdleLimit/2 {
			res, err := m.o.Runtime.GuestExec(ctx, m.tartName(t.name), guest.BusyProbeCmd())
			if err == nil {
				m.mu.Lock()
				lifecycle.Activity(&m.host, t.name, res.ExitCode == 0, now)
				m.mu.Unlock()
			}
		}
	}
	m.Kick()
}

// ScrapeBuildExecutorCount sums bb_worker's
// buildbarn_builder_build_executor_duration_seconds_count (it grows whenever an
// action finishes a stage) from http://ip:port/metrics. hostd (root) makes the
// connection itself (Local Network privacy, R-MAC-10).
func ScrapeBuildExecutorCount(ctx context.Context, ip netip.Addr, port uint32) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(int(port))) + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return SumCounter(io.LimitReader(resp.Body, 16<<20), "buildbarn_builder_build_executor_duration_seconds_count"), nil
}

// SumCounter sums every sample of one metric name in Prometheus text format.
func SumCounter(r io.Reader, name string) uint64 {
	var total float64
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		if rest == "" || (rest[0] != '{' && rest[0] != ' ') {
			continue
		}
		f := strings.Fields(rest[strings.LastIndexByte(rest, '}')+1:])
		if len(f) == 0 {
			continue
		}
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			total += v
		}
	}
	return uint64(total)
}
