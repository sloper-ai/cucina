// SPDX-License-Identifier: FSL-1.1-ALv2

package bbconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"strconv"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/bb_worker"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/cas"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/filesystem"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/configuration/filesystem/virtual"
	authpb "github.com/buildbarn/bb-storage/pkg/proto/auth"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/blobstore"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/blockdevice"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/digest"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/eviction"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/global"
	grpcpb "github.com/buildbarn/bb-storage/pkg/proto/configuration/grpc"
	httpserver "github.com/buildbarn/bb-storage/pkg/proto/configuration/http/server"
	tlspb "github.com/buildbarn/bb-storage/pkg/proto/configuration/tls"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/zstd"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
)

// Fixed tunables (docs/dev/buildbarn.md explains each one).
const (
	// certificateRefreshInterval: bb_worker/bb_storage re-read rotated key pairs.
	certificateRefreshInterval = 5 * time.Minute
	// klmMaximumGetAttempts/PutAttempts: upstream's recommended values.
	klmMaximumGetAttempts = 16
	klmMaximumPutAttempts = 64
	// dataIntegrityValidationCache: validated blobs are not re-hashed on every
	// random read for this long (upstream recommends a limited value, e.g. 4h).
	integrityCacheEntries  = 100000
	integrityCacheDuration = 4 * time.Hour
	minimumEpochInterval   = 300 * time.Second
	// Virtual build directories (recommended values from virtual.proto/bb_worker.proto).
	fuseAttributeValidity            = 300 * time.Second
	fuseBackgroundTasksPerThread     = 128
	maxExecutionTimeCompensation     = time.Hour
	maxWritableFileUploadDelay       = 60 * time.Second
	nfsv4EnforcedLeaseTime           = 120 * time.Second
	nfsv4AnnouncedLeaseTime          = 60 * time.Second
	bloomFilterBitsPerPath           = 14
	bloomFilterMaximumSizeBytes      = 65536
	directoryCacheCount              = 10000
	directoryCacheBytes              = 16 << 20
	pushInterval                     = 30 * time.Second
	wanEncoderLevel                  = 3 // klauspost SpeedBetterCompression ≈ zstd 7–8 (R-DATA-3)
	maxZstdEncoders, maxZstdDecoders = 16, 32
)

// RenderWorker renders bb_worker's configuration (bb_worker.ApplicationConfiguration,
// pinned bb-remote-execution schema) for one machine.
func RenderWorker(s *cucinav1.WorkerSettings, m Machine) ([]byte, error) {
	p, err := PlanWorker(s, m)
	if err != nil {
		return nil, err
	}
	return marshal(workerConfiguration(s, &m, p))
}

func workerConfiguration(s *cucinav1.WorkerSettings, m *Machine, p *WorkerPlan) *bb_worker.ApplicationConfiguration {
	storageName := m.StorageServerName
	if storageName == "" {
		storageName = s.GetServerName()
	}
	storage := &grpcpb.ClientConfiguration{Address: s.GetStorageEndpoint(), Tls: clientTLS(m, storageName)}

	// L1 (R-CACHE-2): reads are served from the local cache, misses are fetched
	// once from the central storage (or hostd's L2) by a deduplicating replicator
	// that merges concurrent fetches of the same blob; writes and FindMissing go
	// to the slow side only.
	contentAddressableStorage := &blobstore.BlobAccessConfiguration{Backend: &blobstore.BlobAccessConfiguration_ReadCaching{
		ReadCaching: &blobstore.ReadCachingBlobAccessConfiguration{
			Slow:       grpcBackend(storage, p.Compression),
			Fast:       localL1(m, &p.L1),
			Replicator: deduplicatingReplicator(p.InputDownloadConcurrency),
		},
	}}

	cfg := &bb_worker.ApplicationConfiguration{
		Blobstore: &blobstore.BlobstoreConfiguration{
			ContentAddressableStorage: contentAddressableStorage,
			// Workers only write the AC (UpdateActionResult); the frontend's
			// worker listener applies authorization and completeness checking.
			ActionCache: grpcBackend(storage, false),
		},
		MaximumMessageSizeBytes:  int64(s.GetMaximumMessageSizeBytes()),
		Scheduler:                &grpcpb.ClientConfiguration{Address: s.GetSchedulerEndpoint(), Tls: clientTLS(m, s.GetServerName())},
		Global:                   workerGlobal(s, m, p),
		BuildDirectories:         []*bb_worker.BuildDirectoryConfiguration{buildDirectory(m, p, s)},
		FilePool:                 &filesystem.FilePoolConfiguration{Backend: &filesystem.FilePoolConfiguration_BlockDevice{BlockDevice: fileDevice(p.FilePoolPath, p.FilePoolBytes)}},
		OutputUploadConcurrency:  int64(p.OutputUploadConcurrency),
		InputDownloadConcurrency: int64(p.InputDownloadConcurrency),
		DirectoryCache: &cas.CachingDirectoryFetcherConfiguration{
			MaximumCount:           directoryCacheCount,
			MaximumSizeBytes:       directoryCacheBytes,
			CacheReplacementPolicy: eviction.CacheReplacementPolicy_LEAST_RECENTLY_USED,
		},
	}
	if p.BuildDirectory != BuildDirectoryNative {
		// FSAC-driven prefetching (R-CACHE-3): profiles of earlier, similar
		// actions let the virtual file system fetch inputs ahead of time.
		cfg.Prefetching = &bb_worker.PrefetchingConfiguration{
			FileSystemAccessCache:       grpcBackend(storage, false),
			BloomFilterBitsPerPath:      bloomFilterBitsPerPath,
			BloomFilterMaximumSizeBytes: bloomFilterMaximumSizeBytes,
		}
	}
	if p.Compression {
		// A bounded pool (R-DATA-3) used by the compressing CAS client only.
		cfg.ZstdPool = &zstd.PoolConfiguration{
			MaximumEncoders: int64(min(p.OutputUploadConcurrency, maxZstdEncoders)),
			MaximumDecoders: int64(min(p.InputDownloadConcurrency, maxZstdDecoders)),
			EncoderLevel:    wanEncoderLevel,
		}
	}
	return cfg
}

