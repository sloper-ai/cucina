// SPDX-License-Identifier: FSL-1.1-ALv2

package bbconfig_test

import (
	"encoding/json"
	"testing"

	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_runner"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_worker"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/bb_storage"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/blobstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"pgregory.net/rapid"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/bbconfig"
)

func renderWorker(t *testing.T, s *cucinav1.WorkerSettings, m bbconfig.Machine) *bb_worker.ApplicationConfiguration {
	t.Helper()
	b, err := bbconfig.RenderWorker(s, m)
	require.NoError(t, err)
	var cfg bb_worker.ApplicationConfiguration
	require.NoError(t, protojson.Unmarshal(b, &cfg))
	return &cfg
}

// R-RE-1, R-RE-4, R-XPLAT-3: every runner × instance name prefix registers its
// exact, sorted property set with worker ID {pool, node} (Buildbarn adds
// "thread"), the pool's size class, and the runner's slots; emulated runners
// get their qemu sysroot; all runners share the one bb_runner socket.
func TestWorkerRunnersRegisterExactPlatformsAndWorkerIDs(t *testing.T) {
	s := linuxX86Settings()
	s.InstanceNamePrefixes = []string{"main", "tenant-b"}
	cfg := renderWorker(t, s, linuxNVMeMachine())

	type registration struct {
		Prefix   string
		Platform string
		Slots    uint64
		Env      map[string]string
	}
	var got []registration
	for _, r := range cfg.BuildDirectories[0].Runners {
		assert.Equal(t, map[string]string{"pool": "linux-x86", "node": "i-0123456789abcdef0"}, r.WorkerId)
		assert.Equal(t, uint32(1), r.SizeClass)
		assert.Equal(t, "unix:///run/cucina/runner.sock", r.Endpoint.Address)
		platform := ""
		for i, p := range r.Platform.Properties {
			if i > 0 {
				platform += ";"
			}
			platform += p.Name + "=" + p.Value
		}
		got = append(got, registration{r.InstanceNamePrefix, platform, r.Concurrency, r.EnvironmentVariables})
	}
	rv := map[string]string{"QEMU_LD_PREFIX": "/usr/riscv64-linux-gnu"}
	s390 := map[string]string{"QEMU_LD_PREFIX": "/usr/s390x-linux-gnu"}
	arm := map[string]string{"QEMU_LD_PREFIX": "/usr/arm-linux-gnueabihf"}
	assert.Equal(t, []registration{
		{"main", "ISA=x86-64;OSFamily=linux", 8, nil},
		{"tenant-b", "ISA=x86-64;OSFamily=linux", 8, nil},
		{"main", "ISA=rv64g;OSFamily=linux;cucina-emulation=qemu", 2, rv},
		{"tenant-b", "ISA=rv64g;OSFamily=linux;cucina-emulation=qemu", 2, rv},
		{"main", "ISA=s390x;OSFamily=linux;cucina-emulation=qemu", 2, s390},
		{"tenant-b", "ISA=s390x;OSFamily=linux;cucina-emulation=qemu", 2, s390},
		{"main", "ISA=arm-a32;OSFamily=linux;cucina-emulation=qemu", 2, arm},
		{"tenant-b", "ISA=arm-a32;OSFamily=linux;cucina-emulation=qemu", 2, arm},
	}, got)
}

// R-DATA-3: zstd (encoderLevel 3, bounded pool) only on the WAN hop; in-AZ hops
// and the VM→host-L2 hop stay uncompressed.
func TestWorkerCompressesOnlyTheWANHop(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wan, hostL2 bool
		want        bool
	}{
		{"ec2 in-AZ", false, false, false},
		{"mac VM straight to central", true, false, true},
		{"mac VM via host L2", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := macSettings(), macMachine()
			s.WanCompression, m.StorageIsHostL2 = tc.wan, tc.hostL2
			cfg := renderWorker(t, s, m)
			slow := cfg.Blobstore.ContentAddressableStorage.GetReadCaching().GetSlow().GetGrpc()
			require.NotNil(t, slow)
			assert.Equal(t, tc.want, slow.EnableCompression)
			assert.False(t, cfg.Blobstore.ActionCache.GetGrpc().EnableCompression)
			if tc.want {
				require.NotNil(t, cfg.ZstdPool)
				assert.Equal(t, int32(3), cfg.ZstdPool.EncoderLevel)
				assert.Positive(t, cfg.ZstdPool.MaximumEncoders)
				assert.Positive(t, cfg.ZstdPool.MaximumDecoders)
			} else {
				assert.Nil(t, cfg.ZstdPool)
			}
		})
	}
}

