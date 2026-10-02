<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0303 — Microsoft Defender — path/process exclusions by default, Dev Drive as an option

* Status: accepted (2026-10-02)

## Context
R-POOL-5: either Defender exclusions for build, cache and toolchain paths and processes, or a Dev Drive (ReFS) with
AV filtering relaxed on the ephemeral data volume / instance store; measure, pick one, document the trade-off.
Build actions on Windows run inside bb_worker's WinFSP virtual file system (`B:`), not on a local volume, so a Dev
Drive can only hold L1/filePool/TMP, while toolchain reads (`C:\BuildTools`, Windows Kits) stay on `C:` either way.

## Decision
Ship both, selected by the Packer variable `defender_mode`:
* `exclusions` (default): path exclusions `C:\bb`, `B:\`, `D:\`, `C:\b`, `C:\BuildTools`, Windows Kits, VS
  installer, `C:\ProgramData\cucina`, `C:\tools`, Git; process exclusions for bb_worker/bb_runner/shawl/agent and
  the MSVC/clang/Bazel tool processes; scheduled and catch-up scans off; real-time protection stays on.
* `devdrive`: additionally `cucina-format-data-volume.ps1` formats a raw instance-store disk (or, called with
  `-IncludeEbs`, an EBS data volume) as a Dev Drive (trusted, Defender in performance mode), falling back to plain
  ReFS where Dev Drive creation is unavailable.

## Consequences
Exclusions remove AV cost from every hot path, including the WinFSP mount, at the price of no on-access scanning of
build inputs/outputs on workers (acceptable: inputs come from the CAS of the same trust domain; workers are
ephemeral and pools are the trust boundary). Dev Drive keeps asynchronous scanning on cache/filePool but needs a
data volume, so it only matters for pools that have one; switch the default only if measurements favour it.
