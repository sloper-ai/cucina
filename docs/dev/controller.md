<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# cucina-controller: process, reconcilers, wiring, RBAC

`cmd/cucina-controller` is one binary with several roles. Code: `internal/controller` (process, configuration,
wiring, self-checks, tools), `internal/reconcile` (WorkerPool/MacHost reconcilers and the autoscaler runtime),
`internal/pools` (catalog and pool resolution), `internal/metrics`, `internal/httpsd`, `internal/proto` (protocol
version). The autoscaler policy itself is `internal/scaling` (see [scaling.md](scaling.md)).

## Subcommands

| Subcommand | Used by | What it does |
| --- | --- | --- |
| `controller --config F [--certs C]` | controller Deployment (2 replicas) | Manager with leader election; every replica serves probes, metrics + HTTP SD and the gRPC servers (enrollment, host stream, management); the leader runs the reconcilers, the autoscaler loops, the orphan sweep, key rotation and certificate renewal. |
| `sts --config F` | STS Deployment (2 replicas, leaderless) | Only the STS HTTPS server (+ key ring/deny-list refresh and TrustPolicy evaluation). |
| `wait-for --file P --tcp H:P [--timeout 5m]` | init containers | Waits until files exist and addresses accept TCP; exit 1 with the pending targets on timeout. |
| `bootstrap --config F [--certs C]` | Helm pre-install/pre-upgrade hook | Idempotent, never overwrites: CA Secret, server certificate Secrets (from `--certs`), signing keys + JWKS/deny-list ConfigMaps, break-glass key. Prints nothing secret. |
| `uninstall-prep [--config F] [--timeout 20m]` | Helm pre-delete hook | Deletes every WorkerPool and MacHost, waits for their finalizers (VMs drained and terminated/stopped), then (with `aws` configured) checks that no instance of the cluster is pending/running/stopped. Exit 1 otherwise (R-OPS-3). |
| `canary cache\|exec [flags]` | `helm test`, canary CronJob, e2e | `internal/canary.Command()` (agent e2e): the cache canary (STS token exchange + JWKS check + AC/CAS round trip, starts no workers) or the execution canary (one uncached action on a pool). Flags default from `CUCINA_CANARY_*`; `--report-to` POSTs the result to the controller's `/canary/results`. |
| `keys rotate` / `keys compromise --kid K\|*` | operators (`kubectl exec` into the controller Pod) | Publish a successor signing key (the leader promotes it once every frontend and the scheduler accept a probe token signed with it), or remove a key from the JWKS at once, activate a fresh key and rolling-restart frontends and scheduler (R-AUTH-9, T10e). |
| `version` | humans, CI | JSON: version, commit, Go, protocol version, compiled-in components. |

`--config` is `internal/config.Controller` as JSON, parsed **strictly** with `encoding/json/v2` (unknown or duplicate
members, wrong types and trailing data fail with the JSON pointer of the member), then defaulted and validated per mode
(`internal/controller/config.go`; e.g. `/scheduler/pollInterval must be between 1s and 2s (R-SCALE-1)`). `--certs` is a
JSON array of `pki.CertSpec` (ADR 0552).

Process plumbing: JSON `slog` on stderr (bridged into controller-runtime via `logr.FromSlogHandler` and into client-go via
`klog.SetSlogLogger`), SIGTERM → graceful shutdown (gRPC `GracefulStop`, 30 s manager grace), probes `/-/healthy` and
`/-/ready` on `listeners.probes` (ready = informer caches synced + component checks), Prometheus `/metrics` and
`/sd/workers` on `listeners.metrics`, optional OTLP hook (`TracingSetup`, a stub that logs when an endpoint is set).

Startup self-checks (R-TEST-7, fail fast with an actionable message): RBAC via SelfSubjectAccessReviews for every rule
below, and with `aws` configured a `DescribeInstances` filtered by `cucina:cluster=<clusterId>`.

## Wiring: build-time optional components (ADR 0550)

Each package owned by another agent is wired by one file `internal/controller/components_<name>.go` whose `init()`
registers it; nothing else refers to the package, so deleting the file removes the feature and the binary still
compiles. Hooks:

