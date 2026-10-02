<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0903 — Where test actions land; macOS-target tests use the Xcode runner

* Status: accepted (2026-10-02) — deviation from the R-XPLAT-1 table ("macOS VMs, generic arm64 runner")

## Context
Bazel 9's default test toolchain sends a test to the **first** execution platform that satisfies all
of the target platform's constraints, and constraint *defaults* count (hermetic-llvm's
`cxxstdlib:variant` defaults to `libcxx`). Measured offline with `aquery` for all 17 targets
(`//tools/xplat/cmd/xplatcheck`): a compile exec platform that satisfies the target wins over the
`test_on_<P>` twin listed after it (x86_64 glibc and aarch64 glibc tests resolve to
`linux-x86-64-native` / `linux-aarch64-native`; musl, qemu, Windows and the twins of other ISAs to
`test_on_<P>`). For Linux and Windows this is harmless: both platforms advertise the same runner.
For macOS the compile platform is the Xcode runner (ADR 0901) and always comes first, so macOS tests
run there, not on the generic runner. Making the Xcode platform *not* satisfy `macos_aarch64` would
need a fake value of hermetic-llvm's C++-library constraint on the exec platform, which would change
how its exec tools are built.

## Decision
* targets.json records the runner the test really uses: macOS targets test on the
  `macos-arm64-xcode27.0` **Xcode** runner (same VMs, `xcode-version` in the test's platform);
  `test_on_macos_aarch64` carries that runner's properties too.
* NFR-X1 is checked as "the test ran on the target's runner property set", not "on the platform
  named test_on_<P>"; the resolution check and the structural test use that definition.
* Compile exec platforms always precede the twins in every list (twins first would also move
  toolchain-less actions such as hermetic-llvm's `llvm-ar` run_binary steps onto the twin's runner —
  observed: `llvm-ar.exe` scheduled on the Windows twin).

## Consequences
The macOS generic runner is unused by the default matrix; it stays in pools.json for work that must
not depend on the Xcode version. A macOS test is pinned to the Xcode pool that compiled it.
