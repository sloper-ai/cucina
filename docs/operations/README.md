<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Operating Cucina

Runbooks for the people who run a Cucina deployment. Each one has the same shape: when to use it, the symptoms, the checks (with exact commands), the fixes,
how to roll back, how to verify, and when to escalate. Start from the symptom.

## Find the runbook

| You see | Runbook |
| --- | --- |
| Workers launch but never register, or an alert says workers fail to start or register | [Worker won't register](worker-wont-register.md) |
| Work is queued and nothing launches, or capacity does not grow | [Scale-out stuck](scale-out-stuck.md) |
| Storage nearly full, retention below Bazel's cache TTL, "evicted" errors, a hit-rate drop; a poisoned action result to purge | [Storage full or retention too short](storage-full-retention.md) |
| The cache is gone (volumes lost, corrupted or deleted) | [Loss of storage](storage-loss.md) |
| Rotating the Cucina CA, TLS certificates or credentials | [Rotate the CA and credentials](rotate-ca-credentials.md) |
| Rotating the macOS package-signing certificate | [Rotate the package-signing certificate](rotate-pkg-signing-cert.md) |
| Adding or removing a Mac mini | [Add or remove a Mac host](add-remove-mac-host.md) |
| Updating macOS on a host | [macOS update](macos-update.md) |
| A new Xcode, Visual Studio or OS release | [New Xcode, Visual Studio or OS release](new-xcode-vs-os-release.md) |
| Bumping the Buildbarn version | [Buildbarn upgrade](buildbarn-upgrade.md) |
| Rolling out or rolling back a worker image | [AMI rollout and rollback](ami-rollout-rollback.md) |
| Installing, upgrading, rolling back or uninstalling the chart | [Helm upgrade, rollback and uninstall](helm-upgrade-rollback-uninstall.md) |
| Someone's access must end now, a key leaked, a signing key is compromised | [Revocation](revocation.md) |
| Instances running with an empty queue, orphaned volumes or interfaces, an unexpected bill | [Cost leak](cost-leak.md) |
| Testing failure handling on purpose | [Game days with AWS FIS](game-days-fis.md) |
| Where do the bytes go, and what do they cost | [Data transfer and client guidance](data-transfer.md) |

Reference documents: [chart](chart.md), [exposure](exposure.md), [EKS](eks.md), [sizing](../sizing.md), [Linux and Windows images](images.md), [macOS images](macos-images.md),
[the AWS acceptance environment](aws-e2e.md), [security](../security.md), [the CLI](../cli.md).

## Find the runbook from an alert

The chart's alert rules (`monitoring.prometheusRules.enabled`) and where to go when each one fires:

| Alert | Severity | Runbook |
| --- | --- | --- |
| `CucinaQueueTimeHigh` | warning | [Scale-out stuck](scale-out-stuck.md) |
| `CucinaCapacityErrors` | warning | [Scale-out stuck](scale-out-stuck.md), the capacity errors check |
| `CucinaWorkersFailingToStart`, `CucinaWorkersFailed` | warning | [Worker won't register](worker-wont-register.md) |
| `CucinaCASRetentionLow`, `CucinaCASRetentionBelowBazelTTL` | warning, critical | [Storage full or retention too short](storage-full-retention.md) |
| `CucinaStorageVolumeNearlyFull` | warning | [Storage full or retention too short](storage-full-retention.md) |
| `CucinaKeyLocationMapTooSmall` | warning | [Sizing](../sizing.md) (`buildbarn.storage.keyLocationMapFactor`); the change is a cold-cache event ([Loss of storage](storage-loss.md)) |
| `CucinaCacheHitRateLow`, `CucinaCacheHitRateDrop` | warning, info | [Storage full or retention too short](storage-full-retention.md); expected after a deliberate wipe ([Loss of storage](storage-loss.md)) |
| `CucinaHostOffline` | warning | [Add or remove a Mac host](add-remove-mac-host.md), troubleshooting; after an update, [macOS update](macos-update.md) |
| `CucinaIdleInstancesWithEmptyQueue`, `CucinaOrphanedResources` | warning | [Cost leak](cost-leak.md) |
| `CucinaCertificateExpiringSoon`, `CucinaCertificateExpiryImminent` | warning, critical | [Rotate the CA and credentials](rotate-ca-credentials.md) |
| `CucinaCertificateTelemetryMissing` | warning | [Rotate the CA and credentials](rotate-ca-credentials.md), private expiry telemetry troubleshooting |
| `CucinaEgressAnomaly` | warning | [Data transfer and client guidance](data-transfer.md) |
| `CucinaInvariantViolated` | critical | No runbook: the controller refused an operation to protect the fleet. Read the leader's log around the time of the alert (it names the invariant), collect `cucinactl diag --include-logs`, and escalate |
| `CucinaFrontendAvailabilityBudgetBurn` | critical (page), warning (ticket) | No dedicated runbook: the frontend is failing too many requests. `cucinactl status`, `kubectl -n cucina get pods -l app.kubernetes.io/component=frontend`, the frontend's log; after an upgrade see [Helm upgrade, rollback and uninstall](helm-upgrade-rollback-uninstall.md); authentication failures after a key or CA change are [Revocation](revocation.md) and [Rotate the CA and credentials](rotate-ca-credentials.md) |
| `CucinaColdStartBudgetBurn` | warning | Workers take too long from launch to their first action: the pool's start timings in `cucinactl pools describe`, then [AMI rollout and rollback](ami-rollout-rollback.md) (a new image can slow the boot: roll back) and [Worker won't register](worker-wont-register.md) |
| `CucinaBuildbarnDown` | critical | No dedicated runbook: `kubectl -n cucina get pods` and `describe` the named component, read its log; after an upgrade see [Helm upgrade, rollback and uninstall](helm-upgrade-rollback-uninstall.md); a lost storage volume is [Loss of storage](storage-loss.md) |

## Conventions in the runbooks

* **Names.** Commands assume the chart is installed as release `cucina` in namespace `cucina`, so the workloads are `deploy/cucina-frontend`, `deploy/cucina-scheduler`, `deploy/cucina-controller`,
  `deploy/cucina-sts` and `statefulset/cucina-storage`. Substitute your release and namespace. Components also carry the labels
  `app.kubernetes.io/instance=<release>` and `app.kubernetes.io/component=<frontend|storage|scheduler|controller|sts>`.
* **Access.** You need `kubectl` on the cluster, an admin session for `cucinactl` (`cucinactl login`), and for the EC2 checks an AWS profile that can read EC2 in the pools' region.
  `cucinactl` reads the same state as the controller; prefer it to `kubectl` where both can answer. Add `--output json` for scripts, `--yes` for destructive commands without a terminal.
* **Tags.** Everything the controller launches carries the tags `cucina:managed-by`, `cucina:cluster` (your `clusterId` value), `cucina:pool`, `cucina:generation`, `cucina:image-version` and
  `cucina:launch-token`. **Every AWS query that touches instances, volumes or network interfaces is filtered by `cucina:cluster`.** Never act on a resource that does not carry it.
* **Shell variables used below**: `CLUSTER=<your clusterId>`, `POOL=<pool name>`, `REGION=<aws region>`. The repository's image and AWS scripts read the region from `AWS_REGION` (and some of them need the acceptance environment's variables, see [AMI rollout and rollback](ami-rollout-rollback.md)): `export AWS_REGION=$REGION`.
* **Logs.** The controller and the STS run two replicas, and `kubectl logs deploy/<name>` reads only one arbitrary Pod. The autoscaler, certificate renewal and key rotation run on the leader only: read it with `kubectl -n cucina logs -l app.kubernetes.io/component=controller,cucina.sloper.ai/leader=true`.
  Enrollment is served by every controller replica and the STS exchanges are spread across its replicas, so read all of them (`-l app.kubernetes.io/component=controller` or `=sts`, with `--prefix`).
* **Custom resources.** `kubectl -n cucina get workerpools` (short name `wp`), `machosts` (`mh`), `trustpolicies` (`tp`); `kubectl -n cucina get cucina` lists all three.
* **Dry runs and rollbacks.** Each runbook says how to undo what it does. Prefer the undo to improvising.
* **When in doubt, collect a support bundle first**: `cucinactl diag --include-logs` (it writes `cucina-support-<time>.tar.gz`; secrets are redacted by the server), and attach it when you escalate.

## Severity and escalation

| Level | Meaning | Examples |
| --- | --- | --- |
| Page | Builds fail or the control plane is down | Frontend or scheduler down; auth outage; every pool failing |
| Soon | Degraded or a cost risk, builds still work | A pool cannot launch; retention falling toward the Bazel TTL; a cost leak |
| Planned | Maintenance | Upgrades, rotations, image rollouts |

Escalate with: what you saw (symptom and time), what you checked, the output of `cucinactl status` and `cucinactl pools describe <pool>`, and the support bundle. Do not paste tokens, keys or
certificates into a ticket.