* `RegisterComponent(Factory{Name, Modes, Listener, Order, New})` — `New(ctx, *Deps)` returns a value implementing any of
  `GRPCService` (`Register(grpc.ServiceRegistrar)`, attached to the listener's gRPC server), `Runner` (`Run(ctx) error`,
  every replica unless it implements `NeedLeaderElection() bool`), `manager.Runnable`, `ReadyChecker`. Components with
  `Order < 0` are built before the reconcilers (they provide ports or shared objects), the others after (they may use
  `Deps.Fleet`). Shared objects travel with `Deps.Share(key, v)` / `Shared[T](d, key)`.
* `ProvideCompute`, `ProvideBuildQueue` — the port adapters; `UserDataFunc` — EC2 boot data.
* `RegisterBootstrapStep(BootstrapStep{Name, Order, Run})` — steps of `cucina-controller bootstrap`.

| File | Package (agent) | Mode | Provides |
| --- | --- | --- | --- |
| `components_ec2.go` | `internal/providers/ec2` (ec2) | controller, tools | `ports.Compute`; its `OnAPIError` feeds `cucina_ec2_api_errors_total` |
| `components_buildqueue.go` | `internal/buildqueue` (bbconfig) | controller | `ports.BuildQueue` over mTLS with the reloadable client identity |
| `components_bootdata.go` | `internal/workeragent/bootdata` (agent) | controller | EC2 user data (enrollment endpoint, server name, CA bundle) |
| `components_pki.go` | `internal/pki` (enroll) | controller, bootstrap | CA source + issuer (shared), `cucina_cert_expiry_seconds`, leader-only server-certificate rotator, bootstrap CA/certificates |
| `components_auth.go`, `components_keyops.go` | `internal/keys`, `internal/auth`, `internal/sts` (auth) | controller, sts, bootstrap, `keys` | key manager (rotation on the leader, `LoadVerifier` probing every ready frontend/scheduler Pod — labels `app.kubernetes.io/instance=<release>`, `app.kubernetes.io/component=frontend\|scheduler`, port `grpc-client` — with a grant-less token signed by the pending key; `FrontendRestarter` = rollout restart of those Deployments), TrustPolicy engine (+ Cloud Identity group resolver from `auth.groupLookup`, + `Valid` conditions from the leader), STS server, bootstrap signing/break-glass keys |
| `components_enroll.go` | `internal/enroll` (enroll) | controller | EnrollmentService; EC2 launches verified against the launch ledger (ADR 0551); also the host certificate renewal and VM identities behind the host stream (built first, Order −15) |
| `components_hostlink.go` | `internal/hostlink` (hostd) | controller | HostService (mTLS) and the `ports.HostFleet`; admission from MacHost objects; Welcome with endpoints, slots, desired images, message size |
| `components_mgmt.go` | `internal/mgmt` (mgmt) | controller | ManagementService with the controller-side views (pools, history, workers, hosts, floors, cordons) |

