<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0411 — Worker L1: placement order and block sizing

* Status: accepted (2026-10-02)

## Context
The worker L1 is `readCaching{slow: central, fast: local, replicator: deduplicating{concurrencyLimiting{local}}}`
(R-CACHE-2, R-DATA-3). R-CP-3 gives upstream's sizing for the central storage: CAS blocks old 8 / current 24 /
new 3 + 3 spare, a key-location map (KLM) of ~66 B per entry at 2–10x the objects. For an L1 those numbers hit a
hard limit: a `local` backend stores no blob larger than one block (`old_current_new_location_blob_map.go:288`),
and `readCaching` returns the replication error to the reader. Ephemeral check (2026-10-02, pinned darwin
binaries, rendered macOS worker with an 8 MiB L1 = 1.6 MiB blocks, a 4 MiB input file):
`InvalidArgument: Failed to obtain input file "big.bin": Failed to replicate blob …: Blob is 4194304 bytes in size,
while this backend is only capable of storing blobs of up to 1671168` — the action fails, it does not bypass the
cache. With 38 blocks, an in-memory L1 of 4 GiB would refuse every input above 108 MiB (a clang binary).

## Decision
* `MinimumBlockSizeBytes` = 512 MiB is the largest blob an L1 must hold. Block counts are the first tier whose
  blocks stay ≥ 512 MiB: 8/24/3 (+3 spare on disk) — upstream's layout, used from 19 GiB (disk) / 17.5 GiB
  (memory) — then 2/6/1 (+1), then 1/2/1 (+1). Below 2.5 GiB the smallest tier is used and the plan notes the
  smaller maximum blob size. Buildbarn's 100-block limit is never reached. (The real block size is rounded down to
  the device's sector size.)
* KLM: one entry per 8 KiB of blocks (2x the objects at a 16 KiB average), at least 65 536; on disk 66 B per entry.
  Upstream's `maximumGetAttempts` 16 / `maximumPutAttempts` 64.
* Disk-backed L1s are `persistent` (state every 300 s and at graceful shutdown) with a
  `dataIntegrityValidationCache` (100 000 entries, 4 h): macOS VMs keep their L1 across shutdowns; EC2 workers
  keep it across a bb_worker restart.
* `auto` placement: instance-store NVMe → the launch-time EBS data volume → (macOS) the VM disk → memory when the
  VM has ≥ 16 GiB (L1 = min(RAM/4, 16 GiB)) → the root volume. Defaults: 60 % of a dedicated volume, 40 GiB on a
  macOS VM, 30 % of the root volume's free space (≤ 64 GiB); an explicit size is clamped to 70 % of the volume
  (50 % of RAM). The file pool (4 GiB per runner thread, sparse) shares the volume and is capped to 90 % of what the
  L1 leaves.

## Consequences
* Small L1s evict in coarse steps (one block is ≥ 20 % of the cache), the price of never failing a read.
* An in-memory L1 counts toward bb_worker's RSS; NFR-M2's 1 GiB bound excludes it.
* `PlanWorker` exposes every number (and the notes) so the agent and hostd can log them.
