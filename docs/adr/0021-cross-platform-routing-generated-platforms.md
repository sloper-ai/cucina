<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0021 — D11: Cross-platform routing is generated Bazel platforms plus exact runner properties

* Status: accepted (2026-10-02)

## Context
Bazel 9's default test toolchain needs the test's exec platform to satisfy every constraint of the target platform,
and Buildbarn needs an action's properties to match a runner's exactly. Hand-maintained platforms drift.

## Decision
Cucina generates `@cucina_platforms` from one matrix: compile exec platforms (one per pool runner, carrying hermetic-llvm's
exec constraints) and one test exec platform per supported target, whose `parents` are hermetic-llvm's target
platforms. `cucinactl bazelrc --cross` emits the matching flags. A structural test checks that platforms, predeclared queues
and runner properties agree for every row of the matrix.

## Consequences
* Tests land on the right runner without per-target configuration, and compile placement is changed through
  exec-platform order alone.
* macOS targets compile on macOS pools; the Apple SDK is resolved on the executing machine, never fetched by clients.
