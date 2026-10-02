<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0304 — Pinning Visual Studio 2026 Build Tools

* Status: accepted (2026-10-02)

## Context
The task names the bootstrapper `https://aka.ms/vs/18/release/vs_BuildTools.exe`; that short link does not resolve
(it redirects to a Bing search). The VS 2026 channels are published as `https://aka.ms/vs/18/stable/...`, which
always points at the newest release, so installing from it is not reproducible (R-POOL-8, §0.5).

## Decision
`workers/windows/scripts/install-vs.ps1` (shared by the worker image and the windows-client) installs from the
**fixed-version bootstrapper** of the pinned release (VS 18.10.3, build 18.10.12224.181; URL + SHA-256) and passes
a **local, SHA-256-verified copy of that release's channel manifest** (`VisualStudio.18.Release.chman`) as both
`--channelUri` and `--installChannelUri`, so neither the installed product nor later update checks can drift.
Components: `Microsoft.VisualStudio.Workload.VCTools`, `...Component.VC.Tools.x86.x64`,
`...Component.Windows11SDK.28000` (the newest SDK in the 18.10.3 catalog). The script records the resulting
versions (`toolchain.json`; copied into `workers/windows/versions.json` -> `installed`) and refuses an install whose
product version differs from the pin. Adopting a new release = update the pins block and rebuild (R-VER-3).

## Consequences
Installed (2026-10-02): VS 18.10.3, MSVC toolset 14.51.36231 (cl.exe 19.51.36260), Windows SDK 10.0.28000.0,
VC++ redistributable 14.51.36247. Bazel pins: `BAZEL_VC=C:\BuildTools\VC`, `BAZEL_VC_FULL_VERSION=14.51.36231`,
`BAZEL_WINSDK_FULL_VERSION=10.0.28000.0`.
