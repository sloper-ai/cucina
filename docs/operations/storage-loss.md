<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: loss of storage

**Use when** the storage volumes are lost, corrupted, deleted, or replaced by empty ones, or you deliberately wipe the cache. **Severity:** Soon: builds keep working but are slow until the cache refills. **Time:** minutes to restore service; hours to a warm cache.

## What this means

**The cache is reconstructible and is not backed up.** Everything in the CAS and AC can be recomputed by running the builds again, so Cucina does not back it up and does not need to. Losing the storage volumes costs a **cold cache** and nothing else: no
configuration, identity or state of record lives there (the controller's state is in Kubernetes custom resources and Secrets; workers are stateless). What you will see:

* Every client's next build executes everything (cache misses), at the cold-build speed of the pools, and uploads results again.
* Clients that were mid-build lose inputs they believed were cached. Bazel retries them (`--experimental_remote_cache_eviction_retries`) or rewinds, so the build slows rather than fails.
* The remote repository contents cache is empty too: until the trusted writer re-seeds it, a fresh client downloads the external repositories (toolchains) itself.
* Workers' local L1 caches refill from the new central cache.
* The frontends' existence cache may hold stale "present" answers for up to its TTL (about a minute) after the volumes are replaced.

The expected cost is the cold-build time of your pools plus the repository-contents re-seed. [MT-008](../testing/manual/MT-008.md) measures it; record the numbers for your deployment here when you have them.

## If the loss was not intended

1. **Stabilise the control plane.** Check the storage pods and what they report:

   ```sh
   kubectl -n cucina get pods,pvc -l app.kubernetes.io/component=storage
   kubectl -n cucina logs -l app.kubernetes.io/component=storage --prefix --tail=100      # every shard, not just one pod
   ```

2. **Bring the storage up on empty volumes.** If the volumes were replaced (new PVCs bound), the pods start with empty state and Buildbarn starts a fresh cache. If one shard's volume holds *partially valid* state and its pod crash-loops with a persistent-state error, give that shard clean volumes: delete its claims and its pod, and the StatefulSet
   provisions new claims when it recreates the pod. The claims of shard `<n>` are `data-cucina-storage-<n>` in file mode, or `meta-cucina-storage-<n>` **and** `cas-cucina-storage-<n>` in block mode (delete both, they only make sense together):

   ```sh
   kubectl -n cucina delete pvc data-cucina-storage-<n> --wait=false     # block mode: meta-cucina-storage-<n> cas-cucina-storage-<n>
   kubectl -n cucina delete pod cucina-storage-<n>
   kubectl -n cucina rollout status statefulset/cucina-storage
   ```

   To start **every** shard clean (a drill, or all volumes damaged) take the whole store down instead: scale the StatefulSet to zero, delete all of its claims, and scale back to the shard count:

   ```sh
   kubectl -n cucina scale statefulset/cucina-storage --replicas=0
   kubectl -n cucina delete pvc -l app.kubernetes.io/component=storage
   kubectl -n cucina scale statefulset/cucina-storage --replicas=<shards>
   kubectl -n cucina rollout status statefulset/cucina-storage
   ```

   Note that deleting PVCs is irreversible by design; the cache they held is gone.

3. **Clear stale frontend knowledge.** Roll the frontends so the existence cache starts empty (not required, it expires within a minute): `kubectl -n cucina rollout restart deploy/cucina-frontend`.
4. **Verify the service.** `helm test cucina -n cucina` (a CAS and AC round trip), and `cucinactl status`. The cache canary should go green.
5. **Re-seed the repository contents cache.** Trigger the trusted writer (CI on `main` holding `ac-write` for the seeding scope) so the toolchains are stored once for each client platform; until then fresh clients download them. The blobs deduplicate in the CAS.
6. **Let clients refill the cache.** Expect cold builds first. Do not "warm" the cache with unnecessary builds on pools that cost money; the next normal builds do it.

## If you wipe it on purpose (a drill or a corruption response)

Do the same steps, on a staging deployment first. [MT-008](../testing/manual/MT-008.md) is the drill with measurements.

## Resizing and layout changes

A change of block layout (store sizes, shard count) can have the same effect as a loss, because Buildbarn discards persistent state that does not match the configured layout. Treat resizes as planned cold-cache events ([storage full or retention too short](storage-full-retention.md)).

## Verify

* The cache hit-rate alerts (action-cache hit ratio low, or halved against yesterday) fire after the loss and clear as the cache refills, within a day or so. The **retention** alerts do not fire: retention reads unbounded until each store has evicted for the first time, so it never grows up from zero ([storage full or retention too short](storage-full-retention.md)).
* The hit rate returns above 99 % for a repeated build ([NFR for warm rebuilds](../architecture.md#5-cache-tiers-and-the-cost-of-moving-bytes)).
* No worker restarts were needed.

## Escalate

If the storage pods will not start even on empty volumes, attach `cucinactl diag --include-logs`, the pod events, and the PVC and storage-class details.
