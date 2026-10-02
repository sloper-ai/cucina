<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0574 — Spot notice drains with a service stop; every bootstrap failure powers off

* Status: accepted (2026-10-02)

## Context
R-POOL-2 (SHOULD): on the 2-minute Spot interruption notice, SIGTERM `bb_worker` so it stops taking work and finishes the
current action. The task suggested `systemctl kill -s TERM`. That delivers the signal, but a unit with `Restart=always` brings
`bb_worker` straight back and it takes new work. On Windows, shawl turns a service stop into CTRL_C (its `--stop-timeout`
bounds the drain).

The task also asked to fail closed and fast: retry transient enrollment errors with jittered backoff for at most 2 min, power
off immediately on a refusal. It did not say what to do on local failures (formatting the L1 volume, rendering, writing files).

## Decision
* Spot drain: `systemctl stop --no-block bb-worker.service` (Linux; SIGTERM via `KillSignal`, no restart, bounded by
  `TimeoutStopSec`) and `sc.exe stop cucina-bb-worker` (Windows; shawl sends CTRL_C). Polled every 5 s from
  `/latest/meta-data/spot/instance-action`, issued once, only when `handle_spot_interruption` is set.
* Every bootstrap failure powers the machine off: a worker that cannot enroll, mount its cache or render its configuration is
  useless and must not keep billing. Refusals (`InvalidArgument`, `NotFound`, `AlreadyExists`, `PermissionDenied`,
  `Unauthenticated`, `FailedPrecondition`, `OutOfRange`, `Unimplemented`, a controller certificate that does not verify) power
  off at once; everything else is retried until the 2-minute deadline. `--no-poweroff` (or `CUCINA_AGENT_NO_POWEROFF=1`) keeps a
  failed instance up for debugging over SSM.
* Exception: an instance **without any user data** (IMDS 404 or an empty body) is not a controller launch (EC2 Fast Launch pre-provisioning instances of the
  Windows AMI, image builds, manual launches). Bootstrap exits 2 (`reason=not-a-worker`) without powering off, so Fast Launch can
  finish its snapshots; Buildbarn is not started and the images' dead-man timer bounds the cost. User data that is present but
  not valid boot data powers off like any other failure.

## Consequences
* The controller must answer "try again" conditions (an instance not yet visible to `DescribeInstances`, rate limits) with
  `Unavailable` or `ResourceExhausted`; any refusal code terminates the instance within seconds.
* bb-worker.service's `TimeoutStopSec` (and shawl's `--stop-timeout`) should stay below the 2-minute notice (about 110 s)
  so a drain that overruns is killed before the instance disappears.
