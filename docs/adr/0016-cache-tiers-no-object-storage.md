<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0016 — D6: Three cache tiers on local storage, no object-storage backend

* Status: accepted (2026-10-02)

## Context
Buildbarn removed its S3/GCS backends in 2020 and Redis/HTTP in 2023, so the central cache must be
Buildbarn's own `local` storage. Transfer cost and latency differ greatly between the places a byte
can live (see `docs/operations/data-transfer.md`).

## Decision
* **L1** is worker-local: persistent on macOS VMs (their disk survives shutdown), instance-lifetime on EC2
  (zero idle cost forbids keeping volumes).
* **L2** is near the workers: a `bb_storage` cache on each Mac host; for EC2 the central storage itself, because it
  sits in the same AZ with free in-AZ traffic. Per-AZ L2 caches for multi-AZ pools are documented but add standing cost.
* **L3** is the central cache: sharded `bb_storage` with persistent `local` storage on PVCs.

## Consequences
* The cache is reconstructible and is not backed up: losing storage costs a cold cache and nothing else
  (`docs/operations/storage-loss.md`).
* Retention is the one capacity metric that matters: it must exceed the longest build and Bazel's remote-cache TTL
  (`docs/operations/storage-full-retention.md`).
