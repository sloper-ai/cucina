// SPDX-License-Identifier: FSL-1.1-ALv2

// Package faketart is an in-process emulator of the `tart` 2.40.1 command line
// behind ports.Exec. It lets the real tart adapter (internal/providers/tart)
// and hostd run in the integration tier without Virtualization.framework
// ("hostd against fake tart", R-TEST-2): it keeps VM and image state, prints
// tart's JSON shapes and error messages, enforces the 2-VM limit the way
// Virtualization.framework does, and models disk-full, crashes and an
// unavailable guest agent. Guest commands (`tart exec`) are answered by a
// Guest emulator (guest.go) implementing the in-VM contract of docs/dev/hostd.md §1.
//
// It is deterministic: no goroutines run on their own, there is no wall clock
// (Now is injectable) and failures are injected explicitly (FailNext, Crash).
package faketart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

const gb = 1000 * 1000 * 1000

// VM is one emulated local VM.
type VM struct {
	Name      string
	DiskGB    int64
	SizeGB    int64
	CPU       int
	MemoryMiB int
	State     string // running | stopped | suspended
	IP        netip.Addr
	Accessed  time.Time
	// AgentDown makes `tart exec` fail as if the guest agent were not running.
	AgentDown bool
	// Guest answers `tart exec` for this VM (created on clone).
	Guest *Guest
	// Args of the last `tart run`.
	RunArgs []string
	proc    *proc
}

// Image is one emulated cached OCI image.
type Image struct {
	Ref      string
	SizeGB   int64
	DiskGB   int64
	Accessed time.Time
	// Private images need TART_REGISTRY_USERNAME/PASSWORD on pull.
	Private bool
}

type failure struct {
	exit   int
	stderr string
}

// Tart is the emulator; it implements ports.Exec for the tart binary.
type Tart struct {
	mu sync.Mutex
	// Now is the clock used for access times (default: a fixed epoch).
	Now func() time.Time
	// MaxRunning is Virtualization.framework's limit (default 2).
	MaxRunning int
	// FreeBytes is the free disk space; clones and pulls fail with
	// "No space left on device" when they would exceed it.
	FreeBytes uint64
	// Registry holds the images that `tart pull` can fetch.
	Registry map[string]Image
	// NewGuest builds the guest emulator of a freshly cloned VM.
	NewGuest func(vm string) *Guest

	vms      map[string]*VM
	images   map[string]*Image
	failNext map[string][]failure
	nextIP   int
	pid      int
	// Pulls counts successful pulls per reference (observable state for tests).
	Pulls map[string]int
}

// New returns an emulator with no VMs, a 2-VM limit and 1 TB free.
func New() *Tart {
	return &Tart{
		Now:        func() time.Time { return time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC) },
		MaxRunning: 2,
		FreeBytes:  1000 * gb,
		Registry:   map[string]Image{},
		NewGuest:   func(string) *Guest { return NewGuest() },
		vms:        map[string]*VM{},
		images:     map[string]*Image{},
		failNext:   map[string][]failure{},
		Pulls:      map[string]int{},
		pid:        1000,
	}
}

var _ ports.Exec = (*Tart)(nil)

// AddImage puts an image into the local OCI cache.
func (t *Tart) AddImage(img Image) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if img.DiskGB == 0 {
		img.DiskGB = 100
	}
	img.Accessed = t.Now()
	t.images[img.Ref] = &img
}

// FailNext makes the next invocation of subcommand (e.g. "clone", "run", "exec")
// fail with tart's exit code and stderr message.
func (t *Tart) FailNext(subcommand string, exit int, stderr string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failNext[subcommand] = append(t.failNext[subcommand], failure{exit: exit, stderr: stderr})
}

// Crash kills a running VM's `tart run` process (Virtualization.framework error).
func (t *Tart) Crash(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if vm := t.vms[name]; vm != nil && vm.proc != nil {
		t.finishLocked(vm, ports.ExecResult{ExitCode: 1, Stderr: []byte("Error: The virtual machine stopped unexpectedly.\n")})
	}
}

// PowerOff stops every running VM (host shutdown); their `tart run` processes exit.
func (t *Tart) PowerOff() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, vm := range t.vms {
		if vm.proc != nil {
			t.finishLocked(vm, ports.ExecResult{ExitCode: 0})
		}
	}
}

// VM returns a copy of an emulated VM, or nil.
func (t *Tart) VM(name string) *VM {
	t.mu.Lock()
	defer t.mu.Unlock()
	vm := t.vms[name]
	if vm == nil {
		return nil
	}
	c := *vm
	return &c
}

