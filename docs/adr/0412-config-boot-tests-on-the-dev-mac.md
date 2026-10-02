<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0412 — Booting every rendered profile with darwin binaries

* Status: accepted (2026-10-02)

## Context
R-CP-2/R-TEST-6 ask to boot every pinned binary with every rendered profile, without Docker. The dev Mac and the
macOS CI lane run the darwin_arm64 release binaries, which cannot mount FUSE or WinFSP, and parse Windows paths
(`D:\l1\blocks`) as relative POSIX names.

## Decision
`internal/bbconfig/boottest` (integration tier) boots, on darwin:
* every worker profile with its bb_runner. macOS profiles (native, NFSv4) must register every runner thread
  — exact platform, instance name, size class and `{pool, node, thread}` — at an in-process mTLS scheduler.
  Linux (FUSE) and Windows (WinFSP) profiles must parse strictly (no "Failed to read configuration") and stop only
  for a platform reason: "FUSE/WinFSP is not supported on this platform" after initialising scheduler client,
  file pool, L1 and FSAC, or (Windows) bb_runner's "Path is relative" for `B:\`;
* a real action round trip (pinned bb_storage + bb_scheduler + rendered bb_runner/bb_worker): native in-AZ, and
  NFSv4 with zstd on the storage hop — including AC write, output upload, worker ID and metrics checks;
* the host L2 between a VM certificate and an upstream bb_storage (write-through, cached read with the WAN down,
  persistence across a restart, foreign VMs refused).

Finding: the NFSv4 build directory mounts as a non-root user on macOS 27 and passes the round trip; tests skip
(with the reason) where a host forbids the mount, and force-unmount after a killed worker.

## Consequences
* FUSE/WinFSP runtime behaviour is first exercised by the campaign (Linux/Windows workers); Linux/Windows CI
  lanes skip the darwin-calibrated matrix.
* The tests take ~6 s together; Bazel provides the binaries via `//tools:bb_*` and `BB_*` env vars.
* NFSv4 cases mount on the developer's machine. A bb_worker killed before it unmounts leaves a stale mount on
  which any `stat` hangs (seen once under load average 115): the harness gives bb_worker 30 s to stop, then finds
  leftover mounts in the mount table (never by `stat`) and force-unmounts them. Only a hard-killed `go test` can
  still leave one behind (`mount | grep nfsv4.sock`, then `umount -f <path>`).
