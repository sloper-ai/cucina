// SPDX-License-Identifier: FSL-1.1-ALv2

package bbconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"strings"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
)

// Sizing constants (docs/dev/buildbarn.md "L1 sizing", ADR 0411).
const (
	MiB = uint64(1) << 20
	GiB = uint64(1) << 30

	// MinimumBlockSizeBytes is the largest blob an L1 must be able to hold.
	// LocalBlobAccess refuses blobs larger than one block, and readCaching then
	// fails the *read* of such a blob ("Replication failed"), so block counts
	// shrink before blocks get smaller than this.
	MinimumBlockSizeBytes = 512 * MiB
	// DefaultVMDiskL1Bytes is the macOS VM L1 (R-CACHE-2: 40 GiB).
	DefaultVMDiskL1Bytes = 40 * GiB
	// DefaultNativeCacheBytes bounds the native build directory's input cache.
	DefaultNativeCacheBytes = 16 * GiB
	// FilePoolBytesPerAction is each runner thread's quota for files written by
	// an action (outputs and temporary files on virtual build directories).
	FilePoolBytesPerAction = 4 * GiB

	keyLocationMapRecordBytes  = 66      // bb-storage BlockDeviceBackedLocationRecordSize
	keyLocationMapBytesPerSlot = 8 << 10 // one KLM entry per 8 KiB of blocks: 2–10x the objects at 16–80 KiB average
	minKeyLocationMapEntries   = 1 << 16
	nativeCacheFileCount       = 1 << 18
	filePoolFilesPerAction     = 100000
)

// Property is one REAPI platform property.
type Property struct {
	Name  string
	Value string
}

// RunnerPlan is one bb_worker RunnerConfiguration: one runner platform for one
// instance name prefix (one Buildbarn size class queue).
type RunnerPlan struct {
	Name               string
	InstanceNamePrefix string
	// Properties are sorted by name, then value (the scheduler rejects any
	// other order).
	Properties  []Property
	Concurrency int
	// Environment is added to every action of this runner (QEMU_LD_PREFIX).
	Environment map[string]string
}

// L1Plan is the worker-local CAS cache (the `fast` side of readCaching).
type L1Plan struct {
	Placement string // instance-store | ebs | memory | vm-disk | root-disk
	Dir       string // holds blocks, key_location_map and state/ ("" in memory)
	// BlocksBytes is the size of the blocks backend (memory: all blocks;
	// disk: the blocks file, spare blocks included).
	BlocksBytes uint64
	// BlockSizeBytes is the size of one block, which is also the largest blob
	// the L1 can store.
	BlockSizeBytes                                   uint64
	OldBlocks, CurrentBlocks, NewBlocks, SpareBlocks int32
	KeyLocationMapEntries                            uint64
	KeyLocationMapBytes                              uint64 // file size on disk; 0 in memory
	Persistent                                       bool
}

// WorkerPlan holds every decision RenderWorker and RenderRunner make for one
// machine. The agent and hostd log Notes and create Directories before
// starting bb_runner and bb_worker.
type WorkerPlan struct {
	OS string
	// BuildDirectory is the resolved mode: fuse, winfsp, nfsv4 or native.
	BuildDirectory string
	// BuildDirectoryPath is where actions run (bb_runner buildDirectoryPath).
	BuildDirectoryPath string
	// MountPath is the virtual file system mount path given to bb_worker.
	MountPath string
	// NativeCachePath is the native build directory's input cache.
	NativeCachePath string
	// RunnerSocket is bb_runner's UNIX socket; NFSv4Socket the NFSv4 server's.
	RunnerSocket string
	NFSv4Socket  string

	L1            L1Plan
	FilePoolPath  string
	FilePoolBytes uint64

	Runners                  []RunnerPlan
	TotalThreads             int
	InputDownloadConcurrency int
	OutputUploadConcurrency  int
	// Compression enables zstd on the storage hop (WAN only, R-DATA-3).
	Compression bool

	Directories []Directory
	Notes       []string
}

