<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: AMI rollout and rollback

**Use when** you put a new worker image (an AMI) into service, or take a bad one out. The same mechanism serves Linux (x86_64 and arm64) and Windows pools; Tart images follow the same generation logic ([new Xcode, Visual Studio or OS release](new-xcode-vs-os-release.md)).
**Severity:** Planned (a rollback to a known-good image is a Page when workers are failing). **Time:** the build takes the longest (Windows can take over an hour); the rollout itself is minutes.

Cold-start times below are planning references, not a measured guarantee for these images. Record your actual launch-to-first-action times during the smoke test and compare them with the acceptance targets.

## How a rollout works

A new image version starts a new **pool generation**. Every AMI carries `cucina:image-version` and `cucina:generation` tags. Once the pool resolves the new image:

* **New launches use it immediately.** Old-generation workers keep running; they finish their actions and are terminated when they go idle (`rollout: lazy`, the default), or are drained and replaced right away (`rollout: eager`).
* **A worker is replaced only after it drains.** A drain waits for the worker's running actions for up to `timers.drainTimeout` (30 minutes by default); an action that is still running then is cut off and retried by the scheduler or Bazel.
* **How the pool finds its image.** `image.ami` pins one AMI. `image.amiSelector` (tags) follows the **newest available AMI in the account that carries every selector tag**, so a freshly built AMI that matches is adopted by itself as soon as it is available. To stage a release, pin `image.ami`, or add a selector tag to the AMI only after its smoke test.
* The previous AMI stays registered for rollback. The only standing EC2 costs of a pool are the storage of the current and previous AMI and, for **Windows** pools with `ec2.fastLaunch` enabled, the snapshots that EC2 Fast Launch pre-provisions.
  The controller manages Fast Launch for those pools: it enables it on the AMI the pool resolves (`targetCount` snapshots, by default the pool's `capacity.max`), disables it on the AMI it replaces and on every image before the pool is deleted, and waits until AWS reports `disabled`. The images it enabled are listed in the pool's
  `cucina.sloper.ai/fast-launch-images` annotation. An AMI must have Fast Launch **disabled before it is deregistered**; otherwise its snapshots and prep resources (tagged `CreatedBy: EC2 Fast Launch`) linger and keep costing money.

## Roll out

1. **Build the image.** Each build produces an AMI tagged with its version and generation (the Makefiles compute the next generation):

   ```sh
   make -C workers/linux image-linux ARCH=x86_64 VARIANT=ubuntu          # also ARCH=arm64; VARIANT=al2023 for Amazon Linux
   make -C workers/windows image-windows STAGE=worker FAST_LAUNCH=1      # STAGE=base first when the base layer (Visual Studio, SDK) changed
   ```

   Today these scripts are wired to the acceptance environment ([Linux and Windows images](images.md)): they read the builder network (VPC, subnet, builder security group, the CIDR allowed to reach the builder) from its outputs file, `~/.config/cucina/aws-e2e/base-outputs.json` (`OUTPUTS=<file>` points them at another file with the same keys), expect the environment's run variables (`CUCINA_RUN_ID`, `CUCINA_EXPIRES`),
   tag what they create with them, and read the region from `AWS_REGION`. To build in another account, give them an outputs file for that account.

   Find what you built:

   ```sh
   aws ec2 describe-images --owners self --region $REGION \
     --filters "Name=tag:cucina:image-version,Values=<version>" \
     --query "Images[].[ImageId,Name,Tags[?Key=='cucina:generation']|[0].Value,CreationDate]" --output table
   ```

2. **Windows only, to avoid a slow first launch: enable Fast Launch on the new AMI and wait for it.** The controller enables it itself when the pool starts using the AMI (step 3), but until the snapshots are ready the first launches take the slow path (about four minutes instead of about 85 seconds). Set the count to the pool's `max` so that a full scale-from-zero burst is fast:

   ```sh
   aws ec2 enable-fast-launch --image-id <new ami> --resource-type snapshot --snapshot-configuration TargetResourceCount=<pool max> --max-parallel-launches 6 \
     --launch-template LaunchTemplateName=<name>,Version=<ver> --region $REGION
   aws ec2 describe-fast-launch-images --image-ids <new ami> --region $REGION --query "FastLaunchImages[].[ImageId,State]" --output text    # wait for: enabled
   ```

   `workers/windows/scripts/fast-launch.sh enable --ami <new ami> --count <pool max> --wait` does the same inside the acceptance environment (it needs the variables above). The first enable in an account creates the service-linked role `AWSServiceRoleForEC2FastLaunch`; leave it in place.

3. **Point the pool at the new image.** (A pool with a tag selector that the new AMI matches has already done this by itself.) Change the pool's image in your Helm values (`image.ami`, or the `image.amiSelector` tags) and `helm upgrade`. This is a live change: it does not restart the scheduler. For an emergency, patch the resource and then put the same change into the values:

   ```sh
   kubectl -n cucina patch wp $POOL --type merge -p '{"spec":{"image":{"ami":"<new ami>"}}}'
   ```

   To replace old workers without waiting for them to go idle: `kubectl -n cucina patch wp $POOL --type merge -p '{"spec":{"rollout":"eager"}}'`.

4. **Watch it.**

   ```sh
   cucinactl pools describe $POOL            # conditions (ImageResolved), recent events, the image generation
   cucinactl images                          # per pool: reference, version, generation, current or previous, workers running, Fast Launch state
   cucinactl workers list --pool $POOL       # generation per worker
   kubectl -n cucina get wp $POOL -o json | jq '.status | {imageGeneration, resolvedImage, oldGenerationVMs}'
   ```

5. **Smoke test the new generation.** Run a tiny remote build that must execute (not hit the cache) on the pool, for example `bazelisk build //path:hello --noremote_accept_cached`, and confirm in `cucinactl workers list --pool $POOL` that a new-generation worker ran it. For Windows measure a cold start with a Fast Launch snapshot available.

6. **Retire the old image later**, when the rollback window has passed (keep the current and one previous). See "Retire an AMI" below.

## Roll back

Rolling back is the same operation in the other direction: point the pool at the previous AMI. New launches use it; newer-generation workers drain and go away.

1. **Windows only:** once the pool points back, the controller enables Fast Launch on the previous AMI and disables it on the bad one, but launches take the slow path (about four minutes instead of about 85 seconds) until the previous AMI's snapshots are ready. To avoid that, enable it first, as in step 2 of the rollout.
2. Set `image.ami` (or the selector) back to the previous AMI in the values and `helm upgrade`, or patch the resource as above. A pool that selects by tags keeps following the **newest** matching AMI, so pin `image.ami` to the previous AMI, or retag or deregister the bad one. Use `rollout: eager` if the bad image is actively failing builds, accepting that busy workers drain first (and are cut off after `drainTimeout`).
3. Verify with steps 4 and 5 above. Then check that Fast Launch is disabled on the bad image (`cucinactl images` shows its state, or use the describe command below) so its snapshots stop costing money.

## Retire an AMI

Keep the current and one previous AMI. To remove older ones by hand (this works in any account), for a Windows AMI first disable Fast Launch and wait:

```sh
aws ec2 disable-fast-launch --image-id <ami> --region $REGION                                                                       # Windows only
aws ec2 describe-fast-launch-images --image-ids <ami> --region $REGION --query "FastLaunchImages[].State" --output text      # wait for: disabled
aws ec2 deregister-image --image-id <ami> --delete-associated-snapshots --region $REGION
```

**Never deregister a Windows AMI before Fast Launch reports `disabled`.** Check for leftovers afterwards:

```sh
aws ec2 describe-snapshots --owner-ids self --region $REGION --filters "Name=tag:CreatedBy,Values=EC2 Fast Launch" --query "Snapshots[].[SnapshotId,StartTime,Description]" --output table
aws ec2 describe-fast-launch-images --region $REGION --query "FastLaunchImages[].[ImageId,State]" --output table
```

The repository script does the Windows ordering for you (`workers/linux/scripts/prune-amis.sh <family> --keep 2 --dry-run`, then without `--dry-run`), but it considers only AMIs that carry the current run's `cucina:run` and `cucina:env` tags (it needs `CUCINA_RUN_ID`), so it never touches AMIs from another run or account: use the commands above for those.

## Verify

* `cucinactl images` shows the intended generation as current, and `oldGenerationVMs` (in the pool status, step 4) falls to zero.
* New workers register within `startupTimeout` ([worker won't register](worker-wont-register.md) if not).
* With pools idle, the only standing resources are the current and previous AMI snapshots and, for Windows, Fast Launch on the current AMI ([cost leak](cost-leak.md)).

## Escalate

Attach `cucinactl pools describe $POOL`, `cucinactl images`, the AMI IDs and versions, and the failing instances' `bootstrap.failed` lines or console output.
