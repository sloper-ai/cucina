// SPDX-License-Identifier: FSL-1.1-ALv2

package boottest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/bbconfig"
	"github.com/sloper-ai/cucina/internal/bbtest"
)

func linuxX86Settings() *cucinav1.WorkerSettings {
	return &cucinav1.WorkerSettings{
		Pool: "linux-x86", Node: "i-0123456789abcdef0",
		Runners: []*cucinav1.RunnerSettings{
			{Name: "native", Platform: props("OSFamily", "linux", "ISA", "x86-64")},
			{Name: "qemu-rv64g", Platform: props("OSFamily", "linux", "ISA", "rv64g", "cucina-emulation", "qemu"), Concurrency: 2, Emulator: "qemu-riscv64"},
			{Name: "qemu-s390x", Platform: props("OSFamily", "linux", "ISA", "s390x", "cucina-emulation", "qemu"), Concurrency: 2, Emulator: "qemu-s390x"},
			{Name: "qemu-arm-a32", Platform: props("OSFamily", "linux", "ISA", "arm-a32", "cucina-emulation", "qemu"), Concurrency: 2, Emulator: "qemu-arm"},
		},
		SchedulerEndpoint:       "localhost:1", // replaced per test
		StorageEndpoint:         "localhost:1",
		ServerName:              "localhost",
		BuildDirectory:          "fuse",
		L1Placement:             "instance-store",
		L1SizeBytes:             64 * mib,
		MaximumMessageSizeBytes: 16 << 20,
		SizeClass:               1,
		InstanceNamePrefixes:    []string{"main"},
	}
}

func macSettings() *cucinav1.WorkerSettings {
	return &cucinav1.WorkerSettings{
		Pool: "macos", Node: "TESTSERIAL01/vm-1",
		Runners: []*cucinav1.RunnerSettings{
			{Name: "xcode", Platform: props("OSFamily", "macos", "ISA", "arm-a64", "xcode-version", "27.0")},
			{Name: "generic", Platform: props("OSFamily", "macos", "ISA", "arm-a64")},
		},
		SchedulerEndpoint:       "localhost:1", // replaced per test
		StorageEndpoint:         "localhost:1",
		ServerName:              "localhost",
		BuildDirectory:          "native",
		L1Placement:             "vm-disk",
		L1SizeBytes:             64 * mib,
		MaximumMessageSizeBytes: 16 << 20,
		SizeClass:               1,
		InstanceNamePrefixes:    []string{"main"},
	}
}

type bootCase struct {
	name     string
	settings func() *cucinav1.WorkerSettings
	// machine returns the machine and the working directory for the processes.
	machine func(t *testing.T, pki *bbtest.PKI, worker bbtest.KeyPair) (bbconfig.Machine, string)
	// unsupported lists what bb_worker and bb_runner may report when the
	// profile needs another OS (its mount, or Windows paths that are relative on
	// POSIX); empty means the worker must register.
	unsupported []string
}

func posixMachine(os string, mutate func(*bbconfig.Machine)) func(*testing.T, *bbtest.PKI, bbtest.KeyPair) (bbconfig.Machine, string) {
	return func(t *testing.T, pki *bbtest.PKI, worker bbtest.KeyPair) (bbconfig.Machine, string) {
		m := testMachine(t, os, pki, worker)
		if mutate != nil {
			mutate(&m)
		}
		return m, t.TempDir()
	}
}