// R-CACHE-2: the worker CAS is readCaching{slow: storage endpoint, fast: local
// (OLD flat schema of the bb_worker build, persistent on disk, with a data
// integrity validation cache), replicator: deduplicating{concurrencyLimiting{local}}}.
func TestWorkerL1IsReadCachingOverLocal(t *testing.T) {
	for _, pr := range profiles {
		t.Run(pr.Name, func(t *testing.T) {
			cfg := renderWorker(t, pr.Settings(), pr.Machine())
			rc := cfg.Blobstore.ContentAddressableStorage.GetReadCaching()
			require.NotNil(t, rc)
			assert.Equal(t, pr.Settings().StorageEndpoint, rc.Slow.GetGrpc().GetClient().GetAddress())
			limiting := rc.Replicator.GetDeduplicating().GetConcurrencyLimiting()
			require.NotNil(t, limiting)
			assert.NotNil(t, limiting.Base.GetLocal())
			assert.Positive(t, limiting.MaximumConcurrency)

			local := rc.Fast.GetLocal()
			require.NotNil(t, local)
			assert.Equal(t, uint32(16), local.KeyLocationMapMaximumGetAttempts)
			assert.Equal(t, int64(64), local.KeyLocationMapMaximumPutAttempts)
			if blocks := local.GetBlocksOnBlockDevice(); blocks != nil {
				assert.NotNil(t, local.GetKeyLocationMapOnBlockDevice())
				assert.NotNil(t, blocks.DataIntegrityValidationCache)
				assert.NotEmpty(t, local.GetPersistent().GetStateDirectoryPath())
			} else {
				assert.NotNil(t, local.GetBlocksInMemory())
				assert.NotNil(t, local.GetKeyLocationMapInMemory())
				assert.Nil(t, local.Persistent)
			}
		})
	}
}

// R-CACHE-2 / ADR 0411: an L1 block is the largest blob the L1 can hold, and
// readCaching fails reads of larger blobs. Blocks therefore never drop below
// MinimumBlockSizeBytes while the budget allows; upstream's 8/24/3 (+3 spare)
// layout is used whenever it fits; Buildbarn's limit of 100 blocks holds; the
// key-location map has room for at least 2x the objects.
func TestL1BlocksHoldTheLargestBlob(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		size := rapid.Uint64Range(64*bbconfig.MiB, 4<<40).Draw(t, "l1SizeBytes")
		memory := rapid.Bool().Draw(t, "memory")
		s, m := linuxX86Settings(), linuxMachine()
		s.L1SizeBytes = size
		if memory {
			s.L1Placement, m.MemoryBytes = bbconfig.PlacementMemory, 3*size
		} else {
			s.L1Placement, m.DataVolumePath, m.DataVolumeBytes = bbconfig.PlacementEBS, "/mnt/data", 2*size
		}
		p, err := bbconfig.PlanWorker(s, m)
		require.NoError(t, err)
		l := p.L1
		blocks := uint64(l.OldBlocks + l.CurrentBlocks + l.NewBlocks + l.SpareBlocks)
		assert.LessOrEqual(t, blocks, uint64(100))
		assert.LessOrEqual(t, l.BlocksBytes, size)
		assert.Equal(t, blocks*l.BlockSizeBytes, l.BlocksBytes)
		minimal, standard := uint64(5), uint64(38) // old+current+new+spare
		if memory {
			minimal, standard = 4, 35
		}
		if size >= minimal*bbconfig.MinimumBlockSizeBytes {
			assert.GreaterOrEqual(t, l.BlockSizeBytes, bbconfig.MinimumBlockSizeBytes)
		}
		if size >= standard*bbconfig.MinimumBlockSizeBytes {
			assert.Equal(t, [3]int32{8, 24, 3}, [3]int32{l.OldBlocks, l.CurrentBlocks, l.NewBlocks})
			assert.Equal(t, standard, blocks)
		}
		// ~66 B per entry, sized for 2x the objects at a 16 KiB average (R-CP-3).
		assert.GreaterOrEqual(t, l.KeyLocationMapEntries, 2*(l.BlocksBytes/(16<<10)))
		if !memory {
			assert.GreaterOrEqual(t, l.KeyLocationMapBytes, 66*l.KeyLocationMapEntries)
		}
	})
}

