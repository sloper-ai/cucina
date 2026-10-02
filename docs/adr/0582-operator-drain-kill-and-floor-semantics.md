<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0582 — Operator drains, kills and floors through the management API

* Status: accepted (2026-10-02)

## Context
UC14 drains workers for maintenance while the autoscaler drains them for scale-in (R-SCALE-3); `ops kill` must stop work;
`pools scale-floor` adds standing cost (§1) and must not be forgotten.

## Decision
* **Drain**: `DrainWorker` adds the pattern `{node: <node>}` on every queue of the worker's pool (instance names × runners × size
  class); `UndrainWorker` removes exactly that pattern. The autoscaler only manages `{pool, node}` drains (docs/dev/scaling.md), so
  operator and autoscaler drains never undo each other; an operator-drained VM is stopped once idle.
* **Kill**: `KillOperations` fails the operation(s) with `FAILED_PRECONDITION` and "operation killed by a Cucina administrator: <message>".
  Bazel retries UNAVAILABLE/ABORTED/CANCELLED but not FAILED_PRECONDITION without missing-blob violations, so a kill sticks; the
  caller is recorded only in the audit log, not in the message clients see.
* **Floor**: `SetPoolFloor` requires `expires_in` (≤ 7 days, `Options.MaxFloorDuration`) unless it clears the floor
  (`min_running = 0`), and `min_running ≤ max`. Recurring floors belong in `spec.floorSchedule`.

## Consequences
An operator who wants a retry instead of a failure drains the worker rather than killing the operation. A floor needs renewal
after a week, which is intended: standing cost is always visible and temporary.
