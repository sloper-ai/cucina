<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0109 — Build the Windows awk tool with its supported GNU ABI

* Status: accepted (2026-10-02); native Windows execution validation pending

## Context

R-BUILD-6 must build the OCI graph on Windows without excluding it. `tar.bzl` already depends
on gawk 5.3.2.bcr.3 to transform and validate archive metadata. That pinned BCR overlay's Windows
configuration defines `__MINGW32__`, includes POSIX compatibility headers and uses GNU linker
flags. In hosted run 37035538688 it inherited Cucina's MSVC host target and failed to compile.

An MSVC cross-build reproduced additional errors (`__STDC__`, `strings.h`). Building the same
source with hermetic MinGW exposed a second issue: gawk's pipe shim undefines MinGW's
`popen`/`pclose` aliases before calling them, leaving undeclared functions.

## Decision

Keep the existing gawk version and source, with a narrow Bzlmod patch:

* Preserve its public `@gawk//:gawk` executable label. Only Windows resolves it through
  `//bazel/toolchains:gawk_windows`, a `platform_transition_binary` selecting hermetic LLVM's
  x86_64 Windows GNU platform. Its underlying binary is `@gawk//:gawk_bin`.
* Call the CRT's `_popen` and `_pclose` directly in the MinGW branches of gawk's pipe shim.
  They are the symbols the original MinGW aliases designate; no declaration or warning is suppressed.
* Other platforms build the original native binary. Cucina's Windows Rust/C++/Go targets and
  execution platform remain MSVC. The exception is a standalone build-time executable, not
  an ABI mixed into project libraries.

## Consequences and evidence

The old MSVC build failed; after the patch this succeeds without changing its requested platform:

```sh
bazelisk build --config=msvc --platforms=@rules_rs//rs/platforms:x86_64-pc-windows-msvc @gawk//:gawk
```

The result is a Windows executable importing only Windows system/UCRT DLLs, with no additional
compiler-runtime DLL to install. The Windows OCI graph passes analysis, and both native smoke
and controller image builds still pass. The hosted Windows job remains the actual execution
proof. There is no new gawk version, system installation, test exclusion or less strict compiler
setting. Revisit the adapter when the BCR overlay supports MSVC directly.
