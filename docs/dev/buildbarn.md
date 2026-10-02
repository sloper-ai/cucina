<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Buildbarn configuration outside the cluster

`internal/bbconfig` renders the Buildbarn configuration of every worker VM (`bb_worker`, `bb_runner`) and of the
host-level L2 cache on Mac hosts (`bb_storage`). `internal/buildqueue` is the controller's client of the
scheduler's BuildQueueState API. `internal/bbtest` boots the pinned release binaries for tests. The cluster side
(frontend, storage shards, scheduler) is rendered by the Helm chart.

Decisions: ADR 0001 (dual schema), 0410 (runner topology), 0411 (L1 sizing), 0412 (boot tests), 0413 (host L2),
0414 (BuildQueue adapter).

## Pinned versions and the two schemas

| Component | Release (GitHub) | Built against bb-storage | `local` blob access schema | Rendered by |
| --- | --- | --- | --- | --- |
| `bb_worker`, `bb_runner`, `bb_scheduler` | bb-remote-execution `20260930T173749Z-1a3be95` | `ae61334ea798` (2026-09-06) | **old flat**: `keyLocationMapOnBlockDevice` / `keyLocationMapInMemory`, `keyLocationMapMaximumGetAttempts`, `keyLocationMapMaximumPutAttempts` | `bbconfig.RenderWorker`/`RenderRunner` (typed Go protos, protojson); scheduler: chart |
| `bb_storage` | bb-storage `20260930T153215Z-086b011` | itself | **new nested**: `keyLocationMap: {inMemory \| onBlockDevice, maximumGetAttempts, maximumPutAttempts}` (lossymap) | `bbconfig.RenderHostL2` (text/template); frontend/shards: chart |

Only `blobstore.proto` (the `local` block) and the new `lossymap.proto` differ between the two bb-storage revisions;
every other configuration proto (grpc, tls, global, x509, jmespath, auth, blockdevice, digest, zstd, bb_storage) is
byte-identical. That is why the host L2 test can parse everything but `local` with the old Go types.

`go.mod` pins `github.com/buildbarn/bb-storage v0.0.0-20260906092937-ae61334ea798`,
`github.com/buildbarn/bb-remote-execution v0.0.0-20260930173749-1a3be9574872` and
`github.com/bazelbuild/remote-apis v0.0.0-20260331222004-becdd8f9ff81` (the version both pin). Only `pkg/proto/...`
packages are imported.

darwin_arm64 assets (verified against each release's `sha256` asset; all platforms are pinned in
`tools/pinned.bzl` as `@bb_release`):

| Asset | SHA-256 |
| --- | --- |
| `bb_scheduler.darwin_arm64` | `6d28e3852ca8ece61c23c7146690ca6313fb01f62b836b2120771bb49812c7ce` |
| `bb_worker.darwin_arm64` | `67fbcf00951b7e1ca90aa07781b496795b3d9f7b588c8f03e59ecbe3298656ff` |
| `bb_runner.darwin_arm64` | `0c8ffbf17da304808a1580d430e0eaeea4dba47fd04b5f2434348980ec181105` |
| `bb_storage.darwin_arm64` | `ca879430c4383cb723144eef3e3069ff8cf58422bd7f0a54c7e4290603139bc3` |

Every binary evaluates its file as Jsonnet and then parses strict protojson (bb-storage `pkg/util/jsonnet.go:47`):
an unknown field aborts start-up. protojson's output is re-indented (`json.Indent`) so goldens are stable.

## Inputs

* `cucinav1.WorkerSettings` (enrollment.proto): pool, node, runners (exact property sets, concurrency, emulator),
  endpoints, server name, build directory mode, L1 placement/size, message size, `wan_compression`, size class,
  instance name prefixes, metrics port, Pushgateway URL. Produced by `internal/pools` for EC2 (`EnrollWorker`) and
  passed on by hostd for Tart VMs (`IssueVMIdentity`).
