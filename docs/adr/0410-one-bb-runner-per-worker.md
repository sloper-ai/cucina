<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0410 — One bb_runner per worker; per-runner settings live in bb_worker

* Status: accepted (2026-10-02)

## Context
Linux x86_64 workers advertise a native runner plus three qemu-user runners (rv64g, s390x, arm-a32); macOS VMs
advertise an Xcode runner and a generic arm64 runner (R-XPLAT-3, R-MAC-7). bb_worker's `RunnerConfiguration`
(bb_worker.proto:316) carries its own endpoint, concurrency, platform, size class, worker ID and
`environment_variables`; several runner configurations may point at the same endpoint. bb_runner
(bb_runner.proto:11) has no per-platform settings: it runs whatever argv it is given, under one build directory,
one `runCommandsAs` and one Xcode mapping.

## Decision
* One bb_runner process per worker, one UNIX socket (`Machine.RunDir/runner.sock`). bb_worker gets one
  `RunnerConfiguration` per (runner × instance name prefix), all pointing at that socket.
* Emulated runners need no wrapper: the Linux image registers qemu-user with binfmt_misc, and bb_worker adds
  `QEMU_LD_PREFIX=<cross glibc sysroot>` to every action of that runner (defaults: Ubuntu
  `/usr/{riscv64-linux-gnu,s390x-linux-gnu,arm-linux-gnueabihf}`, overridable per machine). Their concurrency
  comes from the catalog (2); a derived (0) concurrency becomes vCPUs/4.
* Linux: bb_runner runs as root with `runCommandsAs` = the build user and `cleanProcessTable` (runs only when the
  runner is idle, so concurrent actions are safe); FUSE mounts `allowOther` and presents files as owned by the
  build user. macOS: bb_runner runs in the build user's GUI session (R-MAC-4), no process-table cleaning (the
  session runs unrelated processes), `appleXcodeDeveloperDirectories` from the machine. Native build directories
  are created by bb_worker but written by the build user, so bb_worker runs with umask 0 (as bb-deployments'
  Kubernetes worker does); every state directory (L1, file pool, native cache) is 0700, keeping the
  world-writable files inside unreachable for actions.
* Worker IDs are exactly `{pool, node}`; Buildbarn adds `thread` (zero-padded) only when a runner's
  concurrency is > 1, so the controller must not require it. Two runners with the same property set are rejected
  (their worker IDs would collide in one queue).

## Consequences
* Several instance names multiply runner threads (one queue each); the autoscaler's slots stay per queue.
* A crashed bb_runner stops every runner of the worker (the worker reports itself degraded and stops taking work).
* Windows uses an AF_UNIX socket at an absolute path (`unix:C:\…`), unverified until the Windows campaign.
