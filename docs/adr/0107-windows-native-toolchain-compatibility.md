<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0107 — Enable native Windows Rust builds with narrow toolchain patches

* Status: accepted (2026-10-02); supersedes ADR 0103's temporary Windows CI exclusion, not its release ABI choice

## Context

R-BUILD-6 requires Windows to execute the CLI tests, not merely receive a cross-built binary.
The pinned llvm 0.8.24 and rules_rs 0.0.112 exposed two blockers:

* GitHub run 37000869610 failed analysis because LLVM's Windows SDK case-overlay generator
  needed its own overlay through its C toolchain. Its compiler-rt bootstrap dependency did too.
* The Rust MSVC backend sends link.exe arguments, but the C++ toolchain selects clang-cl.
  The existing Rust smoke binary failed to link with `/NOLOGO` treated as an input filename.

## Decision

Keep the pinned versions and hermetic SDK/CRT downloads; apply two checked-in patches through
Bzlmod, rather than use the runner's Visual Studio toolchain or exclude Rust tests:

* `third_party/llvm/windows-host-tools.patch` omits generated SDK overlays only while building
  stage-0 compiler-rt and stage-1 hosted C construction tools. Complete toolchains retain both
  overlays. Include the pinned CRT's `legacy_stdio_definitions.lib`, which Rust's stdlib requests.
* `third_party/rust/msvc-lld-link.patch` selects the sibling lld-link already declared in the
  clang-cl toolchain's inputs. It unwraps forwarded linker options, retaining hermetic SDK/CRT
  search paths and the case overlay, and removes compiler-only options. Other ABIs are unchanged.
* Windows CI builds and tests `//...`, including the CLI. Release builds retain the gnullvm ABI
  from ADR 0103 until separately validated for a release ABI change.

## Consequences

The existing smoke binary provides the red/green cross-link check:

```sh
bazelisk build --config=msvc --platforms=@rules_rs//rs/platforms:x86_64-pc-windows-msvc //tools/hello/rust:hello_rs
```

It failed before the patches and passed afterwards. Native-Windows C++ and CLI analysis also
passed from the Mac using an ephemeral, checksum-verified Windows embedded-tools fixture;
that is not Windows execution. The hosted Windows CI run remains the execution proof.
Review/remove both patches when updating upstream toolchains. No test is removed or disabled,
and no unpinned compiler, CRT or Windows SDK is introduced.