* `bbconfig.Machine`: OS/arch, vCPUs, memory, instance-store and data-volume mount points and sizes, `StateRoot`,
  `BuildRoot`, `RunDir`, `PKIDir` (`worker.crt`, `worker.key`), the CA bundle PEM, the build user, Xcode
  directories, qemu sysroots, and Mac-VM overrides (`StorageServerName`, `StorageIsHostL2`).
  `DefaultMachine(os)` holds Cucina's standard paths:

  | OS | StateRoot | BuildRoot | RunDir | PKIDir |
  | --- | --- | --- | --- | --- |
  | linux | `/var/lib/cucina` | `/var/lib/cucina` (`build/` = FUSE mount, `cache/`) | `/run/cucina` | `/etc/cucina/pki` |
  | windows | `C:\ProgramData\cucina\state` | `B:` (WinFSP drive via MountManager) | `C:\ProgramData\cucina\run` | `C:\ProgramData\cucina\pki` |
  | darwin | `/var/db/cucina` | `/Volumes/CucinaBuild` (case-sensitive APFS, R-MAC-4) | `/var/run/cucina-runner` (owned by the build user) | `/var/db/cucina/pki` |

The renderer ignores `deadman`, `env` and `handle_spot_interruption` (they configure the worker agent and
the service units, not Buildbarn).

`PlanWorker(settings, machine)` makes every decision and returns it (`WorkerPlan`: build directory, paths, L1
layout, file pool, runners, concurrency, compression, `Directories` with modes and owners, `Notes`). The agent and
hostd must create `Directories` before starting `bb_runner`, then `bb_worker`, and log `Notes`.

## bb_worker (`bb_worker.ApplicationConfiguration`, bb-remote-execution 1a3be95)

Proto paths: `bb_worker.proto` = `pkg/proto/configuration/bb_worker/bb_worker.proto` of bb-remote-execution;
`blobstore.proto`, `grpc.proto`, … = `pkg/proto/configuration/...` of bb-storage `ae61334ea798`.

