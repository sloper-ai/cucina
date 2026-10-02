<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0572 — Dead-man idleness from bb_worker's file-pool and executor metrics

* Status: accepted (2026-10-02)

## Context
R-POOL-7: a worker powers itself off when idle beyond 30 min, unable to reach the scheduler for 10 min, or up for 12 h, even
with the controller gone; the task requires idleness to come from `bb_worker`'s local Prometheus metrics, never from the
controller. The pinned `bb_worker` (bb-remote-execution `20260930T173749Z-1a3be95`) exports no "actions in progress" gauge: the
build executor histogram (`buildbarn_builder_build_executor_duration_seconds`) is observed only when an action completes, so a
40-minute test would look idle for 40 minutes. Alternatives considered: counting child processes of `bb_runner` (OS-specific,
not a metric), gRPC client metrics of `Synchronize` (called both when idle and when executing), FUSE callback counters
(silent during CPU-bound actions, noisy from unrelated scanners).

## Decision
`supervise` scrapes `http://127.0.0.1:<metrics_port>/metrics` every 15 s and counts as activity:
* **busy**: `buildbarn_filesystem_file_pool_files_created_total` > `…_files_closed_total`. With virtual build directories every
  file an action writes — at least its stdout and stderr — is a file-pool file that stays open until the build directory is
  released, so the difference is positive for the whole execution and zero when idle;
* **progress**: any increase of the build executor histogram's sample count (completed actions).
A failed scrape is no activity (a dead `bb_worker` is an idle worker). The scheduler is probed every 30 s with
`grpc.health.v1.Health/Check` over the worker's mTLS identity; any answer from the server counts as contact. Timestamps are
persisted in `/run/cucina/{last-activity,last-contact}` (Unix seconds; the modification time is the same instant) so restarts
of the agent do not reset the timers and the images' shell/PowerShell timers enforce the same limits (bootstrap writes the
pool's limits into their configuration).

## Consequences
* Correct for FUSE (Linux) and WinFSP (Windows) build directories, the EC2 defaults. With a native build directory only
  completions are visible, so a single action longer than the idle limit would be cut off; macOS VMs (native) are supervised by
  hostd, not by this switch.
* Metric names are upstream API: a Buildbarn bump must re-check them (`internal/workeragent/activity.go` lists the sources).
* `metrics_port = 0` makes activity unobservable; supervise logs an error and the worker counts as idle (cost-safe).
