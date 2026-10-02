<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Management API (`internal/mgmt`)

`internal/mgmt` implements `cucina.v1.ManagementService` (`api/proto/cucina/v1/management.proto`), the only endpoint
`cucinactl` talks to (R-CLI-2), and its audit log. It never calls Kubernetes or AWS itself: everything goes through
narrow consumer-owned interfaces (`sources.go`) that `cucina-controller` wires. ADRs: 0580 (guard and access classes),
0581 (worker logs), 0582 (drain/kill/floor semantics).

```go
srv, err := mgmt.New(mgmt.Deps{…}, mgmt.Options{…}) // fails fast on missing deps or an incomplete access table
srv.Register(grpcServer)                            // installs the GUARDED service (never use cucinav1.RegisterManagementServiceServer)
go srv.Run(ctx)                                     // on ctx end, open streams finish with UNAVAILABLE (clients reconnect)
```

`*mgmt.Server` matches the controller's component contract (`GRPCService` + `Runner`, `internal/controller/components.go`).

## Security model

* **Guard** (ADR 0580): `Register` installs a copy of the generated descriptor whose handlers authenticate
  `authorization: Bearer <Cucina JWT>` and apply the access table before the implementation runs, independent of the gRPC
  server's interceptors. Unknown methods are denied; `New` rejects a table that does not match the descriptor.
* **Access classes**: `Read` = JWT with `execute` or `admin` on ≥ 1 instance name; `AdminRead` and `Mutate` = **cluster admin**
  (`admin` on every `Options.InstanceNames` entry). Operations are filtered to instance names the caller holds
  `execute`/`admin` on.
* **Audit**: one JSON record per `AdminRead`/`Mutate` call — also for denied or unauthenticated attempts — via `mgmt.Auditor`
  (`mgmt.NewSlogAuditor(logger)` writes through a `slog.JSONHandler`). Plain reads are not audited; responses (which carry a
  service key or enrollment token exactly once) never are. Streams are recorded when they end.

  ```json
  {"time":"2026-10-02T10:00:00Z","level":"INFO","msg":"audit","log":"audit","principal":"sa:break-glass","display_name":"",
   "session":"…","method":"/cucina.v1.ManagementService/DrainWorker","mutating":true,"request":{"node":"i-0abc…"},
   "code":"OK","duration_seconds":0.004,"peer":"10.0.0.7:51234"}
  ```
* **Redaction** (`RedactJSON`, `RedactText`): secret-named JSON keys (token, secret, password, key, credential(s),
  authorization, private key, account id, …) and credential-shaped text anywhere (JWTs, `cuc_<kind>_…` keys/tokens, PEM private
  keys, GitHub/AWS keys, Bearer/Basic credentials, URL user-info, `password=`/`"client_secret":` assignments, ARN account IDs).
  Applied to audit request summaries, streamed EC2 worker logs, Tart worker logs and every support-bundle file (by construction:
  the bundle writer has no unredacted path).

## Method → access table (`mgmt.MethodAccess()`)

| Access | RPCs |
| --- | --- |
| `Read` | GetStatus, WatchOverview, ListPools, GetPool, ListWorkers, ListHosts, ListQueues, ListOperations, WatchOperations, GetOperation, GetCost, ListImages |
| `AdminRead` | StreamWorkerLogs, HostDiagnostics, ListEnrollTokens, ListServiceKeys, ListRevocations, CollectSupportBundle |
| `Mutate` | SetPoolFloor, CordonPool, GarbageCollectPool, DrainWorker, UndrainWorker, DrainHost, UncordonHost, ReimageHost, RegisterHostSerials, ApproveHost, RemoveHost, CreateEnrollToken, RevokeEnrollToken, KillOperations, CreateServiceKey, RevokeServiceKey, RevokePrincipal |

`TestMethodAccessCoversEveryRPC` fails when an RPC is added without a rule; `TestEveryRPCEnforcesAccess` calls every RPC of the
descriptor over TLS as anonymous, forged, cas-only, execute-only and tenant-admin callers.

## Dependencies and adapters

Required: `Authenticator`, `Auditor`, `Pools`, `Workers`, `Scheduler`. Optional deps that are nil make their RPCs fail with
FAILED_PRECONDITION ("… is not configured"). Read methods are called per request and for each overview snapshot: answer from
memory (informer cache, reconciler state), never from AWS. Errors: wrap `ports.ErrNotFound` / `ports.ErrInvalid` (or return a
gRPC status) to choose the code; anything else becomes UNAVAILABLE.

