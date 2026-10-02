<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Sizing the Cucina control plane

The chart ships three size profiles (`sizeProfile`, defined in
[`charts/cucina/files/profiles.yaml`](../charts/cucina/files/profiles.yaml)); every value can be overridden per
component. Measured numbers from the acceptance campaign (§10.4, NFR-M1/M2) replace the estimates below as they arrive.

## Profiles

| | small (the k3s test) | medium (default) | large |
| --- | --- | --- | --- |
| frontend replicas / requests / limits | 1 / 250m, 256Mi / 2, 1Gi | 2 / 500m, 512Mi / 4, 2Gi | 3 / 1, 1Gi / 8, 6Gi |
| frontend zstd encoders/decoders | 16 / 16 | 64 / 64 | 256 / 256 |
| FindMissing existence cache entries | 100 k | 1 M | 4 M |
| storage shards | 2 | 2 | 4 |
| CAS / AC / FSAC / ISCC per shard | 100 / 1 / 1 / 0.25 GiB | 500 / 4 / 2 / 1 GiB | 2048 / 16 / 8 / 4 GiB |
| storage PVC per shard (file mode) | 115 GiB | 565 GiB | 2310 GiB |
| storage requests / limits | 250m, 512Mi / 2, 4Gi | 500m, 1Gi / 4, 8Gi | 2, 4Gi / 8, 32Gi |
| scheduler requests / limits | 100m, 128Mi / 1, 512Mi | 250m, 256Mi / 2, 2Gi | 1, 1Gi / 4, 4Gi |
| controller replicas / limits | 1 / 500m, 256Mi | 2 / 1, 256Mi | 2 / 2, 256Mi |
| STS replicas / limits | 2 / 250m, 128Mi | 2 / 500m, 128Mi | 3 / 1, 128Mi |

Go processes get `GOMEMLIMIT` = 85 % of their memory limit. NFR-M2 caps `cucina-controller` at 256 MiB in every profile.
The small profile requests 1.5 GiB of memory in total, inside NFR-M1's 2 GiB idle budget; the measured RSS goes in
the table at the end.

## Storage math (R-CP-3, ADR 0405)

Per store and shard, from one `size`:

* blocks: old 8, current 24, new 3 (CAS) or 1 (AC, FSAC, ISCC), spare 3 → block size = size / 38 (CAS). A blob must fit
  in one block, so the CAS needs ≥ 19 GiB (512 MiB blocks); the chart refuses less.
* key-location map: `keyLocationMapFactor` (default 4) × size / `averageObjectSize` entries of 66 bytes (the pinned
  record size). Defaults: CAS 32 KiB, AC 2 KiB, FSAC 1 KiB, ISCC 512 B → a 100 GiB CAS gets a 0.8 GiB map.
  `CucinaKeyLocationMapTooSmall` fires when insertions displace live entries: raise the factor or lower the average.
* PVC (file mode): sum of blocks and maps + 10 % + 1 GiB (filesystem reserve, state files). Block mode: the CAS device
  plus a filesystem PVC for the rest.

**Retention** (R-CP-4) must stay above Bazel's `--experimental_remote_cache_ttl` (3 h) and the longest build with margin.
A rough capacity-planning estimate is `shards × CAS size × 24/38 / write rate`; validate it under the actual workload.
Example: 2 shards × 100 GiB at 50 GB/day of new content ≈ 2.5 days.

The chart's `cucina:cas_retention_seconds` takes the minimum over central-storage targets only
(`cucina_component="storage", storage_type="cas"`), never ephemeral worker L1 or host L2 caches. The
[pinned producer](https://github.com/buildbarn/bb-storage/blob/086b011/pkg/blobstore/local/old_current_new_location_blob_map.go)
initializes its timestamp to current Unix time at map construction/restart, so before eviction the value is map age,
not infinite retention. On eviction the timestamp is when that block entered the old queue, not its blobs' upload time.
A missing L3 series stays absent and is not a successful retention observation. The warning remains below 6 h and the
critical guard below 3 h; startup/restart does not justify lowering either threshold.

## Memory and CPU drivers

* Frontend: zstd decoders (≤ `maximumDecoders` × 8 MiB window) and encoders, the existence cache (~100 B per entry), the
  JWT cache; CPU scales with compressed traffic (client uploads/downloads).
* Storage: page cache of the memory-mapped block files and key-location map counts against the container's memory
  limit; larger limits keep more of the hot set in RAM.
* Scheduler: in-memory queues and operations; grows with queued actions and workers.

## Measured (to be filled from the campaign)

| Measurement (§10.4) | small, k3s m8i.2xlarge | Notes |
| --- | --- | --- |
| control-plane RSS at idle / under load (NFR-M1) | — | |
| frontend / storage / scheduler peak memory and CPU | — | |
| CAS write rate and retention during T1–T22 | — | |
| key-location map displacement (`buildbarn_lossymap_hash_map_put_too_many_iterations_total`) | — | |
