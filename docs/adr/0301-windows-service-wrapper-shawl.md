<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0301 — shawl (not WinSW) wraps bb_worker/bb_runner as Windows services

* Status: accepted (2026-10-02)

## Context
Buildbarn binaries have no Service Control Manager integration. R-POOL-5 names WinSW; the requirement behind it is
that stopping the service delivers CTRL_C so `bb_worker` stops taking work and drains. WinSW 2.x needs .NET
Framework and its 3.x line is still pre-release; both are configured through XML next to each wrapper copy.

## Decision
Use **shawl v1.9.0** (single static Rust binary, `shawl-v1.9.0-win64.zip`, SHA-256 pinned in
`workers/windows/versions.json`, installed to `C:\bb\bin\shawl.exe` in the base stage). Services are created with
`shawl add` (`--stop-timeout 110000` for the worker = Ctrl-C then up to 110 s of drain, inside a Spot notice; `--kill-process-tree` for the
runner, `--dependencies cucina-bb-runner` for the worker, per-service log directories) and tuned with `sc.exe`
(demand start, recovery actions, virtual account for the runner).

## Consequences
No .NET dependency and no per-service XML; the stop semantics required by R-POOL-5 are shawl's default behaviour
and are exercised by the worker-stage smoke test (`verify-worker.ps1` measures the drain stop). shawl's Ctrl-C
reaches every process attached to its console, which is why actions are isolated in the runner service and the
worker service only hosts `bb_worker`.
