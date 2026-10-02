<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 1005 — How the campaign computes the NFRs that need a definition

* Status: accepted (2026-10-02)

## Context
§8 states targets; several need an operational definition before they can be computed from data rather than eyeballed, and a
few depend on resources the test topology lacks.

## Decision
* **NFR-P1** sample = the controller's `launch → first action` (`pools describe` StartLatency) per VM started in the scenario;
  for the first VM of a scale-out add the time from the client's first remote spawn to that launch, so it spans "Execute
  submitted → first action executing". Execution canaries add one queue-time sample per pool.
* **NFR-P2** needs a local build on a worker-type instance: an optional `<lane>-baseline` client (same template, worker
  instance type) runs `test/e2e/abseil/local-baseline.sh`; without it NFR-P2 is reported **not measured** (never passed).
* **NFR-P4** is evaluated on the warm-L1 re-execution of T3/T6 (free slots, no scale-out) from the execution log
  (queue = `queue_time`; overhead = `setup_time + process_outputs_time`).
* **NFR-T1** numerator = bytes Bazel materialised in the client's output tree (with Build without the Bytes these are the
  downloaded outputs); denominator = output bytes of every executed spawn in the execution log. The host NIC counter is
  reported alongside as an upper bound.
* **NFR-T2** compares uploads (BEP NetworkMetrics) of a fresh output base on the Linux client with T1's first client when
  no second Linux client exists.
* **NFR-T3/T8** use the workers' blobstore metrics: L1 share = `local` Get bytes / (`local` + `grpc`) Get bytes; T8 divides
  `grpc` Get bytes by the CppCompile input-root bytes of the execution log (conservative: every remote action's fetches count).
* **NFR-T4** on the client↔frontend path is evidenced by negotiation: `--remote_cache_compression` in the emitted `.bazelrc`
  and `ZSTD` in the endpoint's GetCapabilities (the cache canary). Buildbarn exposes no wire-byte counters, so that path's
  ratio is not measured server-side; the host↔control-plane ratio comes from hostd's WAN byte counters against the L2 blob
  bytes in T13, and the CPU cost from the frontend pods' CPU during T1/T4.
* **NFR-T5/T9** are proven by topology: every tagged instance in one AZ, no NAT gateway, no public IPv4 on workers; the cost
  section adds the billed data-transfer lines when available.
* **NFR-X5** reports the client's upload bytes per configuration (the toolchains arrive from the repo contents cache).
* Spend is priced with `internal/cost` (the controller's model) plus a dated Price List snapshot of the campaign's instance
  types; the budget governor uses the larger of the scenario sum and the AWS-measured actual.

## Consequences
The report states the method next to each number; a missing prerequisite shows as "not measured", never as a pass.