func clientTLS(m *Machine, serverName string) *tlspb.ClientConfiguration {
	return &tlspb.ClientConfiguration{
		ServerCertificateAuthorities: m.CABundlePEM,
		ServerName:                   serverName,
		ClientKeyPair: &tlspb.X509KeyPair{KeyPair: &tlspb.X509KeyPair_Files_{Files: &tlspb.X509KeyPair_Files{
			CertificatePath: m.join(m.PKIDir, ClientCertificateFile),
			PrivateKeyPath:  m.join(m.PKIDir, ClientPrivateKeyFile),
			RefreshInterval: durationpb.New(certificateRefreshInterval),
		}}},
	}
}

func grpcBackend(client *grpcpb.ClientConfiguration, compression bool) *blobstore.BlobAccessConfiguration {
	return &blobstore.BlobAccessConfiguration{Backend: &blobstore.BlobAccessConfiguration_Grpc{
		Grpc: &blobstore.GrpcBlobAccessConfiguration{Client: client, EnableCompression: compression},
	}}
}

func deduplicatingReplicator(concurrency int) *blobstore.BlobReplicatorConfiguration {
	return &blobstore.BlobReplicatorConfiguration{Mode: &blobstore.BlobReplicatorConfiguration_Deduplicating{
		Deduplicating: &blobstore.BlobReplicatorConfiguration{Mode: &blobstore.BlobReplicatorConfiguration_ConcurrencyLimiting{
			ConcurrencyLimiting: &blobstore.ConcurrencyLimitingBlobReplicatorConfiguration{
				Base:               &blobstore.BlobReplicatorConfiguration{Mode: &blobstore.BlobReplicatorConfiguration_Local{Local: &emptypb.Empty{}}},
				MaximumConcurrency: int64(concurrency),
			},
		}},
	}}
}

func fileDevice(path string, sizeBytes uint64) *blockdevice.Configuration {
	return &blockdevice.Configuration{Source: &blockdevice.Configuration_File{
		File: &blockdevice.FileConfiguration{Path: path, SizeBytes: int64(sizeBytes)},
	}}
}

