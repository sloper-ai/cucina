<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Cucina engineering contracts (v1)

Single reference for how Cucina's components fit together. The lead architect owns this file and the contract
code it points to (`api/`, `internal/domain`, `internal/ports`, `internal/config`, `platforms/pools.json`).
Everything below is binding; change it only by editing this file and the code together (additive changes preferred).

## 1. Binaries

| Binary | Language | Runs on | Role |
| --- | --- | --- | --- |
| `cucina-controller` | Go | k8s (2 replicas, leader election) | Fleet reconcilers + autoscaler (`WorkerPool`, `MacHost`), STS (`TrustPolicy`), enrollment, host gRPC, management gRPC, Prometheus HTTP-SD, cost. Subcommands: `controller`, `sts` (STS-only Deployment, 2 replicas), `wait-for` (init helper). One image (`cucina-controller`), STS image alias `cucina-sts`. |
| `cucina-worker-agent` | Go (pure, cross-compiled) | EC2 Linux/Windows workers; inside Tart VMs | `bootstrap` (EnrollWorker at every boot, writes certs + bb_worker/bb_runner configs, formats L1), `run-supervisor` (dead-man switch, spot-interruption SIGTERM, writes `last-activity`/`last-contact`). |
| `cucina-hostd` | Go + cgo | Mac mini LaunchDaemon (root) / dev user mode | Enrolls, dials controller (`HostService.Connect`), runs `tart` as the `cucina` user, host L2 `bb_storage`, VM lifecycle, relays metrics. |
| `cucinactl` (+ `cucina-credential-helper` argv[0]) | Rust | dev machines, CI | CLI + TUI, login, Bazel credential helper, `bazelrc` generation. |
| Buildbarn (unmodified release binaries) | — | k8s / workers / hosts | `bb_storage` (frontend, shards, host L2), `bb_scheduler`, `bb_worker`, `bb_runner`. |

## 2. Package/directory ownership (parallel agents; paths relative to repo root)

| Owner | Paths |
| --- | --- |
| lead | `api/**` (CRDs, protos), `internal/domain`, `internal/ports/ports.go`, `internal/config`, `platforms/pools.json`, `docs/contracts.md`, `docs/architecture.md`, `docs/adr/00xx` |
| bazel | root Bazel files, `bazel/` (macros), `tools/`, `third_party/`, `.github/workflows/ci.yml`, Cargo workspace plumbing |
| aws | `deploy/aws-e2e/**` |
| images | `workers/linux/**`, `workers/windows/**` |
| macimage | `workers/macos/**` |
| core | `internal/{scaling,reconcile,controller,fakes,sim,metrics}`, `internal/ports/porttest`, `invariants/`, `sim/`, `cmd/cucina-controller` (wiring) |
| ec2 | `internal/providers/ec2`, `internal/cost` |
| bbconfig | `internal/bbconfig`, `internal/buildqueue` (real BuildQueue adapter), `internal/bbtest` (boot pinned binaries) |
| auth | `internal/{auth,sts,pki,enroll}`, `cmd/cucina-controller/sts*` |
| mgmt | `internal/mgmt` (ManagementService server), audit log |
| agent | `cmd/cucina-worker-agent`, `internal/workeragent` |
| hostd | `cmd/cucina-hostd`, `internal/hostd`, `internal/providers/tart` |
| chart | `charts/cucina/**` |
| cli | `cli/**` (except workspace plumbing), `docs/cli.md` |
| pkg | `macos/**`, `docs/macos/**`, `docs/mdm/**` |
| cross | `platforms/targets.json`, `bazel/platforms/**`, `docs/upstream/**`, cross-platform scenario data |
| e2e | `test/e2e/**`, `internal/canary/`, `slo/`, `deploy/aws-e2e/scripts/campaign/`, `docs/reports/**` templates |

## 3. Pools, platforms and queues

* `platforms/pools.json` is the catalog of **platforms** (pool identity: OS+ISA+toolchain image), their **runners** (exact REAPI
  property sets) and **size classes** (Buildbarn `uint32`). Operators extend it through Helm `platforms.extra`.