| Field | Interface (signature) | Provided by |
| --- | --- | --- |
| `Authenticator` | `Authenticate(ctx, bearerToken string) (Principal, error)` | **ready**: `mgmt.NewTokenAuthenticator(keyManager.Verifier)` |
| `Auditor` | `Record(ctx, AuditEvent)` | **ready**: `mgmt.NewSlogAuditor(controllerJSONLogger)` |
| `Pools` | `PoolView{ Pools(ctx) ([]Pool, error); History(ctx, pool string, limit int) ([]PoolEvent, []StartLatency, error) }` | coreb: `Pool{Resource: WorkerPool from the cache, Spec: resolved domain.PoolSpec (InstanceNames, Runners, SizeClass, Provider, Generation), Floor}`; History from the scale timeline (Events / `cucina_vm_start_seconds` samples), newest first, pool `""` = all |
| `PoolAdmin` | `SetFloor(ctx, pool string, FloorOverride{MinRunning, ExpiresAt, SetBy}) error; SetPaused(ctx, pool string, paused bool) error` | coreb: store the floor as annotation `cucina.sloper.ai/floor-override` = `{"minRunning":N,"expiresAt":RFC3339,"setBy":sub}` (MinRunning 0 deletes it) and treat it as an active floor window until `expiresAt`; `SetPaused` patches `spec.paused` |
| `Workers` | `WorkerView{ Workers(ctx) ([]Worker, error) }`, `Worker{domain.VM; PrivateIP string}` | coreb: the loops' `Snapshot.VMs` (+ instance private IPs) incl. stopped Tart VMs; `ID` is the `node` label (EC2 instance ID or `<serial>/<vm>`) |
| `Scheduler` | subset of `ports.BuildQueue` | **ready**: pass the `ports.BuildQueue` adapter (`internal/buildqueue`) |
| `Orphans` | `ListOrphans` + `DeleteOrphans` of `ports.Compute` | **ready**: pass `ports.Compute` (nil without AWS) |
| `QueueStats` | `QueueStats(ctx) (map[domain.QueueKey]QueueStat{OldestQueuedAge, QueueTimeP95}, error)` | optional, coreb (fills `QueueSummary` timing) |
| `Shell` | `InstanceShell{ RunScript(ctx, instanceID string, windows bool, script string) ([]byte, error) }` | coreb/ec2: SSM `SendCommand` (`AWS-RunShellScript` / `AWS-RunPowerShellScript`, `TimeoutSeconds` 30) then poll `GetCommandInvocation` until terminal; return `StandardOutputContent`; refuse instances without this cluster's tags |
| `Hosts` | `HostView{ Hosts(ctx) ([]v1alpha1.MacHost, error) }` | coreb: MacHost list from the cache |
| `HostAdmin` | `SetCordon(ctx, serial, bool) error; Reimage(ctx, serial, vm string) error; Diagnostics(ctx, serial, DiagnosticsRequest) (io.ReadCloser, error)` | hostd (`internal/hostlink`): SetCordon = `spec.cordoned` + `HostFleet.SetCordon`; Reimage = `HostFleet.ReimageVM` per VM (`""` = all) with the pool's current image; Diagnostics = `CollectDiagnostics` LogData stream, **redacted at the source** |
| `Enrollment` | token + serial admin (6 methods) | **ready**: `&mgmt.EnrollAdapter{Admin: enrollServer.Admin()}` |
| `Keys`, `Revocations` | service keys (3) / deny-list (2) | **ready**: `a := &mgmt.KeysAdapter{Store: keyManager}`; `Keys, Revocations = a, a` |
| `Cost` | `Cost(ctx, CostQuery) (CostReport, error)` | **ready**: `&mgmt.CostAdapter{Model: costModel, Usage: func(ctx) (cost.Usage, error)}`; coreb supplies the recorded usage |
| `Images` | `Images(ctx) ([]Image, error)` | coreb: current + previous image per pool, Fast Launch state from a cached `Compute.FastLaunch(describe)`; `workers_running` is computed here |
| `Components` | `Components(ctx) ([]Component, error)` | coreb: Deployment/StatefulSet readiness (frontend, storage, scheduler, controller, sts) |
| `Alerts` | `Alerts(ctx) ([]Alert, error)` | optional, coreb (failing data sources are added as `ManagementSourceUnavailable` alerts automatically) |
| `Support` | `TrustPolicies(ctx) ([]v1alpha1.TrustPolicy, error); Metrics(ctx) ([]byte, error)` | coreb: TrustPolicy list; Prometheus text exposition of the controller registry |
| `LogTail` | `Tail(lines int) []byte` | **ready**: `ring := mgmt.NewLogRing(1 << 20)`; log through `slog.NewJSONHandler(io.MultiWriter(os.Stdout, ring), …)` |

