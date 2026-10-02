<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: worker won't register

**Use when** the controller launches workers and they never show up in the scheduler: pool workers stay in `launching`, start timeouts accumulate, launch-terminate cycles burn instance-seconds, or the
alert for workers failing to start or register fires. **Severity:** Soon (Page if it is the only pool serving a platform). **Time:** 15 to 60 minutes.

## Symptoms

* `cucinactl workers list --pool $POOL` shows workers stuck in `launching` (or `failed`), and `cucinactl pools describe $POOL` shows launch events followed by `fail` (startup timeout), repeatedly.
* `cucina_vm_stops_total{reason="startup-timeout"}` rises; builds for the platform wait in the queue ([scale-out stuck](scale-out-stuck.md) covers "nothing launches at all").
* Cost climbs with no work done: each failed launch bills at least 60 seconds ([cost leak](cost-leak.md)).

## First: stop the churn

If workers launch and die in a loop, stop launching while you investigate (running workers finish; nothing new starts):

```sh
cucinactl pools cordon $POOL            # undo later with: cucinactl pools cordon $POOL --undo
```

Know the side effect before you do: **a cordoned pool fails the queued work of any queue that has no worker**, immediately, with `FAILED_PRECONDITION: cucina: pool <name> cannot run this action: pool is paused (cordoned)`.
Clients get a clear error instead of waiting for workers that cannot start, which is usually what you want during an outage; if you would rather leave the queue waiting, do not cordon.

## Checks

Work from the controller outward to the instance.

1. **What the controller thinks.**

   ```sh
   cucinactl pools describe $POOL                      # conditions, recent events, cold-start timings
   kubectl -n cucina get wp $POOL -o json | jq '.status | {desired, launching, registered, queueDeclared, lastCapacityFailure, conditions}'
   ```

   * `QueueDeclared=False` (reason `QueueNotDeclared`) is not a registration problem: the pool never launches. Add the platform and size class to the chart values and `helm upgrade` ([ADR 0002](../adr/0002-queue-declaration-from-values.md)).
   * `ImageResolved=False`: the AMI or tag selector matches nothing. Check `cucinactl images` and the pool's `image` ([AMI rollout](ami-rollout-rollback.md)).
   * `CapacityAvailable=False`: instances do not launch, go to [scale-out stuck](scale-out-stuck.md).

2. **Do instances reach `running`?**

   ```sh
   aws ec2 describe-instances --region $REGION \
     --filters "Name=tag:cucina:cluster,Values=$CLUSTER" "Name=tag:cucina:pool,Values=$POOL" "Name=instance-state-name,Values=pending,running,shutting-down" \
     --query "Reservations[].Instances[].[InstanceId,State.Name,InstanceType,LaunchTime,PrivateIpAddress]" --output table
   ```

   Running but never registering means a boot or connectivity problem; continue below.

3. **Did enrollment reach the controller, and what did it decide?**

   ```sh
   kubectl -n cucina logs -l app.kubernetes.io/component=controller --prefix --since=20m | grep -i enroll
   ```

   Every controller replica serves enrollment, so read all of them (`--prefix` names the Pod). The autoscaler's own decisions are logged by the leader only: add `,cucina.sloper.ai/leader=true` to the selector.

   A refusal names its reason. Typical ones: the identity document's account or region does not match; the instance lacks the `cucina:*` tags or their values differ from the controller's launch record; the
   document is too old; a second enrollment for the same boot. No enrollment line at all means the request never arrived: go to step 5.