// PlanWorker validates settings and machine and resolves every
// machine-dependent decision: build directory mode and paths, L1 placement and
// sizing, file pool, runner concurrency and platform properties.
func PlanWorker(s *cucinav1.WorkerSettings, m Machine) (*WorkerPlan, error) {
	if s == nil {
		return nil, errors.New("worker settings are missing")
	}
	if err := errors.Join(validateSettings(s), m.validate()); err != nil {
		return nil, fmt.Errorf("invalid worker configuration input: %w", err)
	}
	p := &WorkerPlan{OS: m.OS}
	if err := p.planBuildDirectory(s, &m); err != nil {
		return nil, err
	}
	if err := p.planRunners(s, &m); err != nil {
		return nil, err
	}
	p.InputDownloadConcurrency = clamp(4*max(m.VCPUs, 1), 16, 256)
	p.OutputUploadConcurrency = clamp(2*max(m.VCPUs, 1), 16, 128)
	if err := p.planL1(s, &m); err != nil {
		return nil, err
	}
	p.planFilePool(&m)
	p.Compression = s.GetWanCompression() && !m.StorageIsHostL2
	if s.GetWanCompression() && m.StorageIsHostL2 {
		p.note("storage hop to hostd's L2 is uncompressed; the L2 compresses the WAN hop")
	}
	p.planDirectories(&m)
	return p, nil
}

func validateSettings(s *cucinav1.WorkerSettings) error {
	var errs []error
	if s.GetPool() == "" {
		errs = append(errs, errors.New("settings.pool must be set (worker-id label pool)"))
	}
	if s.GetNode() == "" {
		errs = append(errs, errors.New("settings.node must be set (worker-id label node)"))
	}
	if s.GetSchedulerEndpoint() == "" {
		errs = append(errs, errors.New("settings.scheduler_endpoint must be set"))
	}
	if s.GetStorageEndpoint() == "" {
		errs = append(errs, errors.New("settings.storage_endpoint must be set"))
	}
	if n := s.GetMaximumMessageSizeBytes(); n == 0 || n > math.MaxInt64 {
		errs = append(errs, fmt.Errorf("settings.maximum_message_size_bytes %d must be in [1, 2^63)", n))
	}
	if len(s.GetInstanceNamePrefixes()) == 0 {
		errs = append(errs, errors.New("settings.instance_name_prefixes must not be empty"))
	}
	seenPrefix := map[string]bool{}
	for _, in := range s.GetInstanceNamePrefixes() {
		if err := validateInstanceName(in); err != nil {
			errs = append(errs, err)
		}
		if seenPrefix[in] {
			errs = append(errs, fmt.Errorf("settings.instance_name_prefixes lists %q twice", in))
		}
		seenPrefix[in] = true
	}
	if len(s.GetRunners()) == 0 {
		errs = append(errs, errors.New("settings.runners must not be empty"))
	}
	names := map[string]bool{}
	platforms := map[string]string{}
	for i, r := range s.GetRunners() {
		if r.GetName() == "" {
			errs = append(errs, fmt.Errorf("settings.runners[%d].name must be set", i))
		} else if names[r.GetName()] {
			errs = append(errs, fmt.Errorf("settings.runners: duplicate runner %q", r.GetName()))
		}
		names[r.GetName()] = true
		props, err := sortedProperties(r.GetPlatform())
		if err != nil {
			errs = append(errs, fmt.Errorf("settings.runners[%s]: %w", r.GetName(), err))
			continue
		}
		// Two runners with the same property set would register the same worker
		// IDs ({pool, node, thread}) in one size class queue.
		key := propertiesKey(props)
		if other, ok := platforms[key]; ok {
			errs = append(errs, fmt.Errorf("settings.runners %q and %q advertise the same platform {%s}", other, r.GetName(), key))
		}
		platforms[key] = r.GetName()
	}
	return errors.Join(errs...)
}

var reservedInstanceNameKeywords = []string{"blobs", "uploads", "actions", "actionResults", "operations", "capabilities", "compressed-blobs"}

// validateInstanceName applies bb-storage's digest.NewInstanceName rules, which
// bb_worker enforces at start-up for every runner's instance name prefix.
func validateInstanceName(in string) error {
	if strings.HasPrefix(in, "/") || strings.HasSuffix(in, "/") || strings.Contains(in, "//") {
		return fmt.Errorf("instance name prefix %q contains redundant slashes", in)
	}
	for _, c := range strings.Split(in, "/") {
		if slices.Contains(reservedInstanceNameKeywords, c) {
			return fmt.Errorf("instance name prefix %q contains reserved keyword %q", in, c)
		}
	}
	return nil
}