Management API sources wired by the controller: pools/history/workers (Fleet), hosts, floor/pause/cordon/reimage/
diagnostics, cost (`reconcile.CostModel`), images (current image per pool and Fast Launch state), components
(Deployment/StatefulSet readiness by `app.kubernetes.io/component`), queue timing (`reconcile.QueueTimer`: oldest queued
age and queue-time p95 per queue, also `cucina_queue_oldest_seconds`), alerts (`controller.DeriveAlerts`: queue not
declared, no capacity, image missing, startup failures, scheduler unreachable, host offline, orphans, idle VMs with empty
queues beyond the idle timeout, invariant violations, failing canary), EC2 worker logs (`controller.SSMShell`: SSM Run
Command, only on running workers that a tag-filtered Describe shows as this cluster's), the controller log ring
(`LogTee` → `mgmt.NewLogRing`) and the support sources (TrustPolicies, metrics text).

Cache canary (R-TEST-7): the leader runs `internal/canary` every 5 min (±10 %) in-process with the mounted canary key
(`CUCINA_CANARY_KEY_FILE`, default `/var/run/secrets/cucina/canary/key`) or else the break-glass key, against
`CUCINA_CANARY_ENDPOINT`/`CUCINA_CANARY_STS_URL` (default: the configured public endpoints) and exports
`cucina_canary_*`; one-shot runs POST to `/canary/results` on the metrics listener. `CUCINA_CANARY_DISABLE=true` turns
the loop off.

## Replicas and leader routing

Every replica serves probes, metrics/HTTP SD, enrollment, the host stream and the management API; the leader alone runs
the reconcilers and the autoscaler, so pool/worker views and history exist on the leader only. The controller therefore
labels its own Pod `cucina.sloper.ai/leader: "false"` at start, `"true"` once it holds the lease, and `"false"` again on
shutdown (`internal/controller/leader.go`; a crashed leader restarts as a follower and resets the label before it is
ready). **Chart contract:** the management Service selects `cucina.sloper.ai/leader: "true"` (in addition to the
controller selector); the Pod spec sets `POD_NAME`/`POD_NAMESPACE` from the downward API; the Role grants `patch` on
`pods`. Host streams (`HostService.Connect`) may land on any replica; route the host Service to the leader too, since
the autoscaler drives hosts through the leader's HostFleet.

## Reconcilers and the autoscaler runtime

* **WorkerPool** (leader-only, level-triggered, crash-only): adds the finalizer `cucina.sloper.ai/fleet`; resolves the
  pool (`internal/pools`: catalog platform + CR + per-platform timer defaults + concurrency rules, image via
  `Compute.ResolveImage` with a short cache so a newly tagged AMI starts a new generation, vCPUs of the preferred instance
  type); hands a `PoolRuntime` to the `Fleet`; writes `status` and the conditions `QueueDeclared`, `ImageResolved`,
  `CapacityAvailable`, `Degraded`, `Ready` from the loop's latest snapshot, with events on transitions; manages EC2 Fast
  Launch on Windows pools (enable on the new image first, disable old images, disable everything before the finalizer
  goes, waiting for `disabled`); on deletion keeps the loop running with `Deleting` until it is empty and a tag-filtered
  Describe confirms no pending/running instance, then removes the finalizer (R-OPS-3).
* **Fleet** (`manager.Runnable`, leader-only): one loop per pool, every `scheduler.pollInterval`: observe
  (`ListPlatformQueues`, `Describe` and `Hosts` coalesced across pools; `ListWorkers`/`ListDrains` per pool queue) →
  `scaling.Plan` → persist the launch ledger annotation **before** launches (write-ahead) → execute (launch with the
  planner's token, the shared RunInstances token bucket and an `InstancesNeverExceedMax` re-check; batched terminate;
  Tart placement + `StartVM`/`StopVM`; drains on every pool queue; `KillOperations{queue without workers}` unless another
  eligible pool serves the queue) → feed results back. Also: shared-queue attribution (`scaling.AttributeShared`),
  shadow mode (`autoscaler.shadow`: decide, log, never act), metrics, start-latency histograms, history for the
  management API, the periodic orphan sweep (`SweepOrphans`, also on demand).
* **MacHost** (leader-only): status from the host stream (phase Pending/Online/Offline/Draining/Cordoned, VMs, images,
  heartbeat), `Hosts.StaleAfter` offline detection, cordon pushed to hostd, pre-pull of `spec.desiredImages` and the
  images of matching Tart pools, finalizer stops the host's VMs. Placement (`placement.go`) is a pure, property-tested
  function: eligible hosts only, ≤ 2 VMs per host, `vmsPerHost`, one VM per host before a second, stopped VMs (warm L1)
  first; MacHost objects are authoritative for approval, labels, cordon and slots.

Annotations on WorkerPool objects (kept by Helm upgrades): `cucina.sloper.ai/launch-ledger` (launch ledger, R-SCALE-5),
`cucina.sloper.ai/fast-launch-images`, `cucina.sloper.ai/floor-override` (temporary floor from `cucinactl`).

## Metrics

`internal/metrics` registers the fleet metrics of contracts §6 on controller-runtime's registry
(`TestMetricsMatchContractAndLintClean` pins names, labels and lint). Owned elsewhere and registered on the same
registry by the wiring: `cucina_sts_exchanges_total`, `cucina_sts_token_ttl_seconds` (`internal/sts`) and
`cucina_cert_expiry_seconds` (`internal/pki`). `cucina_vm_stops_total{reason}` also carries `retire` and `external`
(see scaling.md). `cucina_queue_oldest_seconds` is registered but not yet populated. Cost (R-OBS-5): the loops record
every EC2 launch they observe (type, volumes, public IPv4, start/end) and each pool's AMI; `reconcile.CostModel`
(leader-only, every minute, `observability.costEnabled`) prices them with `internal/cost` (region rates, on-demand
prices from `Compute.InstancePrices`, 60 s minimum per launch) into `cucina_cost_usd_total{pool,category}` and
`cucina_standing_cost_usd_per_month{category}`, and backs `GetCost`. The usage lives on the leader: after a leader change
month-to-date figures restart from the stored usage (below).

`cucina_idle_instances_with_empty_queue{pool}` is an **overdue idle-worker count**, not the raw number of idle VMs.
A worker must be observed running by the provider and idle by the scheduler, with every pool queue continuously empty,
for **longer than the resolved `idleTimeout` plus 2 minutes** of idle drain/termination grace. Draining or stopping workers
still reported running remain eligible; busy, launching, stopped, terminated and unavailable workers do not. The 2-minute
grace is not the busy-worker `drainTimeout` (normally 30 minutes): executing workers are excluded regardless of age.
The planner's effective floor (`minRunning`, active scheduled or temporary override, bounded by max and disabled while
paused/deleting) protects active capacity; a stuck retiring worker does not satisfy that floor. Normal idle capacity is
still visible in `cucina_pool_vms{state="idle"}`. Time spent protecting a floor is excluded: releasing it starts fresh
non-floor evidence instead of flagging the first ordinary drain. Per-VM evidence also restarts after work, queued arrivals,
unknown scheduler or provider observations, or a leader restart. The registered-idle timestamp used by management alerts
excludes protected floor workers too. A zero gauge is **not proof of zero cloud residue**: acceptance must also check tag-filtered instances,
volumes, ENIs and public IPs after idle/drain grace.

