// SPDX-License-Identifier: FSL-1.1-ALv2

// Package tart implements ports.VMRuntime over the `tart` CLI (R-MAC-3).
//
// Every call shells out through ports.Exec, so the same adapter runs against the
// real tart binary (acceptance tier, T13/T14) and against the in-process CLI
// emulator in internal/providers/tart/faketart (integration tier). Commands run
// as Options.RunAs (the dedicated `cucina` user, R-MAC-2) when set; in hostd's
// user mode RunAs is nil and tart runs as the current user.
//
// The adapter enforces the Apple licence / Virtualization.framework limit of at
// most two running macOS VMs per host itself (ErrVMLimit) instead of relying on
// tart's own error, so the invariant holds deterministically (R-TEST-7).
package tart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// MaxRunningVMs is the Apple licence and Virtualization.framework limit (R-MAC-3).
const MaxRunningVMs = 2

// Options configures a Runtime.
type Options struct {
	Exec ports.Exec
	// Binary is the tart executable (default "tart", resolved through PATH).
	Binary string
	// RunAs drops privileges for every tart invocation (root LaunchDaemon mode).
	RunAs *ports.RunAs
	// Env is the base environment of every tart invocation (HOME, PATH, TART_HOME, …).
	Env []string
	// MaxRunning caps running VMs; values outside 1..2 are clamped to 2.
	MaxRunning int
	// RootDiskOpts is the default for RunOptions.RootDiskOpts when empty.
	RootDiskOpts string
	// CloneStacked uses `tart clone --stacked` (stacked disks on macOS 27 hosts, R-DATA-5).
	CloneStacked bool
	// OnExit, if set, is called when a `tart run` child exits (crash or stop).
	OnExit func(name string, res ports.ExecResult, err error)
	Logger *slog.Logger
	// Clock paces the wait for a started VM to show up as running (default: system clock).
	Clock ports.Clock
	// RunWait bounds that wait (default 60s).
	RunWait time.Duration
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Runtime is the tart-backed ports.VMRuntime.
type Runtime struct {
	o     Options
	mu    sync.Mutex
	procs map[string]ports.Process // live `tart run` children started by this Runtime
	exits map[string]exitInfo      // last exit of a `tart run` child (start failures)
}

type exitInfo struct {
	p   ports.Process
	err error
}

var _ ports.VMRuntime = (*Runtime)(nil)

// New returns a Runtime.
func New(o Options) *Runtime {
	if o.Binary == "" {
		o.Binary = "tart"
	}
	if o.MaxRunning < 1 || o.MaxRunning > MaxRunningVMs {
		o.MaxRunning = MaxRunningVMs
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Clock == nil {
		o.Clock = systemClock{}
	}
	if o.RunWait <= 0 {
		o.RunWait = time.Minute
	}
	return &Runtime{o: o, procs: map[string]ports.Process{}, exits: map[string]exitInfo{}}
}

// listEntry is one element of `tart list --format json` (tart 2.40.1,
// Sources/tart/Commands/List.swift). Disk and Size are decimal gigabytes
// (integer division of bytes by 1000^3); Disk is null when unknown.
type listEntry struct {
	Source   string `json:"Source"`
	Name     string `json:"Name"`
	Disk     *int64 `json:"Disk"`
	Size     *int64 `json:"Size"`
	Accessed string `json:"Accessed"`
	Running  bool   `json:"Running"`
	State    string `json:"State"`
}

// getEntry is `tart get <vm> --format json` (Sources/tart/Commands/Get.swift).
type getEntry struct {
	CPU    int    `json:"CPU"`
	Memory int    `json:"Memory"` // MiB
	State  string `json:"State"`
}

const gb = 1000 * 1000 * 1000

// ParseList parses `tart list --format json` output.
func ParseList(out []byte) ([]listEntry, error) {
	var entries []listEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("tart list: unexpected output: %w", err)
	}
	return entries, nil
}

func (r *Runtime) cmd(args ...string) ports.Command {
	return ports.Command{Path: r.o.Binary, Args: args, Env: r.o.Env, RunAs: r.o.RunAs}
}

// run executes tart and maps failures to the ports sentinels.
func (r *Runtime) run(ctx context.Context, c ports.Command) (ports.ExecResult, error) {
	res, err := r.o.Exec.Run(ctx, c)
	if err != nil {
		return res, fmt.Errorf("tart %s: %w", firstArg(c.Args), err)
	}
	if res.ExitCode != 0 {
		return res, classify(c.Args, res)
	}
	return res, nil
}

func firstArg(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return a[0]
}

// classify maps tart's stderr messages (Sources/tart/VMStorageHelper.swift and
// the command sources) to ports sentinels.
func classify(args []string, res ports.ExecResult) error {
	msg := strings.TrimSpace(string(res.Stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(res.Stdout))
	}
	if len(msg) > 512 {
		msg = msg[:512]
	}
	base := fmt.Errorf("tart %s: exit %d: %s", firstArg(args), res.ExitCode, msg)
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "does not exist"):
		return errors.Join(ports.ErrVMNotFound, base)
	case strings.Contains(low, "already exists"):
		return errors.Join(ports.ErrVMExists, base)
	case strings.Contains(low, "exceeds the system limit"):
		return errors.Join(ports.ErrVMLimit, base)
	case strings.Contains(low, "no space left on device"), strings.Contains(low, "not enough disk space"),
		strings.Contains(low, "disk is full"):
		return errors.Join(ports.ErrDiskFull, base)
	case strings.Contains(low, "is the tart guest agent running"), strings.Contains(low, "guest agent"):
		return errors.Join(ports.ErrGuestAgent, base)
	}
	return base
}