func sortedProperties(in []*cucinav1.PlatformProperty) ([]Property, error) {
	if len(in) == 0 {
		return nil, errors.New("platform has no properties (every Cucina runner advertises at least OSFamily and ISA)")
	}
	out := make([]Property, 0, len(in))
	for _, p := range in {
		if p.GetName() == "" {
			return nil, errors.New("platform property with an empty name")
		}
		out = append(out, Property{Name: p.GetName(), Value: p.GetValue()})
	}
	slices.SortFunc(out, func(a, b Property) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
	for i := 1; i < len(out); i++ {
		if out[i] == out[i-1] {
			return nil, fmt.Errorf("platform property %s=%s is listed twice", out[i].Name, out[i].Value)
		}
	}
	return out, nil
}

func propertiesKey(props []Property) string {
	parts := make([]string, len(props))
	for i, p := range props {
		parts[i] = p.Name + "=" + p.Value
	}
	return strings.Join(parts, ";")
}

func (p *WorkerPlan) note(format string, args ...any) {
	p.Notes = append(p.Notes, fmt.Sprintf(format, args...))
}

func (p *WorkerPlan) planBuildDirectory(s *cucinav1.WorkerSettings, m *Machine) error {
	mode := s.GetBuildDirectory()
	if mode == "" || mode == BuildDirectoryAuto {
		// Same defaults as internal/pools: macOS stays native until the
		// NFSv4-in-Tart measurement says otherwise (R-CACHE-3).
		switch m.OS {
		case OSLinux:
			mode = BuildDirectoryFUSE
		case OSWindows:
			mode = BuildDirectoryWinFSP
		default:
			mode = BuildDirectoryNative
		}
		p.note("build directory: auto resolved to %s on %s", mode, m.OS)
	}
	supported := map[string]string{
		BuildDirectoryFUSE:   OSLinux,
		BuildDirectoryWinFSP: OSWindows,
		BuildDirectoryNFSv4:  OSDarwin,
		BuildDirectoryNative: m.OS,
	}
	osForMode, ok := supported[mode]
	switch {
	case !ok:
		return fmt.Errorf("settings.build_directory %q is not one of fuse, winfsp, nfsv4, native, auto", mode)
	case osForMode != m.OS:
		return fmt.Errorf("settings.build_directory %q is not available on %s (only on %s)", mode, m.OS, osForMode)
	}
	if m.NativeBuildDirectoryReason != "" && mode != BuildDirectoryNative {
		p.note("build directory: native instead of %s: %s", mode, m.NativeBuildDirectoryReason)
		mode = BuildDirectoryNative
	}
	if mode == BuildDirectoryNative && m.OS == OSWindows {
		p.note("build directory: native on Windows hardlinks inputs; NTFS caps a file at 1023 hardlinks")
	}
	p.BuildDirectory = mode

	switch {
	case mode == BuildDirectoryWinFSP && windowsDrive.MatchString(m.BuildRoot):
		// MountManager form, as bb-deployments recommends (winfsp#573).
		drive := strings.ToUpper(m.BuildRoot)
		p.MountPath = `\\.\` + drive
		p.BuildDirectoryPath = drive + `\`
	case mode == BuildDirectoryNative:
		p.BuildDirectoryPath = m.join(m.BuildRoot, "build")
		p.NativeCachePath = m.join(m.BuildRoot, "cache")
	default:
		p.MountPath = m.join(m.BuildRoot, "build")
		p.BuildDirectoryPath = p.MountPath
	}
	if mode == BuildDirectoryNFSv4 {
		p.NFSv4Socket = m.join(m.StateRoot, "nfsv4.sock")
	}
	p.RunnerSocket = m.join(m.RunDir, "runner.sock")
	return nil
}

func (p *WorkerPlan) planRunners(s *cucinav1.WorkerSettings, m *Machine) error {
	prefixes := m.ldPrefixes()
	for _, r := range s.GetRunners() {
		props, err := sortedProperties(r.GetPlatform())
		if err != nil {
			return err
		}
		concurrency := int(r.GetConcurrency())
		if concurrency == 0 {
			if m.VCPUs <= 0 {
				return fmt.Errorf("runner %q derives its concurrency from vCPUs, but machine.VCPUs is unknown", r.GetName())
			}
			concurrency = m.VCPUs
			if r.GetEmulator() != "" {
				// Emulated runners are 5–20x slower and memory hungry (R-XPLAT-3).
				concurrency = max(1, m.VCPUs/4)
			}
		}
		var env map[string]string
		if e := r.GetEmulator(); e != "" {
			if m.OS != OSLinux {
				return fmt.Errorf("runner %q: emulator %q requires a linux worker (qemu-user + binfmt_misc)", r.GetName(), e)
			}
			if sysroot, ok := prefixes[e]; ok {
				env = map[string]string{"QEMU_LD_PREFIX": sysroot}
			} else {
				p.note("runner %s: no QEMU_LD_PREFIX for emulator %s; only static binaries will run", r.GetName(), e)
			}
		}
		for _, in := range s.GetInstanceNamePrefixes() {
			p.Runners = append(p.Runners, RunnerPlan{
				Name:               r.GetName(),
				InstanceNamePrefix: in,
				Properties:         props,
				Concurrency:        concurrency,
				Environment:        env,
			})
			p.TotalThreads += concurrency
		}
	}
	if n := len(s.GetInstanceNamePrefixes()); n > 1 {
		p.note("runners: %d instance name prefixes multiply the runner threads (one queue each); slots per runner stay as configured", n)
	}
	return nil
}

func (m *Machine) ldPrefixes() map[string]string {
	if m.EmulatorLDPrefixes != nil {
		return m.EmulatorLDPrefixes
	}
	return DefaultEmulatorLDPrefixes
}

// blockTiers are the old/current/new/spare block counts tried in order. The
// first is upstream's recommendation (R-CP-3); the smaller ones keep blocks at
// least MinimumBlockSizeBytes large when the L1 budget is small.
var blockTiers = [][4]int32{
	{8, 24, 3, 3},
	{2, 6, 1, 1},
	{1, 2, 1, 1},
}

func (p *WorkerPlan) planL1(s *cucinav1.WorkerSettings, m *Machine) error {
	placement := s.GetL1Placement()
	if placement == "" || placement == PlacementAuto {
		switch {
		case m.InstanceStorePath != "":
			placement = PlacementInstanceStore
		case m.DataVolumePath != "":
			placement = PlacementEBS
		case m.OS == OSDarwin:
			placement = PlacementVMDisk
		case m.MemoryBytes >= 16*GiB:
			placement = PlacementMemory
		default:
			placement = PlacementRootDisk
		}
		p.note("l1: auto resolved to %s", placement)
	}
	l := L1Plan{Placement: placement}
	var volume uint64 // bytes of the file system or memory holding the L1; 0 = unknown
	switch placement {
	case PlacementInstanceStore:
		if m.InstanceStorePath == "" || m.InstanceStoreBytes == 0 {
			return errors.New("l1 placement instance-store needs machine.InstanceStorePath and InstanceStoreBytes")
		}
		l.Dir, volume = m.join(m.InstanceStorePath, "l1"), m.InstanceStoreBytes
	case PlacementEBS:
		if m.DataVolumePath == "" || m.DataVolumeBytes == 0 {
			return errors.New("l1 placement ebs needs machine.DataVolumePath and DataVolumeBytes")
		}
		l.Dir, volume = m.join(m.DataVolumePath, "l1"), m.DataVolumeBytes
	case PlacementVMDisk, PlacementRootDisk:
		l.Dir, volume = m.join(m.StateRoot, "l1"), m.StateRootBytes
	case PlacementMemory:
		if m.MemoryBytes == 0 {
			return errors.New("l1 placement memory needs machine.MemoryBytes")
		}
		volume = m.MemoryBytes
	default:
		return fmt.Errorf("settings.l1_placement %q is not one of instance-store, ebs, memory, vm-disk, auto", placement)
	}

	size := s.GetL1SizeBytes()
	maxFraction := 0.7
	if placement == PlacementMemory {
		maxFraction = 0.5
	}
	switch {
	case size > 0:
	case placement == PlacementInstanceStore || placement == PlacementEBS:
		size = uint64(0.6 * float64(volume))
	case placement == PlacementVMDisk:
		size = DefaultVMDiskL1Bytes
	case placement == PlacementMemory:
		size = min(volume/4, 16*GiB)
	case volume > 0: // root disk with known free space
		size = min(uint64(0.3*float64(volume)), 64*GiB)
	default:
		size = 8 * GiB
		p.note("l1: free space under %s unknown; assuming 8 GiB", m.StateRoot)
	}
	if limit := uint64(maxFraction * float64(volume)); volume > 0 && size > limit {
		p.note("l1: %d bytes requested, clamped to %.0f%% of %d bytes", size, 100*maxFraction, volume)
		size = limit
	}

	disk := placement != PlacementMemory
	tier := blockTiers[len(blockTiers)-1]
	for _, t := range blockTiers {
		if size/uint64(blockCount(t, disk)) >= MinimumBlockSizeBytes {
			tier = t
			break
		}
	}
	l.OldBlocks, l.CurrentBlocks, l.NewBlocks = tier[0], tier[1], tier[2]
	if disk {
		l.SpareBlocks = tier[3]
	}
	n := uint64(blockCount(tier, disk))
	if size < n*MiB {
		return fmt.Errorf("l1 of %d bytes is too small for %d blocks of at least 1 MiB", size, n)
	}
	l.BlockSizeBytes = size / n
	l.BlocksBytes = l.BlockSizeBytes * n
	if l.BlockSizeBytes < MinimumBlockSizeBytes {
		p.note("l1: blocks of %d bytes are smaller than %d; reads of larger blobs fail", l.BlockSizeBytes, MinimumBlockSizeBytes)
	}
	l.KeyLocationMapEntries = max(minKeyLocationMapEntries, l.BlocksBytes/keyLocationMapBytesPerSlot)
	if disk {
		l.KeyLocationMapBytes = roundUp(l.KeyLocationMapEntries*keyLocationMapRecordBytes, 4096)
		l.Persistent = true
	}
	p.L1 = l
	return nil
}

func blockCount(t [4]int32, disk bool) int32 {
	n := t[0] + t[1] + t[2]
	if disk {
		n += t[3]
	}
	return n
}

func (p *WorkerPlan) planFilePool(m *Machine) {
	root, volume := m.StateRoot, m.StateRootBytes
	used := uint64(0)
	switch p.L1.Placement {
	case PlacementInstanceStore:
		root, volume = m.InstanceStorePath, m.InstanceStoreBytes
		used = p.L1.BlocksBytes + p.L1.KeyLocationMapBytes
	case PlacementEBS:
		root, volume = m.DataVolumePath, m.DataVolumeBytes
		used = p.L1.BlocksBytes + p.L1.KeyLocationMapBytes
	case PlacementVMDisk, PlacementRootDisk:
		used = p.L1.BlocksBytes + p.L1.KeyLocationMapBytes
	}
	p.FilePoolPath = m.join(root, "filepool", "pool")
	want := uint64(p.TotalThreads) * FilePoolBytesPerAction
	// The pool is a sparse file: its size only caps what actions may write. Keep
	// that cap inside the volume's remaining space when it is known.
	if volume > used {
		if budget := (volume - used) * 9 / 10; budget < want {
			want = max(budget, GiB)
			p.note("file pool: capped at %d bytes (%d threads x %d wanted)", want, p.TotalThreads, FilePoolBytesPerAction)
		}
	}
	p.FilePoolBytes = want
}

func (p *WorkerPlan) planDirectories(m *Machine) {
	dirs := map[string]Directory{}
	add := func(path string, mode fs.FileMode, buildUser bool) {
		dirs[path] = Directory{Path: path, Mode: mode, BuildUserOwned: buildUser}
	}
	add(m.StateRoot, 0o700, false)
	if !windowsDrive.MatchString(m.BuildRoot) {
		add(m.BuildRoot, 0o755, false)
	}
	if p.L1.Dir != "" {
		add(p.L1.Dir, 0o700, false)
		add(m.join(p.L1.Dir, "state"), 0o700, false)
	}
	add(dirOf(m, p.FilePoolPath), 0o700, false)
	// FUSE and NFSv4 mount on an existing directory; WinFSP creates its mount
	// point itself (drive letter or a directory that must not exist).
	if p.BuildDirectory == BuildDirectoryFUSE || p.BuildDirectory == BuildDirectoryNFSv4 {
		add(p.MountPath, 0o755, false)
	}
	if p.BuildDirectory == BuildDirectoryNative {
		add(p.BuildDirectoryPath, 0o755, false)
		add(p.NativeCachePath, 0o700, false)
	}
	// bb_runner creates its socket here: it runs as root on Linux and in the
	// build user's GUI session on macOS.
	add(m.RunDir, 0o700, m.OS == OSDarwin && m.BuildUser != nil)
	keys := make([]string, 0, len(dirs))
	for k := range dirs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		p.Directories = append(p.Directories, dirs[k])
	}
}

func dirOf(m *Machine, p string) string {
	sep := "/"
	if m.OS == OSWindows {
		sep = `\`
	}
	if i := strings.LastIndex(p, sep); i > 0 {
		return p[:i]
	}
	return p
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

func roundUp(v, to uint64) uint64 { return (v + to - 1) / to * to }