// localL1 renders the L1 in the OLD flat `local` schema of the bb-storage
// version bb_worker is built against (ADR 0001).
func localL1(m *Machine, l *L1Plan) *blobstore.BlobAccessConfiguration {
	local := &blobstore.LocalBlobAccessConfiguration{
		KeyLocationMapMaximumGetAttempts: klmMaximumGetAttempts,
		KeyLocationMapMaximumPutAttempts: klmMaximumPutAttempts,
		OldBlocks:                        l.OldBlocks,
		CurrentBlocks:                    l.CurrentBlocks,
		NewBlocks:                        l.NewBlocks,
	}
	if l.Placement == PlacementMemory {
		local.KeyLocationMapBackend = &blobstore.LocalBlobAccessConfiguration_KeyLocationMapInMemory_{
			KeyLocationMapInMemory: &blobstore.LocalBlobAccessConfiguration_KeyLocationMapInMemory{Entries: int64(l.KeyLocationMapEntries)},
		}
		local.BlocksBackend = &blobstore.LocalBlobAccessConfiguration_BlocksInMemory_{
			BlocksInMemory: &blobstore.LocalBlobAccessConfiguration_BlocksInMemory{BlockSizeBytes: int64(l.BlockSizeBytes)},
		}
	} else {
		local.KeyLocationMapBackend = &blobstore.LocalBlobAccessConfiguration_KeyLocationMapOnBlockDevice{
			KeyLocationMapOnBlockDevice: fileDevice(m.join(l.Dir, "key_location_map"), l.KeyLocationMapBytes),
		}
		local.BlocksBackend = &blobstore.LocalBlobAccessConfiguration_BlocksOnBlockDevice_{
			BlocksOnBlockDevice: &blobstore.LocalBlobAccessConfiguration_BlocksOnBlockDevice{
				Source:      fileDevice(m.join(l.Dir, "blocks"), l.BlocksBytes),
				SpareBlocks: l.SpareBlocks,
				// Required for fast random access from virtual build directories.
				DataIntegrityValidationCache: &digest.ExistenceCacheConfiguration{
					CacheSize:              integrityCacheEntries,
					CacheDuration:          durationpb.New(integrityCacheDuration),
					CacheReplacementPolicy: eviction.CacheReplacementPolicy_LEAST_RECENTLY_USED,
				},
			},
		}
		// Survives bb_worker restarts and (macOS) VM shutdown (R-CACHE-2).
		local.Persistent = &blobstore.LocalBlobAccessConfiguration_Persistent{
			StateDirectoryPath:   m.join(l.Dir, "state"),
			MinimumEpochInterval: durationpb.New(minimumEpochInterval),
		}
	}
	return &blobstore.BlobAccessConfiguration{Backend: &blobstore.BlobAccessConfiguration_Local{Local: local}}
}

func workerGlobal(s *cucinav1.WorkerSettings, m *Machine, p *WorkerPlan) *global.Configuration {
	g := &global.Configuration{}
	if port := s.GetMetricsPort(); port > 0 {
		g.DiagnosticsHttpServer = &global.DiagnosticsHTTPServerConfiguration{
			HttpServers: []*httpserver.Configuration{{
				ListenAddresses:      []string{net.JoinHostPort(m.MetricsHost, strconv.FormatUint(uint64(port), 10))},
				AuthenticationPolicy: &httpserver.AuthenticationPolicy{Policy: &httpserver.AuthenticationPolicy_Allow{Allow: &authpb.AuthenticationMetadata{}}},
			}},
			EnablePrometheus: true,
		}
	}
	if url := s.GetPushgatewayUrl(); url != "" {
		g.PrometheusPushgateway = &global.PrometheusPushgatewayConfiguration{
			Url:          url,
			Job:          "bb_worker",
			Grouping:     map[string]string{"pool": s.GetPool(), "node": s.GetNode()},
			PushInterval: durationpb.New(pushInterval),
		}
	}
	if p.BuildDirectory == BuildDirectoryNative && m.OS != OSWindows {
		// Native build directories are created by bb_worker but written by
		// actions running as another user (Linux runCommandsAs, macOS GUI session);
		// as in bb-deployments, umask 0 keeps them writable. State directories are
		// 0700, so the world-writable L1 and file pool files stay unreachable.
		g.SetUmask = &global.SetUmaskConfiguration{Umask: 0}
	}
	if proto.Equal(g, &global.Configuration{}) {
		return nil
	}
	return g
}