Cost details: launch volumes are the pool's root volume (the AMI size when unset) and data volume for the instance's
lifetime; public IPv4 for EC2 pools with `associatePublicIP`; the current and previous AMI of each pool (rollback,
R-OPS-2) as standing snapshot storage; EC2 Fast Launch pre-provisioned snapshots (standing, from the Fast Launch
manager). The leader persists the usage every minute in the ConfigMap `<release>-cost-usage` (`usage.json`) and a new
leader resumes from it; `status.estimatedCostTodayUSD` shows each pool's cost since 00:00 UTC. Orphaned volumes are
counted (`cucina_orphans`) but not priced (their size is unknown to the sweep). HTTP SD (`/sd/workers`) returns
`{targets: ["<private-ip>:<worker.metricsPort>"], labels: {pool, node, generation, instance_type}}` per running EC2
worker, from the leader's observations or (other replicas) a 30 s cached tag-filtered Describe.

## RBAC

Namespaced `Role` for the controller ServiceAccount (generated from `RequiredRules`/`PolicyRules` in
`internal/controller/selfcheck.go`; `TestSelfChecksFailFast` proves this Role is sufficient):

| API group | Resources | Verbs |
| --- | --- | --- |
| `cucina.sloper.ai` | `workerpools` | get, list, watch, update, patch |
| `cucina.sloper.ai` | `workerpools/status`, `machosts/status`, `trustpolicies/status` | get, update, patch |
| `cucina.sloper.ai` | `workerpools/finalizers`, `machosts/finalizers` | update |
| `cucina.sloper.ai` | `machosts` | get, list, watch, create, update, patch, delete |
| `cucina.sloper.ai` | `trustpolicies` | get, list, watch |
| `coordination.k8s.io` | `leases` | get, list, create, update, delete (leader election, enrollment replay leases) |
| `""`, `events.k8s.io` | `events` | create, patch |
| `""` | `secrets`, `configmaps` | get, list, watch, create, update, patch (CA, server certificates, signing keys, JWKS, deny-list, service keys, break-glass, enrollment tokens `<release>-enroll-tokens`, registry credential) |
| `apps` | `deployments` | get, list, watch, patch |
| `apps` | `statefulsets` | get, list, watch |
| `""` | `pods` | get, list, watch, patch (the leader label on its own Pod) |

