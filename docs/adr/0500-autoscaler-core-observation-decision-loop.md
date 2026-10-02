<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0500 — Autoscaler core: one pure decision function over raw observations

* Status: accepted (2026-10-02)

## Context
R-TEST-8c asks for reconcilers expressed as `Step(state, event, now)` and a deterministic simulation; contracts §3 fixes the scale-out
formula, idle-timer scale-in and the drain protocol. `domain.Observation` carries already-classified `VM`s, but classifying a VM
(launching / registered / draining / stopping / failed …) *is* the state machine, and R-SCALE-5 needs it rebuilt from raw sources
(tag-filtered `Describe`, hostd reports, the scheduler's worker list and drains) after a restart.

## Decision
`internal/scaling` exposes `Planner.Plan(spec scaling.Spec, obs scaling.Observation, st *PoolState) Decision`: no I/O, goroutines or
wall clock; randomness only from an injected `ports.Rand`. `scaling.Spec` embeds `domain.PoolSpec` and adds autoscaler-only inputs
(vCPUs, instance types/subnets, floor windows, daily cap, deleting). `scaling.Observation` carries the *raw* sources plus `Known`
flags and the `Results` of the previous actions (the executor reports exactly one result per action, `ErrSkipped` if not run). The
VM state machine runs inside `Plan`; `Decision.VMs` is the classified `[]domain.VM` view for status and metrics, so `domain.Observation`
is not used by the planner. Actions are ordered (remove-drain, undrain, terminate/stop, drain, launch, fail-queues) and carry reason
codes: actions map to `cucina_scale_decisions_total{action}`, stop reasons to `cucina_vm_stops_total{reason}`. Two stop reasons are
added to contracts §6: `retire` (pool deleted / max lowered) and `external` (VM vanished without a controller decision).
Every stop goes through a drain that is confirmed idle in the worker list (also for failed and retired VMs), except forced stops after
`drainTimeout`; the planner re-checks its own actions against `invariants` and drops violating ones.

## Consequences
One function to property-test and simulate; the reconciler is plumbing. Partial observations degrade safely (no scale-in or stop
confirmation without the worker list, no launch before the first provider listing). The lead may drop or align `domain.Observation`.