func buildDirectory(m *Machine, p *WorkerPlan, s *cucinav1.WorkerSettings) *bb_worker.BuildDirectoryConfiguration {
	bd := &bb_worker.BuildDirectoryConfiguration{}
	if p.BuildDirectory == BuildDirectoryNative {
		cacheBytes := m.NativeCacheBytes
		if cacheBytes == 0 {
			cacheBytes = DefaultNativeCacheBytes
		}
		bd.Backend = &bb_worker.BuildDirectoryConfiguration_Native{Native: &bb_worker.NativeBuildDirectoryConfiguration{
			BuildDirectoryPath:     p.BuildDirectoryPath,
			CacheDirectoryPath:     p.NativeCachePath,
			MaximumCacheFileCount:  nativeCacheFileCount,
			MaximumCacheSizeBytes:  int64(cacheBytes),
			CacheReplacementPolicy: eviction.CacheReplacementPolicy_LEAST_RECENTLY_USED,
		}}
	} else {
		mount := &virtual.MountConfiguration{MountPath: p.MountPath}
		vbd := &bb_worker.VirtualBuildDirectoryConfiguration{
			Mount:                               mount,
			MaximumExecutionTimeoutCompensation: durationpb.New(maxExecutionTimeCompensation),
			// Alphabetical listings keep irreproducible actions' outputs stable,
			// which favours cache hits over detecting irreproducibility.
			ShuffleDirectoryListings:       false,
			MaximumWritableFileUploadDelay: durationpb.New(maxWritableFileUploadDelay),
		}
		switch p.BuildDirectory {
		case BuildDirectoryFUSE:
			mount.Backend = &virtual.MountConfiguration_Fuse{Fuse: &virtual.FUSEMountConfiguration{
				DirectoryEntryValidity: durationpb.New(fuseAttributeValidity),
				InodeAttributeValidity: durationpb.New(fuseAttributeValidity),
				// Actions run as the build user, not as bb_worker (root).
				AllowOther:             true,
				MountMethod:            virtual.FUSEMountConfiguration_DIRECT_AND_FUSERMOUNT,
				MaximumBackgroundTasks: uint32(min(fuseBackgroundTasksPerThread*p.TotalThreads, 65535)),
			}}
		case BuildDirectoryNFSv4:
			mount.Backend = &virtual.MountConfiguration_Nfsv4{Nfsv4: &virtual.NFSv4MountConfiguration{
				OperatingSystem: &virtual.NFSv4MountConfiguration_Darwin{Darwin: &virtual.NFSv4DarwinMountConfiguration{
					SocketPath: p.NFSv4Socket,
					// minor_version unset: the newest the guest's macOS supports
					// (NFSv4.1, which needs no hidden_files_pattern).
				}},
				EnforcedLeaseTime:  durationpb.New(nfsv4EnforcedLeaseTime),
				AnnouncedLeaseTime: durationpb.New(nfsv4AnnouncedLeaseTime),
			}}
		case BuildDirectoryWinFSP:
			mount.Backend = &virtual.MountConfiguration_Winfsp{Winfsp: &emptypb.Empty{}}
			// Bazel on Windows assumes a case-insensitive file system.
			vbd.CaseInsensitive = true
		}
		bd.Backend = &bb_worker.BuildDirectoryConfiguration_Virtual{Virtual: vbd}
	}

	endpoint := "unix://" + p.RunnerSocket
	if m.OS == OSWindows {
		endpoint = "unix:" + p.RunnerSocket
	}
	for _, r := range p.Runners {
		rc := &bb_worker.RunnerConfiguration{
			Endpoint:                 &grpcpb.ClientConfiguration{Address: endpoint},
			Concurrency:              uint64(r.Concurrency),
			InstanceNamePrefix:       r.InstanceNamePrefix,
			Platform:                 &remoteexecution.Platform{},
			SizeClass:                s.GetSizeClass(),
			MaximumFilePoolFileCount: filePoolFilesPerAction,
			MaximumFilePoolSizeBytes: FilePoolBytesPerAction,
			WorkerId:                 map[string]string{"pool": s.GetPool(), "node": s.GetNode()},
			EnvironmentVariables:     maps.Clone(r.Environment),
		}
		for _, prop := range r.Properties {
			rc.Platform.Properties = append(rc.Platform.Properties, &remoteexecution.Platform_Property{Name: prop.Name, Value: prop.Value})
		}
		if m.BuildUser != nil && m.OS != OSWindows && p.BuildDirectory != BuildDirectoryNative {
			rc.BuildDirectoryOwnerUserId = m.BuildUser.UID
			rc.BuildDirectoryOwnerGroupId = m.BuildUser.GID
		}
		bd.Runners = append(bd.Runners, rc)
	}
	return bd
}

// marshal renders a configuration message as indented protojson with stable
// formatting (protojson deliberately randomises whitespace).
func marshal(msg proto.Message) ([]byte, error) {
	raw, err := protojson.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", msg.ProtoReflect().Descriptor().FullName(), err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}
