<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0305 — qemu-user and cross glibc runtimes on the x86_64 Linux image

* Status: accepted (2026-10-02)

## Context
R-POOL-4 / R-XPLAT-3: the x86_64 image runs riscv64, s390x and armv7 test binaries under qemu-user via binfmt_misc,
including dynamically linked glibc binaries. `QEMU_LD_PREFIX` is a single environment variable for all emulated
architectures and is not set by Bazel/bb_runner for actions. Ubuntu 26.04 turned `qemu-user-static` into a virtual
package provided by `qemu-user-binfmt`.

## Decision
Install `qemu-user-binfmt` (static emulators + systemd-binfmt registrations with flags `POF`, i.e. fix-binary, so they
also work in other mount namespaces/chroots) and the `libc6-*-cross` / `libstdc++6-*-cross` runtimes for riscv64,
s390x and armhf. Expose each runtime at the multiarch path its own `ld.so` searches (`/usr/lib/<triplet>` ->
`/usr/<triplet>/lib`) and each ELF interpreter at `/lib/<ld.so>` (merged /usr). No `QEMU_LD_PREFIX` is needed.
A smoke test (`/opt/cucina/bin/cucina-qemu-smoke-test`) runs static (musl) and dynamic (glibc 2.28) hello binaries
built with a hermetic cross toolchain (zig cc, pinned) during every image build, after a reboot.

## Consequences
Foreign dynamic binaries resolve their libc without per-action environment. The symlinked paths are unused by the
amd64 system. musl-dynamic foreign binaries would need the musl loaders as well (hermetic-llvm's musl targets link
statically by default). The Amazon Linux 2023 alternative ships no qemu-user packages and therefore has no emulated
runners (it is the boot-time comparison template, not a runner image).
