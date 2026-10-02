<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: Helm upgrade, rollback and uninstall

**Use when** you upgrade the Cucina chart, change values, roll a release back, or remove Cucina. [`chart.md`](chart.md) describes the chart and its values; this is the operator's procedure and what to check.
**Severity:** Planned. **Time:** 15 to 30 minutes; an uninstall waits for workers to drain (allow up to 35 minutes).

## What an upgrade keeps and what it restarts

* **The cache is kept.** Storage PVCs are not touched by an upgrade, and Buildbarn's storage restarts on its persistent state. (A change of storage layout is the exception: see [storage full or retention too short](storage-full-retention.md).)
* **CRDs are upgraded by a hook.** `crds/` supplies the first install; the pre-install/pre-upgrade `crds` hook runs `cucina-controller crds apply`, server-side applies the definitions compiled into the controller image, and waits for Established. If `crds.install=false`, manage them yourself and use `helm install --skip-crds` ([chart guide](chart.md#upgrade)).
* **Pods roll when their configuration changes**, by checksum. The scheduler restarts only when its configuration changes, which includes adding a platform or size class (the queue set); changes to pool settings that do not alter the queue set (instance types, image, max, timers, cache sizes) apply live through the `WorkerPool` objects and restart nothing
  ([ADR 0002](../adr/0002-queue-declaration-from-values.md)).
* **A scheduler restart loses its in-memory queue.** Clients retry (Bazel's retry tolerance), workers re-register, and in-flight builds slow down rather than fail. Choose a quiet moment when you add a platform.
* Controller and STS replicas roll one at a time behind PodDisruptionBudgets; Buildbarn keeps validating tokens locally while the STS restarts.

## Upgrade

1. **Read what will change.** Render and compare against the cluster before applying (the `helm diff` plugin is not required):

   ```sh
   helm lint --strict ./charts/cucina -f values.yaml
   helm template cucina ./charts/cucina -n cucina -f values.yaml | kubectl diff -n cucina -f - | head -200
   helm upgrade cucina ./charts/cucina -n cucina -f values.yaml --dry-run=server >/dev/null && echo "server-side dry run ok"
   ```

   Keep **one values file in version control**. `--reuse-values` merges with the old release's values and hides drift; prefer `-f values.yaml`, or `--reset-then-reuse-values` when you must set one value on the command line.

2. **Check the cluster is healthy first**: `cucinactl status`, `kubectl -n cucina get pods`, no alert firing.
3. **Upgrade and wait.** Helm 4 uses `--rollback-on-failure`; with Helm 3 the equivalent flag is `--atomic`:

   ```sh
   helm upgrade cucina ./charts/cucina -n cucina -f values.yaml --wait --timeout 15m --rollback-on-failure     # Helm 3: --atomic
   ```

4. **Verify the release.**

   ```sh
   helm test cucina -n cucina                       # a CAS and AC round trip through the client endpoint
   cucinactl status
   kubectl get workerpools -n cucina                # Ready pools, desired and actual workers
   ```

   Then prove the cache survived: repeat a known build, which should be at least 99 % cache hits with no workers launched ([MT-008](../testing/manual/MT-008.md) measures the opposite case).

## Roll back

```sh
helm history cucina -n cucina
helm rollback cucina <revision> -n cucina --wait --timeout 15m
```

* The bootstrap and CRD hooks run on install/upgrade, **not on rollback**. Existing runtime Secrets remain; rollback does not recreate a missing credential.
* CRDs are **not rolled back**. Changes must remain additive so the older controller can use the newer schema. Check `kubectl get workerpools,machosts,trustpolicies -n cucina` afterwards and read [`chart.md`](chart.md) before crossing a schema change.
* A rollback does not bring back a cache that an upgrade discarded.
* A change you made by patching a resource is not in the release; re-apply or put it in the values.

## Uninstall

`helm uninstall` removes the workloads and first runs a **pre-delete hook** that deletes every `WorkerPool` and `MacHost` and waits for their finalizers: the controller drains and terminates every EC2 instance it launched (their volumes go with them) and tells hosts to stop their VMs.
Nothing may be left running in your AWS account.

```sh
helm uninstall cucina -n cucina --timeout 35m
```

Then check, with the tag filter and **only** the tag filter (`CLUSTER` is the `clusterId` of the release):

```sh
aws ec2 describe-instances --region $REGION --filters "Name=tag:cucina:cluster,Values=$CLUSTER" "Name=instance-state-name,Values=pending,running,stopping,stopped" --query "Reservations[].Instances[].InstanceId" --output text
aws ec2 describe-volumes --region $REGION --filters "Name=tag:cucina:cluster,Values=$CLUSTER" --query "Volumes[].[VolumeId,State]" --output text
aws ec2 describe-network-interfaces --region $REGION --filters "Name=tag:cucina:cluster,Values=$CLUSTER" --query "NetworkInterfaces[].[NetworkInterfaceId,Status]" --output text
```

All three must be empty. What an uninstall deliberately leaves, so that a reinstall can reuse or you can decide:

| Left behind | Why | Remove with |
| --- | --- | --- |
| Storage PVCs | The cache is reconstructible but expensive; PVCs are retained by default | `kubectl -n cucina delete pvc -l app.kubernetes.io/instance=cucina` |
| CRDs and the custom resources' definitions | Helm does not delete definitions installed through `crds/`, and the apply hook does not delete them | `kubectl delete crd workerpools.cucina.sloper.ai machosts.cucina.sloper.ai trustpolicies.cucina.sloper.ai` (after the objects are gone) |
| The CA, signing keys, break-glass key and JWKS or deny-list ConfigMaps created by the bootstrap hook | They are not templated, so Helm does not own them; removing them destroys your identities | `kubectl -n cucina delete secret,configmap` on the specific names, deliberately |
| AMIs, Fast Launch snapshots | They belong to the image lifecycle, not the release | [AMI rollout and rollback](ami-rollout-rollback.md), "Retire an AMI" |
| Mac hosts' packages, VMs and images | Hosts are separate machines | [Add or remove a Mac host](add-remove-mac-host.md) |

### If the uninstall hangs

A `WorkerPool` or `MacHost` that stays in `Terminating` means its finalizer (`cucina.sloper.ai/fleet`) is waiting for the controller, which cannot finish (it is down, lost its permissions, or an instance will not terminate).

1. Look first: the leader's log, `kubectl -n cucina logs -l app.kubernetes.io/component=controller,cucina.sloper.ai/leader=true --tail=100` (finalizers run on the leader; `kubectl logs deploy/...` would read one arbitrary replica), and `cucinactl pools describe <pool>`.
2. If the controller is gone for good, **terminate the instances yourself, filtered by the cluster tag**, check what you are about to delete, and only then release the finalizer:

   ```sh
   aws ec2 describe-instances --region $REGION --filters "Name=tag:cucina:cluster,Values=$CLUSTER" "Name=instance-state-name,Values=pending,running,stopping,stopped" --query "Reservations[].Instances[].[InstanceId,Tags[?Key=='cucina:pool']|[0].Value]" --output table
   aws ec2 terminate-instances --region $REGION --instance-ids <ids from the list above>
   kubectl patch workerpool <pool> -n cucina --type merge -p '{"metadata":{"finalizers":null}}'
   ```

3. Finish with the three checks above, and run the [cost leak](cost-leak.md) checks for orphaned volumes and interfaces. Never terminate or delete anything that does not carry your cluster's tag.

## Escalate

Attach `helm history`, `helm get values cucina -n cucina` (review it for secrets first), the failing hook's logs (`kubectl -n cucina logs job/<hook job>`), and `cucinactl diag --include-logs` if the controller is still up.
