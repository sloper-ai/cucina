// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sloper-ai/cucina/internal/bbconfig"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Machine is the JSON form of bbconfig.Machine: every machine-dependent input
// of the Buildbarn configuration renderer, i.e. what the agent (EC2) or hostd
// (Tart VMs) adds to the pool-level WorkerSettings. It is the document
// `cucina-worker-agent render --machine FILE` reads; field names and meaning
// follow bbconfig.Machine one to one (a structural test keeps them in sync).
type Machine struct {
	OS          string `json:"os"`   // linux | windows | darwin
	Arch        string `json:"arch"` // x86_64 | arm64
	VCPUs       int    `json:"vcpus"`
	MemoryBytes uint64 `json:"memoryBytes"`

	// InstanceStorePath is the mount point of the formatted instance-store
	// volume ("" when the instance has none or it is not used for L1).
	InstanceStorePath  string `json:"instanceStorePath,omitempty"`
	InstanceStoreBytes uint64 `json:"instanceStoreBytes,omitempty"`
	// DataVolumePath is the mount point of the ephemeral EBS data volume.
	DataVolumePath  string `json:"dataVolumePath,omitempty"`
	DataVolumeBytes uint64 `json:"dataVolumeBytes,omitempty"`
	// StateRoot is a root-owned directory on the boot/VM disk for worker state.
	StateRoot      string `json:"stateRoot"`
	StateRootBytes uint64 `json:"stateRootBytes,omitempty"`

	BuildRoot   string `json:"buildRoot"`
	RunDir      string `json:"runDir"`
	PKIDir      string `json:"pkiDir"`
	CABundlePEM string `json:"caBundlePem,omitempty"`
	// CABundleFile is a `render` convenience for shell callers: a PEM file read
	// into CABundlePEM when that is empty. Not a renderer input.
	CABundleFile string `json:"caBundleFile,omitempty"`

	StorageServerName string    `json:"storageServerName,omitempty"`
	StorageIsHostL2   bool      `json:"storageIsHostL2,omitempty"`
	MetricsHost       string    `json:"metricsHost,omitempty"`
	BuildUser         *UnixUser `json:"buildUser,omitempty"`

	EmulatorLDPrefixes         map[string]string `json:"emulatorLdPrefixes,omitempty"`
	XcodeDeveloperDirectories  map[string]string `json:"xcodeDeveloperDirectories,omitempty"`
	NativeBuildDirectoryReason string            `json:"nativeBuildDirectoryReason,omitempty"`
	NativeCacheBytes           uint64            `json:"nativeCacheBytes,omitempty"`
}

// UnixUser is the JSON form of bbconfig.UnixUser.
type UnixUser struct {
	UID            uint32   `json:"uid"`
	GID            uint32   `json:"gid"`
	AdditionalGIDs []uint32 `json:"additionalGids,omitempty"`
}

// BBConfig converts to the renderer's input type.
func (m Machine) BBConfig() bbconfig.Machine {
	out := bbconfig.Machine{
		OS: m.OS, Arch: m.Arch, VCPUs: m.VCPUs, MemoryBytes: m.MemoryBytes,
		InstanceStorePath: m.InstanceStorePath, InstanceStoreBytes: m.InstanceStoreBytes,
		DataVolumePath: m.DataVolumePath, DataVolumeBytes: m.DataVolumeBytes,
		StateRoot: m.StateRoot, StateRootBytes: m.StateRootBytes,
		BuildRoot: m.BuildRoot, RunDir: m.RunDir, PKIDir: m.PKIDir, CABundlePEM: m.CABundlePEM,
		StorageServerName: m.StorageServerName, StorageIsHostL2: m.StorageIsHostL2, MetricsHost: m.MetricsHost,
		EmulatorLDPrefixes: m.EmulatorLDPrefixes, XcodeDeveloperDirectories: m.XcodeDeveloperDirectories,
		NativeBuildDirectoryReason: m.NativeBuildDirectoryReason, NativeCacheBytes: m.NativeCacheBytes,
	}
	if u := m.BuildUser; u != nil {
		out.BuildUser = &bbconfig.UnixUser{UID: u.UID, GID: u.GID, AdditionalGIDs: u.AdditionalGIDs}
	}
	return out
}

// LoadCABundle fills CABundlePEM from CABundleFile when only the file is given.
func (m *Machine) LoadCABundle(fs ports.FS) error {
	if m.CABundlePEM != "" || m.CABundleFile == "" {
		return nil
	}
	b, err := fs.ReadFile(m.CABundleFile)
	if err != nil {
		return fmt.Errorf("machine: caBundleFile: %w", err)
	}
	m.CABundlePEM = string(b)
	return nil
}

// DecodeMachine strictly parses a Machine document (unknown fields fail).
func DecodeMachine(b []byte) (Machine, error) {
	var m Machine
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Machine{}, fmt.Errorf("machine: %w", err)
	}
	if dec.More() {
		return Machine{}, fmt.Errorf("machine: trailing content")
	}
	return m, nil
}

// archName maps GOARCH to the names used in WorkerSettings/EnrollWorkerRequest.
func archName(goarch string) string {
	if goarch == "amd64" {
		return "x86_64"
	}
	return goarch
}

// DiskKind classifies a block device.
type DiskKind string

const (
	// DiskInstanceStore is an EC2 NVMe instance-store device
	// (model "Amazon EC2 NVMe Instance Storage").
	DiskInstanceStore DiskKind = "instance-store"
	// DiskEBS is an EBS volume (model "Amazon Elastic Block Store").
	DiskEBS DiskKind = "ebs"
	// DiskOther is anything else.
	DiskOther DiskKind = "other"
)

// Disk is a whole block device as seen by the machine prober.
type Disk struct {
	// ID is the device path (/dev/nvme1n1) or, on Windows, the disk number.
	ID        string   `json:"id"`
	Kind      DiskKind `json:"kind"`
	Model     string   `json:"model,omitempty"`
	SizeBytes uint64   `json:"sizeBytes"`
	// Root marks the disk holding the operating system; it is never touched.
	Root bool `json:"root,omitempty"`
	// Partitioned disks are never formatted.
	Partitioned bool `json:"partitioned,omitempty"`
	// MountPoints lists where the disk (or its partitions) is mounted.
	MountPoints []string `json:"mountPoints,omitempty"`
}

// Host is the operating-system facts port (monotonic uptime, boot identity,
// memory). The production implementation is OS-specific (host_*.go).
type Host interface {
	// BootID identifies the current boot ("" when the OS has no boot ID; then
	// the boot time is compared instead).
	BootID() (string, error)
	// Uptime is the time since boot from a monotonic source.
	Uptime() (time.Duration, error)
	MemoryBytes() (uint64, error)
}
