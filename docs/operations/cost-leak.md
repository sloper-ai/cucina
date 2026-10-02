<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: cost leak

**Use when** an alert says instances are running with an empty queue or that pool volumes or network interfaces are orphaned, `cucinactl cost` shows spend you cannot explain, or the AWS bill is higher than your workload justifies.
**Severity:** Soon (every hour costs money). **Time:** 15 to 45 minutes.

## The invariant that makes the cost model true

With every pool idle, **no worker instance exists**, and no volume, network interface, Elastic IP or public IP belongs to a pool. The only standing EC2 costs are AMI storage (the current image plus one previous) and, for Windows pools, EC2 Fast Launch's pre-provisioned snapshots. Anything else is a leak.
A worker idle beyond `idleTimeout` with an empty queue is a bug, and so is any resource carrying a pool tag with nothing using it. The controller enforces this and alerts on violations; this runbook is for when it did not hold ([architecture: the cost model](../architecture.md#8-the-scale-to-zero-cost-model)).

## Symptoms

* The alert for instances running with an empty queue (`cucina_idle_instances_with_empty_queue{pool}` above zero) or for orphans (`cucina_orphans{kind}` above zero).
* `cucinactl cost` shows compute for a pool that was not busy, or a standing cost that is not just AMIs and Fast Launch.
* Cost Explorer shows EC2, EBS, public IPv4, NAT gateway or data-transfer charges that do not match the build load.

## Checks

Always filter by the cluster tag. `CLUSTER` is the `clusterId` of the release.

1. **What does Cucina think it spent, and where?**

   ```sh
   cucinactl cost                                   # per pool and category: compute, EBS, public IPv4, AMI storage, Fast Launch, data transfer
   cucinactl cost --pool $POOL --since 2026-10-01   # RFC 3339 time or YYYY-MM-DD
   cucinactl pools list                             # desired versus actual
   ```

2. **Workers that should be gone.** Compare idle time with the pool's `idleTimeout`:

   ```sh
   cucinactl workers list --pool $POOL              # state and idle time per worker
   cucinactl queues                                 # an empty queue for the pool's platform?
   ```

3. **Anything running, in any state, in the cluster tag:**

   ```sh
   aws ec2 describe-instances --region $REGION \
     --filters "Name=tag:cucina:cluster,Values=$CLUSTER" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
     --query "Reservations[].Instances[].[InstanceId,State.Name,InstanceType,LaunchTime,PublicIpAddress,Tags[?Key=='cucina:pool']|[0].Value]" --output table
   ```

   **Stopped instances must never exist** (they still bill for their volumes); `PublicIpAddress` should be empty for workers on the private path ([data transfer](data-transfer.md)).

4. **Orphans:** volumes and network interfaces that carry the cluster tag but are attached to nothing.

   ```sh
   aws ec2 describe-volumes --region $REGION --filters "Name=tag:cucina:cluster,Values=$CLUSTER" "Name=status,Values=available" --query "Volumes[].[VolumeId,Size,CreateTime]" --output table
   aws ec2 describe-network-interfaces --region $REGION --filters "Name=tag:cucina:cluster,Values=$CLUSTER" "Name=status,Values=available" --query "NetworkInterfaces[].[NetworkInterfaceId,Description]" --output table
   ```

5. **Resources that must not exist at all** (every one of them is a standing cost the design avoids):

   ```sh
   aws ec2 describe-addresses --region $REGION --query "Addresses[?AssociationId==null].[PublicIp,AllocationId]" --output table          # unassociated Elastic IPs
   aws ec2 describe-nat-gateways --region $REGION --filter Name=state,Values=pending,available --query "NatGateways[].[NatGatewayId,VpcId]" --output table
   ```

6. **Image-related standing cost.** Only the current and one previous AMI per family should exist, and Fast Launch should be enabled only on current Windows AMIs:

   ```sh
   aws ec2 describe-images --owners self --region $REGION --filters "Name=tag:cucina:generation,Values=*" --query "Images[].[ImageId,Name,CreationDate]" --output table
   aws ec2 describe-fast-launch-images --region $REGION --query "FastLaunchImages[].[ImageId,State,SnapshotConfiguration.TargetResourceCount]" --output table
   aws ec2 describe-snapshots --owner-ids self --region $REGION --filters "Name=tag:CreatedBy,Values=EC2 Fast Launch" --query "Snapshots[].[SnapshotId,StartTime]" --output table
   ```

7. **Data transfer.** A rise in the data-transfer lines of `cucinactl cost` or an egress alert means bytes are taking a paid path: workers or the control plane across AZs, through a NAT gateway, over public addresses, or clients pulling large outputs out of AWS ([data transfer](data-transfer.md)).

## Why a worker stays up (and the fix)

| Cause | Evidence | Fix |
| --- | --- | --- |
| A floor is keeping workers on | `cucinactl pools describe $POOL` shows a `minRunning` above 0 or an active `floorSchedule` window; `scale-floor` always expires, a values `minRunning` does not | Lower `minRunning` in the values; wait out or remove the floor window |
| The controller cannot drain or terminate | Errors in the leader's log (`kubectl -n cucina logs -l app.kubernetes.io/component=controller,cucina.sloper.ai/leader=true`); `UnauthorizedOperation` on `TerminateInstances` | Fix the controller's IAM permissions and tag conditions |
| No controller leader, or it cannot read the scheduler | `kubectl -n cucina get pods -l app.kubernetes.io/component=controller`; `cucinactl status` | Restore the controller; scale-in resumes from the true state |
| Workers are not seen as idle | A runner thread stuck on an operation; workers of another generation | `cucinactl workers drain <node>` then terminate; investigate the stuck action (`cucinactl ops list --stage executing`) |
| A pool is paused with workers still up | `spec.paused` | `cucinactl pools cordon $POOL --undo`, or drain the workers |
| Timers too long | A very large `idleTimeout` or `drainTimeout` | Set sensible values (defaults: 5 minutes Linux, 10 minutes Windows and macOS) |

**The dead-man switch is the last line of defence**: a worker powers itself off (which terminates it on EC2) if it is idle beyond a hard limit (30 minutes by default), cannot reach the scheduler for 10 minutes, or exceeds its maximum uptime (12 hours), even with no controller. Do not rely on it; it limits the damage of a controller outage.

## Fix what is leaking now

1. **Orphaned volumes and interfaces** (each carries your cluster tag and is unattached): list what the controller would delete, then let it, then check:

   ```sh
   cucinactl pools gc --dry-run
   cucinactl pools gc
   ```

   If the controller is unavailable, delete a resource by hand **only after** confirming in the listings above that it has your cluster tag and the status `available`: `aws ec2 delete-volume --volume-id <id>` and `aws ec2 delete-network-interface --network-interface-id <id>`.
2. **Idle or stopped instances:** drain a worker, then terminate it. Terminate only instances that carry the cluster tag:

   ```sh
   cucinactl workers drain <instance id>
   aws ec2 terminate-instances --region $REGION --instance-ids <instance id>
   ```

3. **Stray Elastic IPs, NAT gateways, extra AMIs and Fast Launch snapshots:** release or delete only what you created for Cucina; follow [AMI rollout and rollback](ami-rollout-rollback.md), "Retire an AMI", for images (never deregister a Windows AMI before Fast Launch is `disabled`).

## Verify

With pools idle: the instance, volume and interface listings above are empty; `cucinactl pools list` shows every pool at zero; `cucinactl cost` shows only AMI storage and Fast Launch as standing cost; the alerts clear.

## Prevent

* Keep the alerts on, and set `dailyInstanceHourCap` on pools where a runaway would be expensive.
* Use `scale-floor` (it expires) instead of editing `minRunning` for temporary needs.
* Run the zero-idle check after every deployment and image rollout ([MT-007](../testing/manual/MT-007.md) step 7).

## Escalate

Attach `cucinactl cost`, the listings above, the controller log for the period, and the pool's `describe` output.