* A `WorkerPool` references a platform by name. Every runner of a pool's platform is a separate Buildbarn queue
  (`instanceNamePrefix` × `Platform{properties}` × `sizeClass`). The chart renders `predeclaredPlatformQueues` for every
  (instance name × runner × size class) of every platform used by a pool in `values.pools`, and the scheduler rolls on config change.
  A `WorkerPool` whose queues are not declared gets `QueueDeclared=False/reason=QueueNotDeclared` (the controller reads the
  scheduler's `ListPlatformQueues`), never launches, and fails its queued work immediately with a clear message
  (`KillOperations{size_class_queue_without_workers}`) — ADR 0001.
* Several pools may serve the same queue (the macOS "generic" runner has identical properties in every macOS pool). Demand is attributed to
  pools in lexical order of `spec.priority`-less name; ties are documented in `internal/scaling`.
* **Worker id labels**: `pool` and `node` (EC2 instance ID or `<host>/<vm>`); Buildbarn adds `thread`. The scheduler counts one worker per runner
  thread; the controller groups threads by `node` to get VMs.
* **Autoscaler policy (default, refine with an ADR)**: with `D_r = queued_r + executing_r` over the queues of runner r, `N = vCPUs per VM`,
  `S_r` = runner slots per VM: `desired = clamp(max(ceil(Σ_r D_r / N), max_r ceil(D_r / S_r)), minRunning', max)` where `minRunning'` is
  `max(spec.minRunning, active floor window)`, and booting VMs count as pending capacity. Scale-in only through per-VM idle timers (no flapping), never
  below `minRunning'`, never while the pool's queues are non-empty, always via `AddDrain` → confirm idle (`ListWorkers`) → terminate/stop → `RemoveDrain`
  (EC2: right after termination; Tart: when the VM next starts). `drainTimeout` bounds draining.

## 4. Buildbarn configuration ownership and schema skew (ADR 0001)

* The **chart** renders the control-plane configs (frontend, storage shards, scheduler) as protojson from values. They are validated by booting the
  pinned release binaries (R-CP-2).
* **Go** renders worker/runner configs (`internal/bbconfig`, typed from `bb-remote-execution` protos) from `WorkerSettings` + local machine facts, in
  the agent (EC2) and in hostd (Tart VMs), and the hostd L2 `bb_storage` config.
* **Version pins**: bb-remote-execution `20260930T173749Z-1a3be95` (go.mod pins bb-storage `ae61334ea798`, the *old flat* `local` schema:
  `keyLocationMapOnBlockDevice`, `keyLocationMapMaximumGetAttempts`, …) for `bb_worker`, `bb_runner`, `bb_scheduler`; bb-storage `20260930T153215Z-086b011`
  (*new* nested `keyLocationMap` lossymap schema, contains the 2026-09-30 zstd-decoder-leak fix — required, a denied `Put` leaks decoders and a bounded
  `zstdPool` would otherwise deadlock) for every `bb_storage` use (frontend, storage shards, hostd L2). Hence:
  * `go.mod` pins `github.com/buildbarn/bb-storage` at `ae61334ea798` and `bb-remote-execution` at `1a3be95` so worker/scheduler/runner configs are
    type-checked at compile time against exactly the schema of the release binaries.
  * `bb_storage` configs (new schema) are templates validated only by booting the pinned `bb_storage` binary. The worker's L1 `local` block uses the old
    flat schema; the L2/frontend/storage `local` blocks use the new nested schema.
* Everything that parses config is **strict**: an unknown field aborts startup. Never add a field without checking the pinned proto.
* No bb-browser / bb-portal. `maximumMessageSizeBytes` is identical in every component (`worker.maximumMessageSizeBytes`).

## 5. Identity, tokens and flows

### 5.1 Client login and JWT (R-AUTH)
`cucinactl login <url>` → `GET <sts>/.well-known/cucina-configuration` → OAuth2 auth-code+PKCE loopback to the IdP (Google "Desktop app") → ID token →
`POST <sts>/token` (RFC 8693) → Cucina JWT (ES256, `kid`, 15 min) → `cucina-credential-helper` returns it to Bazel.
Discovery document (JSON):
```json
{"version": 1, "issuer": "https://cucina.example.com", "token_endpoint": "https://…/token", "jwks_uri": "https://…/jwks.json",
 "endpoints": {"remote_execution": "grpcs://cucina.example.com:443", "instance_name": "main", "management": "cucina.example.com:8444"},
 "identity_providers": [{"name": "google", "type": "oidc", "issuer": "https://accounts.google.com", "client_id": "…", "client_secret": "…",
                         "scopes": ["openid","email","profile"], "hosted_domain_hint": "example.com", "redirect_ports": []}]}
```
`POST /token` form: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange&subject_token=<…>&subject_token_type=<…:id_token|…:jwt>&audience=<optional instance name>`;
response `{"access_token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":900}`; errors per RFC 6749
(`invalid_request`, `invalid_grant`, `access_denied`, `server_error`) with an `error_description` that never echoes token material.

`config.Endpoints.STSAliases` optionally lists explicit HTTPS transport origins (chart `endpoints.sts.aliases`) for private clients.
Discovery may use a configured alias for `token_endpoint` and `jwks_uri` only when the incoming authority matches that allow-list;
arbitrary Host/forwarded headers must never select a destination. JWT `iss` remains the canonical `stsUrl`, including when token
exchange and renewal use a private address. Without an alias match, discovery keeps its canonical public URLs.

Cucina JWT claims: `iss` (STS URL), `aud:"buildbarn"` (string), `sub`, `exp`, `iat`, `jti`, `sid`, no `nbf`, and
`cucina:{"cas_read":[…],"cas_write":[…],"ac_read":[…],"ac_write":[…],"execute":[…],"admin":[…]}` (explicit instance-name lists; `"*"` expanded by the STS).
Optional extra claim: `name` (display name from `claimMappings.displayName`, for audit only). Service-account keys are exchanged with
`subject_token_type=urn:cucina:params:oauth:token-type:service-key`; STS error statuses: 403 `access_denied`, 429 `slow_down` (rate limits).
Buildbarn `jwt` authenticator: `metadataExtractionJmespathExpression` →
`{"public": {"user": payload.sub}, "private": merge(payload.cucina, {"sid": payload.sid, "sub": payload.sub})}` (Go constant
`keys.BuildbarnMetadataExtraction`; the chart renders the `keys.Buildbarn*` expressions); authorizers
`contains(authenticationMetadata.private.ac_write, instanceName)` AND-ed with the deny-list check on `sid`/`sub` against the deny-list file
(exact match on quoted prefixed entries, ADR 0600; R-AUTH-4/-9). Worker/host identities (URI SANs `spiffe://cucina/worker/*`,
`spiffe://cucina/host/*`) may write CAS/AC on the worker listener (the host L2 forwards its VMs' writes).

### 5.2 EC2 worker boot (R-POOL-3, R-SEC-3)
Controller `Compute.Launch` with tags `cucina:{managed-by,cluster,pool,generation,image-version,launch-token,role}` + `extraTags`; IMDS exposes tags
(instance-metadata tags enabled) → image boot → `cucina-worker-agent bootstrap` (systemd `ExecStartPre` / Windows service pre-start): reads tags,
generates a key, `EnrollmentService.EnrollWorker` with the signed identity document → cert (≤ 24 h) + `WorkerSettings` → formats L1 / detects
instance store → renders `bb_worker`/`bb_runner` JSON into `/etc/cucina/bb/` (Windows `C:\ProgramData\cucina\bb\`) → `bb_runner`, `bb_worker` start →
`bb_worker` registers with the scheduler with worker id `{pool, node=<instance-id>}`. The agent keeps `/run/cucina/{last-activity,last-contact}`
(Windows `C:\ProgramData\cucina\run\`) fresh for the dead-man switch (R-POOL-7).

### 5.3 Mac VM start (R-MAC-3/4)
Controller `StartVM` → hostd: ensure image (pull via `GetRegistryCredentials`) → clone/reuse VM (re-clone on generation/age/health) → `tart run --no-graphics
--root-disk-opts=caching=cached,sync=none` as the `cucina` user → wait IP + guest agent → generate VM key → `IssueVMIdentity` (short-lived cert + settings
whose storage endpoint is the host's L2 and whose scheduler endpoint is the host's TCP relay) → push config/certs via `tart exec` → worker registers
`{pool, node=<host>/<vm>}`. Hostd makes all connections to VM IPs itself (root), never from the `tart` child (Local Network privacy).

Mac-facing transport addresses are independent of EC2's private addresses: `config.Endpoints.HostStorage` and `HostScheduler`
carry the upstreams sent in `HostSettings`; an empty value falls back to `WorkerStorage`/`WorkerScheduler` for compatibility.
The chart exposes `endpoints.hosts.storageHost` and `schedulerHost`, defaulting to an explicitly supplied `endpoints.hosts.host`
when present. Corresponding worker-listener certificates include these aliases. A host behind NAT must not be sent VPC-private
upstreams it cannot reach. Host streams and `/sd/hosts` discovery use leader-routed Services because host sessions are local to
the elected controller; per-target metric addresses still identify the pod holding that session.

### 5.3.1 Host metric relay (protocol 1.1, R-OBS-1)

`HostMessage.metrics_snapshot` carries a newly scraped `MetricsSnapshot`: `source` is `SOURCE_HOSTD`, `SOURCE_HOST_L2` or
`SOURCE_WORKER`, `vm_name` is present only for a running worker VM belonging to the authenticated host, and `prometheus_text`
is Prometheus text format 0.0.4. Hostd sends it only after a `Welcome` with protocol minor >= 1; protocol 1.0 peers continue
using the existing heartbeat aggregates. Failed scrapes must not resend a cached snapshot as fresh.

Limits: 256 KiB per snapshot, at most four targets per host (hostd, L2, two VMs), a 15-second scrape cadence and a 16 MiB
controller-wide raw snapshot cache. The implementation also bounds metric families, samples and labels. Oversized, malformed,
unowned or unsupported snapshots are rejected, never truncated; there is no log/configuration/credential payload. Identity labels
(`serial`, `pool`, `node`, `job`, `instance`) cannot be supplied by the metric text: the controller derives authoritative source
labels from the certificate/session and VM inventory. Snapshots expire within 45 seconds of receipt or immediately when the host
disconnects; their absence is missing telemetry, not a measured zero. The 256 KiB bound replaces 128 KiB after a real
2-vCPU / 8-GiB macOS worker's post-action scrape measured 153,598 selected bytes, eight families and 1,027 samples
(raw 370,849 bytes); complete blob and staging histograms are retained. Other limits remain 1 MiB local scrape input,
64 selected families, 4,096 samples, 16 labels per series and 256 bytes per label value. **Roll out the controller first,
then hostd**: earlier protocol-1.1 controllers still enforce 128 KiB and will reject larger snapshots during mixed-version operation.

Darwin workers may not expose native `process_resident_memory_bytes`. Hostd supplements the worker target with
`cucina_worker_resident_memory_bytes{source="guest-ps"}` only after a fresh bounded guest-OS measurement verifies the
known system launchd worker job's PID, root ownership and executable before/after `ps`; failed, ambiguous or zero
observations are omitted. This is separately named OS evidence, never a fabricated native metric or a Go-heap estimate.

The controller metrics listener exposes `/sd/hosts` for Prometheus HTTP discovery and a separate per-target scrape path
`/metrics/hosts/<serial>/<source>[/<vm>]`. Separating targets avoids collisions between the processes' own `process_*` and `go_*`
metric families. Prometheus attaches the discovery labels to samples; aggregate host counters are also available on the controller's
normal metrics endpoint with an authenticated `serial` label. Physical WAN series require a protocol-1.1 host; legacy logical-byte
heartbeat values are not exported as physical bytes. The management CR WAN fields are unqualified until a provenance-bearing
heartbeat-to-CR path exists. These endpoints are internal, just like `/sd/workers`.

### 5.4 Management API
`cucinactl` ↔ `ManagementService` over TLS with `Authorization: Bearer <Cucina JWT>`; mutating methods need `admin`. Audit log line per mutating call
(JSON: time, principal, method, request summary, result). Break-glass: `cucinactl login --key <service-account key>`.

## 6. Metrics contract (names are API: dashboards, alerts, SLOs, scenarios use them)

Controller (`/metrics` on the controller): `cucina_pool_desired{pool}`, `cucina_pool_vms{pool,state}` (state ∈ launching, registered, busy, idle, draining,
stopped, failed), `cucina_pool_max{pool}`, `cucina_vm_start_seconds{pool,phase}` histogram (phase ∈ to_running, to_registered, to_first_action),
`cucina_vm_stops_total{pool,reason}` (idle, drain, rollout, startup-timeout, deadman, maintenance), `cucina_ec2_api_errors_total{op,code}`,
`cucina_ec2_capacity_errors_total{pool,type,kind}` (ice, quota), `cucina_instance_seconds_total{pool,type}`,
`cucina_cost_usd_total{pool,category}`, `cucina_standing_cost_usd_per_month{category}`, `cucina_queue_queued{platform,size_class,instance}`,
`cucina_queue_oldest_seconds{…}`, `cucina_scale_decisions_total{pool,action}`, `cucina_invariant_violations_total{invariant}`,
`cucina_orphans{kind}`, `cucina_idle_instances_with_empty_queue{pool}` (cost-leak alert), `cucina_cert_expiry_seconds{role}`,
`cucina_sts_exchanges_total{issuer,result}`, `cucina_sts_token_ttl_seconds`, `cucina_hosts{phase}`, `cucina_host_heartbeat_age_seconds{serial}`.
The cost-leak gauge counts only live, scheduler-confirmed idle VMs above the effective floor after continuous non-floor idleness
and empty queues exceed the resolved idle timeout plus two minutes of idle drain/termination grace. Busy workers are excluded;
floor protection, work and unknown observations reset the evidence. Zero is not proof of zero cloud resources: acceptance checks
must separately inventory tagged instances, volumes, interfaces and IPs.

Hostd (`/metrics`): `cucina_hostd_vms{state}`, `cucina_hostd_l2_requests_total{result}`, `cucina_hostd_wan_bytes_total{direction}`,
`cucina_hostd_l2_size_bytes`, `cucina_hostd_disk_free_bytes`. All metrics pass `testutil.CollectAndLint`.
Canaries (`internal/canary`, `cucina-controller canary cache|exec`): `cucina_canary_up{canary}`, `cucina_canary_runs_total{canary,result}`,
`cucina_canary_duration_seconds`, `cucina_canary_step_duration_seconds{step}`, `cucina_canary_exec_queue_seconds{pool}`,
`cucina_canary_last_run_timestamp_seconds`, `cucina_canary_last_success_timestamp_seconds`. The shared SLO / recording-rule definitions live in
`slo/` (Go data, exported as `slo/rules.json` and `slo/sloth.json`); scenarios, canaries and the chart's PrometheusRules query the same rules.
Storage retention: the chart's storage config exposes `buildbarn_blobstore_local_blob_access_oldest_block_age_seconds`-style data; alerts use the
recording rule `cucina:cas_retention_seconds` defined in `slo/` and `charts/cucina/files/rules`.

## 7. Fakes, conformance suites, simulation (R-TEST-8)

* `internal/fakes` holds one stateful fake per port (Clock, Rand, Compute, VMRuntime, BuildQueue, IdentityProvider, SecretStore, Exec, FS); each exposes
  `FailNext(op, err)`, is seeded, and models latency/eventual consistency/quotas/ICE/throttling/the 2-VM cap/disk-full/crashes.
* `internal/ports/porttest.Run*(t, newPort)` — one conformance suite per port; runs against the fake (integration tier) and the real adapter (acceptance).
* `sim/` — deterministic controller simulation: scenario YAML `{fleet, workload trace, faults, seed, expectations}`; `invariants/` predicates are checked each step
  and also compiled into the production controller (R-TEST-7).
* Scenario harness: `test/e2e` (Go `testing`); scenarios declare `id`, `requires`, cost class, timeout, post-conditions as SLI queries over the metrics above.

## 8. Status of decisions that affect several agents

1. ADR 0001 — Buildbarn dual-schema pins (above).
2. ADR 0002 — Queue declaration is chart-rendered from `values.pools`; CR-only pools need a declared queue (above).
3. `go.mod` module `github.com/sloper-ai/cucina`, Go 1.27.1; deps are added with `lockf $CUCINA_DEV_STORAGE/gomod.lock go get …` (never concurrent `go mod tidy`).
4. Generated code is checked in: Go protobufs next to the `.proto` files (`api/proto/cucina/v1/*.pb.go`), Rust protobufs (buffa) and service stubs/clients (connect-rust, ADR 0003) under `cli/cucina-api/src/gen/{buffa,connect}/`,
   CRDs in `api/crds/`, deepcopy in `api/v1alpha1/zz_generated.deepcopy.go` (regenerate: `controller-gen object paths=./api/v1alpha1/...`,
   `controller-gen crd paths=./api/v1alpha1/... output:crd:dir=api/crds`).