4. **What did the instance do?** The worker agent logs one JSON line per event. While the instance is up, read it over SSM (Linux; Windows below):

   ```sh
   aws ssm start-session --target <instance-id> --region $REGION
   sudo journalctl -u bb-runner -u bb-worker -u cucina-worker-agent -o cat --no-pager | tail -100
   ```

   Look for `event=bootstrap.failed reason=...`:

   | `reason` | Meaning | Look at |
   | --- | --- | --- |
   | `boot-data` | The launch user data is missing or malformed | Controller and agent versions match? Image from this release? |
   | `imds` | Instance metadata or its tags are unreachable | IMDSv2 required with an adequate hop limit; instance metadata tags enabled in the launch request |
   | `host` | The controller's name or certificate does not verify | The CA in the boot data versus the server certificate (a CA rotation in progress? [rotate CA](rotate-ca-credentials.md)); DNS for the worker endpoint |
   | `enrollment-unreachable` | The worker endpoint is not reachable | Step 5 |
   | `enrollment-refused` | The controller rejected the identity | Step 3's log |
   | `enrollment-invalid` | The response failed validation | Agent and controller protocol versions; clock skew |
   | `storage` | The L1 volume could not be prepared | Instance type has the expected instance store, or the pool's data volume spec |
   | `render` or `write` | Configuration could not be rendered or written | Agent versus settings version; disk space |

   **A failed bootstrap powers the instance off, which terminates it**, usually within two minutes, so it may be gone before you get a shell. Read what it left behind:

   ```sh
   aws ec2 get-console-output --instance-id <instance-id> --latest --region $REGION --output text | tail -60
   ```

   and the controller log above. Windows workers: `Get-Service cucina-bb-runner,cucina-bb-worker,cucina-worker-agent`, `C:\ProgramData\cucina\logs\agent.log`, `C:\bb\log\boot.log`.

5. **Connectivity and time.** From the instance (or an identical debug instance in the same subnet and security group):

   ```sh
   sudo /opt/cucina/bin/cucina-worker-agent selftest        # IMDS, disks, time sync, virtual file system, Buildbarn binaries, build user (JSON report)
   chronyc tracking                         # the clock must be within seconds; a certificate is not valid before its start time
   ```

   Then check the path to the worker endpoint (the address in the chart notes under "Worker endpoint"): the worker security group must allow egress to it; the control-plane side must allow the worker CIDR on the enrollment,
   storage and scheduler ports; the load balancer's idle timeout must exceed two minutes (workers long-poll the scheduler); workers must reach it by **private** address.

6. **Registered at the agent but not at the scheduler.** `bb_worker` connects to the scheduler's worker port with mTLS. Check `journalctl -u bb-worker -o cat`. A certificate with the wrong URI SAN, a CA that the scheduler does not
   trust, or a blocked port all end here. `docs/security.md` lists the exact identities.

7. **macOS pools.** The host, not EC2, launches the VM. `cucinactl hosts list` shows the host's state, slots and images; `cucinactl hosts diag <host>` collects its logs. Usual causes: the host is cordoned, offline or unapproved; the image is not
   pulled yet; the VM could not start (a locked keychain shows as `SecKeyCreateRandomKey_ios failed`, see the [setup guide](../macos/mac-mini-setup.md)); the controller could not issue the VM identity.

## Fixes

* Wrong or missing tags, IAM: fix the controller's launch permissions and tag conditions, then let the pool relaunch.
* Network: security groups, routes, the worker endpoint address, load-balancer timeouts, DNS. Prefer private addresses ([data transfer](data-transfer.md)).
* CA or boot-data mismatch after a rotation: finish the rotation ([rotate CA](rotate-ca-credentials.md)); new launches use the new boot data.
* A bad image: roll back the pool's image to the previous AMI ([AMI rollout and rollback](ami-rollout-rollback.md)).
* Clock: the images use chrony against the Amazon Time Sync Service; check the AMI's configuration if instances start with a wrong clock.

## Roll back

`cucinactl pools cordon $POOL --undo` resumes launching. If you changed the image or settings, revert them; the previous generation keeps serving.

## Verify

Launch one worker (queue a small action, or `cucinactl pools scale-floor $POOL --min 1 --for 30m` and remove it afterwards by letting it expire). `cucinactl workers list --pool $POOL` shows it `idle` (or `busy`) within the pool's
`startupTimeout`: a worker that has registered with the scheduler is reported by its activity, and `cucinactl pools describe $POOL` shows the start timings (API call, running, registered, first action).

## Escalate

Attach `cucinactl diag --include-logs`, the instance IDs, the `bootstrap.failed` lines or console output, and the controller's enrollment log lines for the same period.