| Field | Value | Why | Proto |
| --- | --- | --- | --- |
| `blobstore.contentAddressableStorage` | `readCaching{slow, fast, replicator}` | L1 (R-CACHE-2); reads served locally, writes and FindMissing go to `slow` | bb_worker.proto:22, blobstore.proto:21, :31, :254/:259/:263 |
| `…readCaching.slow` | `grpc{client: storage_endpoint + mTLS, enableCompression}` | central storage (EC2: frontend worker listener; Mac VM: hostd L2) | blobstore.proto:35, :932, :938 |
| `…enableCompression` | `wan_compression && !Machine.StorageIsHostL2` | zstd only on the WAN hop (R-DATA-3); used by the CAS client only (`cas_blob_access_creator.go:100`) | blobstore.proto:938 |
| `…readCaching.fast.local` | old flat schema, see "L1" | | blobstore.proto:54 |
| `…readCaching.replicator` | `deduplicating{concurrencyLimiting{base: local, maximumConcurrency: inputDownloadConcurrency}}` | merges concurrent fetches of one blob, bounds fetch parallelism (R-DATA-3) | blobstore.proto:728, :784, :799, :819 |
| `blobstore.actionCache` | `grpc{storage client}` | workers only write the AC; the frontend applies authorization and completeness checking | blobstore.proto:24 |
| (ISCC) | not configured | only bb_scheduler reads and writes the initial size class cache | — |
| `maximumMessageSizeBytes` | `settings.maximum_message_size_bytes` (required) | identical everywhere (R-CP-6) | bb_worker.proto:28 |
| `scheduler` | `{address: scheduler_endpoint, tls}` | Synchronize over mTLS (R-SEC-2) | bb_worker.proto:31, grpc.proto:22/:25 |
| `…tls.serverCertificateAuthorities` | `Machine.CABundlePEM` (inline PEM) | verifies Cucina endpoints | tls.proto:13 |
| `…tls.serverName` | `settings.server_name` (storage: `Machine.StorageServerName` if set) | endpoints may be IPs | tls.proto:40 |
| `…tls.clientKeyPair.files` | `PKIDir/worker.crt`, `PKIDir/worker.key`, `refreshInterval: 300s` | worker identity; renewals without restart | tls.proto:44, :78, :80, :85 |
| `global.diagnosticsHttpServer` | `[MetricsHost]:metrics_port`, `allow`, `enablePrometheus` (when `metrics_port` > 0) | Prometheus HTTP-SD scrape (R-OBS-1) | global.proto:257, :308, :316, server.proto:17/:25 |
| `global.prometheusPushgateway` | `{url, job: bb_worker, grouping: {pool, node}, pushInterval: 30s}` (when `pushgateway_url` set) | Mac VMs push via hostd | global.proto:246, :18–:31 |
| `global.setUmask` | `{umask: 0}` for native build directories (not Windows) | actions run as another user (ADR 0410); state dirs are 0700 | global.proto:265, :223 |
| `buildDirectories[0]` | one build directory shared by all runners | | bb_worker.proto:52 |
| `…virtual.mount.fuse` (Linux) | `directoryEntryValidity`/`inodeAttributeValidity` 300s, `allowOther`, `mountMethod: DIRECT_AND_FUSERMOUNT`, `maximumBackgroundTasks` 128 × threads (≤ 65535) | upstream's recommended values; build user ≠ root | virtual.proto:24, :69, :82, :104, :172, :192 |
| `…virtual.mount.nfsv4.darwin` (macOS) | `socketPath: StateRoot/nfsv4.sock`, `enforcedLeaseTime` 120s, `announcedLeaseTime` 60s, minor version unset (newest) | R-CACHE-3; works as a non-root user on macOS 27 (ADR 0412) | virtual.proto:38, :199, :215, :223, :248 |
| `…virtual.mount.winfsp` (Windows) | `mountPath: \\.\B:` (MountManager) + `caseInsensitive: true` | bb-deployments' Windows pattern; Bazel assumes case-insensitivity | virtual.proto:53, bb_worker.proto:313 |
| `…virtual.maximumExecutionTimeoutCompensation` | 3600s | recommended | bb_worker.proto:255 |
| `…virtual.shuffleDirectoryListings` | false (omitted) | stable outputs of irreproducible actions → cache hits | bb_worker.proto:269 |
| `…virtual.maximumWritableFileUploadDelay` | 60s | recommended | bb_worker.proto:308 |
| `…native` | `BuildRoot/build`, cache `BuildRoot/cache` (same file system), 262 144 files / 16 GiB, LRU | macOS default until the NFSv4-in-Tart measurement; fallback elsewhere with a reason | bb_worker.proto:187, :212–:234 |
| `…runners[]` | one per runner × instance name prefix, all at `unix://RunDir/runner.sock` | ADR 0410 | bb_worker.proto:207, :318 |
| `…runners[].concurrency` | settings, or vCPUs when 0 (emulated: vCPUs/4) | R-POOL-1, R-XPLAT-3 | bb_worker.proto:321 |
| `…runners[].instanceNamePrefix` | each of `instance_name_prefixes` | predeclared queues are per instance name (contracts §3) | bb_worker.proto:325 |
| `…runners[].platform` | exact property set, sorted by name then value | the scheduler rejects unsorted sets (`platform/key.go:42`) | bb_worker.proto:328 |
| `…runners[].sizeClass` | `settings.size_class` | must equal the predeclared size class | bb_worker.proto:333 |
| `…runners[].workerId` | `{pool, node}` | R-RE-4, see below | bb_worker.proto:354 |
| `…runners[].environmentVariables` | `QEMU_LD_PREFIX` for emulated runners | qemu-user via binfmt_misc (ADR 0410) | bb_worker.proto:375 |
| `…runners[].maximumFilePoolFileCount/SizeBytes` | 100 000 / 4 GiB per action | per-action output quota | bb_worker.proto:345, :349 |
| `…runners[].buildDirectoryOwnerUserId/GroupId` | the build user (virtual build directories) | files appear owned by the user actions run as | bb_worker.proto:399, :404 |
| `filePool.blockDevice.file` | `<volume>/filepool/pool`, threads × 4 GiB (sparse, capped by the volume) | all files actions write on virtual build directories | bb_worker.proto:61, filesystem.proto:24, blockdevice.proto:9/:13/:30 |
| `inputDownloadConcurrency` / `outputUploadConcurrency` | clamp(4·vCPUs, 16, 256) / clamp(2·vCPUs, 16, 128) | | bb_worker.proto:159, :77 |
| `directoryCache` | 10 000 objects / 16 MiB, LRU | REv2 Directory objects ≈ 1 KiB | bb_worker.proto:83, cas.proto:11/:20/:26 |
| `prefetching` | `fileSystemAccessCache: grpc(storage)`, 14 bits/path, 64 KiB (virtual only) | FSAC-driven prefetching (R-CACHE-3) | bb_worker.proto:103, :427, :455, :473 |
| `zstdPool` | `{maximumEncoders ≤ 16, maximumDecoders ≤ 32, encoderLevel: 3}` when compressing | bounded pool; level 3 = klauspost SpeedBetterCompression (`pkg/zstd/configuration.go:27`) | bb_worker.proto:178, zstd.proto:12/:18/:38 |
| `portalUrl` | unset | no bb-portal (D5) | bb_worker.proto:25 |

