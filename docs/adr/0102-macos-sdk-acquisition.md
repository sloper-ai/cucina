<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0102 — How hermetic-llvm acquires the macOS SDK today (input to R-XPLAT-8)

* Status: accepted as the interim state (2026-10-02); superseded by the cross-platform agent's
  exec-side SDK resolution (R-XPLAT-8)

## Context
R-XPLAT-8 requires that no client — Mac or otherwise — downloads Apple's SDK: macOS-target
toolchains must resolve the SDK on the macOS exec machine. hermetic-llvm 0.8.24 and rules_rs
0.0.112 do the opposite by default.

## Decision (current behaviour, unchanged for now)
* `@llvm//extensions:osx.bzl` creates `@macos_sdk` from `osx.from_archive(...)` declared in
  hermetic-llvm's own MODULE.bazel: Apple's `CLTools_macOSNMOS_SDK.pkg`
  (`MacOSX26.5.sdk`, sha256 `debe353b…eb2`) downloaded from `swcdn.apple.com` (web.archive.org
  mirror) and unpacked with a cross-platform `pkgutil` reimplementation — **on the client**,
  whenever a toolchain for a macOS target is resolved (including plain host builds on the dev Mac).
* rules_rs uses the same `@macos_sdk` as a hermetic sysroot for Rust
  (`@rules_rust//rust/settings:use_hermetic_macos_sdkroot`, default true).
* Escape hatches that exist today: `osx.from_host()` in the root module or
  `BAZEL_MACOS_USE_HOST_SDK=1` (only when Bazel runs on macOS; uses the local Xcode/CLT SDK),
  and `--@rules_rust//rust/settings:use_hermetic_macos_sdkroot=false` for Rust.
* Cucina sets `--xcode_version_config=//bazel/toolchains:xcode_disabled` so Bazel never runs
  xcode-locator.

## What must change (owner: cross-platform agent)
1. Replace the downloaded `@macos_sdk` for macOS targets with a toolchain configuration whose
   compile/link actions reference the SDK on the macOS exec machine (`-isysroot` into the pinned
   VM image's Xcode/CLT, selected by `xcode-version`), so `@macos_sdk` is never fetched on any
   client and the SDK never enters the CAS.
2. Make rules_rs follow it (`use_hermetic_macos_sdkroot=false` plus a sysroot pointing at the
   exec-side SDK) and verify cgo (`cucina-hostd`) and Rust linking.
3. Keep Mac and Linux clients' action keys identical (`--experimental_platform_in_output_dir`
   already removes host-CPU path differences).

## Consequences
Until then, a macOS-target build on the dev Mac downloads Apple's SDK pkg once into the
repository cache (Apple SDK licence: used on Apple hardware only). Linux/Windows clients don't
fetch it unless they resolve a macOS target toolchain.
