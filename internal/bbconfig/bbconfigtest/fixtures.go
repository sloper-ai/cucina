// SPDX-License-Identifier: FSL-1.1-ALv2

// Package bbconfigtest holds the worker fixtures and golden profiles of
// internal/bbconfig: settings as internal/pools hands them out for the shipped
// catalog, typical machines per platform, and Goldens, which renders every
// golden file (the unit test compares them; //internal/bbconfig:goldens writes
// them through write_source_files / `bazel run //:update_goldens`). Test only.
package bbconfigtest

import (
	"fmt"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/bbconfig"
)

// PlaceholderCA is a placeholder, not a certificate: goldens never carry generated
// certificates (public-repo hygiene, PROMPT §12). Boot tests use real ones.
const PlaceholderCA = "-----BEGIN CERTIFICATE-----\nPLACEHOLDER-FOR-THE-CUCINA-CA-BUNDLE\n-----END CERTIFICATE-----\n"

func Props(kv ...string) []*cucinav1.PlatformProperty {
	var out []*cucinav1.PlatformProperty
	for i := 0; i < len(kv); i += 2 {
		out = append(out, &cucinav1.PlatformProperty{Name: kv[i], Value: kv[i+1]})
	}
	return out
}

// Settings as internal/pools hands them out for the shipped catalog
// (platforms/pools.json): concurrency 0 = derive from vCPUs.
func LinuxX86Settings() *cucinav1.WorkerSettings {
	return &cucinav1.WorkerSettings{
		Pool: "linux-x86", Node: "i-0123456789abcdef0",
		Runners: []*cucinav1.RunnerSettings{
			{Name: "native", Platform: Props("OSFamily", "linux", "ISA", "x86-64")},
			{Name: "qemu-rv64g", Platform: Props("OSFamily", "linux", "ISA", "rv64g", "cucina-emulation", "qemu"), Concurrency: 2, Emulator: "qemu-riscv64"},
			{Name: "qemu-s390x", Platform: Props("OSFamily", "linux", "ISA", "s390x", "cucina-emulation", "qemu"), Concurrency: 2, Emulator: "qemu-s390x"},
			{Name: "qemu-arm-a32", Platform: Props("OSFamily", "linux", "ISA", "arm-a32", "cucina-emulation", "qemu"), Concurrency: 2, Emulator: "qemu-arm"},
		},
		SchedulerEndpoint:       "workers.cucina.example.com:8983",
		StorageEndpoint:         "workers.cucina.example.com:8981",
		ServerName:              "workers.cucina.example.com",
		BuildDirectory:          "fuse",
		L1Placement:             "auto",
		MaximumMessageSizeBytes: 16 << 20,
		SizeClass:               1,
		InstanceNamePrefixes:    []string{"main"},
		MetricsPort:             9986,
	}
}

func LinuxArmSettings() *cucinav1.WorkerSettings {
	s := LinuxX86Settings()
	s.Pool, s.Node = "linux-arm", "i-0fedcba9876543210"
	s.Runners = []*cucinav1.RunnerSettings{{Name: "native", Platform: Props("OSFamily", "linux", "ISA", "arm-a64")}}
	return s
}

func WindowsSettings() *cucinav1.WorkerSettings {
	s := LinuxX86Settings()
	s.Pool, s.Node = "windows", "i-0a1b2c3d4e5f60718"
	s.Runners = []*cucinav1.RunnerSettings{{Name: "native", Platform: Props("OSFamily", "windows", "ISA", "x86-64")}}
	s.BuildDirectory = "winfsp"
	return s
}

// macOS VM settings as hostd passes them on: storage is the host's L2,
// scheduler the host's TCP relay; wan_compression stays set by the controller.
func MacSettings() *cucinav1.WorkerSettings {
	return &cucinav1.WorkerSettings{
		Pool: "macos", Node: "TESTSERIAL01/vm-1",
		Runners: []*cucinav1.RunnerSettings{
			{Name: "xcode", Platform: Props("OSFamily", "macos", "ISA", "arm-a64", "xcode-version", "27.0")},
			{Name: "generic", Platform: Props("OSFamily", "macos", "ISA", "arm-a64")},
		},
		SchedulerEndpoint:       "192.0.2.1:8983",
		StorageEndpoint:         "192.0.2.1:8981",
		ServerName:              "workers.cucina.example.com",
		BuildDirectory:          "native",
		L1Placement:             "vm-disk",
		L1SizeBytes:             40 << 30,
		MaximumMessageSizeBytes: 16 << 20,
		WanCompression:          true,
		SizeClass:               1,
		InstanceNamePrefixes:    []string{"main"},
		MetricsPort:             9986,
		PushgatewayUrl:          "http://192.0.2.1:9091",
	}
}

func LinuxMachine() bbconfig.Machine {
	m := bbconfig.DefaultMachine(bbconfig.OSLinux)
	m.Arch, m.VCPUs, m.MemoryBytes = "x86_64", 8, 32<<30
	m.CABundlePEM = PlaceholderCA
	m.BuildUser = &bbconfig.UnixUser{UID: 2000, GID: 2000}
	return m
}

