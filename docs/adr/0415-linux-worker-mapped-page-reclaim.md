<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0415 — Apply Linux worker reclaim policy only to disk-backed L1

* Status: proposed — candidate for a fresh campaign image and representative Abseil validation; not a qualified release default

## Context

T1 measured approximately 4090 MiB of `bb_worker` RSS against NFR-M2's 1024 MiB target. Its Go heap was small; the pinned, unmodified Buildbarn maps disk-backed L1 and file-pool storage. A separate bounded two-vCPU Linux experiment supported testing worker-only cgroup reclaim, but its audit does **not** establish instantaneous compliance or representative performance:

* Baseline scraped RSS was 3307089920 bytes (3153.887 MiB); a later treatment scrape was 781254656 bytes (745.0625 MiB).
* The immediate post-change `/proc` observation still showed approximately **3154 MiB**, before the later Prometheus observation of approximately **745 MiB**. `MemoryHigh` is soft reclaim/throttling, not an instantaneous RSS ceiling.
* The repeated warm workload's wall time increased from **7.712 s to 18.117 s**; p95 execution time increased from **0.241 s to 2.141 s**, with **15917** cgroup `high` events. Cache warmth differs between cold baseline and treatment; raw timing differences are not an isolated causal performance estimate.

An unconditional 768 MiB policy would also constrain supported anonymous-memory L1 stores. The renderer defaults those to `min(detected RAM / 4, 16 GiB)` and permits explicit sizes up to half of detected RAM. Live Go cache blocks cannot be discarded like clean file-backed pages. Shrinking cache capacity, adding a hard `MemoryMax`, relying on `GOMEMLIMIT`, or changing the RSS definition would not preserve the intended behavior.

## Decision

Implement a **Linux-only candidate**, without changing Buildbarn, the shared worker/runner units, cache sizes, or image resource settings:

* After successful enrollment/configuration rendering, `cucina-bootstrap` runs `cucina-worker-memory-policy`. Agent-absent, non-worker and failed-enrollment paths do not apply policy.
* Parse exactly one canonical, freshly written `CUCINA_L1_PLACEMENT` assignment from `/etc/cucina/env`; never evaluate the file as shell or use the pre-bootstrap inherited value. Unresolved, missing, duplicate or unknown values fail closed.
* For resolved `ebs`, `root-disk` or `instance-store`, atomically publish the helper-owned `/run/systemd/system/bb-worker.service.d/20-cucina-memory-high.conf` with `MemoryHigh=768M`. For `memory`, remove only that exact owned file. Preserve unrelated overrides, the runner/action cgroup, and `MemoryMax`.
* Refuse symlinked, unowned, multiply-linked or unsafe writable destinations. An existing file must match this helper's exact payload; operator edits are not overwritten. Publication does not replace a concurrently created entry.
* Bound `systemctl daemon-reload` to ten seconds and propagate failures before bootstrap returns. Reload on idempotent retries too, so a previous file update followed by a failed reload cannot authorize startup. Existing `bb-worker` ordering after `bb-runner` places this hook before worker startup.

The bootstrap's optional absolute filesystem root and the helper's explicit paths let one table-driven public-boundary test use staged files and a stateful command fake, without a live service manager. Provisioning already installs the helper's directory; it needs no new package or image configuration.

## Consequences

The shipped policy has a deterministic red-first boundary regression, but that proves configuration and ordering, **not** reclaim effectiveness. A fresh campaign image and representative Abseil validation remain required, including startup RSS, pressure, action correctness and slowdown. The bounded experiment did not qualify startup application, full Abseil, non-disk L1, Windows or macOS. Memory placement remains unchanged, not exempt from NFR-M2. The original T1 RSS failure remains a failure; no limit is raised and no RSS component is excluded. The lead owns the ADR index update and any subsequent rollout decision.
