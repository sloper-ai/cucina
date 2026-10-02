// SPDX-License-Identifier: FSL-1.1-ALv2

package bbconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// Operating systems (Machine.OS).
const (
	OSLinux   = "linux"
	OSWindows = "windows"
	OSDarwin  = "darwin"
)

// Build directory modes (WorkerSettings.build_directory, R-CACHE-3).
const (
	BuildDirectoryAuto   = "auto"
	BuildDirectoryFUSE   = "fuse"
	BuildDirectoryWinFSP = "winfsp"
	BuildDirectoryNFSv4  = "nfsv4"
	BuildDirectoryNative = "native"
)

// L1 placements (WorkerSettings.l1_placement, R-CACHE-2). PlacementRootDisk is
// only ever a resolved value: "auto" on an EC2 worker without instance store,
// data volume or enough memory.
const (
	PlacementAuto          = "auto"
	PlacementInstanceStore = "instance-store"
	PlacementEBS           = "ebs"
	PlacementMemory        = "memory"
	PlacementVMDisk        = "vm-disk"
	PlacementRootDisk      = "root-disk"
)

// File names below Machine.PKIDir. The worker agent (EC2) and hostd (Tart VMs)
// write the client key pair there; bb_worker re-reads it every
// certificateRefreshInterval, so renewals need no restart.
const (
	ClientCertificateFile = "worker.crt"
	ClientPrivateKeyFile  = "worker.key"
)

// UnixUser is a numeric UNIX identity.
type UnixUser struct {
	UID            uint32
	GID            uint32
	AdditionalGIDs []uint32
}

// Machine holds the machine-dependent inputs of RenderWorker and RenderRunner:
// facts the worker agent (EC2) or hostd (Tart VMs) knows about the machine it
// configures. Paths use the target OS's syntax; rendering never looks at the
// host it runs on, so the output is identical everywhere (goldens).
type Machine struct {
	// OS is "linux", "windows" or "darwin".
	OS string
	// Arch is "x86_64" or "arm64" (informational, recorded in plan notes).
	Arch string
	// VCPUs sizes runners whose concurrency is 0 ("derive") and the I/O
	// concurrency limits.
	VCPUs int
	// MemoryBytes sizes an in-memory L1.
	MemoryBytes uint64

	// InstanceStorePath is the mount point of the formatted instance-store
	// (NVMe) volume; empty when the instance type has none.
	InstanceStorePath  string
	InstanceStoreBytes uint64
	// DataVolumePath is the mount point of the ephemeral EBS data volume created
	// at launch with DeleteOnTermination; empty when there is none.
	DataVolumePath  string
	DataVolumeBytes uint64
	// StateRoot is a root-owned directory (0700) on the boot or VM disk for
	// worker state: the L1 for the vm-disk and root-disk placements, the file
	// pool when no dedicated volume holds it, and the NFSv4 server socket. It
	// must not contain BuildRoot (actions traverse to the build directory).
	StateRoot string
	// StateRootBytes is the free space under StateRoot; 0 means unknown.
	StateRootBytes uint64

	// BuildRoot holds the build directory (BuildRoot/build: the FUSE or NFSv4
	// mount point, or the native build directory) and the native input cache
	// (BuildRoot/cache), which must share a file system for hardlinking. With
	// WinFSP it may be a drive letter such as "B:" (mounted through the
	// MountManager, as bb-deployments recommends). macOS: a case-sensitive APFS
	// volume (R-MAC-4).
	BuildRoot string
	// RunDir holds the bb_runner UNIX socket. Linux: root-owned (bb_runner runs
	// as root); macOS: owned by BuildUser (bb_runner runs in its GUI session).
	RunDir string
	// PKIDir holds ClientCertificateFile and ClientPrivateKeyFile.
	PKIDir string
	// CABundlePEM verifies the scheduler and storage endpoints (Cucina CA
	// bundle, possibly two roots during a rotation).
	CABundlePEM string

	// StorageServerName overrides WorkerSettings.server_name for the storage
	// endpoint (Mac VMs: the name in hostd's L2 server certificate).
	StorageServerName string
	// StorageIsHostL2 marks WorkerSettings.storage_endpoint as hostd's L2 cache
	// on the same Mac. That hop is never compressed, whatever
	// WorkerSettings.wan_compression says: the L2 compresses its WAN hop.
	StorageIsHostL2 bool
	// MetricsHost is the host part of the diagnostics listener
	// (MetricsHost:metrics_port); empty means all interfaces.
	MetricsHost string

	// BuildUser is the unprivileged user build actions run as (R-SEC-5). Linux:
	// bb_runner runs as root and drops to it (runCommandsAs, with process-table
	// cleaning); macOS: bb_runner already runs as it. Virtual build directories
	// present their files as owned by it. Ignored on Windows.
	BuildUser *UnixUser

	// EmulatorLDPrefixes maps RunnerSettings.emulator to QEMU_LD_PREFIX, the
	// cross glibc sysroot qemu-user resolves dynamic loaders in. Nil means
	// DefaultEmulatorLDPrefixes.
	EmulatorLDPrefixes map[string]string
	// XcodeDeveloperDirectories maps XCODE_VERSION_OVERRIDE values to
	// DEVELOPER_DIR paths (macOS bb_runner).
	XcodeDeveloperDirectories map[string]string
	// NativeBuildDirectoryReason, when set, forces a native build directory and
	// records why (for example "WinFSP not installed"): the R-CACHE-3 fallback.
	NativeBuildDirectoryReason string
	// NativeCacheBytes bounds the native build directory's input cache (default
	// DefaultNativeCacheBytes).
	NativeCacheBytes uint64
}