// R-CACHE-2: "auto" picks instance-store NVMe, else the launch-time data volume,
// else (macOS) the VM disk, else memory when RAM allows, else the root disk.
func TestL1AutoPlacement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		machine func() bbconfig.Machine
		want    string
	}{
		{"nvme", linuxNVMeMachine, bbconfig.PlacementInstanceStore},
		{"data volume", windowsMachine, bbconfig.PlacementEBS},
		{"mac vm", macMachine, bbconfig.PlacementVMDisk},
		{"16 GiB ram", linuxArmMachine, bbconfig.PlacementMemory},
		{"small", func() bbconfig.Machine {
			m := linuxMachine()
			m.MemoryBytes, m.StateRootBytes = 8<<30, 30<<30
			return m
		}, bbconfig.PlacementRootDisk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := linuxX86Settings()
			m := tc.machine()
			switch m.OS {
			case bbconfig.OSWindows:
				s = windowsSettings()
			case bbconfig.OSDarwin:
				s = macSettings()
			}
			s.L1Placement, s.L1SizeBytes = "auto", 0
			p, err := bbconfig.PlanWorker(s, m)
			require.NoError(t, err)
			assert.Equal(t, tc.want, p.L1.Placement)
		})
	}
}

// R-CACHE-3, D7: FUSE on Linux, WinFSP (case-insensitive) on Windows, NFSv4 or
// native on macOS, and native anywhere only with a recorded reason.
func TestBuildDirectoryPerOS(t *testing.T) {
	type want struct{ fuse, winfsp, nfsv4, native, caseInsensitive bool }
	for _, tc := range []struct {
		name     string
		settings *cucinav1.WorkerSettings
		machine  bbconfig.Machine
		want     want
	}{
		{"linux", linuxX86Settings(), linuxMachine(), want{fuse: true}},
		{"windows", windowsSettings(), windowsMachine(), want{winfsp: true, caseInsensitive: true}},
		{"macos nfsv4", func() *cucinav1.WorkerSettings { s := macSettings(); s.BuildDirectory = "nfsv4"; return s }(), macMachine(), want{nfsv4: true}},
		{"macos native", macSettings(), macMachine(), want{native: true}},
		{"linux fallback", linuxX86Settings(), func() bbconfig.Machine {
			m := linuxMachine()
			m.NativeBuildDirectoryReason = "/dev/fuse missing"
			return m
		}(), want{native: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := bbconfig.PlanWorker(tc.settings, tc.machine)
			require.NoError(t, err)
			cfg := renderWorker(t, tc.settings, tc.machine)
			bd := cfg.BuildDirectories[0]
			mount := bd.GetVirtual().GetMount()
			assert.Equal(t, tc.want.fuse, mount.GetFuse() != nil)
			assert.Equal(t, tc.want.winfsp, mount.GetWinfsp() != nil)
			assert.Equal(t, tc.want.nfsv4, mount.GetNfsv4() != nil)
			assert.Equal(t, tc.want.native, bd.GetNative() != nil)
			assert.Equal(t, tc.want.caseInsensitive, bd.GetVirtual().GetCaseInsensitive())
			// FSAC-driven prefetching accompanies every virtual build directory.
			assert.Equal(t, !tc.want.native, cfg.Prefetching != nil)
			if tc.machine.NativeBuildDirectoryReason != "" {
				assert.Contains(t, p.Notes, "build directory: native instead of fuse: /dev/fuse missing")
			}
		})
	}
}

