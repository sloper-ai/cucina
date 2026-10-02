<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 1004 — Canary results reach Prometheus through the controller, not a Pushgateway

* Status: accepted (2026-10-02)

## Context
R-TEST-7 asks for a cache canary every 5 minutes and an execution canary per pool daily and after deploys, exported as
Prometheus metrics. A CronJob pod lives too briefly to be scraped; a Pushgateway would add a component (and RSS to NFR-M1).

## Decision
* `internal/canary` is one library: `Probe` (cache: STS mint + JWKS verify + AC/CAS round trip; exec: a tiny action with
  `skip_cache_lookup` and `do_not_cache` on the pool's exact runner properties), `Metrics` (`cucina_canary_*`), `Loop`
  (in-process cadence with jitter) and `Handler`/`Push` for one-shot runs.
* The controller runs the cache canary in-process every 5 minutes (`Loop`) and serves `Handler` on its in-cluster metrics
  listener; `cucina-controller canary cache|exec` (cobra `canary.Command()`, agent coreb) runs one probe, prints the result,
  exits non-zero on failure (helm test, post-upgrade hook) and pushes the result with `--report-to` (daily exec CronJob).
* Metric names: `cucina_canary_runs_total{kind,pool,result}`, `cucina_canary_duration_seconds{kind,pool}`,
  `cucina_canary_step_duration_seconds{kind,step}`, `cucina_canary_last_run_timestamp_seconds{kind,pool}`,
  `cucina_canary_last_success_timestamp_seconds{kind,pool}`, `cucina_canary_up{kind,pool}`,
  `cucina_canary_exec_queue_seconds{pool}`; `slo/` builds `cucina:canary_success:ratio_1h` and the `cache-canary` SLO on them.

## Consequences
* The names must be appended to docs/contracts.md §6 (lead).
* The canary's service key needs `cas-read`, `cas-write`, `ac-read`, `ac-write` and `execute` on the probed instance name;
  its AC entries are tiny and keyed by a per-run synthetic digest.
* The probes are proven against the pinned `bb_storage` and `bb_scheduler` binaries (integration tier, `internal/bbtest`).
