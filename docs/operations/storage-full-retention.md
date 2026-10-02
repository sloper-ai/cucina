<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: storage full or retention too short

**Use when** the storage-nearly-full or retention-too-short alert fires, the cache hit rate drops, Bazel clients report evicted blobs and rewind or retry, or an action cache entry is wrong and must be purged.
**Severity:** Soon (builds still work, but slower and with retries). **Time:** 30 minutes to diagnose; a resize is planned work.

## Background

Cucina's central cache (L3) is Buildbarn's `local` storage: a ring of blocks on a volume. A full **configured store** evicts its oldest blocks normally. That is different from exhausting the backing filesystem/PVC: it still needs headroom for metadata and state, and filesystem exhaustion can make `bb_storage` fail.
What matters is **retention**: how old the oldest data is. Retention must stay above two things: the longest build, and Bazel's remote-cache TTL (`--experimental_remote_cache_ttl`, three hours by default). If retention drops below the TTL, a client may be told that a blob exists
(from its own knowledge) when the cluster has already evicted it; Bazel then retries (`--experimental_remote_cache_eviction_retries`) or rewinds the build. The action cache is wrapped in `completenessChecking`, so an AC entry whose outputs were evicted is reported as missing rather than
returned broken.

## Symptoms

* The retention alert fires: `cucina:cas_retention_seconds` is below its threshold (the TTL plus a margin).
* Cache hit rate falls without a code change; more actions execute remotely than before.
* Clients log lost inputs, `FindMissingBlobs` reporting many missing blobs, or rewinding.

## Checks

1. **Retention against the TTL.** In Prometheus (or Grafana's Cucina overview, "storage retention"):

   ```promql
   min(cucina:cas_retention_seconds)          # the oldest shard decides
   ```

   Compare it with Bazel's `--experimental_remote_cache_ttl` (three hours unless your clients' `.bazelrc` sets it; the file `cucinactl bazelrc` generates sets the eviction retries and `--rewind_lost_inputs`, and carries a reminder comment, but does not set the TTL) and with your longest build. Retention shorter than the TTL plus a margin of your choosing is the problem.

   A fresh cache, or a store that was just emptied (a resize, a [storage loss](storage-loss.md)), has not evicted anything yet, and until the first eviction the gauge reads "since 1970" (about 1.8e9 seconds): retention is unbounded, it does **not** grow up from zero, and **no retention alert fires** until the store has filled once. A low retention is therefore always a real, post-eviction number.

2. **Which shard, which size?**

   ```sh
   kubectl -n cucina get pvc -l app.kubernetes.io/component=storage      # sizes: data-* (file mode) or meta-* and cas-* (block mode), one set per shard
   ```

   ```promql
   min by (pod) (time() - buildbarn_blobstore_old_current_new_location_blob_map_last_removed_old_block_insertion_time_seconds{cucina_component="storage", storage_type="cas"})
   ```

   One shard with much shorter retention than the others points at skewed load (very large blobs hashing to one shard) or a failing volume; all shards short point at too little capacity or too much written.

3. **Why more is written.** A sudden rise in ingest explains falling retention: a new large build, a client uploading local results that it need not (`--remote_upload_local_results` for read-only principals), outputs that are not needed downloaded and re-uploaded, or builds without `--remote_download_outputs=minimal` on CI.
   `cucinactl cost` and the data-transfer panels show bytes by path ([data transfer](data-transfer.md)).

## Fixes

* **Reduce what is written.** Read-only clients should not upload (`cucinactl bazelrc --platform linux --read-only`); CI uses `--remote_download_outputs=minimal`; make sure toolchains come from the repository contents cache rather than being re-uploaded per client.
* **Raise the alert's patience only if the TTL is lowered.** You may set a lower `--experimental_remote_cache_ttl` in your own `.bazelrc` (the generated file does not set it) to stay under retention, at the cost of more re-checking.
* **Add capacity.** Increase the store sizes in the chart values (`storage.stores.<store>.size`), or add shards (`storage.shards`). Understand the cost first: **changing a store's size re-lays it out and starts that store empty on every shard**, and a different shard count changes where blobs live, so **plan for a cold cache**
  ([loss of storage](storage-loss.md)); size it once. Choose a quiet window, announce it, and apply with `helm upgrade` ([Helm upgrade, rollback and uninstall](helm-upgrade-rollback-uninstall.md)).
  If the volumes themselves must grow, the storage StatefulSet's volume claim template cannot be edited after install: expand the PVCs and orphan-delete the StatefulSet as described in [`chart.md`](chart.md#upgrade) before the `helm upgrade`.
* **Purge poisoned action results.** If bad action results got into the AC (a non-hermetic action cached a wrong output, a toolchain was broken for a day), invalidate every AC entry produced before a point in time without touching the CAS. The frontend wraps the AC in `actionResultExpiring`, whose `minimumTimestamp` hides every result whose
  worker completion time is earlier. In the chart this is `buildbarn.frontend.actionCachePurgeBefore` (an RFC 3339 time); it applies after the frontends roll:

  ```sh
  helm upgrade cucina ./charts/cucina -n cucina -f values.yaml \
    --set buildbarn.frontend.actionCachePurgeBefore=2026-10-02T12:00:00Z
  kubectl -n cucina rollout status deploy/cucina-frontend
  ```

  Only results produced by workers are hidden: results that clients uploaded themselves, without execution metadata (`--remote_upload_local_results`), are not affected. To drop those too, empty the action cache by changing `storage.stores.ac.size` (a cold AC).

  First stop the source of bad results (fix or drain the affected toolchain/workers), then choose a cutoff **after the last potentially bad result was produced**. A cutoff before the bad interval would leave those results visible. Clients re-execute affected actions; existing CAS blobs can be reused without uploading them again. The `--set` is only for this one command: put the same value into your version-controlled values file (see [Helm upgrade, rollback and uninstall](helm-upgrade-rollback-uninstall.md)), or the next `helm upgrade -f values.yaml` drops it and the purge is undone. Leaving it set is harmless. Existence caching never exceeds retention, and
  its TTL is a minute, so there is nothing else to flush.

## Roll back

Clearing or moving `actionCachePurgeBefore` backwards can expose old, poisoned entries again: it is a read filter, not deletion. Keep the cutoff until those entries have expired or the AC has been emptied; do not undo it merely to improve the hit rate. A capacity change is undone with `helm rollback`, which does not bring a discarded cache back.

## Verify

Retention climbs back above the TTL plus margin (`min(cucina:cas_retention_seconds)`), the hit rate recovers, and clients stop logging evictions. After a purge, rebuild a known target: it re-executes once and is cached afterwards.

## Escalate

If retention stays low with normal ingest, or one shard misbehaves, attach `cucinactl diag --include-logs`, the retention graph for the last week, and the PVC and storage-class details.