// R-SEC-5, R-MAC-4: Linux actions run as the unprivileged build user (bb_runner
// root + runCommandsAs + process-table cleaning); macOS bb_runner runs in the
// user's session and resolves Xcode; never chroot into the input root.
func TestRunnerPrivilegeSeparation(t *testing.T) {
	render := func(s *cucinav1.WorkerSettings, m bbconfig.Machine) *bb_runner.ApplicationConfiguration {
		b, err := bbconfig.RenderRunner(s, m)
		require.NoError(t, err)
		var cfg bb_runner.ApplicationConfiguration
		require.NoError(t, protojson.Unmarshal(b, &cfg))
		assert.False(t, cfg.ChrootIntoInputRoot)
		assert.True(t, cfg.SetTmpdirEnvironmentVariable)
		return &cfg
	}
	linux := render(linuxX86Settings(), linuxMachine())
	require.NotNil(t, linux.RunCommandsAs)
	assert.Equal(t, uint32(2000), linux.RunCommandsAs.UserId)
	assert.True(t, linux.CleanProcessTable)

	mac := render(macSettings(), macMachine())
	assert.Nil(t, mac.RunCommandsAs)
	assert.False(t, mac.CleanProcessTable)
	assert.Equal(t, "/Applications/Xcode.app/Contents/Developer", mac.AppleXcodeDeveloperDirectories["27.0.0.27A266a"])

	worker := renderWorker(t, linuxX86Settings(), linuxMachine())
	assert.Equal(t, linux.BuildDirectoryPath, worker.BuildDirectories[0].GetVirtual().GetMount().GetMountPath())
}

