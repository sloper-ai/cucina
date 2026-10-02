<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Operating the Cucina chart

Install, upgrade, roll back and uninstall Cucina with Helm (UC10), and the routine operations that touch the chart:
certificates, keys, storage, Buildbarn upgrades. Values are documented in
[`charts/cucina/README.md`](../../charts/cucina/README.md); exposure options in [exposure.md](exposure.md); EKS in
[eks.md](eks.md); sizing in [../sizing.md](../sizing.md).

## What the chart installs

| Object | Kind | Notes |
| --- | --- | --- |
| `<release>-frontend` | Deployment (bb_storage) | client listener `:8980` (TLS + Cucina JWT), worker/host listener `:8981` (mTLS), diagnostics `:9980` |
| `<release>-storage` | StatefulSet (bb_storage), headless Service | one PVC per shard; CAS, AC, ISCC, FSAC `local` with persistent state |
| `<release>-scheduler` | Deployment (bb_scheduler), 1 replica, `Recreate` | client `:8982` (forwarded JWT), worker `:8983` (mTLS), BuildQueueState `:8984` (controller mTLS) |
| `<release>-controller` | Deployment, leader election | probes `:8081`, metrics + `/sd/workers` `:9090`, STS `:8443`, management `:8444`, enrollment `:8445`, host API `:8446` |
| `<release>-sts` | Deployment (`cucina-controller sts`) | STS `:8443` |
| exposure Services | `client`, `api-sts`, `api-management` (leader only), `worker-storage`, `worker-scheduler`, `worker-controller` | see [exposure.md](exposure.md) |
| CRDs | WorkerPool, MacHost, TrustPolicy | from templates (upgraded by `helm upgrade`), kept on uninstall |
| WorkerPool / TrustPolicy objects | from `pools` / `trustPolicies` (+ the break-glass policy) | |
| hooks | `bootstrap` (pre-install/pre-upgrade), `uninstall-prep` (pre-delete), `test-canary` (`helm test`) | |
| optional | ServiceMonitors, ScrapeConfig, PrometheusRule, dashboards, Pushgateway, cert-manager Certificates, NetworkPolicies (on by default), PDBs | |

The bootstrap hook (`cucina-controller bootstrap --config /etc/cucina/controller.json --certs /etc/cucina/certs.json`)
creates, idempotently and never overwriting, the objects Helm must not own because the controller writes them at runtime:

| Object | Written by | Content |
| --- | --- | --- |
| `<release>-ca` (Secret) | bootstrap (unless `pki.existingCASecret`) | Cucina's CA: `ca.crt` (bundle), `ca.key` |
| `<release>-tls-{frontend,sts,frontend-workers,scheduler,controller}` | bootstrap, renewed by the leader | server certificates of the chart-generated TLS groups (`certs.json`) |
| `<release>-controller-client` | bootstrap, renewed by the leader | `spiffe://cucina/controller` client certificate for BuildQueueState |
| `<release>-signing-keys`, `<release>-jwks`, `<release>-denylist` | controller | JWT signing keys, the JWKS (`jwks.json`) and deny-list (`denylist.json`) Buildbarn mounts as directories |
| `<release>-break-glass`, `<release>-service-keys` | controller | the break-glass admin key (key `key`) and hashed service-account keys |

## Install (UC10)

1. Prerequisites: Kubernetes ≥ 1.32 (tested 1.36), Helm 3.x or 4.x, a StorageClass (k3s `local-path`; EKS: EBS CSI,
   see [eks.md](eks.md)). Optional: cert-manager, Prometheus Operator, Gateway API CRDs.
2. Write a values file with at least `endpoints.client.host` (public name/IP of the client endpoint),
   `endpoints.worker.host` (private address workers use), `pools` ([`samples/pools.yaml`](../../charts/cucina/samples/pools.yaml)),
   `trustPolicies` ([`samples/trust-policies.yaml`](../../charts/cucina/samples/trust-policies.yaml)) and, for EC2 pools,
   `controller.aws` (region, account) — never commit the account ID or environment addresses.
3. Install and smoke-test:

   ```sh
   helm install cucina oci://ghcr.io/sloper-ai/charts/cucina --version 0.1.0 -n cucina --create-namespace -f my-values.yaml
   kubectl -n cucina rollout status deploy/cucina-frontend deploy/cucina-scheduler deploy/cucina-controller
   helm test -n cucina cucina      # STS exchange, GetCapabilities, CAS + AC round trip through the client endpoint
   ```

   `values.schema.json` rejects unknown or malformed values before anything is created (R-TEST-7); cross-field checks
   (unknown platform, size class, instance name, minRunning > max, CAS < 19 GiB, timeouts) fail the same way.
4. Log in with the break-glass key printed by `NOTES.txt` (`cucinactl login <sts-url> --key …`), create the OIDC trust
   policies, then disable the key: `helm upgrade … --set auth.breakGlass.enabled=false` (the TrustPolicy that scopes it is
   removed; the controller stops accepting it). Rotate it by deleting the Secret and running `helm upgrade` (the bootstrap
   hook creates a new one).

## Upgrade

`helm upgrade cucina … -f my-values.yaml` keeps the cache (PVCs, persistent state) and upgrades the CRDs (R-OPS-1).
What restarts:

| Change | Effect |
| --- | --- |
| pool settings on existing platforms (types, image, max, timers, caches, new pool of an existing platform/size class) | applied live by the controller (UC11); no restart |
| a pool on a **new** platform, size class or instance name | scheduler restarts (its predeclared queues change, ADR 0002); in-flight Execute streams are retried by Bazel (UC8) |
| frontend configuration | rolling update, one extra replica at a time (`maxUnavailable: 0`) |
| storage configuration | rolling restart one shard at a time; each shard reloads its persistent state (no cache loss) |
| `storage.stores.<store>.size` | that store is re-laid out and **starts empty** on every shard (cold cache for that store); size it once |
| PVC size (`storage.persistence.size`) | StatefulSet claim templates are immutable: expand the PVCs (`kubectl patch pvc … spec.resources.requests.storage`, StorageClass with `allowVolumeExpansion`), then `kubectl delete sts cucina-storage --cascade=orphan` and `helm upgrade` |
| controller configuration | rolling update; leader election hands over |

`helm rollback cucina <revision>` follows the same table. CRDs roll back to the revision's manifests (kept compatible:
additive changes only).

## Uninstall (R-OPS-3)

```sh
helm uninstall cucina -n cucina --timeout 35m
```

The pre-delete hook deletes every WorkerPool and MacHost and waits until their finalizers drained and terminated the
EC2 instances (volumes go with them) and hosts stopped their VMs, while the controller still runs; it fails (and the
uninstall stops) if instances remain. Allow up to `hooks.uninstallPrep.timeout` (30 min) plus the default 5 min.

Left behind on purpose: the CRDs (`crds.keep`; deleting a CRD deletes every object of its kind), the storage PVCs (the
cache; `storage.persistence.whenDeleted: Retain`) and the runtime Secrets/ConfigMaps above (CA, keys, deny-list). A
reinstall with the same release name reuses all of them. To remove everything:

```sh
kubectl -n cucina delete pvc -l app.kubernetes.io/instance=cucina,app.kubernetes.io/component=storage
kubectl -n cucina delete secret,configmap cucina-ca cucina-signing-keys cucina-break-glass cucina-service-keys \
  cucina-controller-client cucina-tls-frontend cucina-tls-sts cucina-tls-frontend-workers cucina-tls-scheduler \
  cucina-tls-controller cucina-jwks cucina-denylist cucina-enroll-tokens --ignore-not-found
kubectl delete crd workerpools.cucina.sloper.ai machosts.cucina.sloper.ai trustpolicies.cucina.sloper.ai
```

## Certificates and keys

* **Server certificates** (chart-generated groups): issued by Cucina's CA, renewed by the leader at 2/3 of
  `tls.serverCertificateDuration`; Buildbarn re-reads them every `tls.refreshInterval`, the controller via fsnotify — no
  restarts. With cert-manager, the `Certificate` objects renew them (`renewBefore`).
* **CA rotation** (docs/security.md §PKI): introduce the new root into the CA bundle (`ca.crt` holds both), restart the
  Buildbarn pods (`kubectl rollout restart deploy/cucina-frontend deploy/cucina-scheduler sts/cucina-storage`;
  `clientCertificateAuthorities` is read at start, ADR 0400), switch issuance, wait for leaf renewal, remove the old root,
  restart again.
* **JWT signing keys** rotate in the controller (R-AUTH-9: publish ≥ 10 min before use). A compromised key: remove it from
  the JWKS and restart the frontends and the scheduler (Buildbarn caches validated tokens).
* **Revocation**: `cucinactl` adds the sid/sub to the deny-list ConfigMap; every authorizer reads it (≤ 2–3 min).

## Buildbarn upgrades (R-OPS-1)

1. Pick a matched pair of upstream date tags; re-pin them in `charts/cucina/values.yaml` (image digests, resolved from
   GHCR), `Chart.yaml` annotations, `internal/bbtest`, `tools/pinned.bzl` and ADR 0001.
2. Read the upstream proto diffs (`pkg/proto/configuration/**`) between the old and new tags; adapt
   `templates/_buildbarn.tpl`, `_authz.tpl`, `_configs.tpl`.
3. Run `make -C charts/cucina test` (or `bazel test //charts/cucina/...`): the config-render check renders every profile
   and boots the new binaries against it; an unknown/renamed field or a failing JMESPath test vector fails it.
4. `helm upgrade` in a staging cluster, then production.

## Action-cache purge (R-CACHE-5)

Set `buildbarn.frontend.actionCachePurgeBefore` to an RFC 3339 time: results produced by workers before it are hidden
(`actionResultExpiring.minimumTimestamp`) and rebuilt on demand. Results uploaded by clients without execution metadata
are not affected; to drop those too, empty the AC (`storage.stores.ac.size` change, cold AC).

## Storage loss (R-OPS-4)

The cache is reconstructible and not backed up. Losing a shard (volume or node) makes its slice of the CAS/AC cold:
builds re-execute or re-upload what they need; CAS retention and hit-ratio alerts show the recovery.

## Troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| Pods stuck in `ContainerCreating` (missing Secret/ConfigMap) | the bootstrap hook did not run or failed: `kubectl -n cucina logs job/cucina-bootstrap`; for `existingSecret`/cert-manager sources the named Secrets must exist |
| A Buildbarn pod exits at start with a JMESPath test-vector error | the rendered authorizer does not behave as specified; a chart bug — the config-render check should have caught it |
| Clients get `UNAUTHENTICATED` | token issuer (`endpoints.sts.url`) or audience mismatch, expired token, or JWKS not yet reloaded (≤ 5 min after a key change) |
| Execute fails with `No workers exist for … platform …` | no pool declares that platform/size class/instance name (ADR 0002): add the pool to values and `helm upgrade` |
| Workers cannot connect | worker endpoint address/TLS name (`endpoints.worker.*`), security groups, `loadBalancerSourceRanges` |