// DefaultEmulatorLDPrefixes are the Ubuntu cross-libc sysroots
// (libc6-<arch>-cross) of the qemu-user runners on linux-x86-64 (R-XPLAT-3).
var DefaultEmulatorLDPrefixes = map[string]string{
	"qemu-riscv64": "/usr/riscv64-linux-gnu",
	"qemu-s390x":   "/usr/s390x-linux-gnu",
	"qemu-arm":     "/usr/arm-linux-gnueabihf",
}

// DefaultMachine returns a Machine with Cucina's standard paths for os (the
// worker images create them). Callers fill in the facts: vCPUs, memory,
// volumes, the CA bundle and the build user.
func DefaultMachine(os string) Machine {
	switch os {
	case OSWindows:
		return Machine{
			OS:        OSWindows,
			StateRoot: `C:\ProgramData\cucina\state`,
			BuildRoot: "B:",
			RunDir:    `C:\ProgramData\cucina\run`,
			PKIDir:    `C:\ProgramData\cucina\pki`,
		}
	case OSDarwin:
		return Machine{
			OS:        OSDarwin,
			StateRoot: "/var/db/cucina",
			BuildRoot: "/Volumes/CucinaBuild",
			RunDir:    "/var/run/cucina-runner",
			PKIDir:    "/var/db/cucina/pki",
		}
	default:
		return Machine{
			OS:        OSLinux,
			StateRoot: "/var/lib/cucina/state",
			BuildRoot: "/var/lib/cucina",
			RunDir:    "/run/cucina",
			PKIDir:    "/etc/cucina/pki",
		}
	}
}

func (m *Machine) validate() error {
	var errs []error
	switch m.OS {
	case OSLinux, OSWindows, OSDarwin:
	default:
		errs = append(errs, fmt.Errorf("machine.OS %q is not one of linux, windows, darwin", m.OS))
	}
	for _, f := range []struct{ name, value string }{
		{"StateRoot", m.StateRoot},
		{"BuildRoot", m.BuildRoot},
		{"RunDir", m.RunDir},
		{"PKIDir", m.PKIDir},
	} {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("machine.%s must be set", f.name))
		} else if !m.isAbs(f.value) {
			errs = append(errs, fmt.Errorf("machine.%s %q must be an absolute path", f.name, f.value))
		}
	}
	if !strings.Contains(m.CABundlePEM, "-----BEGIN CERTIFICATE-----") {
		errs = append(errs, errors.New("machine.CABundlePEM must hold at least one PEM certificate"))
	}
	if m.InstanceStorePath != "" && !m.isAbs(m.InstanceStorePath) {
		errs = append(errs, fmt.Errorf("machine.InstanceStorePath %q must be an absolute path", m.InstanceStorePath))
	}
	if m.DataVolumePath != "" && !m.isAbs(m.DataVolumePath) {
		errs = append(errs, fmt.Errorf("machine.DataVolumePath %q must be an absolute path", m.DataVolumePath))
	}
	if m.StateRoot != "" && m.BuildRoot != "" && m.within(m.BuildRoot, m.StateRoot) {
		// StateRoot is private (0700); actions must be able to traverse to the
		// build directory as the build user.
		errs = append(errs, fmt.Errorf("machine.BuildRoot %q must not be inside StateRoot %q", m.BuildRoot, m.StateRoot))
	}
	if m.VCPUs < 0 {
		errs = append(errs, fmt.Errorf("machine.VCPUs %d must not be negative", m.VCPUs))
	}
	return errors.Join(errs...)
}

var (
	windowsDrive = regexp.MustCompile(`^[A-Za-z]:$`)
	windowsAbs   = regexp.MustCompile(`^[A-Za-z]:(\\|$)`)
)

// isAbs reports whether p is absolute in the target OS's syntax. A bare drive
// letter ("B:") counts: it names a WinFSP mount through the MountManager.
func (m *Machine) isAbs(p string) bool {
	if m.OS == OSWindows {
		return windowsAbs.MatchString(p)
	}
	return path.IsAbs(p)
}

// within reports whether p equals dir or lies below it.
func (m *Machine) within(p, dir string) bool {
	sep := "/"
	if m.OS == OSWindows {
		sep = `\`
		p, dir = strings.ToLower(p), strings.ToLower(dir)
	}
	dir = strings.TrimRight(dir, sep)
	return p == dir || strings.HasPrefix(p, dir+sep)
}

// join joins path elements with the target OS's separator.
func (m *Machine) join(elem ...string) string {
	if m.OS != OSWindows {
		return path.Join(elem...)
	}
	var parts []string
	for i, e := range elem {
		if i > 0 {
			e = strings.Trim(e, `\/`)
		} else {
			e = strings.TrimRight(e, `\/`)
		}
		if e != "" {
			parts = append(parts, strings.ReplaceAll(e, "/", `\`))
		}
	}
	return strings.Join(parts, `\`)
}

// Directory is a directory that must exist before bb_runner and bb_worker start.
type Directory struct {
	Path string
	Mode fs.FileMode
	// BuildUserOwned asks for ownership by Machine.BuildUser (for example the
	// bb_runner socket directory on macOS, where bb_runner runs as that user).
	// Otherwise the directory belongs to the user running bb_worker.
	BuildUserOwned bool
}
