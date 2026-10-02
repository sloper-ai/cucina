# 0002 — Platform queues are declared by the chart, not at runtime

* Status: accepted (2026-10-02)

## Context
R-RE-2 needs `predeclaredPlatformQueues` for scale-from-zero, and UC11 wants pool changes to apply "without restarting the control
plane". `bb_scheduler` reads its predeclared queues once at startup; there is no runtime API to add one.

## Decision
The chart renders `predeclaredPlatformQueues` (instance-name prefix × runner platform × size class) from `values.pools` and the platform
catalog; the scheduler (Recreate strategy) restarts on a config checksum change. Changes that do not alter the queue set — instance types,
image, max, idle timeout, concurrency, cache sizes — apply live through the `WorkerPool` resource. A `WorkerPool` created outside Helm for a
platform/size class whose queues are not declared reports `QueueDeclared=False` (reason `QueueNotDeclared`), never launches, and fails its
queued work immediately with a message that names the values to add. Adding a platform therefore needs `helm upgrade` (a scheduler restart,
which clients ride out with Bazel retries, UC8).

## Consequences
Simple, upstream-native, and testable (the render check boots `bb_scheduler`). The rare "new platform" operation restarts the scheduler.
