<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Cucina Helm chart

Cucina is a managed [Buildbarn](https://github.com/buildbarn) distribution for Bazel remote execution and remote
caching (REAPI v2). This chart installs the whole control plane and renders every Buildbarn configuration from
values — nobody writes Jsonnet:

| Component | Workload | Role |
| --- | --- | --- |
| `frontend` | `bb_storage` Deployment (stateless) | Client listener (TLS + Cucina JWT) and worker/host listener (mTLS); shards to storage, routes Execute to the scheduler, advertises ZSTD, caches FindMissingBlobs, checks AC completeness |
| `storage` | `bb_storage` StatefulSet, one PVC per shard | `local` backends with persistent state: CAS, AC, ISCC, FSAC |
| `scheduler` | `bb_scheduler` Deployment (1 replica, `Recreate`) | Predeclared queues for every pool platform (scale from zero), worker API (mTLS), BuildQueueState (controller only) |
| `controller` | `cucina-controller` Deployment (leader election) | Fleet controller, autoscaler, enrollment, host and management APIs, HTTP service discovery |
| `sts` | `cucina-controller sts` Deployment | OIDC → Cucina JWT token exchange (RFC 8693) |
| hooks | Jobs | `bootstrap` (CA, certificates, signing keys, JWKS, deny-list, break-glass key), `uninstall-prep` (drain and terminate workers), `helm test` (cache canary) |

Pinned upstream releases (ADR 0001): bb-storage `20260930T153215Z-086b011` and bb-remote-execution
`20260930T173749Z-1a3be95`, images by digest. Kubernetes ≥ 1.32 (tested on 1.36); Helm 3.x and 4.x.

## Quick start

```sh
helm install cucina oci://ghcr.io/sloper-ai/charts/cucina --version 0.1.0 \
  --namespace cucina --create-namespace \
  --set endpoints.client.host=cucina.example.com \
  --set endpoints.worker.host=10.0.1.10 \
  -f my-pools.yaml -f my-trust-policies.yaml
helm test -n cucina cucina
```

`NOTES.txt` prints the endpoints, how to fetch the CA bundle and the break-glass key, and the first `cucinactl`
commands. Operations (upgrade, rollback, uninstall, rotation): [`docs/operations/chart.md`](../../docs/operations/chart.md).
Exposure options (k3s ServiceLB, AWS NLB, Ingress/Gateway, NodePort): [`docs/operations/exposure.md`](../../docs/operations/exposure.md).
EKS: [`docs/operations/eks.md`](../../docs/operations/eks.md). Sizing: [`docs/sizing.md`](../../docs/sizing.md).

## Production canaries

The controller leader runs cache canaries every five minutes and independently runs one uncached execution per pool daily
and after each Helm revision. `canary.execution.enabled=false` disables paid execution probes without disabling cache checks;
execution timeout defaults to `15m` and cannot exceed it. Both paths use `hooks.test.credentialSecret`, or the bootstrap
break-glass key until it is replaced. `helm test` remains cache-only.

Unavailable pools and shared routes without verifiable per-pool attribution are non-passing. Naturally zero pools may launch
through the normal autoscaler, but the canary never drains workers to manufacture a cold start. A historical success does not
certify a paused or unverified deployment. See [ADR 1007](../../docs/adr/1007-leader-scheduled-execution-canaries.md) for
reservation, recovery and failure semantics.

## Values

`values.schema.json` validates every key (unknown keys are errors); `sizeProfile` (`small`, `medium`, `large`,
defined in `files/profiles.yaml`) supplies replicas, resources and cache sizes, and any non-empty per-component value
overrides it. Samples: [`samples/pools.yaml`](samples/pools.yaml), [`samples/trust-policies.yaml`](samples/trust-policies.yaml);
test profiles: [`ci/`](ci).

<!-- BEGIN VALUES TABLE (generated from values.schema.json) -->
| Key | Type | Description |
| --- | --- | --- |
| `auth.audience` | string | JWT aud claim Buildbarn requires. |
| `auth.breakGlass.enabled` | boolean | Generate the break-glass admin key (bootstrap hook) and its TrustPolicy (R-AUTH-12). |
| `auth.groupLookup` | see schema | Optional Cloud Identity group lookup. |
| `auth.keyRotationPublishLead` | string | How long a new signing key is published before it signs (>= 10m). |
| `auth.rateLimitPerMinute` | integer | STS exchanges per minute per client. |
| `auth.tokenTTL` | string | Cucina JWT lifetime (R-AUTH-3: 15m). |
| `buildbarn.authorizerTestVectors` | boolean | Embed JMESPath test vectors in every authorizer; Buildbarn refuses to start if one fails. |
| `buildbarn.frontend.actionCachePurgeBefore` | string | Hide worker-produced action results completed before this RFC 3339 time (AC purge, R-CACHE-5). |
| `buildbarn.frontend.existenceCacheDuration` | string | FindMissingBlobs existence-cache TTL (<= 3600s; must stay below storage retention). |
| `buildbarn.frontend.maximumTotalTreeSizeBytes` | integer | completenessChecking limit, above the largest toolchain Tree (R-DATA-2). |
| `buildbarn.frontend.zstd` | object |  |
| `buildbarn.maximumMessageSizeBytes` | integer | maximumMessageSizeBytes/maximumReceivedMessageSizeBytes in every component and worker (R-CP-6). |
| `buildbarn.scheduler.controllerIdentity` | string | SPIFFE ID of the controller's BuildQueueState client certificate. |
| `buildbarn.scheduler.defaultExecutionTimeout` | string |  |
| `buildbarn.scheduler.feedbackDrivenSizeClasses` | boolean | Persist execution statistics in the ISCC (useful with several size classes). |
| `buildbarn.scheduler.maximumExecutionTimeout` | string | Must be >= 3600s (R-RE-2). |
| `buildbarn.scheduler.platformQueueWithNoWorkersTimeout` | string | Upstream recommends 900s (R-RE-2). |
| `buildbarn.storage.averageObjectSize` | object | Average object size per store, used to size the key-location maps. |
| `buildbarn.storage.keyLocationMapFactor` | integer | Key-location map entries per expected object (upstream: 2-10). |
| `buildbarn.storage.minimumEpochInterval` | string | fsync interval of the persistent state (upstream: 300s). |
| `canary` | object | Controller-managed production canaries. |
| `canary.execution.enabled` | boolean | Run a tiny uncached action per pool daily and after each Helm deployment; this may launch workers from zero without interrupting busy workers. |
| `canary.execution.timeout` | string | Positive Go duration no greater than 15m, including cold start; bounds are enforced by the controller. |
| `clusterDomain` | string | Kubernetes cluster DNS domain. |
| `clusterId` | string | Installation ID (cucina:cluster tag on cloud resources); unique per installation. Default <namespace>-<release>. |
| `commonLabels` | object | Labels added to every object. |
| `controller` | object | cucina-controller settings. |
| `controller.affinity` | object |  |
| `controller.autoscaler.deadmanIdleLimit` | string |  |
| `controller.autoscaler.deadmanMaxUptime` | string |  |
| `controller.autoscaler.deadmanUnreachableLimit` | string |  |
| `controller.autoscaler.iceBackoffMax` | string |  |
| `controller.autoscaler.iceBackoffMin` | string |  |
| `controller.autoscaler.runInstancesBurst` | integer |  |
| `controller.autoscaler.runInstancesRefillPerSecond` | number |  |
| `controller.autoscaler.shadow` | boolean | Decide without acting (R-TEST-7 shadow mode). |
| `controller.aws.accountId` | string | AWS account of the instance identity documents; set at install time, never commit it. |
| `controller.aws.enabled` | boolean | Enable the EC2 provider. |
| `controller.aws.extraTags` | object | Tags added to every resource the controller creates. |
| `controller.aws.pricingRegionCode` | string |  |
| `controller.aws.region` | string |  |
| `controller.aws.sweepInterval` | string |  |
| `controller.costEnabled` | boolean |  |
| `controller.hosts.defaultTokenTtl` | string |  |
| `controller.hosts.staleAfter` | string |  |
| `controller.leaderElection.leaseDuration` | string |  |
| `controller.leaderElection.renewDeadline` | string |  |
| `controller.leaderElection.retryPeriod` | string |  |
| `controller.logLevel` | string (debug, info, warn, error) |  |
| `controller.nodeSelector` | object |  |
| `controller.otlpEndpoint` | string |  |
| `controller.podAnnotations` | object |  |
| `controller.podLabels` | object |  |
| `controller.priorityClassName` | string |  |
| `controller.registry.enabled` | boolean |  |
| `controller.registry.host` | string |  |
| `controller.registry.mode` | string (static-readonly, github-app) |  |
| `controller.replicas` | integer or null |  |
| `controller.resources` | object | requests/limits; empty = size profile. |
| `controller.resources.limits` | object |  |
| `controller.resources.requests` | object |  |
| `controller.scheduler.pollInterval` | string | BuildQueueState poll interval (R-SCALE-1: 1-2 s). |
| `controller.scheduler.queueFailAfter` | string | Fail queued work of a pool without capacity after this long. |
| `controller.serviceAccount.annotations` | object | e.g. eks.amazonaws.com/role-arn (IRSA, R-CP-8). |
| `controller.tolerations` | array |  |
| `controller.topologySpreadConstraints` | array | Default: spread over nodes (kubernetes.io/hostname, ScheduleAnyway). |
| `controller.worker.metricsPort` | integer |  |
| `controller.worker.pushgatewayUrl` | string |  |
| `controller.worker.wanCompressionForHosts` | boolean |  |
| `crds.install` | boolean | Apply the WorkerPool, MacHost and TrustPolicy CRDs from the controller image before every install and upgrade (cluster-scoped hook RBAC). false: the CRDs are managed elsewhere (use helm --skip-crds). |
| `endpoints.client.extraNames` | array | Extra DNS names/IPs for the public certificate. |
| `endpoints.client.host` | see schema | Public DNS name or IP of the remote-execution endpoint. |
| `endpoints.client.port` | integer | External port of the remote-execution endpoint. |
| `endpoints.hosts.host` | see schema | Address Mac hosts use for the HostService. Default the enrollment host. |
| `endpoints.hosts.port` | integer | External port of the HostService. |
| `endpoints.hosts.schedulerHost` | see schema | Scheduler hostname/IP reachable from Mac sites; uses worker.schedulerPort. Default: explicit hosts.host, else the worker scheduler host. Does not change EC2 endpoints. |
| `endpoints.hosts.storageHost` | see schema | Storage hostname/IP reachable from Mac sites; uses worker.storagePort. Default: explicit hosts.host, else the worker storage host. Does not change EC2 endpoints. |
| `endpoints.management.host` | see schema | Host of the management API. Default endpoints.client.host. |
| `endpoints.management.port` | integer | External port of the management API. |
| `endpoints.sts.aliases` | array | Allowed HTTPS transport origins for the STS; discovery token/JWKS URLs may use them, but the canonical JWT issuer stays sts.url. Their hostname/IP SANs are included in STS certificates. |
| `endpoints.sts.port` | integer | External port of the STS. |
| `endpoints.sts.url` | string | Issuer URL of Cucina JWTs (no trailing slash needed). Default https://<client.host>:<sts.port>. |
| `endpoints.worker.enrollmentHost` | see schema | Host of the enrollment API when it has its own load balancer. |
| `endpoints.worker.enrollmentPort` | integer | External port of the enrollment API. |
| `endpoints.worker.extraNames` | array | Extra DNS names/IPs for the internal certificate. |
| `endpoints.worker.host` | see schema | Private IP or DNS name of the worker endpoint (VPC/Mac sites only). |
| `endpoints.worker.schedulerHost` | see schema | Host of the scheduler worker API when it has its own load balancer. |
| `endpoints.worker.schedulerPort` | integer | External port of the scheduler worker API. |
| `endpoints.worker.serverName` | see schema | TLS server name workers verify. Default endpoints.worker.host. |
| `endpoints.worker.storageHost` | see schema | Host of the frontend worker listener when it has its own load balancer. |
| `endpoints.worker.storagePort` | integer | External port of the frontend worker listener (CAS/AC/FSAC for workers and hosts). |
| `exposure.api` | object | Exposure of the STS and the management API (TLS by the pods). |
| `exposure.api.annotations` | object |  |
| `exposure.api.externalTrafficPolicy` | string (Cluster, Local) |  |
| `exposure.api.loadBalancerClass` | string |  |
| `exposure.api.loadBalancerSourceRanges` | array |  |
| `exposure.api.nodePorts` | object |  |
| `exposure.api.type` | string (LoadBalancer, NodePort, ClusterIP) |  |
| `exposure.client.annotations` | object | Annotations of the client Service (e.g. AWS NLB settings). |
| `exposure.client.backendServiceAnnotations` | object | Annotations of the frontend ClusterIP Service an Ingress/Gateway uses (e.g. traefik.ingress.kubernetes.io/service.serversscheme: h2c). |
| `exposure.client.externalTrafficPolicy` | string (Cluster, Local) |  |
| `exposure.client.gateway` | object |  |
| `exposure.client.ingress` | object |  |
| `exposure.client.loadBalancerClass` | string |  |
| `exposure.client.loadBalancerSourceRanges` | array |  |
| `exposure.client.nodePort` | integer or null |  |
| `exposure.client.type` | string (LoadBalancer, NodePort, ClusterIP, Ingress, Gateway) | How the remote-execution endpoint is exposed; Ingress/Gateway terminate TLS and reach the frontend over h2c. |
| `exposure.worker` | object | Exposure of the worker endpoint (mTLS needs L4 pass-through). |
| `exposure.worker.annotations` | object |  |
| `exposure.worker.externalTrafficPolicy` | string (Cluster, Local) |  |
| `exposure.worker.loadBalancerClass` | string |  |
| `exposure.worker.loadBalancerSourceRanges` | array |  |
| `exposure.worker.nodePorts` | object |  |
| `exposure.worker.type` | string (LoadBalancer, NodePort, ClusterIP) |  |
| `frontend` | object | Frontend (stateless bb_storage) settings; null/empty = size profile. |
| `frontend.affinity` | object |  |
| `frontend.existenceCacheSize` | integer or null |  |
| `frontend.jwtCacheSize` | integer or null |  |
| `frontend.nodeSelector` | object |  |
| `frontend.podAnnotations` | object |  |
| `frontend.podLabels` | object |  |
| `frontend.priorityClassName` | string |  |
| `frontend.replicas` | integer or null |  |
| `frontend.resources` | object | requests/limits; empty = size profile. |
| `frontend.resources.limits` | object |  |
| `frontend.resources.requests` | object |  |
| `frontend.tolerations` | array |  |
| `frontend.topologySpreadConstraints` | array | Default: spread over nodes (kubernetes.io/hostname, ScheduleAnyway). |
| `frontend.zstd` | object | Bounded zstd pool (R-DATA-3); empty = profile. |
| `frontend.zstd.maximumDecoders` | integer |  |
| `frontend.zstd.maximumEncoders` | integer |  |
| `fullnameOverride` | string | Replaces <release>-<chart> as the prefix of every object name (max 40 characters). |
| `hooks.bootstrap.activeDeadlineSeconds` | integer |  |
| `hooks.bootstrap.enabled` | boolean | pre-install/pre-upgrade bootstrap Job (CA, certificates, keys, JWKS, deny-list, break-glass key). |
| `hooks.resources.limits` | object |  |
| `hooks.resources.requests` | object |  |
| `hooks.test.credentialKey` | string |  |
| `hooks.test.credentialSecret` | string | Secret with the service-account key of the canaries (helm test and the controller's cache/execution loops); empty = the break-glass key. |
| `hooks.test.enabled` | boolean | helm test cache canary (R-CP-7). |
| `hooks.uninstallPrep.enabled` | boolean | pre-delete Job that drains and terminates every worker (R-OPS-3). |
| `hooks.uninstallPrep.timeout` | string |  |
| `images.bbScheduler` | object | bb_scheduler image, pinned by digest. |
| `images.bbScheduler.digest` | string |  |
| `images.bbScheduler.repository` | string |  |
| `images.bbScheduler.tag` | string | Informational; the digest is what is pulled. |
| `images.bbStorage` | object | bb_storage image (frontend, storage shards), pinned by digest. |
| `images.bbStorage.digest` | string |  |
| `images.bbStorage.repository` | string |  |
| `images.bbStorage.tag` | string | Informational; the digest is what is pulled. |
| `images.controller` | object | cucina-controller image (controller, STS, hooks, helm test). |
| `images.controller.digest` | string |  |
| `images.controller.repository` | string |  |
| `images.controller.tag` | string |  |
| `images.pullPolicy` | string (Always, IfNotPresent, Never) | Image pull policy of every container. |
| `images.pullSecrets` | array | Names of image pull Secrets. |
| `images.sts` | object | STS image; empty repository = the controller image. |
| `images.sts.digest` | string |  |
| `images.sts.repository` | string |  |
| `images.sts.tag` | string |  |
| `instanceNames` | array | Buildbarn instance names (tenants, R-RE-3); the first is the default. Lower-case path segments. |
| `monitoring.grafanaDashboards.annotations` | object |  |
| `monitoring.grafanaDashboards.enabled` | boolean |  |
| `monitoring.grafanaDashboards.labels` | object |  |
| `monitoring.labels` | object | Labels on ServiceMonitor/ScrapeConfig/PrometheusRule objects. |
| `monitoring.prometheusRules.enabled` | boolean |  |
| `monitoring.prometheusRules.thresholds` | object |  |
| `monitoring.pushgateway.enabled` | boolean |  |
| `monitoring.pushgateway.image` | object |  |
| `monitoring.pushgateway.resources` | object |  |
| `monitoring.serviceMonitors.enabled` | boolean |  |
| `monitoring.serviceMonitors.interval` | string |  |
| `monitoring.workerScrapeConfig.enabled` | boolean | Separate ScrapeConfigs using the leader's HTTP discovery for EC2 Buildbarn workers, private worker-agent diagnostics, and Mac host/VM metrics relayed through HostService. |
| `monitoring.workerScrapeConfig.refreshInterval` | string |  |
| `nameOverride` | string | Replaces the chart name in object names. |
| `networkPolicy.enabled` | boolean | Restrict the in-cluster-only ports (storage gRPC, scheduler client and BuildQueueState). |
| `networkPolicy.metricsFrom` | array | NetworkPolicy peers allowed to scrape metrics (empty = anyone). |
| `pki.controllerClientCertTTL` | string | Lifetime of the controller's BuildQueueState client certificate. |
| `pki.existingCASecret` | string | Existing Secret with ca.crt and ca.key; empty = <fullname>-ca created by the bootstrap hook. |
| `pki.hostCertTTL` | string | Mac host certificate lifetime (max 168h). |
| `pki.vmCertTTL` | string | Mac VM certificate lifetime (max 12h). |
| `pki.workerCertTTL` | string | Worker certificate lifetime (max 168h). |
| `platforms.extra` | array | Additional platform catalog entries (schema of platforms/pools.json). |
| `podDisruptionBudgets.enabled` | boolean |  |
| `pools` | array | WorkerPool objects (R-POOL-1): [{name, enabled, ...WorkerPool spec}]. See samples/pools.yaml. |
| `scheduler` | object | Scheduler (bb_scheduler, one replica, Recreate). |
| `scheduler.affinity` | object |  |
| `scheduler.nodeSelector` | object |  |
| `scheduler.podAnnotations` | object |  |
| `scheduler.podLabels` | object |  |
| `scheduler.priorityClassName` | string |  |
| `scheduler.resources` | object | requests/limits; empty = size profile. |
| `scheduler.resources.limits` | object |  |
| `scheduler.resources.requests` | object |  |
| `scheduler.tolerations` | array |  |
| `sizeProfile` | string (small, medium, large) | Size profile (files/profiles.yaml): small (single-node test), medium (default production), large. Per-component values override it. |
| `storage` | object | Storage shards (bb_storage local, persistent). |
| `storage.affinity` | object |  |
| `storage.mode` | string (file, block) | file: file-backed blocks on a filesystem PVC (local-path); block: raw volumeMode Block for the CAS (EBS CSI) plus a filesystem PVC. |
| `storage.nodeSelector` | object |  |
| `storage.persistence.annotations` | object |  |
| `storage.persistence.size` | see schema | Filesystem PVC size; empty = derived from the stores. Immutable after install. |
| `storage.persistence.whenDeleted` | string (Retain, Delete) |  |
| `storage.persistence.whenScaled` | string (Retain, Delete) |  |
| `storage.podAnnotations` | object |  |
| `storage.podLabels` | object |  |
| `storage.priorityClassName` | string |  |
| `storage.resources` | object | requests/limits; empty = size profile. |
| `storage.resources.limits` | object |  |
| `storage.resources.requests` | object |  |
| `storage.shards` | integer or null | Number of storage shards (StatefulSet replicas). |
| `storage.storageClassName` | string |  |
| `storage.stores` | object | One size per store; blocks and key-location maps derive from it (R-CP-3). |
| `storage.stores.ac` | object |  |
| `storage.stores.cas` | object |  |
| `storage.stores.fsac` | object |  |
| `storage.stores.iscc` | object |  |
| `storage.supplementalGroups` | array | Extra groups of the storage container (raw block devices). |
| `storage.tolerations` | array |  |
| `storage.topologySpreadConstraints` | array | Default: spread over nodes (kubernetes.io/hostname, ScheduleAnyway). |
| `sts` | object | STS Deployment settings. |
| `sts.affinity` | object |  |
| `sts.nodeSelector` | object |  |
| `sts.podAnnotations` | object |  |
| `sts.podLabels` | object |  |
| `sts.priorityClassName` | string |  |
| `sts.replicas` | integer or null |  |
| `sts.resources` | object | requests/limits; empty = size profile. |
| `sts.resources.limits` | object |  |
| `sts.resources.requests` | object |  |
| `sts.tolerations` | array |  |
| `sts.topologySpreadConstraints` | array | Default: spread over nodes (kubernetes.io/hostname, ScheduleAnyway). |
| `tls.internal` | object | Certificate of the worker endpoint, the controller and the in-cluster mTLS listeners (must chain to Cucina's CA). |
| `tls.internal.certManager` | object |  |
| `tls.internal.existingSecret` | string |  |
| `tls.internal.source` | string (generated, certManager, existingSecret) |  |
| `tls.public` | object | Certificate of the client endpoint and the STS. |
| `tls.public.certManager` | object |  |
| `tls.public.existingSecret` | string |  |
| `tls.public.source` | string (generated, certManager, existingSecret) |  |
| `tls.refreshInterval` | string | How often Buildbarn re-reads mounted certificates (rotation without restart). |
| `tls.serverCertificateDuration` | string | Lifetime of chart-generated server certificates (renewed by the controller at 2/3). |
| `trustPolicies` | array | TrustPolicy objects (R-AUTH-2): [{name, spec}]. See samples/trust-policies.yaml. |
<!-- END VALUES TABLE -->

## Development

`make -C charts/cucina test` runs every chart check (lint with Helm 4 and 3, schema fail-fast cases, helm-unittest,
kubeconform against vendored schemas, promtool rule tests, chart-testing lint, copies/schema/README freshness and the
config-render check that boots the pinned Buildbarn binaries against every profile). Under Bazel the same checks are
`bazel test //charts/cucina/...`.
