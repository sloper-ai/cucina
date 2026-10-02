<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0901 — The Apple SDK is resolved on the macOS exec machine (R-XPLAT-8)

* Status: accepted (2026-10-02); supersedes ADR 0102's interim state

## Context
hermetic-llvm 0.8.24 takes the macOS sysroot from `@macos_sdk//sysroot` (`cc_sysroot`: `-isysroot`,
`--sysroot=`, include-checker allowlist, module map) and rules_rs takes SDKROOT from it. The repository
downloads Apple's SDK on the client; `osx.from_host()` also resolves on the client. R-XPLAT-8 forbids
both: the SDK must come from the worker image, never enter the CAS, and keys must match across clients.

## Decision
* Root module: `override_repo(osx, macos_sdk = "cucina_macos_exec_sdk")`, the repo made by
  `@cucina_platforms//apple:extensions.bzl` — no patch to hermetic-llvm or rules_rs.
* Its `//sysroot` (`exec_macos_sdk`) returns `BuildSettingInfo(value = <absolute SDK path>)` (rules_cc
  formats it as a raw string; a DirectoryInfo path would become `%{path:…}`, which must be relative),
  `DirectoryInfo(path = <absolute path>, no files)` (absolute builtin include dir, so the include
  checker accepts the SDK headers in `.d` files) and one unresolved symlink artifact
  `MacOSX<v>.sdk -> <path>` as DefaultInfo (rules_rs SDKROOT; a symlink node in the input root —
  Buildbarn allows absolute symlinks).
* Path: `/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX<v>.sdk`
  (macimage's fixed paths). `@cucina_platforms//apple:sdk_version` (universal scope) is set to the pool's
  Xcode version by every remote config (targets.json exec `flags`), so the version is in every
  compile/link command line; the default `""` uses Xcode's unversioned `MacOSX.sdk` link so local Mac
  and CI builds work; `:sdk_path` overrides the path (CLT-only Macs).
* The code lives in `@cucina_platforms//apple` rather than `bazel/toolchains/apple`: other workspaces
  (Abseil in the campaign) need it from the module, and only the root module can `override_repo`.
* macOS-target compiles run on the Xcode runner (`xcode-version` in the action's platform), never on the
  generic runner, whose properties are shared by all Xcode pools.

## Consequences
Verified on the dev Mac (exec == dev Mac, Xcode 27.0/27A266a): hermetic-llvm cc_binary with
CoreFoundation, `cucina-hostd` with cgo (Foundation, CoreFoundation, Security) and `cucinactl` (Rust)
build, link and run; `@macos_sdk` is never fetched; a compile action's only SDK input is the symlink.
Failure modes: a machine without the SDK fails with clang's `no such sysroot directory: '…/MacOSX27.0.sdk'`
and missing headers; a macOS target with no macOS exec platform listed fails analysis with "No matching
toolchains found" (ADR 0902). `layering_check` cannot cover SDK headers (not in the module map).
