<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0900 — `@cucina_platforms`: a generated module, `targets.json` schema v1, labels and configs

* Status: accepted (2026-10-02)

## Context
R-XPLAT-2 / D11 / R-BUILD-4 ask for Bazel routing generated from the platform matrix, consumed by
`cucinactl bazelrc --cross`, the e2e matrix runner and the structural test, and usable by other
workspaces (Abseil in the campaign). The CLI agent had already coded against a fixture schema
(ADR 0802). The bazel agent `.bazelignore`s `bazel/platforms` as a separate module.

## Decision
* `platforms/targets.json` (schema in docs/cross-compilation.md §Schema) is the CLI fixture's schema
  plus additive fields (`libc`, `testMode`, `testSkip`, `coverage`, `testTimeoutScale`, `notes`,
  `excluded`/`reason`, `excludedPlatforms`, exec `constraints`/`flags`). Runner property sets stay only
  in `platforms/pools.json`.
* `bazel/platforms` is the Bzlmod module `cucina_platforms` (deps: llvm, platforms, bazel_skylib):
  `//exec:<pool>-<runner>` compile exec platforms (one per native/Xcode runner, hermetic-llvm exec
  constraints: Linux `libc:gnu.2.28`, Windows `windows/abi:gnullvm` so exec tools build with MinGW and
  Rust gnullvm without the MSVC EULA), `//test:test_on_<P>` twins (`parents` = the hermetic-llvm
  platform), `//apple` (ADR 0901), `defs.bzl` (`cucina_test_exec_platform`), and the generated
  `catalog.bzl`/`cucina.bazelrc`. Labels inside the module are package-relative so it loads in any
  context.
* `//tools/xplat` (Go) validates both catalogs and generates those files plus the root-module segment
  `tools/xplat/cucina_platforms.MODULE.bazel` (bazel_dep + override, the Apple SDK override and the
  toolchain pairs of ADR 0902), included by the root `MODULE.bazel`. `bazel run //tools/xplat:update`
  (also picked up by `//:update_goldens`) writes them; `diff_test`s against `@cucina_platforms` files
  (tier-static) catch drift, because `write_source_files`' own diff tests cannot see an ignored package.
* `cucina.bazelrc` (imported by the root `.bazelrc`) uses `build:` lines: the root `.bazelrc` has
  `build:cucina-macos --config=cucina`, and Bazel expands a config's `common:` lines before its
  `build:` lines, so `common:` here would let `:cucina`'s Linux-first list win inside
  `:cucina-macos` (observed with `--announce_rc`). Each config sets the complete
  `--extra_execution_platforms` list (compile platforms first, then every twin); the last value wins.

## Consequences
The CLI fixture can be replaced by `platforms/targets.json` unchanged. Excluded rows are listed with
`excluded: true`; consumers must reject them with `reason`. The structural test reads pools.json for
queue keys (the chart's copy is private to `//charts/cucina`; the chart's own test keeps it equal).
