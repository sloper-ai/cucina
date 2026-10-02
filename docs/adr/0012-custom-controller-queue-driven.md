<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0012 — D2: One Go controller drives pools from the scheduler's queue state

* Status: accepted (2026-10-02)

## Context
Existing autoscalers do not fit. `bb-autoscaler` is a cron job keyed on a Prometheus p95 and only
resizes ASGs, EKS node groups and Deployments. ASG warm pools add more than a minute of polling
latency and keep stopped instances, which bill for their EBS volumes. KEDA only scales pods.
None of them can launch Tart VMs, and none can see Buildbarn's per-platform queues directly.

## Decision
One custom controller (Go, controller-runtime, leader elected, two replicas) with the `WorkerPool`
and `MacHost` CRDs. It polls the scheduler's BuildQueueState API every one to two seconds
(Prometheus is not in the scaling path), runs a pure `Plan` function per pool, and executes its
decisions through the `Compute`, `VMRuntime`/host-fleet and `BuildQueue` ports. It is level
triggered and crash-only: after a restart it rebuilds state from EC2 (tag-filtered), the hosts'
reports and the scheduler's worker list.

## Consequences
* We own the autoscaler, and cost runaway, orphaned instances and starvation are its failure modes.
  Hence the investment in property tests, the deterministic simulation tier and the invariants that
  stay on in production (ADR 0024, `TESTING.md`).
* Decision logic is separate from I/O, so policy changes can be shadowed and replayed against queue traces.