func (r *Runtime) list(ctx context.Context, source string) ([]listEntry, error) {
	res, err := r.run(ctx, r.cmd("list", "--source", source, "--format", "json"))
	if err != nil {
		return nil, err
	}
	return ParseList(res.Stdout)
}

// List returns the local VMs.
func (r *Runtime) List(ctx context.Context) ([]ports.TartVM, error) {
	entries, err := r.list(ctx, "local")
	if err != nil {
		return nil, err
	}
	vms := make([]ports.TartVM, 0, len(entries))
	for _, e := range entries {
		vm := ports.TartVM{Name: e.Name, State: toState(e)}
		if e.Disk != nil {
			vm.DiskGiB = int(*e.Disk)
		}
		if e.Size != nil {
			vm.SizeOnDiskBytes = uint64(*e.Size) * gb
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

func toState(e listEntry) ports.TartState {
	switch e.State {
	case "running":
		return ports.TartRunning
	case "suspended":
		return ports.TartSuspended
	case "stopped":
		return ports.TartStopped
	}
	if e.Running {
		return ports.TartRunning
	}
	return ports.TartStopped
}

// Get returns CPU and memory of one VM (`tart get`).
func (r *Runtime) Get(ctx context.Context, name string) (cpu, memoryMiB int, err error) {
	res, err := r.run(ctx, r.cmd("get", name, "--format", "json"))
	if err != nil {
		return 0, 0, err
	}
	var g getEntry
	if err := json.Unmarshal(res.Stdout, &g); err != nil {
		return 0, 0, fmt.Errorf("tart get: unexpected output: %w", err)
	}
	return g.CPU, g.Memory, nil
}

// Clone clones image into a new local VM and grows its disk to diskGiB when
// that is larger than the image's disk (Tart can only grow disks).
func (r *Runtime) Clone(ctx context.Context, image, name string, diskGiB int) error {
	args := []string{"clone", image, name}
	if r.o.CloneStacked {
		args = append(args, "--stacked")
	}
	if _, err := r.run(ctx, r.cmd(args...)); err != nil {
		return err
	}
	if diskGiB <= 0 {
		return nil
	}
	entries, err := r.list(ctx, "local")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name == name && e.Disk != nil && int64(diskGiB) <= *e.Disk {
			return nil // the image's disk is already at least as large
		}
	}
	_, err = r.run(ctx, r.cmd("set", name, "--disk-size", strconv.Itoa(diskGiB)))
	return err
}

// RunArgs builds the `tart run` argument vector (R-MAC-3: --no-graphics and the
// measured-faster root disk options; never --dir/virtiofs).
func RunArgs(name string, opts ports.RunOptions, defaultRootDiskOpts string) []string {
	args := []string{"run", name}
	if opts.NoGraphics {
		args = append(args, "--no-graphics")
	}
	rdo := opts.RootDiskOpts
	if rdo == "" {
		rdo = defaultRootDiskOpts
	}
	if rdo != "" {
		args = append(args, "--root-disk-opts="+rdo)
	}
	if opts.Suspendable {
		args = append(args, "--suspendable")
	}
	return args
}

// SetArgs builds `tart set` arguments for the VM size, or nil when nothing changes.
func SetArgs(name string, opts ports.RunOptions) []string {
	args := []string{"set", name}
	if opts.CPU > 0 {
		args = append(args, "--cpu", strconv.Itoa(opts.CPU))
	}
	if opts.MemoryMiB > 0 {
		args = append(args, "--memory", strconv.Itoa(opts.MemoryMiB))
	}
	if opts.DiskGiB > 0 {
		args = append(args, "--disk-size", strconv.Itoa(opts.DiskGiB))
	}
	if len(args) == 2 {
		return nil
	}
	return args
}

// Run starts the VM as a supervised child process and returns once it is
// launched. It returns ErrVMLimit when MaxRunning VMs already run.
func (r *Runtime) Run(ctx context.Context, name string, opts ports.RunOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := r.list(ctx, "local")
	if err != nil {
		return err
	}
	running := map[string]bool{}
	found := false
	for _, e := range entries {
		if e.Name == name {
			found = true
		}
		if toState(e) == ports.TartRunning {
			running[e.Name] = true
		}
	}
	for n := range r.procs {
		running[n] = true
	}
	if running[name] {
		return nil // already running: idempotent
	}
	if !found {
		return fmt.Errorf("tart run %s: %w", name, ports.ErrVMNotFound)
	}
	if len(running) >= r.o.MaxRunning {
		return fmt.Errorf("tart run %s: %d VMs running: %w", name, len(running), ports.ErrVMLimit)
	}
	if set := SetArgs(name, opts); set != nil {
		if _, err := r.run(ctx, r.cmd(set...)); err != nil {
			return err
		}
	}
	c := r.cmd(RunArgs(name, opts, r.o.RootDiskOpts)...)
	// The VM must outlive the request context; it is stopped with Stop.
	p, err := r.o.Exec.Start(context.WithoutCancel(ctx), c)
	if err != nil {
		return fmt.Errorf("tart run %s: %w", name, err)
	}
	r.procs[name] = p
	go r.wait(name, p)
	r.mu.Unlock()
	err = r.awaitRunning(ctx, name, p)
	r.mu.Lock()
	return err
}

// awaitRunning returns once Tart reports the VM running, or the `tart run`
// child's error if it exits first (e.g. Virtualization.framework refusing it).
func (r *Runtime) awaitRunning(ctx context.Context, name string, p ports.Process) error {
	deadline := r.o.Clock.Now().Add(r.o.RunWait)
	for {
		r.mu.Lock()
		ex, exited := r.exits[name]
		r.mu.Unlock()
		if exited && ex.p == p {
			if ex.err != nil {
				return ex.err
			}
			return fmt.Errorf("tart run %s: exited immediately", name)
		}
		entries, err := r.list(ctx, "local")
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name == name && toState(e) == ports.TartRunning {
				return nil
			}
		}
		if !r.o.Clock.Now().Before(deadline) {
			return fmt.Errorf("tart run %s: not running after %s", name, r.o.RunWait)
		}
		if err := r.o.Clock.Sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

func (r *Runtime) wait(name string, p ports.Process) {
	res, err := p.Wait(context.Background())
	if err == nil && res.ExitCode != 0 {
		err = classify([]string{"run", name}, res)
	}
	r.mu.Lock()
	if r.procs[name] == p {
		delete(r.procs, name)
	}
	r.exits[name] = exitInfo{p: p, err: err}
	r.mu.Unlock()
	r.o.Logger.Info("tart run exited", "vm", name, "exit_code", res.ExitCode, "err", err)
	if r.o.OnExit != nil {
		r.o.OnExit(name, res, err)
	}
}

// Stop shuts the VM down gracefully (tart stop --timeout), keeping its disk.
// Stopping a VM that is not running succeeds. It returns once the `tart run`
// child (if this Runtime started it) has exited, so the slot is free.
func (r *Runtime) Stop(ctx context.Context, name string, timeout time.Duration) error {
	secs := int64(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	_, err := r.run(ctx, r.cmd("stop", name, "--timeout", strconv.FormatInt(secs, 10)))
	if err != nil && strings.Contains(err.Error(), "is not running") {
		err = nil
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	p := r.procs[name]
	r.mu.Unlock()
	if p != nil {
		wctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
		defer cancel()
		if _, werr := p.Wait(wctx); werr != nil {
			return fmt.Errorf("tart run %s did not exit after stop: %w", name, werr)
		}
		r.mu.Lock()
		if r.procs[name] == p {
			delete(r.procs, name)
		}
		r.mu.Unlock()
	}
	return nil
}

// Delete removes the VM and its disk.
func (r *Runtime) Delete(ctx context.Context, name string) error {
	_, err := r.run(ctx, r.cmd("delete", name))
	return err
}

// IP waits up to wait for the VM's address (`tart ip --wait`).
func (r *Runtime) IP(ctx context.Context, name string, wait time.Duration) (netip.Addr, error) {
	secs := int64(wait / time.Second)
	if secs < 0 {
		secs = 0
	}
	res, err := r.run(ctx, r.cmd("ip", name, "--wait", strconv.FormatInt(secs, 10)))
	if err != nil {
		return netip.Addr{}, err
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(string(res.Stdout)))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("tart ip %s: unexpected output: %w", name, err)
	}
	return addr, nil
}

// GuestExec runs a command in the guest through the Tart Guest Agent. A
// non-zero guest exit code is returned in ExecResult with a nil error; failures
// of tart itself (agent unreachable, VM missing) are errors.
func (r *Runtime) GuestExec(ctx context.Context, name string, gc ports.Command) (ports.ExecResult, error) {
	args := []string{"exec"}
	if gc.Stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, name, gc.Path)
	args = append(args, gc.Args...)
	c := r.cmd(args...)
	c.Stdin = gc.Stdin
	res, err := r.o.Exec.Run(ctx, c)
	if err != nil {
		return res, fmt.Errorf("tart exec %s: %w", name, err)
	}
	if res.ExitCode != 0 && isTartExecFailure(res.Stderr) {
		return res, classify(args, res)
	}
	return res, nil
}

// isTartExecFailure tells tart's own `tart exec` errors apart from the guest
// command's non-zero exit (which tart forwards as its exit code).
func isTartExecFailure(stderr []byte) bool {
	s := string(stderr)
	for _, p := range []string{
		"Failed to connect to the VM using its control socket",
		"is the Tart Guest Agent running",
		"does not exist\n", "\" does not exist",
		"\" is not running",
		"is only available on macOS",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// RegistryEnv returns the TART_REGISTRY_* variables for short-lived pull
// credentials (R-MAC-5: environment, never the keychain).
func RegistryEnv(creds *ports.RegistryCreds) []string {
	if creds == nil || creds.Username == "" {
		return nil
	}
	env := []string{"TART_REGISTRY_USERNAME=" + creds.Username, "TART_REGISTRY_PASSWORD=" + creds.Password}
	if creds.Host != "" {
		env = append(env, "TART_REGISTRY_HOSTNAME="+creds.Host)
	}
	return env
}

// Pull fetches an OCI image into the local cache.
func (r *Runtime) Pull(ctx context.Context, image string, creds *ports.RegistryCreds) error {
	c := r.cmd("pull", image)
	c.Env = append(append([]string{}, r.o.Env...), RegistryEnv(creds)...)
	_, err := r.run(ctx, c)
	return err
}

// Images lists the cached OCI images.
func (r *Runtime) Images(ctx context.Context) ([]ports.ImageInfo, error) {
	entries, err := r.list(ctx, "oci")
	if err != nil {
		return nil, err
	}
	out := make([]ports.ImageInfo, 0, len(entries))
	for _, e := range entries {
		info := ports.ImageInfo{Reference: e.Name}
		if e.Size != nil {
			info.SizeBytes = uint64(*e.Size) * gb
		}
		if t, err := time.Parse(time.RFC3339, e.Accessed); err == nil {
			info.Pulled = t
		}
		out = append(out, info)
	}
	return out, nil
}

// PruneArgs builds `tart prune` for a space budget in bytes (rounded up to whole GB).
func PruneArgs(spaceBudgetBytes uint64) []string {
	g := (spaceBudgetBytes + gb - 1) / gb
	return []string{"prune", "--entries", "caches", "--space-budget", strconv.FormatUint(g, 10)}
}

// Prune removes least-recently-used cached images beyond the space budget.
func (r *Runtime) Prune(ctx context.Context, spaceBudgetBytes uint64) error {
	_, err := r.run(ctx, r.cmd(PruneArgs(spaceBudgetBytes)...))
	return err
}

// Version returns `tart --version` (host facts, R-MAC-5).
func (r *Runtime) Version(ctx context.Context) (string, error) {
	res, err := r.run(ctx, r.cmd("--version"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}