// SetAgentDown toggles guest-agent availability for a VM.
func (t *Tart) SetAgentDown(name string, down bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if vm := t.vms[name]; vm != nil {
		vm.AgentDown = down
	}
}

// RunningCount is the number of running VMs (observable invariant check).
func (t *Tart) RunningCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, vm := range t.vms {
		if vm.State == "running" {
			n++
		}
	}
	return n
}

// Run implements ports.Exec.
func (t *Tart) Run(_ context.Context, c ports.Command) (ports.ExecResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dispatchLocked(c), nil
}

func ok(stdout string) ports.ExecResult { return ports.ExecResult{Stdout: []byte(stdout)} }

func fail(code int, format string, a ...any) ports.ExecResult {
	return ports.ExecResult{ExitCode: code, Stderr: []byte(fmt.Sprintf(format, a...) + "\n")}
}

func notExist(name string) ports.ExecResult {
	return fail(2, "the specified VM %q does not exist", name)
}

// flags splits args into positional arguments and --flag values.
func flags(args []string) (pos []string, fl map[string]string) {
	fl = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			k, v, has := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			if !has {
				switch k {
				case "no-graphics", "suspendable", "stacked", "insecure", "overwrite":
					v = "true"
				default:
					if i+1 < len(args) {
						v = args[i+1]
						i++
					}
				}
			}
			fl[k] = v
			continue
		}
		pos = append(pos, a)
	}
	return pos, fl
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func (t *Tart) usedBytesLocked() uint64 {
	var used uint64
	for _, img := range t.images {
		used += uint64(img.SizeGB) * gb
	}
	for _, vm := range t.vms {
		used += uint64(vm.SizeGB) * gb
	}
	return used
}

func (t *Tart) dispatchLocked(c ports.Command) ports.ExecResult {
	if len(c.Args) == 0 {
		return fail(64, "Error: Missing expected argument '<subcommand>'")
	}
	sub := c.Args[0]
	if q := t.failNext[sub]; len(q) > 0 {
		t.failNext[sub] = q[1:]
		return ports.ExecResult{ExitCode: q[0].exit, Stderr: []byte(q[0].stderr + "\n")}
	}
	pos, fl := flags(c.Args[1:])
	switch sub {
	case "--version":
		return ok("2.40.1\n")
	case "list":
		return t.listLocked(fl["source"])
	case "get":
		if len(pos) != 1 {
			return fail(64, "Error: Missing expected argument '<name>'")
		}
		vm := t.vms[pos[0]]
		if vm == nil {
			return notExist(pos[0])
		}
		b, _ := json.Marshal(map[string]any{"OS": "darwin", "CPU": vm.CPU, "Memory": vm.MemoryMiB, "Disk": vm.DiskGB,
			"DiskFormat": "raw", "Size": vm.SizeGB, "Display": "1024x768", "Running": vm.State == "running", "State": vm.State})
		return ok(string(b) + "\n")
	case "clone":
		return t.cloneLocked(pos, fl)
	case "set":
		return t.setLocked(pos, fl)
	case "stop":
		if len(pos) != 1 {
			return fail(64, "Error: Missing expected argument '<name>'")
		}
		vm := t.vms[pos[0]]
		if vm == nil {
			return notExist(pos[0])
		}
		if vm.State != "running" || vm.proc == nil {
			return fail(2, "VM %q is not running", vm.Name)
		}
		t.finishLocked(vm, ports.ExecResult{})
		return ok("")
	case "delete":
		for _, name := range pos {
			vm := t.vms[name]
			if vm == nil {
				return notExist(name)
			}
			if vm.State == "running" {
				return fail(1, "VM %q is running", name)
			}
			delete(t.vms, name)
		}
		return ok("")
	case "ip":
		if len(pos) != 1 {
			return fail(64, "Error: Missing expected argument '<name>'")
		}
		vm := t.vms[pos[0]]
		if vm == nil {
			return notExist(pos[0])
		}
		if vm.State != "running" {
			return fail(1, "no IP address found, is your VM running?")
		}
		return ok(vm.IP.String() + "\n")
	case "exec":
		return t.execLocked(c)
	case "pull":
		return t.pullLocked(c, pos)
	case "prune":
		return t.pruneLocked(fl)
	case "run":
		return fail(1, "faketart: `tart run` must be started with Exec.Start")
	}
	return fail(64, "Error: Unexpected argument '%s'", sub)
}

