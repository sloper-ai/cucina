<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 1006 — Cross-campaign setup and incomplete evidence

Status: Accepted

## Context

The cross-platform catalog and CLI now implement `platforms/targets.json` schema 1. The initial campaign skeleton registered every LLVM toolchain and injected the platforms directory as a repository, which does not implement the module's exec-side Apple SDK override. Client-dependent Abseil rc flags also give different action keys on Windows, Linux and macOS. Missing collectors or partially run configurations must not produce acceptance passes.

## Decision

* Render Abseil's MODULE addition from `tools/xplat/cucina_platforms.MODULE.bazel`, replacing only the platforms module's local path. Hermetic lanes use the catalog's LLVM version, supported toolchain pairs and exec-side SDK override. Native MSVC/Xcode lanes get the platforms dependency without registering LLVM toolchains. This replaces the initial `--inject_repository` overlay mechanism; Abseil's tracked C++/BUILD sources remain untouched.
* Select native or cross C++ flags explicitly, not from the client OS. MSVC targets use clang-cl options; cross-configured host tools use GNU-style options. Windows cross clients get the documented action environment exception of ADR 0904, not the native MSVC lane's environment. Pin Bazel 9.2.0 in every prepared workspace.
* Run the real `xplatcheck -exec-pools` offline before cross scenarios. T16 consumes catalog coverage: full build/test, full build plus timeout-scaled qemu smoke tests, or a freestanding C library for wasm/BPF with the test step explicitly **not applicable**. The freestanding package is a harness-authored equivalent of hermetic-llvm's cross-compilation library example, not an Abseil modification.
* T17 selects every non-default compile pool; tests retain the target's runner. macOS tests expect the **Xcode** property set (ADR 0903). T18 uses the Windows test overlay on the Mac; T16 uses it on Linux. T19 uses unmodified Windows Bazel, private client endpoints, and no `BAZEL_SH` override for Linux tests.
* NFR-X1 compares compact-log **REAPI property sets**, never platform label spellings. Cache-hit placement is recorded separately from actual executions; local compile/test spawns are misroutes. Missing logs and zero denominators do not pass.
* A declared NFR without evidence fails. An unavailable required check carries an explicit skip reason and prevents scenario PASS; a recorded failure takes precedence. Aggregate NFRs report **PARTIAL** when catalog scenarios remain unmeasured. A partial cold-start sample set cannot establish a maximum.
* Forced re-execution uses a fresh output base, bypasses remote action/test-result caches and requires nonzero executed remote spawns. Cancellation stops the detached client job with a separately bounded cleanup context.

## Consequences

The matrix and prerequisites can be validated without AWS, but offline checks are not campaign results. Bootstrap and infrastructure remain lead-owned. An optional private `kubernetes.bootstrapCLI` command connects T0's install to that bootstrap; otherwise the preconfigured local CLI profile must match the descriptor exactly. Temporary client keys travel through the private transfer path, are supplied as filenames, and are removed after login; the CA remains for the credential helper and Bazel.

The current user override restricts the campaign to small runners. Generated descriptors record `small-functional` measurement scope; original NFR-P2 measurements are retained only as unqualified diagnostics, never an original benchmark PASS. The baseline launcher requires an explicit `.large` type and checks its vCPU/memory limits before launch; it has no large-shape fallback. The lead records the deployment override separately.

T8 still needs live per-worker idle timestamps correlated with provider-confirmed termination; its bounded post-hoc management history is not sufficient. It records that missing timing evidence explicitly rather than treating drain API return as termination. Total upload counters alone likewise do not prove NFR-X5's per-toolchain-version upload guarantee; the report must retain that limitation. These limitations are not acceptance passes.
