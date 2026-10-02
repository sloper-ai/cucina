<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0414 — BuildQueue adapter: counts, errors and retries

* Status: accepted (2026-10-02)

## Context
`internal/buildqueue` implements `ports.BuildQueue` over bb_scheduler's BuildQueueState API (pinned
bb-remote-execution 1a3be95). The proto comments leave several semantics to the implementation
(`in_memory_build_queue.go`).

## Decision
* `QueueObservation` per size class queue, from its **root invocation**: `Queued = queued_operations_count.direct +
  .indirect` (direct = operations of the root invocation itself, indirect = of all descendant invocations);
  `Executing = executing_workers_count` and `Idle = idle_workers_count` are propagated to every ancestor by the
  scheduler, so the root's values cover the queue; idle includes idle-synchronizing workers
  (`idle_synchronizing_workers_count` is per invocation, not aggregated, so it is not used). `Workers =
  workers_count`, `Drains = drains_count`.
* Errors: NotFound on queue-scoped calls → `ports.ErrQueueUnknown`; on operations → `ports.ErrNotFound`;
  InvalidArgument → `ports.ErrInvalid`; the gRPC status stays reachable (`status.Code`).
  `KillOperations{QueueWithoutWorkers}` on a queue that has workers (FailedPrecondition) returns nil and kills
  nothing — the porttest contract.
* Retries: gRPC service-config retry (3 attempts, 0.1–1 s backoff, UNAVAILABLE) for every method except
  KillOperations; 5 s per-call timeout; connections are lazy (a starting scheduler is not an error).
* `OperationState` does not name its worker: executing operations get `Worker` from one paged `ListWorkers` per
  involved queue. `InvocationID` is the REAPI `tool_invocation_id` (else correlated invocations ID, else the key's
  type). `QueueKey.PlatformKey` maps back to properties from the last `ListPlatformQueues`, else by parsing
  `name=value;…` (values containing `;` are not representable in `domain.PropertiesKey`).
* mTLS: `TLSFromFiles` re-reads the controller key pair when its files change (rotation without restart).

## Consequences
The adapter passes `porttest.RunBuildQueue` against the real pinned scheduler plus a Synchronize-driven fake
worker (`internal/bbtest.FakeWorker`), in the integration tier.