STS Deployment: `trustpolicies` get/list/watch; `secrets`, `configmaps` get/list/watch; `events` create/patch.
Bootstrap hook Job: `secrets`, `configmaps` get/create/update. Uninstall hook Job: `workerpools`, `machosts`
get/list/watch/delete. `SelfSubjectAccessReview` create is granted to every authenticated user by default.

## Tests (R-TEST-3)

| Test | Tier | Guards |
| --- | --- | --- |
| `internal/pools` `TestShippedCatalogIsValid`, `TestCatalogRejectsInvalidInput`, `TestResolve`, `TestWorkerSettingsCarryExactRunnerPlatforms`, `TestGenerationIsLabelSafeAndDistinguishesImages` | unit | R-RE-1, R-POOL-1, R-POOL-8, R-TEST-7 |
| `internal/controller` `TestConfigIsStrict` | unit | R-TEST-7 strict configuration |
| `internal/controller` `TestSelfChecksFailFast` | integration (envtest) | R-TEST-7 RBAC/AWS self-checks, documented Role sufficient |
| `internal/metrics` `TestMetricsMatchContractAndLintClean` | unit | contracts §6, R-TEST-8f |
| `internal/httpsd` `TestWorkersEndpoint` | unit | R-OBS-1 HTTP SD, R-SCALE-6 |
| `internal/controller/canary` `TestCacheCanary` | unit | R-TEST-7 cache canary (pass, corrupted read, rejected token), R-CP-7 |
| `internal/reconcile` `TestPlacementProperties` | unit (rapid) | R-POOL-6, R-MAC-3 |
| `internal/reconcile` `TestScenarioScaleFromZeroToZero`, `TestScenarioTartSpreadStopRestart` | integration (fakes) | full loop: R-SCALE-1..3, NFR-C1, R-POOL-6, R-MAC-3, R-OBS-5 (priced usage) |
| `internal/reconcile` `TestScenarioIdleCostLeakGauge` | integration (fakes) | NFR-C1, R-SCALE-3/7: configured idle timeout + grace, stuck drain/terminate, allowed floors, busy/queued work and evidence resets |
| `internal/reconcile` `TestCRDValidation`, `TestWorkerPoolLifecycleAndUninstall`, `TestPoolConditionsFailFast`, `TestMacHostReconcile` | integration (envtest) | CEL rules, finalizers, conditions, R-OPS-3, R-RE-2, R-MAC-6 |

Bazel: `//internal/pools:pools_test` and `//internal/reconcile:reconcile_test` need `platforms/pools.json` and
`api/crds/*.yaml` as data, which requires an `exports_files` in the root BUILD (requested from the bazel owner).

Ephemeral smoke check (2026-10-02, not committed, R-TEST-1): against an envtest API server, `bootstrap` created and
then left unchanged the CA, two certificates, signing keys and the break-glass key; `controller` passed its self-checks,
took the lease, served `/-/healthy`, `/-/ready`, `/metrics`, `/sd/workers`, the STS (discovery, JWKS, a service-key
token exchange), and ManagementService (GetStatus/ListPools/ListHosts/ListWorkers with a break-glass JWT via grpcurl);
a Tart WorkerPool got its finalizer and conditions (`Ready=False SchedulerUnreachable` without a scheduler);
`uninstall-prep` removed pool and host in 2 s; SIGTERM shut down cleanly in < 2 s; `sts` mode served the STS alone.

Run locally: `export KUBEBUILDER_ASSETS=$CUCINA_DEV_STORAGE/envtest/envtest-v1.36.2-darwin-arm64/controller-tools/envtest`
(download: `envtest-v1.36.2-darwin-arm64.tar.gz` from controller-tools releases, sha512 in its `envtest-releases.yaml`),
then `go test ./internal/controller/... ./internal/reconcile/... ./internal/pools/... ./internal/metrics/... ./internal/httpsd/...`.
Without the assets the envtest tests skip with a reason. `CUCINA_CRD_DIR` overrides the CRD directory (Bazel runfiles).
In Bazel, the envtest tests belong to `cucina_go_test(tier = "integration")`, the rest to `tier = "unit"`.
