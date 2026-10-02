<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: scale-out stuck

**Use when** work is queued for a platform and the pool does not grow: queued operations stay queued, `desired` is zero or higher than the number of workers for a long time, nothing launches, or capacity
grows far slower than the queue. If workers *do* launch but never register, use [worker won't register](worker-wont-register.md). **Severity:** Page when builds wait; Soon otherwise. **Time:** 10 to 30 minutes.

## Symptoms

* `cucinactl queues` shows queued operations for a platform and size class with no executing workers behind them.
* `cucinactl pools list` shows `desired` above `registered` for a long time, or `desired` of 0 while the queue is not empty.
* Clients wait only while the cause is a **capacity** problem: after the capacity window (`queueFailAfter`, 10 minutes by default) the controller fails the queue and an alert fires. A pool that cannot launch **by configuration** fails its queued work **at once**:
  `FAILED_PRECONDITION: cucina: pool <name> cannot run this action: <reason>`, where the reason is one of `pool max is 0 (disabled)`, `pool is paused (cordoned)`, `platform queues not declared in the scheduler (add the pool to values.pools and helm upgrade)`,
  `pool image cannot be resolved: ...`, `pool reached its daily instance-hour cap`, or `pool is being deleted`. So work that waits with no error points at capacity errors, a controller that is not running, or a scheduler it cannot reach.
* A client error `FAILED_PRECONDITION: No workers exist for instance name prefix ... platform ...` comes from Buildbarn itself and means the action's platform matches **no declared queue** (a platform typo or a missing platform), not a capacity problem.

## Checks

1. **Which queue, and is the platform right?**

   ```sh
   cucinactl queues                              # per platform and size class: queued, executing, workers
   cucinactl ops list --stage queued --limit 20  # what is waiting, with its platform
   ```

   Compare the waiting actions' platform properties with the pool's runners (`cucinactl pools describe $POOL`) and regenerate the client configuration with `cucinactl bazelrc` if they differ.

2. **What does the pool say?**

   ```sh
   cucinactl pools describe $POOL
   kubectl -n cucina get wp $POOL -o json | jq '{spec: (.spec | {capacity, paused, dailyInstanceHourCap, rollout}), status: (.status | {desired, launching, registered, queueDeclared, lastCapacityFailure, conditions})}'
   ```

   * `capacity.max` of 0, `paused: true` (a cordoned pool), or `dailyInstanceHourCap` reached (the pool stops launching until 00:00 UTC) all stop launches by design, and fail the queued work at once with the message above.
   * `QueueDeclared=False`: the queue for this platform is not declared; add it to the chart values and `helm upgrade` ([ADR 0002](../adr/0002-queue-declaration-from-values.md)).
   * `lastCapacityFailure` names the last capacity error with its reason and time.

3. **Is the controller doing its job?**

   ```sh
   kubectl -n cucina get pods -l app.kubernetes.io/component=controller
   kubectl -n cucina logs -l app.kubernetes.io/component=controller,cucina.sloper.ai/leader=true --since=15m | tail -50     # the autoscaler runs on the leader only
   cucinactl status                                # component health, including the controller's view of the scheduler
   ```

   Exactly one replica leads. The controller needs its mTLS identity for the scheduler's BuildQueueState API; a certificate or network problem there shows as errors reading the queue.
   Metrics: `cucina_scale_decisions_total{pool,action}` moving means decisions are being made; `cucina_ec2_api_errors_total{op,code}` and `cucina_ec2_capacity_errors_total{pool,type,kind}` show why launches fail.

4. **Capacity errors.** The last failure tells you which:

   | Cause | Evidence | Fix |
   | --- | --- | --- |
   | No capacity for the instance type (`InsufficientInstanceCapacity`) | `lastCapacityFailure`; `cucina_ec2_capacity_errors_total{kind="ice"}` | Add instance types and subnets to the pool's ordered lists; try another size |
   | vCPU quota | `kind="quota"`; `VcpuLimitExceeded` in the log | Check usage and raise the quota (below) |
   | API throttling | `RequestLimitExceeded`; `cucina_ec2_api_errors_total` | The controller already backs off; reduce simultaneous launches or check for another caller in the account |
   | Permission or tag condition | `UnauthorizedOperation` | The controller's IAM policy requires the request tags on `RunInstances`; check the policy and the instance profile pass-role |
   | Image missing | `ImageResolved=False`; `InvalidAMIID` | `cucinactl images`; fix the pool's image reference |

   ```sh
   aws service-quotas get-service-quota --service-code ec2 --quota-code L-1216C47A --region $REGION --query Quota.Value   # Running On-Demand Standard instances (vCPUs)
   aws service-quotas get-service-quota --service-code ec2 --quota-code L-34B43A08 --region $REGION --query Quota.Value   # Spot Standard
   ```

5. **Mac pools.** Capacity is the sum of eligible host slots. `cucinactl hosts list` shows hosts that are cordoned, offline, unapproved, at their two-VM cap, or without the image; a pool's `tart.hostSelector` that matches no host also gives zero slots.

## Fixes

* **Instance types and subnets.** If the pool comes from Helm values, change the values and `helm upgrade`. For an emergency, patch the resource (a merge patch replaces the whole list), then put the same change into the values, because the next
  `helm upgrade` overwrites a manual patch:

  ```sh
  kubectl -n cucina patch wp $POOL --type merge -p '{"spec":{"ec2":{"instanceTypes":["c8i.8xlarge","c7i.8xlarge","c7a.8xlarge"]}}}'
  ```

* **Quota.** `aws service-quotas request-service-quota-increase --service-code ec2 --quota-code L-1216C47A --desired-value <vCPUs> --region $REGION`.
* **Capacity ceiling or pause.** Raise `capacity.max` in the values; resume a cordoned pool with `cucinactl pools cordon $POOL --undo`.
* **Force a worker while you investigate** (a standing cost that always expires): `cucinactl pools scale-floor $POOL --min 1 --for 2h`.
* **Release clients that are waiting on a pool that cannot recover.** Fail its queued operations with a message instead of letting them hang:

  ```sh
  cucinactl ops kill --queue-without-workers --platform OSFamily=linux --platform ISA=x86-64 --instance main --size-class 1 --message "pool linux-x86-64 has no capacity, see the status page"
  ```

## Roll back

Remove the floor by letting it expire (or `scale-floor --min 0`); revert a patch by restoring the previous list; `helm rollback` reverts values ([Helm upgrade, rollback and uninstall](helm-upgrade-rollback-uninstall.md)).

## Verify

`cucinactl pools describe $POOL` shows launches followed by registrations, `cucinactl queues` drains, and `cucina_pool_vms{pool,state}` shows VMs going `launching`, `registered`, `busy`, then back to zero after `idleTimeout`.

## Escalate

Attach `cucinactl diag --include-logs`, the pool's `describe` output, the controller log for the period, and the AWS error codes you saw.