// m6id.2xlarge: 474 GB instance-store NVMe.
func LinuxNVMeMachine() bbconfig.Machine {
	m := LinuxMachine()
	m.InstanceStorePath, m.InstanceStoreBytes = "/mnt/instance-store", 474_000_000_000
	return m
}

// c8g.2xlarge without volumes: 8 vCPUs, 16 GiB.
func LinuxArmMachine() bbconfig.Machine {
	m := LinuxMachine()
	m.Arch, m.MemoryBytes = "arm64", 16<<30
	return m
}

// c7a.2xlarge with a 200 GiB gp3 data volume on D:.
func WindowsMachine() bbconfig.Machine {
	m := bbconfig.DefaultMachine(bbconfig.OSWindows)
	m.Arch, m.VCPUs, m.MemoryBytes = "x86_64", 8, 16<<30
	m.DataVolumePath, m.DataVolumeBytes = `D:\`, 200<<30
	m.CABundlePEM = PlaceholderCA
	return m
}

// A Tart VM: 8 vCPUs, 16 GiB, behind hostd's L2.
func MacMachine() bbconfig.Machine {
	m := bbconfig.DefaultMachine(bbconfig.OSDarwin)
	m.Arch, m.VCPUs, m.MemoryBytes = "arm64", 8, 16<<30
	m.StateRootBytes = 120 << 30
	m.CABundlePEM = PlaceholderCA
	m.StorageServerName = "l2.cucina.internal"
	m.StorageIsHostL2 = true
	m.BuildUser = &bbconfig.UnixUser{UID: 501, GID: 20}
	m.XcodeDeveloperDirectories = map[string]string{"27.0.0.27A266a": "/Applications/Xcode.app/Contents/Developer"}
	return m
}

// Profile is one shipped worker profile: platform × L1 placement × build
// directory mode (R-TEST-6 "Config rendering").
type Profile struct {
	Name     string
	Settings func() *cucinav1.WorkerSettings
	Machine  func() bbconfig.Machine
}

// Profiles are the golden worker profiles.
var Profiles = []Profile{
	{"linux-x86-64_instance-store_fuse", LinuxX86Settings, LinuxNVMeMachine},
	{"linux-aarch64_memory_fuse", LinuxArmSettings, LinuxArmMachine},
	{"windows-x86-64_ebs_winfsp", WindowsSettings, WindowsMachine},
	{"macos_vm-disk_native_l2", MacSettings, MacMachine},
	{"macos_vm-disk_nfsv4_wan", func() *cucinav1.WorkerSettings {
		s := MacSettings()
		s.BuildDirectory = "nfsv4"
		s.StorageEndpoint = "workers.cucina.example.com:8981"
		return s
	}, func() bbconfig.Machine {
		m := MacMachine()
		m.StorageServerName, m.StorageIsHostL2 = "", false
		return m
	}},
}

// HostL2Settings is the golden host L2.
func HostL2Settings() bbconfig.HostL2Settings {
	return bbconfig.HostL2Settings{
		ListenAddresses:         []string{"192.0.2.1:8981"},
		ServerCertificatePath:   "/var/db/cucina/pki/l2-server.crt",
		ServerPrivateKeyPath:    "/var/db/cucina/pki/l2-server.key",
		CABundlePEM:             PlaceholderCA,
		HostSerial:              "TESTSERIAL01",
		UpstreamAddress:         "workers.cucina.example.com:8981",
		UpstreamServerName:      "workers.cucina.example.com",
		ClientCertificatePath:   "/var/db/cucina/pki/host.crt",
		ClientPrivateKeyPath:    "/var/db/cucina/pki/host.key",
		CacheDir:                "/var/db/cucina/l2",
		MaximumMessageSizeBytes: 16 << 20,
		MetricsListenAddress:    "127.0.0.1:9981",
	}
}

// GoldenFiles lists the golden file names Goldens produces; BUILD.bazel's
// genrule declares the same list (the unit test keeps them in sync).
var GoldenFiles = []string{
	"hostl2.json",
	"runner_darwin.json",
	"runner_linux.json",
	"runner_windows.json",
	"worker_linux-aarch64_memory_fuse.json",
	"worker_linux-x86-64_instance-store_fuse.json",
	"worker_macos_vm-disk_native_l2.json",
	"worker_macos_vm-disk_nfsv4_wan.json",
	"worker_windows-x86-64_ebs_winfsp.json",
}

// Goldens renders every golden file: one worker configuration per profile,
// one runner configuration per OS (it depends on nothing else) and the host L2.
func Goldens() (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, p := range Profiles {
		w, err := bbconfig.RenderWorker(p.Settings(), p.Machine())
		if err != nil {
			return nil, fmt.Errorf("worker %s: %w", p.Name, err)
		}
		out["worker_"+p.Name+".json"] = w
		name := "runner_" + p.Machine().OS + ".json"
		if _, ok := out[name]; ok {
			continue
		}
		r, err := bbconfig.RenderRunner(p.Settings(), p.Machine())
		if err != nil {
			return nil, fmt.Errorf("runner %s: %w", p.Name, err)
		}
		out[name] = r
	}
	l2, err := bbconfig.RenderHostL2(HostL2Settings())
	if err != nil {
		return nil, fmt.Errorf("host L2: %w", err)
	}
	out["hostl2.json"] = l2
	return out, nil
}
