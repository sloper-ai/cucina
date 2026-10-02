<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0902 — Register only the supported hermetic-llvm toolchain pairs (macOS targets on macOS exec)

* Status: accepted (2026-10-02)

## Context
`register_toolchains("@llvm//toolchain:all")` offers every exec × target pair, including Linux and
Windows exec → macOS targets and the excluded macOS x86_64 / Windows arm64. With the exec-side SDK
(ADR 0901) a macOS-target compile resolved to a Linux exec platform would reach a worker without the
SDK. Exec-platform order alone (`:cucina-macos`) prevents that only if every configuration orders
platforms correctly.

## Decision
The root module registers, through the generated segment, hermetic-llvm's stage-0 toolchains for
(every compile exec OS/CPU of targets.json) × (every in-scope non-macOS target, incl. the `_msvc`
variants) plus macOS arm64 → macOS arm64 — 45 labels instead of `@llvm//toolchain:all`. Labels are
hermetic-llvm's own toolchain targets (no wrappers), so a renamed toolchain fails loudly on upgrade.

## Consequences
* A macOS target compiles, links and tests on the macOS exec platform even when Linux comes first in
  the list (verified with `aquery`: `:cucina`'s Linux-first list puts all of them on
  `macos-arm64-xcode27.0-xcode`). Without a macOS exec platform, analysis fails fast with Bazel's
  "No matching toolchains found for types @bazel_tools//tools/cpp:toolchain_type".
* Excluded platforms have no toolchain. Bootstrap (stage1+) toolchains are not registered; building
  LLVM from source needs them added back.
* Workspaces that register `@llvm//toolchain:all` themselves lose this guarantee (documented).
