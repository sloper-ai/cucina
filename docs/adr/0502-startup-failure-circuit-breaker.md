<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0502 — Startup failures: probe one VM at a time, back off per event

* Status: accepted (2026-10-02)

## Context
R-SCALE-2 fails and replaces VMs that do not register within `startupTimeout`. With a broken image or boot path, replacing every
failed VM relaunches the whole pool (`max` VMs) every `startupTimeout` + backoff — a cost runaway for large pools, and the queued work
still hangs until `QueueFailAfter`.

## Decision
A startup failure starts a streak that lasts until a VM registers. During the streak at most one VM may be starting (a probe); launches
beyond it are held with reason `startup-failures`. Backoff (equal-jitter exponential, `BackoffMin..BackoffMax`) advances once per
decision with failures, not per failed VM. With work waiting and no registered VM, `QueueFailAfter` fails the queued work (R-RE-2).

## Consequences
Simulation: a 10-minute boot outage costs 9 launches instead of 12 and recovers faster; a permanent one 7 instead of 11 over 40
minutes, independent of `max`. A transient fault delays full recovery by one probe cycle (≈ `startupTimeout`).
