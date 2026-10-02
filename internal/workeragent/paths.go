// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"path"
	"strings"

	"github.com/sloper-ai/cucina/internal/bbconfig"
)

// PathStyle selects the path syntax of the target machine, so Windows layouts
// can be computed (and tested) on any host OS.
type PathStyle int

const (
	// Unix paths use "/".
	Unix PathStyle = iota
	// Windows paths use "\" and drive letters.
	Windows
)

// StyleFor returns the path style of a GOOS.
func StyleFor(goos string) PathStyle {
	if goos == "windows" {
		return Windows
	}
	return Unix
}

// Join joins path elements with the style's separator and cleans the result.
func (s PathStyle) Join(elem ...string) string {
	if s == Unix {
		return path.Join(elem...)
	}
	var parts []string
	for _, e := range elem {
		e = strings.ReplaceAll(e, "/", `\`)
		for _, p := range strings.Split(e, `\`) {
			if p != "" && p != "." {
				parts = append(parts, p)
			}
		}
	}
	out := strings.Join(parts, `\`)
	if len(parts) == 1 && strings.HasSuffix(parts[0], ":") {
		out += `\` // "C:" alone would mean the current directory on drive C
	}
	return out
}

// Dir returns all but the last element of p.
func (s PathStyle) Dir(p string) string {
	if s == Unix {
		return path.Dir(p)
	}
	i := strings.LastIndex(p, `\`)
	if i < 0 {
		return "."
	}
	return s.Join(p[:i])
}

// Paths is the on-disk layout of a worker (contracts §5.2), aligned with
// bbconfig.DefaultMachine. Linux:
//
//	/etc/cucina/bb/{worker,runner}.json       rendered Buildbarn configuration
//	/etc/cucina/pki/{worker.crt,ca.crt}       leaf certificate and CA bundle
//	/etc/cucina/pki/worker.key                per-boot private key (0600, wiped at shutdown)
//	/etc/cucina/env                           service environment (EnvironmentFile=)
//	/var/lib/cucina/agent.state.json          pool, node, generation, enrolled-at, settings
//	/etc/cucina/deadman.env                   limits of the image's shell dead-man timer (synced from settings)
//	/var/lib/cucina                           bbconfig StateRoot (0700) and BuildRoot (build/)
//	/var/lib/cucina/ephemeral                 instance-store mount (the image's format unit, else the agent)
//	/var/lib/cucina/data                      ephemeral EBS data-volume mount
//	/run/cucina/{last-activity,last-contact}  dead-man timestamps (R-POOL-7); bb_runner socket
//
// Windows keeps configuration, state and run files under C:\ProgramData\cucina,
// uses the image's instance-store drive (D:) or mounts volumes below C:\bb, and
// mounts the WinFSP build directory on B:.
type Paths struct {
	Style         PathStyle
	BBDir         string
	PKIDir        string
	EnvFile       string
	DeadmanConfig string
	StateFile     string
	StateRoot     string
	RunDir        string
	BuildRoot     string
	// InstanceStoreMount and DataVolumeMount are where the agent mounts an
	// unmounted instance-store device or EBS data volume.
	InstanceStoreMount string
	DataVolumeMount    string
	// SysRoot prefixes /proc and /sys reads (tests point it at a fixture tree).
	SysRoot string
}

// DefaultPaths returns the layout for a GOOS.
func DefaultPaths(goos string) Paths {
	bm := bbconfig.DefaultMachine(goos)
	switch goos {
	case "windows":
		s := Windows
		data := `C:\ProgramData\cucina`
		return Paths{
			Style: s, BBDir: s.Join(data, "bb"), PKIDir: bm.PKIDir, EnvFile: s.Join(data, "env"),
			DeadmanConfig: s.Join(data, "deadman.json"),
			StateFile:     s.Join(data, "agent.state.json"), StateRoot: bm.StateRoot, RunDir: bm.RunDir,
			BuildRoot: bm.BuildRoot, InstanceStoreMount: `C:\bb\ephemeral`, DataVolumeMount: `C:\bb\data`,
		}
	case "darwin":
		// Tart VMs: hostd renders through `render`; bootstrap is EC2-only.
		return Paths{
			Style: Unix, BBDir: "/var/db/cucina/bb", PKIDir: bm.PKIDir, EnvFile: "/var/db/cucina/env",
			StateFile: "/var/db/cucina/agent.state.json", StateRoot: bm.StateRoot, RunDir: bm.RunDir,
			BuildRoot: bm.BuildRoot, SysRoot: "/",
		}
	default:
		return Paths{
			Style: Unix, BBDir: "/etc/cucina/bb", PKIDir: bm.PKIDir, EnvFile: "/etc/cucina/env",
			DeadmanConfig: "/etc/cucina/deadman.env",
			StateFile:     "/var/lib/cucina/agent.state.json", StateRoot: bm.StateRoot, RunDir: bm.RunDir,
			BuildRoot: bm.BuildRoot, SysRoot: "/",
			InstanceStoreMount: "/var/lib/cucina/ephemeral", DataVolumeMount: "/var/lib/cucina/data",
		}
	}
}

// Under returns a copy of p re-rooted below root (tests and dry runs on a
// developer machine). Drive letters are dropped on Windows.
func (p Paths) Under(root string) Paths {
	re := func(x string) string {
		if x == "" {
			return ""
		}
		if p.Style == Windows && len(x) >= 2 && x[1] == ':' {
			x = x[2:]
		}
		return p.Style.Join(root, x)
	}
	return Paths{
		Style: p.Style, BBDir: re(p.BBDir), PKIDir: re(p.PKIDir), EnvFile: re(p.EnvFile),
		DeadmanConfig: re(p.DeadmanConfig), StateFile: re(p.StateFile), StateRoot: re(p.StateRoot),
		RunDir: re(p.RunDir), BuildRoot: re(p.BuildRoot), SysRoot: re(p.SysRoot),
		InstanceStoreMount: re(p.InstanceStoreMount), DataVolumeMount: re(p.DataVolumeMount),
	}
}

// MountFor returns where a device of the given placement is mounted when the
// agent has to mount it itself.
func (p Paths) MountFor(placement string) string {
	if placement == PlacementInstanceStore {
		return p.InstanceStoreMount
	}
	return p.DataVolumeMount
}

// CertFile is the leaf certificate (PEM).
func (p Paths) CertFile() string { return p.Style.Join(p.PKIDir, bbconfig.ClientCertificateFile) }

// KeyFile is the per-boot private key (PEM PKCS#8, 0600).
func (p Paths) KeyFile() string { return p.Style.Join(p.PKIDir, bbconfig.ClientPrivateKeyFile) }

// CAFile is the Cucina CA bundle (PEM).
func (p Paths) CAFile() string { return p.Style.Join(p.PKIDir, "ca.crt") }

// WorkerConfig is bb_worker's configuration.
func (p Paths) WorkerConfig() string { return p.Style.Join(p.BBDir, "worker.json") }

// RunnerConfig is bb_runner's configuration.
func (p Paths) RunnerConfig() string { return p.Style.Join(p.BBDir, "runner.json") }

// LastActivityFile holds the Unix time of the last observed build activity.
func (p Paths) LastActivityFile() string { return p.Style.Join(p.RunDir, "last-activity") }

// LastContactFile holds the Unix time of the last successful scheduler contact.
func (p Paths) LastContactFile() string { return p.Style.Join(p.RunDir, "last-contact") }