// R-TEST-7 (fail fast on configuration): inputs Buildbarn would reject, or that
// would make workers collide in the scheduler, fail rendering with a precise error.
func TestRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*cucinav1.WorkerSettings, *bbconfig.Machine)
		want   string
	}{
		{"no node", func(s *cucinav1.WorkerSettings, _ *bbconfig.Machine) { s.Node = "" }, "settings.node"},
		{"no instance name", func(s *cucinav1.WorkerSettings, _ *bbconfig.Machine) { s.InstanceNamePrefixes = nil }, "instance_name_prefixes"},
		{"reserved instance name", func(s *cucinav1.WorkerSettings, _ *bbconfig.Machine) { s.InstanceNamePrefixes = []string{"main/blobs"} }, "reserved keyword"},
		{"same platform twice", func(s *cucinav1.WorkerSettings, _ *bbconfig.Machine) {
			s.Runners[1].Platform = props("ISA", "x86-64", "OSFamily", "linux")
		}, "advertise the same platform"},
		{"no message size", func(s *cucinav1.WorkerSettings, _ *bbconfig.Machine) { s.MaximumMessageSizeBytes = 0 }, "maximum_message_size_bytes"},
		{"fuse on macOS", func(s *cucinav1.WorkerSettings, m *bbconfig.Machine) { *m = macMachine() }, `"fuse" is not available on darwin`},
		{"unknown placement", func(s *cucinav1.WorkerSettings, _ *bbconfig.Machine) { s.L1Placement = "s3" }, "l1_placement"},
		{"instance store missing", func(s *cucinav1.WorkerSettings, _ *bbconfig.Machine) { s.L1Placement = "instance-store" }, "InstanceStorePath"},
		{"no CA", func(_ *cucinav1.WorkerSettings, m *bbconfig.Machine) { m.CABundlePEM = "" }, "CABundlePEM"},
		{"build root inside private state", func(_ *cucinav1.WorkerSettings, m *bbconfig.Machine) { m.BuildRoot = m.StateRoot + "/bb" }, "must not be inside StateRoot"},
		{"derive without vCPUs", func(_ *cucinav1.WorkerSettings, m *bbconfig.Machine) { m.VCPUs = 0 }, "vCPUs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := linuxX86Settings(), linuxMachine()
			tc.mutate(s, &m)
			_, err := bbconfig.RenderWorker(s, m)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// R-CACHE-4, ADR 0001: the host L2 uses bb_storage's NEW nested keyLocationMap
// schema for its `local` block, and every other field type-checks against the
// pinned Go types (identical outside `local` in both bb-storage versions).
func TestHostL2RendersNewSchemaAndTypeChecks(t *testing.T) {
	b, err := bbconfig.RenderHostL2(hostL2Settings())
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	locals := 0
	walk(doc, func(m map[string]any) {
		local, ok := m["local"].(map[string]any)
		if !ok || len(local) == 0 { // {"local": {}} is the replicator mode
			return
		}
		locals++
		klm, ok := local["keyLocationMap"].(map[string]any)
		require.True(t, ok, "local block lacks the nested keyLocationMap")
		for _, flat := range []string{"keyLocationMapOnBlockDevice", "keyLocationMapInMemory", "keyLocationMapMaximumGetAttempts", "keyLocationMapMaximumPutAttempts"} {
			assert.NotContains(t, local, flat, "old flat field in a bb_storage config")
		}
		// Rewrite to the old schema so the strict parse below covers the rest.
		delete(local, "keyLocationMap")
		local["keyLocationMapOnBlockDevice"] = klm["onBlockDevice"]
		local["keyLocationMapMaximumGetAttempts"] = klm["maximumGetAttempts"]
		local["keyLocationMapMaximumPutAttempts"] = klm["maximumPutAttempts"]
	})
	assert.Equal(t, 1, locals)
	old, err := json.Marshal(doc)
	require.NoError(t, err)
	var cfg bb_storage.ApplicationConfiguration
	require.NoError(t, protojson.Unmarshal(old, &cfg))

	rc := cfg.ContentAddressableStorage.GetBackend().GetReadCaching()
	require.NotNil(t, rc)
	assert.True(t, rc.Slow.GetGrpc().GetEnableCompression(), "the L2's upstream hop is the WAN hop (R-DATA-3)")
	assert.Equal(t, int32(3), cfg.ZstdPool.GetEncoderLevel())
	assert.NotNil(t, rc.Fast.GetLocal().GetPersistent(), "the L2 survives hostd restarts")
	assert.IsType(t, &blobstore.BlobReplicatorConfiguration_Deduplicating{}, rc.Replicator.GetMode())
	assert.Contains(t, cfg.GrpcServers[0].GetAuthenticationPolicy().GetTlsClientCertificate().GetValidationJmespathExpression().GetExpression(), "'/TESTSERIAL01/'")
}

// Guards: R-BUILD-6 / Windows CI 37074297279 — L2 cache paths describe the
// target filesystem, not the renderer host; only fully qualified roots are valid.
func TestHostL2CachePaths(t *testing.T) {
	for _, tc := range []struct {
		name, cache, prefix string
	}{
		{"POSIX", "/var/db/cucina/l2", "/var/db/cucina/l2/"},
		{"Windows drive", `C:\cache\l2`, `C:\cache\l2\`},
		{"Windows forward slashes", "C:/cache/l2", `C:\cache\l2\`},
		{"Windows drive root", `C:\`, `C:\`},
		{"UNC directory", `\\fileserver\cucina\l2`, `\\fileserver\cucina\l2\`},
		{"UNC share root", `\\fileserver\cucina`, `\\fileserver\cucina\`},
		{"UNC forward slashes", "//fileserver/cucina/l2", `\\fileserver\cucina\l2\`},
		{"relative", "cache/l2", ""},
		{"drive relative", `C:cache\l2`, ""},
		{"bare drive", "C:", ""},
		{"drive-root relative", `\cache\l2`, ""},
		{"extended drive root", `\\?\C:\cache\l2`, `\\?\C:\cache\l2\`},
		{"extended drive relative", `\\?\C:cache\l2`, ""},
		{"device drive relative", `\\.\C:cache\l2`, ""},
		{"NT drive relative", `\??\C:cache\l2`, ""},
		{"incomplete UNC server", `\\fileserver`, ""},
		{"incomplete UNC share", `\\fileserver\`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := hostL2Settings()
			s.CacheDir = tc.cache
			b, err := bbconfig.RenderHostL2(s)
			if tc.prefix == "" {
				require.ErrorContains(t, err, "CacheDir")
				return
			}
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, json.Unmarshal(b, &doc))
			var paths []string
			walk(doc, func(m map[string]any) {
				for _, key := range []string{"path", "stateDirectoryPath"} {
					if p, ok := m[key].(string); ok {
						paths = append(paths, p)
					}
				}
			})
			require.ElementsMatch(t, []string{tc.prefix + "blocks", tc.prefix + "key_location_map", tc.prefix + "state"}, paths)
			require.Equal(t, []bbconfig.Directory{{Path: tc.cache, Mode: 0o700}, {Path: tc.prefix + "state", Mode: 0o700}}, bbconfig.HostL2Directories(s))
		})
	}
}

func walk(v any, f func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		f(x)
		for _, c := range x {
			walk(c, f)
		}
	case []any:
		for _, c := range x {
			walk(c, f)
		}
	}
}
