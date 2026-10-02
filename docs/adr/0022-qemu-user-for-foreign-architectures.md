<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0022 — D12: riscv64, s390x and armv7 tests run under qemu-user

* Status: accepted (2026-10-02)

## Context
AWS offers no riscv64, s390x or AArch32 hardware, and a standing emulator fleet would break the zero-idle-cost goal.

## Decision
Tests for riscv64, s390x and armv7 run under `qemu-user` (with `binfmt_misc` and cross glibc runtimes) on the Linux x86_64
pool, as separate runners with lower concurrency. macOS x86_64 and Windows arm64 are out of scope, as targets and as exec platforms.

## Consequences
* Emulated runners are slower; timeouts are scaled and the expected slowdown is documented.
* The runner set is data (`platforms/pools.json`): adding a target is a catalog change, not code.
