<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 1007 — Schedule execution canaries on the controller leader with durable reservations

* Status: accepted (2026-10-02)

## Context

R-TEST-7 requires cache probes every five minutes and one uncached execution per pool daily and after deployment,
including scale from zero. Execution probes already exist in `internal/canary`, but the controller scheduled only
cache probes. A CronJob plus deployment hook would duplicate credential mounts, TLS/configuration and overlap
handling; a controller restart must not purchase another cold start. This refines the execution scheduling
anticipated by ADR 1004 without changing its result-export design.

REAPI selects an instance and exact runner properties, not a WorkerPool or a size class. Two pools serving the same
route cannot independently be targeted merely by changing the canary's metric label. Pausing peers or draining
workers to create a cold start would violate normal fleet operation.

## Decision

* Keep the cache loop unchanged. Add a separate, leader-elected execution scheduler using the same `Probe`,
  metrics, service key, STS token verification, endpoint and TLS configuration. No execution CronJob or hook Pod.
* Helm passes `<release>/<revision>` as `CUCINA_CANARY_DEPLOYMENT`, changing the controller Pod template even on
  configuration-identical upgrades. Check each pool's durable schedule every minute; run serially once per 24 hours
  or new deployment identity. A newly created/unobserved pool waits for reconciliation. Standalone controllers stay
  cache-only unless an operator supplies an explicit stable deployment identity; a process restart is not a deploy.
* `canary.execution.enabled` defaults to true in Helm. Disabling it preserves cache probes. The execution timeout
  defaults to 15 minutes and must be positive and no greater than 15 minutes. Existing global
  `CUCINA_CANARY_DISABLE=true` disables both schedules. The action itself retains its one-minute timeout,
  `skip_cache_lookup`, unique nonce and `do_not_cache`.
* Optimistically write `cucina.sloper.ai/execution-canary` on the WorkerPool before submission. The annotation stores
  deployment identity, attempt time, overlap hold, last result and historical last-success time. An outstanding hold
  anywhere blocks new executions. One absolute probe deadline starts **before** reservation I/O, so delayed or
  ambiguous API responses consume that budget and cannot extend execution beyond the persisted hold. Hold until that
  deadline plus two minutes on failure/interruption: the pinned scheduler retains abandoned operations for one minute,
  and an action picked up then can run for another minute. Success clears the hold. API errors never authorize
  execution. Lease loss cancels the client context.
* An interrupted reservation immediately restores non-passing metrics and its attempt time on the next leader;
  after the hold expires it becomes a persisted incomplete failure. Do not retry that daily/deployment attempt:
  exactly-once execution across a Kubernetes write and REAPI submission is impossible. This deliberately chooses
  bounded spend and honest missing qualification over automatic duplicate work after a crash.
* Select a catalog-native/non-generic runner and a served instance whose exact route belongs to only one configured
  pool. Confirm the returned worker identity's `pool` label, and reject cached results. Where no unique route exists,
  record an explicit failed preflight; do not claim that another pool qualified. Distinct runner properties or instance
  assignments are required for independent qualification of such pools.
* Paused, deleting, capacity-disabled, unready or degraded pools do not receive work. Known preflight failures consume
  that scheduled attempt; recovery does not trigger an extra paid retry. Unknown/stale initial readiness waits for
  reconciliation. Current unavailability or pending deployment evidence masks execution `up` to zero even when a prior
  attempt succeeded; historical last-success timestamps remain historical. A failed inventory/state refresh masks all
  known execution health; deleted pools remain non-passing. Cache health and historical successes are not reset.
  Restore gauges, not run counters/histograms.

The settle allowance comes from the exact pinned upstream constructor
[`cmd/bb_scheduler/main.go:132–155`](https://github.com/buildbarn/bb-remote-execution/blob/1a3be9574872/cmd/bb_scheduler/main.go#L132-L155),
which hardcodes `OperationWithNoWaitersTimeout: time.Minute` (not a runtime override), and
[`maybeStartCleanup`](https://github.com/buildbarn/bb-remote-execution/blob/1a3be9574872/pkg/scheduler/in_memory_build_queue.go#L2341-L2345).
Cleanup advances on queue API entry, not an independent real-time timer. Combined with the probe's one-minute action
timeout, this is a conservative scheduling allowance for responsive pinned components, **not** proof that a remote
process is quiescent under arbitrary scheduler/worker stalls, network failure or failed timeout enforcement. Revisit
the bound when upgrading Buildbarn; live fault qualification remains outstanding.

## Consequences

Naturally empty pools exercise the ordinary queue-driven scale-from-zero path, including its capacity/cost limits.
Busy workers are neither interrupted nor drained, and nonempty pools are not forced to zero. Queue time and stale
WorkerPool counts do not prove a cold start; live qualification needs independent provider/worker observations.
The service key must authorize execution and cache access on each selected instance; retiring break-glass requires
`hooks.test.credentialSecret` for both schedules. Per-pool results remain visible in metrics and the annotation.

Focused fake-client scheduling, pinned local Buildbarn probe and Helm rendering regressions cover the implementation.
No deployment, cloud/VM start, live leader failover or measured scale-from-zero qualification was performed for this
change. Shared-queue per-pool qualification remains unsupported and explicitly non-passing; revisit it only with a
routing contract that genuinely distinguishes those pools.
