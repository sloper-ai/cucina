<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0103 — Windows `cucinactl` targets x86_64-pc-windows-gnullvm (MSVC fallback)

* Status: accepted (2026-10-02) — release fallback allowed by R-BUILD-3. The temporary Windows
  CI exclusion is superseded by [ADR 0107](0107-windows-native-toolchain-compatibility.md).

## Context
R-BUILD-3 asks for `cucinactl` for Windows x86_64 MSVC, built on Linux/macOS exec through hermetic
LLVM, falling back to `x86_64-pc-windows-gnullvm` with an ADR if the MSVC path fails. On
2026-10-02 (rules_rs 0.0.112, llvm 0.8.24, rustc 1.98.1), from this Mac:

* `bazel build //tools/hello/rust:hello_rs --config=msvc --platforms=@rules_rs//rs/platforms:x86_64-pc-windows-msvc`
  compiles every crate (incl. aws-lc/zstd for MSVC) but **fails at the final link**: rustc's MSVC
  linker flavour emits link.exe arguments (`/NOLOGO`, `/defaultlib:msvcrt`, `/LIBPATH:…`), while
  rules_rust uses the cc toolchain's link tool, which hermetic-llvm sets to `clang-cl` for every
  MSVC action, so clang-cl treats them as input files.
* rules_rs only sets a Rust-side linker (`rust_toolchain.linker`, lld) for wasm and bare-metal
  RISC-V; `--@rules_rust//rust/settings:toolchain_linker_preference=rust` therefore fails analysis
  ("a `rust_toolchain.linker` must be provided"). Upstream CI cross-builds only the gnullvm
  Windows triples (and runs its Windows job on Linux RBE).
* The same gap applies to native Windows hosts: hermetic-llvm links MSVC through clang-cl on every
  exec OS, so MSVC-flavoured Rust exec tools (proc macros, build scripts) cannot link either.

## Decision
* Windows Rust binaries target **x86_64-pc-windows-gnullvm** (MinGW/UCRT ABI, LLVM toolchain):
  `bazel build //cli/cucinactl --platforms=@rules_rs//rs/platforms:x86_64-pc-windows-gnullvm`.
  Verified on `//tools/hello/rust:hello_rs` (aws-lc via rustls, zstd, buffa/connect): a PE32+
  console binary importing only system DLLs (kernel32, msvcrt, ntdll, bcryptprimitives,
  api-ms-win-core-synch) — no libunwind/libc++ DLLs, so it runs on stock Windows.
* C/C++ for Windows keeps both ABIs (hermetic-llvm MSVC with `--config=msvc`, and MinGW).
* CI: the Linux job cross-builds the Windows Rust targets; the windows-latest job tests the
  non-Rust targets until Windows exec links Rust (owner: cross-platform agent, R-XPLAT-4).

## Consequences
`cucinactl.exe` uses the UCRT/MinGW ABI instead of MSVC; for a self-contained CLI that talks
HTTP/2 + TLS this is invisible to users. Fix path for MSVC: patch rules_rs' generated
`rust_toolchain` so MSVC targets get `linker = lld-link` (LLVM's) with `linker_preference = "rust"`
and the Windows SDK/CRT library paths (or an upstream fix in hermeticbuild/rules_rs), then flip
the release target back to `x86_64-pc-windows-msvc` and re-run the cross build above.