Options to set: `ClusterID`, `Version`, `InstanceNames` (= `config.instanceNames`), `Config` (the parsed `config.Controller`,
redacted into bundles), `DefaultEnrollTokenTTL` (= `config.hosts.defaultTokenTtl`, so the CLI default matches enrollment's).

### Replicas
The autoscaler loops (and so `Workers`, `History`, `QueueStats`) live on the leader only, while every replica serves the
management listener. Until followers can answer from shared state, **route the management Service to the leader** (e.g. the
controller labels its own Pod `cucina.sloper.ai/leader=true` while it holds the lease and the chart's management Service selects
that label), or make followers forward management calls to the leader. Streams end with UNAVAILABLE on `Run` cancellation, and
the CLI/TUI reconnect.

## Streams and bounds

| Stream | Bound | Cost |
| --- | --- | --- |
| WatchOverview | 64 subscribers; interval 2 s (1–60 s) | one shared snapshot per second (single-flight), whatever the subscriber count; a slow client only delays itself |
| WatchOperations | 16 subscribers; diffs every 2 s | one shared scheduler listing per second (≤ 10,000 operations); per-subscriber diff ADDED/CHANGED/REMOVED |
| StreamWorkerLogs, HostDiagnostics | 8 concurrent (shared) | SSM: 1 call/s, burst 5 (shared); ≤ 22 KB per poll; follow every 5 s, at most 30 min; Tart: snapshot (no follow yet) |
| CollectSupportBundle | 1 at a time, 60 s | tar.gz streamed in 64 KiB chunks, ends with `last` |

## Semantics worth knowing (ADR 0582)

* `DrainWorker`/`UndrainWorker`: pattern `{node}` on every queue of the worker's pool; the autoscaler owns `{pool,node}` drains.
* `KillOperations`: FAILED_PRECONDITION "operation killed by a Cucina administrator: <message>" (Bazel does not retry it).
* `SetPoolFloor`: `expires_in` required (≤ 7 days) unless `min_running` is 0; `min_running ≤ max`.
* `RevokePrincipal`: `effective_by` = the deny-list store's enforcement time (≤ now + 3 min).
* `GetCost` via `CostAdapter`: today and month-to-date (UTC) windows; a pool filter narrows pools and lines, totals stay cluster-wide.

## Worker log locations (contract with the images and the worker agent)

| Unit (`--unit`) | Linux (journald) | Windows (file, UTF-8, append) |
| --- | --- | --- |
| `bb-worker` (default) | `bb-worker.service` | `C:\ProgramData\cucina\logs\bb-worker.log` |
| `bb-runner` | `bb-runner.service` | `C:\ProgramData\cucina\logs\bb-runner.log` |
| `agent` | `cucina-worker-agent.service` | `C:\ProgramData\cucina\logs\cucina-worker-agent.log` |

macOS VMs: hostd serves `/var/log/cucina/bb_worker.log` / `bb_runner.log` (docs/dev/hostd.md) through `CollectDiagnostics`.

## RBAC and IAM the wiring needs

* Kubernetes (controller ServiceAccount): get/list/watch `workerpools`, `machosts`, `trustpolicies`, `deployments`,
  `statefulsets`, `events`; patch `workerpools` (annotation `cucina.sloper.ai/floor-override`, `spec.paused`); patch `machosts`
  (`spec.cordoned`; create/update/delete via enrollment); Secrets/ConfigMaps as internal/keys already requires.
* AWS (controller role): `ssm:SendCommand` on `arn:aws:ssm:<region>::document/AWS-RunShellScript` and `…/AWS-RunPowerShellScript`,
  and on `arn:aws:ec2:<region>:<account>:instance/*` with `ssm:resourceTag/cucina:managed-by = cucina-controller` and
  `ssm:resourceTag/cucina:cluster = <clusterId>`; `ssm:GetCommandInvocation` (no resource-level permissions: `*`). Workers need
  the SSM agent and `AmazonSSMManagedInstanceCore` (R-POOL-3). Orphan GC uses the Compute policy (tag-conditioned
  `ec2:DeleteVolume`/`ec2:DeleteNetworkInterface`).

## Tests

`go test ./internal/mgmt/...` (Bazel: `//internal/mgmt:mgmt_test`, tier `integration`): access-table completeness; every RPC
over TLS × five callers; audit log over TLS (records, redaction, denied attempts, no read records, watcher release on cancel);
real Cucina JWTs and a real `keys.Manager` through the adapters (revocation enforced at once); table tests per RPC group;
synctest streaming tests (shared snapshots, backpressure, limits, shutdown, operation diffs, SSM follow/offsets/rate limit,
Tart snapshot); support-bundle redaction; a property test of the redactor.