// windowsMachine keeps Windows path syntax. On a POSIX host those paths are
// relative file names, so the processes run in a directory that holds them.
func windowsMachine(t *testing.T, pki *bbtest.PKI, worker bbtest.KeyPair) (bbconfig.Machine, string) {
	dir := bbtest.ShortTempDir(t)
	m := bbconfig.Machine{
		OS: bbconfig.OSWindows, Arch: "x86_64", VCPUs: 2, MemoryBytes: 8 << 30,
		StateRoot: `C:\s`, BuildRoot: "B:", RunDir: `C:\r`, PKIDir: `C:\p`,
		DataVolumePath: `D:\`, DataVolumeBytes: 1 << 30,
		CABundlePEM: pki.CAPEM, MetricsHost: "127.0.0.1",
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, `C:\p\`+bbconfig.ClientCertificateFile), worker.CertPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, `C:\p\`+bbconfig.ClientPrivateKeyFile), worker.KeyPEM, 0o600))
	// Stands in for the WinFSP drive that bb_runner opens at start-up.
	require.NoError(t, os.Mkdir(filepath.Join(dir, `B:\`), 0o755))
	return m, dir
}

// TestRenderedProfilesBoot boots the pinned bb_runner and bb_worker with every
// rendered worker profile. Profiles for this host's OS must register their
// exact runner threads at a (fake, mTLS) scheduler; profiles for other OSes
// must parse strictly and initialise everything up to the build directory
// mount, which only their own OS provides (ADR 0412).
func TestRenderedProfilesBoot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the boot matrix is calibrated for darwin binaries; Linux/Windows hosts validate their own profiles in the campaign")
	}
	cases := []bootCase{
		{"linux-x86-64_instance-store_fuse", linuxX86Settings, posixMachine(bbconfig.OSLinux, func(m *bbconfig.Machine) {
			m.InstanceStorePath, m.InstanceStoreBytes = m.StateRoot+"-nvme", 2<<30
			m.BuildUser = &bbconfig.UnixUser{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
		}), []string{"FUSE is not supported on this platform"}},
		{"linux-aarch64_memory_fuse", func() *cucinav1.WorkerSettings {
			s := linuxX86Settings()
			s.Runners = s.Runners[:1]
			s.Runners[0].Platform = props("OSFamily", "linux", "ISA", "arm-a64")
			s.L1Placement = "memory"
			return s
		}, posixMachine(bbconfig.OSLinux, nil), []string{"FUSE is not supported on this platform"}},
		{"windows-x86-64_ebs_winfsp", func() *cucinav1.WorkerSettings {
			s := linuxX86Settings()
			s.Runners = s.Runners[:1]
			s.Runners[0].Platform = props("OSFamily", "windows", "ISA", "x86-64")
			s.BuildDirectory, s.L1Placement = "winfsp", "ebs"
			return s
		}, windowsMachine, []string{"WinFSP is not supported on this platform", "Path is relative, while an absolute path was expected"}},
		{"macos_vm-disk_native", macSettings, posixMachine(bbconfig.OSDarwin, nil), nil},
		{"macos_vm-disk_nfsv4", func() *cucinav1.WorkerSettings {
			s := macSettings()
			s.BuildDirectory = "nfsv4"
			return s
		}, posixMachine(bbconfig.OSDarwin, nil), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pki := bbtest.NewPKI(t)
			server := pki.LoopbackServer(t, "scheduler", "spiffe://cucina/server/scheduler")
			worker := pki.Workload(t, "worker", workerURI)
			m, dir := tc.machine(t, pki, worker)
			s := tc.settings()
			plan, err := bbconfig.PlanWorker(s, m)
			require.NoError(t, err)
			sched := newFakeScheduler(t, pki, server, plan.TotalThreads)
			s.SchedulerEndpoint = sched.Addr
			s.StorageEndpoint = bbtest.FreeAddr(t) // contacted only when actions run
			s.MetricsPort = uint32(freePort(t))
			createDirectories(t, dir, plan.Directories)

			runnerConfig, err := bbconfig.RenderRunner(s, m)
			require.NoError(t, err)
			socket := plan.RunnerSocket
			if !filepath.IsAbs(socket) {
				socket = filepath.Join(dir, socket)
			}
			r := bbtest.Start(t, "bb_runner", bbtest.Binary(t, bbtest.EnvRunner), runnerConfig, bbtest.WithDir(dir))
			if err := r.WaitReady(context.Background(), bbtest.GRPCReady("unix://"+socket, nil)); err != nil {
				expectPlatformLimit(t, r, tc.unsupported)
			}

			workerConfig, err := bbconfig.RenderWorker(s, m)
			require.NoError(t, err)
			w := startWorker(t, workerConfig, dir, plan)
			t.Cleanup(sched.Release) // runs before bb_worker is stopped
			if len(tc.unsupported) > 0 {
				select {
				case <-w.Done():
				case <-time.After(bbtest.ReadyTimeout):
					t.Fatal("bb_worker kept running although this host cannot provide its build directory")
				}
				expectPlatformLimit(t, w, tc.unsupported)
				return
			}
			waitRegistered(t, w, sched.Registered)

			// R-RE-4: every thread registers {pool, node, thread} for its exact
			// platform and instance name prefix, with the pool's size class.
			threads := map[string][]string{}
			for _, req := range sched.Requests() {
				assert.Equal(t, s.Pool, req.WorkerId["pool"])
				assert.Equal(t, s.Node, req.WorkerId["node"])
				assert.Len(t, req.WorkerId, 3)
				assert.Equal(t, s.SizeClass, req.SizeClass)
				threads[platformKey(req)] = append(threads[platformKey(req)], req.WorkerId["thread"])
			}
			want := map[string][]string{}
			for _, r := range plan.Runners {
				key := r.InstanceNamePrefix + "|"
				for i, p := range r.Properties {
					if i > 0 {
						key += ";"
					}
					key += p.Name + "=" + p.Value
				}
				for i := 0; i < r.Concurrency; i++ {
					want[key] = append(want[key], strconv.Itoa(i))
				}
			}
			for k := range threads {
				slices.Sort(threads[k])
			}
			assert.Equal(t, want, threads)
		})
	}
}

// expectPlatformLimit asserts that p parsed its configuration (strict
// protojson) and stopped only for one of the given platform reasons.
func expectPlatformLimit(t *testing.T, p *bbtest.Process, reasons []string) {
	t.Helper()
	<-p.Done()
	logs := p.Logs()
	require.NotContains(t, logs, "Failed to read configuration", "%s rejected the rendered configuration", p.Name)
	require.NotContains(t, logs, "unknown field", "%s rejected the rendered configuration", p.Name)
	for _, r := range reasons {
		if strings.Contains(logs, r) {
			return
		}
	}
	t.Fatalf("%s stopped for another reason than %q:\n%s", p.Name, reasons, bbtest.Tail(logs, 20))
}

func freePort(t *testing.T) int {
	addr := bbtest.FreeAddr(t)
	_, port, err := splitHostPort(addr)
	require.NoError(t, err)
	return port
}

func splitHostPort(addr string) (string, int, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			p, err := strconv.Atoi(addr[i+1:])
			return addr[:i], p, err
		}
	}
	return "", 0, fmt.Errorf("no port in %q", addr)
}