func (t *Tart) listLocked(source string) ports.ExecResult {
	type entry struct {
		Source   string `json:"Source"`
		Name     string `json:"Name"`
		Disk     int64  `json:"Disk"`
		Size     int64  `json:"Size"`
		Accessed string `json:"Accessed"`
		Running  bool   `json:"Running"`
		State    string `json:"State"`
	}
	out := []entry{}
	if source == "" || source == "local" {
		names := make([]string, 0, len(t.vms))
		for n := range t.vms {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			vm := t.vms[n]
			out = append(out, entry{"local", n, vm.DiskGB, vm.SizeGB, vm.Accessed.UTC().Format(time.RFC3339), vm.State == "running", vm.State})
		}
	}
	if source == "" || source == "oci" {
		refs := make([]string, 0, len(t.images))
		for r := range t.images {
			refs = append(refs, r)
		}
		sort.Strings(refs)
		for _, r := range refs {
			img := t.images[r]
			out = append(out, entry{"OCI", r, img.DiskGB, img.SizeGB, img.Accessed.UTC().Format(time.RFC3339), false, "stopped"})
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return ok(string(b) + "\n")
}

func (t *Tart) cloneLocked(pos []string, fl map[string]string) ports.ExecResult {
	if len(pos) != 2 {
		return fail(64, "Error: Missing expected argument '<new-name>'")
	}
	src, dst := pos[0], pos[1]
	if _, exists := t.vms[dst]; exists && fl["overwrite"] == "" {
		return fail(1, "Error: VM %q already exists, use --overwrite to replace it", dst)
	}
	var disk int64
	switch {
	case t.images[src] != nil:
		disk = t.images[src].DiskGB
		t.images[src].Accessed = t.Now()
	case t.vms[src] != nil:
		disk = t.vms[src].DiskGB
	default:
		if _, remote := t.Registry[src]; !remote {
			return fail(1, "Error: failed to pull %s: manifest unknown", src)
		}
		return fail(1, "Error: failed to pull %s: unauthorized (pull it with credentials first)", src)
	}
	// APFS clones are copy-on-write; model a small fixed allocation per clone.
	const cloneGB = 1
	if t.usedBytesLocked()+cloneGB*gb > t.FreeBytes {
		return fail(1, "Error: No space left on device")
	}
	t.vms[dst] = &VM{Name: dst, DiskGB: disk, SizeGB: cloneGB, CPU: 4, MemoryMiB: 8192, State: "stopped",
		Accessed: t.Now(), Guest: t.NewGuest(dst)}
	return ok("")
}

func (t *Tart) setLocked(pos []string, fl map[string]string) ports.ExecResult {
	if len(pos) != 1 {
		return fail(64, "Error: Missing expected argument '<name>'")
	}
	vm := t.vms[pos[0]]
	if vm == nil {
		return notExist(pos[0])
	}
	if v, has := fl["cpu"]; has {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fail(64, "Error: The value '%s' is invalid for '--cpu <cpu>'", v)
		}
		vm.CPU = n
	}
	if v, has := fl["memory"]; has {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fail(64, "Error: The value '%s' is invalid for '--memory <memory>'", v)
		}
		vm.MemoryMiB = n
	}
	if v, has := fl["disk-size"]; has {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fail(64, "Error: The value '%s' is invalid for '--disk-size <disk-size>'", v)
		}
		if n < vm.DiskGB {
			return fail(1, "Error: the new disk size %d GB is smaller than the current size %d GB, which is not supported", n, vm.DiskGB)
		}
		vm.DiskGB = n
	}
	return ok("")
}

func (t *Tart) execLocked(c ports.Command) ports.ExecResult {
	// tart exec [-i] [-t] <name> <command> ...: everything after the name is the guest argv, verbatim.
	rest := c.Args[1:]
	for len(rest) > 0 && (rest[0] == "-i" || rest[0] == "-t") {
		rest = rest[1:]
	}
	if len(rest) < 2 {
		return fail(64, "Error: Missing expected argument '<command> ...'")
	}
	vm := t.vms[rest[0]]
	if vm == nil {
		return notExist(rest[0])
	}
	if vm.State != "running" {
		return fail(2, "VM %q is not running", vm.Name)
	}
	if vm.AgentDown || vm.Guest == nil {
		return fail(1, "Failed to connect to the VM using its control socket: Connection refused, is the Tart Guest Agent running?")
	}
	return vm.Guest.Handle(rest[1:], c.Stdin)
}