### L1 (`local`, OLD schema)

| Field | Disk placements | Memory | Proto |
| --- | --- | --- | --- |
| `keyLocationMapOnBlockDevice.file` / `keyLocationMapInMemory.entries` | `<dir>/key_location_map`, entries × 66 B | entries | blobstore.proto:427 / :422 |
| `keyLocationMapMaximumGetAttempts` / `PutAttempts` | 16 / 64 | 16 / 64 | blobstore.proto:439 / :449 |
| `oldBlocks` / `currentBlocks` / `newBlocks` | 8/24/3, 2/6/1 or 1/2/1 (ADR 0411) | same | blobstore.proto:462 / :470 / :491 |
| `blocksOnBlockDevice.source.file` / `blocksInMemory.blockSizeBytes` | `<dir>/blocks`, block × blocks | block size | blobstore.proto:511 / :506 |
| `blocksOnBlockDevice.spareBlocks` | 3, 1 or 1 | — | blobstore.proto:526 |
| `blocksOnBlockDevice.dataIntegrityValidationCache` | 100 000 entries, 4h, LRU | — | blobstore.proto:537, digest.proto:12/:20/:25 |
| `persistent` | `stateDirectoryPath: <dir>/state`, `minimumEpochInterval: 300s` | — (needs both on block devices) | blobstore.proto:605, :573, :594 |

Sizing (ADR 0411): a block is the largest blob the L1 can hold (`old_current_new_location_blob_map.go:292`), and
`readCaching` fails the read of a larger blob (`local_blob_replicator.go:39`), so blocks stay ≥ 512 MiB while the
budget allows. KLM: one entry per 8 KiB of blocks (≥ 65 536); 66 B per on-disk entry
(`block_device_backed_location_record_array.go:23`). Placement `auto`: instance store → data volume → VM disk
(macOS) → memory (≥ 16 GiB RAM) → root volume.

## bb_runner (`bb_runner.ApplicationConfiguration`, bb-remote-execution 1a3be95)

| Field | Value | Proto (`bb_runner.proto`) |
| --- | --- | --- |
| `buildDirectoryPath` | the worker's build directory (FUSE/NFSv4 mount path, `B:\`, native directory) | :13 |
| `grpcServers[0]` | `listenPaths: [RunDir/runner.sock]`, `authenticationPolicy: allow` (directory permissions guard the socket) | :16, grpc.proto:165/:173/:306 |
| `setTmpdirEnvironmentVariable` | true (TMPDIR; TMP/TEMP on Windows) | :30 |
| `chrootIntoInputRoot` | false (omitted) | :39 |
| `cleanProcessTable` | Linux with a build user | :50 |
| `runCommandsAs` | Linux: the build user (`credentials.proto:9/:12/:15`) | :64 |
| `appleXcodeDeveloperDirectories` | macOS: `Machine.XcodeDeveloperDirectories` | :137 |

## Host L2 (`bb_storage.ApplicationConfiguration`, bb-storage 086b011, NEW schema)

`RenderHostL2(HostL2Settings)`; template `internal/bbconfig/hostl2.json.tmpl`. Proto paths: bb-storage `086b011`.

