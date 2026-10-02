// SPDX-License-Identifier: FSL-1.1-ALv2

// Package render adapts internal/bbconfig (agent bbconfig) to hostd: the
// bb_worker/bb_runner configuration of a Tart VM (in-VM contract,
// docs/dev/hostd.md §1.5) and the host L2 bb_storage configuration.
package render

import (
	"fmt"
	"io/fs"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/bbconfig"
	"github.com/sloper-ai/cucina/internal/hostd/guest"
	"github.com/sloper-ai/cucina/internal/hostd/l2"
)

// VM is what hostd knows about one Tart VM it configures.
type VM struct {
	VCPUs       int
	MemoryBytes uint64
	CABundlePEM string // Cucina CA bundle + host L2 CA
	Console     guest.Console
	Manifest    guest.Manifest
}

// Dir is a directory to create in the guest before the Buildbarn jobs start.
type Dir struct {
	Path           string
	Mode           fs.FileMode
	BuildUserOwned bool
}

// Rendered is the per-boot output.
type Rendered struct {
	WorkerJSON []byte
	RunnerJSON []byte
	Dirs       []Dir
	Notes      []string
}

// Renderer renders configurations for hostd.
type Renderer interface {
	Worker(ws *cucinav1.WorkerSettings, vm VM) (Rendered, error)
	l2.Renderer
}

// BBConfig is the production Renderer over internal/bbconfig.
type BBConfig struct{}

// Machine maps a VM to bbconfig's machine facts with the in-VM contract's paths.
func Machine(vm VM) bbconfig.Machine {
	m := bbconfig.DefaultMachine(bbconfig.OSDarwin)
	m.Arch = "arm64"
	m.VCPUs = vm.VCPUs
	m.MemoryBytes = vm.MemoryBytes
	m.StateRoot = "/var/db/cucina"
	m.BuildRoot = "/Volumes/cucina"
	m.RunDir = "/var/run/cucina"
	m.PKIDir = guest.PKIDir
	m.CABundlePEM = vm.CABundlePEM
	m.StorageServerName = l2.ServerName
	m.StorageIsHostL2 = true
	m.BuildUser = &bbconfig.UnixUser{UID: uint32(vm.Console.UID), GID: uint32(vm.Console.GID)}
	dev := vm.Manifest.Xcode.DeveloperDir
	if dev == "" {
		dev = "/Applications/Xcode.app/Contents/Developer"
	}
	// Bazel's XCODE_VERSION_OVERRIDE (e.g. 27.0.0.27A266a) selects DEVELOPER_DIR.
	if o := vm.Manifest.Xcode.XcodeVersionOverride; o != "" {
		m.XcodeDeveloperDirectories = map[string]string{o: dev}
	}
	return m
}

// Worker implements Renderer.
func (BBConfig) Worker(ws *cucinav1.WorkerSettings, vm VM) (Rendered, error) {
	m := Machine(vm)
	plan, err := bbconfig.PlanWorker(ws, m)
	if err != nil {
		return Rendered{}, err
	}
	w, err := bbconfig.RenderWorker(ws, m)
	if err != nil {
		return Rendered{}, fmt.Errorf("bb_worker config: %w", err)
	}
	r, err := bbconfig.RenderRunner(ws, m)
	if err != nil {
		return Rendered{}, fmt.Errorf("bb_runner config: %w", err)
	}
	out := Rendered{WorkerJSON: w, RunnerJSON: r, Notes: plan.Notes}
	for _, d := range plan.Directories {
		out.Dirs = append(out.Dirs, Dir{Path: d.Path, Mode: d.Mode, BuildUserOwned: d.BuildUserOwned})
	}
	return out, nil
}

// HostL2 implements l2.Renderer.
func (BBConfig) HostL2(s l2.Settings) ([]byte, error) {
	return bbconfig.RenderHostL2(bbconfig.HostL2Settings{
		ListenAddresses:         []string{s.ListenAddress},
		ServerCertificatePath:   s.ServerCertPath,
		ServerPrivateKeyPath:    s.ServerKeyPath,
		CABundlePEM:             string(s.CABundlePEM),
		HostSerial:              s.HostSerial,
		UpstreamAddress:         s.UpstreamAddress,
		UpstreamServerName:      s.UpstreamServerName,
		ClientCertificatePath:   s.ClientCertPath,
		ClientPrivateKeyPath:    s.ClientKeyPath,
		CacheDir:                s.CacheDir,
		CacheSizeBytes:          s.CacheSizeBytes,
		MaximumMessageSizeBytes: s.MaximumMessageSizeBytes,
		MetricsListenAddress:    s.MetricsListenAddress,
	})
}