func (t *Tart) pullLocked(c ports.Command, pos []string) ports.ExecResult {
	if len(pos) != 1 {
		return fail(64, "Error: Missing expected argument '<remote-name>'")
	}
	ref := pos[0]
	img, found := t.Registry[ref]
	if !found {
		return fail(1, "Error: failed to pull %s: manifest unknown", ref)
	}
	if img.Private && (envValue(c.Env, "TART_REGISTRY_USERNAME") == "" || envValue(c.Env, "TART_REGISTRY_PASSWORD") == "") {
		return fail(1, "Error: failed to pull %s: unauthorized", ref)
	}
	if _, cached := t.images[ref]; !cached {
		if t.usedBytesLocked()+uint64(img.SizeGB)*gb > t.FreeBytes {
			return fail(1, "Error: No space left on device")
		}
	}
	if img.DiskGB == 0 {
		img.DiskGB = 100
	}
	img.Accessed = t.Now()
	t.images[ref] = &img
	t.Pulls[ref]++
	return ok("pulling manifest...\n")
}

func (t *Tart) pruneLocked(fl map[string]string) ports.ExecResult {
	budget, err := strconv.ParseUint(fl["space-budget"], 10, 64)
	if err != nil {
		return fail(64, "Error: The value '%s' is invalid for '--space-budget <n>'", fl["space-budget"])
	}
	refs := make([]*Image, 0, len(t.images))
	var total uint64
	for _, img := range t.images {
		refs = append(refs, img)
		total += uint64(img.SizeGB)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Accessed.Before(refs[j].Accessed) })
	for _, img := range refs {
		if total <= budget {
			break
		}
		delete(t.images, img.Ref)
		total -= uint64(img.SizeGB)
	}
	return ok("")
}

// Start implements ports.Exec; only `tart run` is long-running.
func (t *Tart) Start(_ context.Context, c ports.Command) (ports.Process, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pid++
	p := &proc{pid: t.pid, done: make(chan struct{})}
	if len(c.Args) == 0 || c.Args[0] != "run" {
		p.finish(fail(64, "faketart: only `tart run` can be started"))
		return p, nil
	}
	if q := t.failNext["run"]; len(q) > 0 {
		t.failNext["run"] = q[1:]
		p.finish(ports.ExecResult{ExitCode: q[0].exit, Stderr: []byte(q[0].stderr + "\n")})
		return p, nil
	}
	pos, fl := flags(c.Args[1:])
	if len(pos) != 1 {
		p.finish(fail(64, "Error: Missing expected argument '<name>'"))
		return p, nil
	}
	if fl["dir"] != "" {
		p.finish(fail(1, "faketart: --dir is forbidden for Cucina VMs (R-MAC-3)"))
		return p, nil
	}
	vm := t.vms[pos[0]]
	if vm == nil {
		p.finish(notExist(pos[0]))
		return p, nil
	}
	if vm.State == "running" {
		p.finish(fail(2, "VM %q is already running", vm.Name))
		return p, nil
	}
	running := 0
	for _, o := range t.vms {
		if o.State == "running" {
			running++
		}
	}
	if running >= t.MaxRunning {
		p.finish(fail(1, "Error: The number of VMs exceeds the system limit"))
		return p, nil
	}
	t.nextIP++
	vm.IP = netip.AddrFrom4([4]byte{192, 168, 64, byte(1 + t.nextIP%250)})
	vm.State = "running"
	vm.Accessed = t.Now()
	vm.RunArgs = append([]string(nil), c.Args...)
	vm.proc = p
	if vm.Guest != nil {
		vm.Guest.Boot()
	}
	p.signal = func(sig string) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		if vm.proc == p {
			t.finishLocked(vm, ports.ExecResult{ExitCode: 0})
		}
		return nil
	}
	return p, nil
}

func (t *Tart) finishLocked(vm *VM, res ports.ExecResult) {
	vm.State = "stopped"
	p := vm.proc
	vm.proc = nil
	if vm.Guest != nil {
		vm.Guest.Shutdown()
	}
	if p != nil {
		p.finish(res)
	}
}

type proc struct {
	pid    int
	done   chan struct{}
	once   sync.Once
	res    ports.ExecResult
	signal func(string) error
}

func (p *proc) finish(res ports.ExecResult) {
	p.once.Do(func() {
		p.res = res
		close(p.done)
	})
}

func (p *proc) PID() int { return p.pid }

func (p *proc) Wait(ctx context.Context) (ports.ExecResult, error) {
	select {
	case <-p.done:
		return p.res, nil
	case <-ctx.Done():
		return ports.ExecResult{}, ctx.Err()
	}
}

func (p *proc) Signal(sig string) error {
	if p.signal == nil {
		return errors.New("process finished")
	}
	return p.signal(sig)
}