| Field | Value | Proto |
| --- | --- | --- |
| `grpcServers[0].listenAddresses` | the vmnet bridge address only (wildcards refused) | bb_storage.proto:28, grpc.proto:152 |
| `…authenticationPolicy.tlsClientCertificate` | Cucina CA; `length(uris) == \`1\` && starts_with(uris[0], 'spiffe://cucina/worker/') && contains(uris[0], '/<SERIAL>/')`; metadata `{public: {user}, private: {sub}}` | grpc.proto:353, x509.proto:12/:51/:66 |
| `…tls.serverKeyPair.files` | hostd's L2 server key pair, refreshed every 300s | tls.proto:65 |
| `…maximumReceivedMessageSizeBytes` | as everywhere | grpc.proto:177 |
| `contentAddressableStorage.backend` | `readCaching{slow: grpc(upstream, enableCompression), fast: local, replicator: deduplicating{concurrencyLimiting{local, 64}}}` | bb_storage.proto:62/:118 |
| `…fast.local.keyLocationMap` | `{onBlockDevice: {file}, maximumGetAttempts: 16, maximumPutAttempts: 64}` | blobstore.proto:415, lossymap.proto:40/:52/:62 |
| `…fast.local` blocks/persistent | as the L1 (200 GiB default, 8/24/3 + 3) | blobstore.proto (086b011) |
| upstream client | `address`, CA, `serverName`, host key pair files, `keepalive {time 60s, timeout 20s, permitWithoutStream}` | grpc.proto:22/:25/:36, :139/:143/:147 |
| `actionCache`, `fileSystemAccessCache` | `grpc(upstream)` pass-through | bb_storage.proto:65, :77 |
| `get/put/findMissingAuthorizer` | `allow` (authentication restricts to this host's VMs) | bb_storage.proto:121, :127, :146 |
| `zstdPool` | bounded, `encoderLevel: 3` | bb_storage.proto:111 |
| `global.diagnosticsHttpServer` | `MetricsListenAddress` (loopback; hostd relays) | bb_storage.proto:58 |
| `maximumMessageSizeBytes` | as everywhere | bb_storage.proto:55 |

hostd creates `HostL2Directories` (`CacheDir`, `CacheDir/state`, 0700) first.

## Worker IDs, labels and drains (what the controller relies on)

* Every RunnerConfiguration carries `workerId: {pool: <WorkerPool name>, node: <EC2 instance ID | <serial>/<vm>>}`.
* bb_worker adds `thread` itself — **only when the runner's concurrency is > 1** — zero-padded to the width of
  `concurrency-1` (`cmd/bb_worker/main.go:334`, `:457`). With concurrency 1 there is no `thread` label.
* The scheduler counts one worker per runner thread per size class queue; the controller groups threads by `node`
  to get VMs (contracts §3). Several runners (and instance names) of one VM are separate queues with the same
  `{pool, node}`: drain a VM with `AddDrain(queue, {node: …})` on **each** of its queues.
* Two runners with identical property sets are rejected at render time (their worker IDs would collide).

## BuildQueueState → `ports.BuildQueue` (`internal/buildqueue`)

`buildqueuestate.proto` = bb-remote-execution `pkg/proto/buildqueuestate/buildqueuestate.proto`.

| Port | RPC | Mapping | Proto / code |
| --- | --- | --- | --- |
| `ListPlatformQueues` | `ListPlatformQueues` (:39) | per size class queue: `Queued = root_invocation.queued_operations_count.direct + .indirect`; `Executing = root.executing_workers_count`; `Idle = root.idle_workers_count` (incl. idle-synchronizing); `Workers = workers_count`; `Drains = drains_count` | :226, :241, :245, :254, :264, :269, :218, :222; aggregation `in_memory_build_queue.go:1905`, `:1960`, `:2917` |
| `ListWorkers` | `ListWorkers` (:58), filter `all`, `page_size` (:549), `start_after.worker_id` | `Executing = current_operation != nil`, `Operation = current_operation.name`, `Drained`, `Timeout` | :339, :345 |
| `AddDrain` / `RemoveDrain` / `ListDrains` | :78 / :82 / :71 | pattern = subset of the worker ID; adding twice keeps one | :589, :352 |
| `KillOperations` | :28 | `operation_name` or `size_class_queue_without_workers` (:419); with workers present the scheduler answers FailedPrecondition (`in_memory_build_queue.go:844`) and the adapter returns nil | |
| `ListOperations` / `GetOperation` | :24 / :20 | digest `hash-size` (:146), stage, `target_id` (:181), invocation = `tool_invocation_id` from `invocation_name.ids` (:122); executing operations get `Worker` from `ListWorkers` | |
| errors | | NotFound → `ErrQueueUnknown` (queue calls) / `ErrNotFound` (operations); InvalidArgument → `ErrInvalid` | `in_memory_build_queue.go:841`, `:901`, `:1193` |

## What the cluster side must provide (chart)

The rendered workers and host L2s rely on these frontend/scheduler settings:

* Frontend **worker listener**: mTLS admitting `spiffe://cucina/worker/…` and `spiffe://cucina/host/…`
  (docs/security.md), CAS get/put/findMissing and AC put for both, and a **File System Access Cache** with get and
  put for both. bb_worker fails an action outright when the FSAC answers anything but NotFound
  (`pkg/builder/prefetching_build_executor.go:107-111`, `:165`) — every virtual build directory uses prefetching.
* Frontend: `supportedCompressors: [ZSTD]` and a bounded `zstdPool` (Mac VMs without an L2 and host L2s compress);
  `keepaliveEnforcementPolicy {minTime ≤ 60s, permitWithoutStream: true}` on the worker listener (host L2s ping
  every 60 s).
* Scheduler `workerGrpcServers`: mTLS for `spiffe://cucina/worker/…`; `predeclaredPlatformQueues` for every
  (instance name × runner property set × size class) with exactly the size class workers announce.
* One `maximumMessageSizeBytes` everywhere.

## Running the tests

* Unit (goldens, semantics): `go test ./internal/bbconfig/`. Goldens live in `internal/bbconfig/testdata/` and are
  rendered by `internal/bbconfig/bbconfigtest.Goldens`; regenerate them with `bazel run //:update_goldens` (or
  `bazel run //internal/bbconfig:goldens`; the tier-static diff tests `//internal/bbconfig:goldens_*` fail on drift)
  or, without Bazel, `go test ./internal/bbconfig -run TestGoldens -update` (refused when `CI` is set). A golden
  change needs a `Test-Change:` trailer.
* Integration (pinned binaries): `go test ./internal/bbconfig/... ./internal/buildqueue/...` with
  `BB_STORAGE`, `BB_SCHEDULER`, `BB_WORKER`, `BB_RUNNER` pointing at the binaries; on a developer machine
  `internal/bbtest` falls back to `$CUCINA_DEV_STORAGE/bb-release/<tag>/<name>.<os>_<arch>`. Without binaries the
  tests skip under `go test` and fail under Bazel (whose targets get them from `//tools:bb_*`).
* The boot matrix is calibrated for darwin_arm64 binaries (ADR 0412).
* Children stay in the test's process group, so Bazel's timeout handling kills them with the test. A `go test`
  killed hard (SIGKILL, `-timeout` panic) skips cleanups and leaves them running: find them with
  `pgrep -fl bb-release`. Ports come from [20000, 32768), outside the kernels' ephemeral ranges.

## Using `internal/bbtest` from other packages

```go
pki := bbtest.NewPKI(t)                                    // throwaway CA in t.TempDir()
srv := pki.LoopbackServer(t, "frontend", "spiffe://cucina/server/frontend")
addr := bbtest.FreeAddr(t)                                 // 127.0.0.1:<free port>
cfg := renderMyConfig(addr, srv.CertPath, srv.KeyPath, pki.CAPEM)
bbtest.BootStorage(t, cfg, bbtest.GRPCReady(addr, pki.ClientTLS(nil, "localhost")))
conn, _ := bbtest.Dial(addr, pki.ClientTLS(&clientKeyPair, "localhost"), grpc.WithPerRPCCredentials(jwt))
re := bbtest.NewREClient(conn, "main")                     // Upload / Start / Execute / Read
```

`Boot*` fail the test (with the process's log tail) when the binary exits or is not ready within 20 s;
`bbtest.Start` + `Process.Done()/Logs()` check expected start-up failures. `bbtest.StorageConfig` and
`bbtest.SchedulerConfig` give minimal frontend/storage and scheduler fixtures; `bbtest.FakeWorker` registers and
drives runner threads through Synchronize.
