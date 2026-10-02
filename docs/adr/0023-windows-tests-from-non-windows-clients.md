<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0023 — D13: Windows tests from Linux and macOS clients via a `@bazel_tools` overlay, else a patched Bazel

* Status: accepted (2026-10-02)

## Context
A non-Windows Bazel build embeds a dummy script where the Windows test wrapper and XML writer (`tw.exe`, `xml.exe`)
belong, so a test whose exec platform is Windows cannot run (bazelbuild/bazel#19209). Windows clients are unaffected.

## Decision
First try overlaying `@bazel_tools` for the pinned Bazel version with the two binaries taken from the matching official
Windows release. If Bazel 9 does not allow that under Bzlmod, ship a pinned, minimally patched Bazel for Linux and macOS
clients through Bazelisk. Either way an upstream-ready patch and PR description live in `docs/upstream/`. The
Windows-client pattern (compile on Linux workers, test on Windows) is supported unmodified.

## Consequences
* The outcome and its evidence are recorded by the cross-platform ADRs (09xx) and `docs/upstream/`.
* Bazel upgrades re-check the overlay; the Bazel version is pinned in `.bazelversion`.
