<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0503 — Idle means "idle at every poll"; blind polls restart timers; drain ownership by pattern

* Status: accepted (2026-10-02)

## Context
BuildQueueState gives instantaneous worker state, no per-worker activity counters; the controller polls every 1–2 s (R-SCALE-1).
The simulation sweep showed that when the scheduler is unreachable the idle and queue-empty timers kept running through the gap, so a
VM that worked during the gap was scaled in early. Operators also drain workers (UC14); the planner must not remove their drains.

## Decision
A VM is idle when no runner thread executes at a poll; its idle timer and the pool's "queues empty since" timer restart after any poll
whose worker list or queue list is unknown (a gap only delays scale-in). Sub-poll actions on an otherwise idle VM are invisible;
this is accepted (idleTimeout ≥ minutes) and draining VMs are rescued (`undrain`) before any new launch when demand returns. The
controller's drains use exactly the pattern `{pool, node}`; operator drains (management API) use `{node}`. A VM drained by someone
else stops counting as capacity and is stopped once idle for `idleTimeout` (reason `drain`); the planner never removes such drains.
Re-issued stops are limited to stops the controller itself sent.

## Consequences
Scale-in can be delayed by observation gaps (never accelerated). The management API must use the `{node}` pattern for operator drains.
