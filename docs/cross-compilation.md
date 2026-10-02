<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Cross-compilation and cross-platform testing

From a Linux, macOS or Windows client, Cucina builds every in-scope target of
[hermetic-llvm](https://github.com/hermeticbuild/hermetic-llvm) (Bazel module `llvm` 0.8.24, LLVM
23.1.2) and runs its tests on a worker of the target's OS and architecture, natively or under
qemu-user (R-XPLAT). Routing is plain Bazel: the generated module `@cucina_platforms`
(`bazel/platforms`) declares one execution platform per Cucina runner, and Bazel's toolchain
resolution sends each action to the right one. Buildbarn then matches the action's platform
properties exactly against its predeclared queues.

* [The matrix](#the-matrix) · [Quick start](#quick-start) · [Schema](#schema) ·
  [How routing works](#how-routing-works) · [Choosing the compile pool](#choosing-the-compile-pool-uc25) ·
  [Apple SDK on the exec machine](#apple-sdk-on-the-exec-machine-r-xplat-8) ·
  [Windows tests from Linux and macOS](#windows-tests-from-linux-and-macos-clients-r-xplat-4) ·
  [qemu-user runners](#qemu-user-runners) · [Build-only targets](#build-only-targets-wasm-bpf) ·
  [Fast failure](#fast-failure-when-no-runner-exists-uc27) ·
  [Hermeticity and caching](#hermeticity-and-cache-hygiene) ·
  [Maintaining the module](#maintaining-the-module) · [Known limitations](#known-limitations)

## The matrix

Generated from `platforms/targets.json`; "compile" is the default compile pool (UC25 lets you pick
another), "test" the runner that executes the tests.

| Target (`--target`) | `--platforms` | Compile | Test runner | Mode | Campaign coverage |
| --- | --- | --- | --- | --- | --- |
| `aarch64-apple-darwin` | `@llvm//platforms:macos_aarch64` | `macos-arm64-xcode27.0` (only) | `macos-arm64-xcode27.0`/`xcode` | native | full |
| `x86_64-linux-gnu` | `@llvm//platforms:linux_x86_64_gnu.2.28` | `linux-x86-64` | `linux-x86-64`/`native` | native | full |
| `x86_64-linux-musl` | `@llvm//platforms:linux_x86_64_musl` | `linux-x86-64` | `linux-x86-64`/`native` | native | full |
| `aarch64-linux-gnu` | `@llvm//platforms:linux_aarch64_gnu.2.28` | `linux-x86-64` | `linux-aarch64`/`native` | native | full |
| `aarch64-linux-musl` | `@llvm//platforms:linux_aarch64_musl` | `linux-x86-64` | `linux-aarch64`/`native` | native | full |
| `riscv64-linux-gnu` | `@llvm//platforms:linux_riscv64_gnu.2.33` | `linux-x86-64` | `linux-x86-64`/`qemu-rv64g` | qemu-user | build; smoke tests |
| `riscv64-linux-musl` | `@llvm//platforms:linux_riscv64_musl` | `linux-x86-64` | `linux-x86-64`/`qemu-rv64g` | qemu-user | build; smoke tests |
| `s390x-linux-gnu` | `@llvm//platforms:linux_s390x_gnu.2.28` | `linux-x86-64` | `linux-x86-64`/`qemu-s390x` | qemu-user | build; smoke tests |
| `s390x-linux-musl` | `@llvm//platforms:linux_s390x_musl` | `linux-x86-64` | `linux-x86-64`/`qemu-s390x` | qemu-user | build; smoke tests |
| `armv7-linux-gnueabihf` | `@llvm//platforms:linux_armv7_gnu.2.28` | `linux-x86-64` | `linux-x86-64`/`qemu-arm-a32` | qemu-user | build; smoke tests |
| `armv7-linux-musleabihf` | `@llvm//platforms:linux_armv7_musl` | `linux-x86-64` | `linux-x86-64`/`qemu-arm-a32` | qemu-user | build; smoke tests |
| `x86_64-windows-msvc` | `@llvm//platforms:windows_x86_64_msvc` | `linux-x86-64` | `windows-x86-64`/`native` | native | full |
| `x86_64-windows-gnu` | `@llvm//platforms:windows_x86_64` | `linux-x86-64` | `windows-x86-64`/`native` | native | full |
| `wasm32-unknown-unknown`, `wasm64-unknown-unknown` | `@llvm//platforms:none_wasm32`, `none_wasm64` | `linux-x86-64` | — | none | build (example) |
| `bpfeb`, `bpfel` | `@llvm//platforms:none_bpfeb`, `none_bpfel` | `linux-x86-64` | — | none | build (example) |

Excluded by the user and rejected everywhere: macOS x86_64 (`macos_x86_64`) and Windows arm64
(`windows_aarch64`, `windows_aarch64_msvc`), as targets and as exec platforms. riscv64 glibc uses
glibc 2.33, hermetic-llvm's default for that architecture (compiler-rt needs Linux headers ≥ 5.10).

## Quick start

### 1. Workspace setup (once)

`MODULE.bazel` of a workspace that builds through Cucina (the Cucina repository itself gets the same
lines from `tools/xplat/cucina_platforms.MODULE.bazel` through `include()`):

```starlark
bazel_dep(name = "llvm", version = "0.8.24")
bazel_dep(name = "platforms", version = "1.1.0")
bazel_dep(name = "cucina_platforms", version = "0.1.0")
archive_override(                          # or git_override / local_path_override
    module_name = "cucina_platforms",
    urls = ["https://github.com/sloper-ai/cucina/archive/<commit>.tar.gz"],
    strip_prefix = "cucina-<commit>/bazel/platforms",
    integrity = "sha256-…",
)

# R-XPLAT-8: the Apple SDK comes from the macOS worker, never from the client.
osx = use_extension("@llvm//extensions:osx.bzl", "osx")
cucina_apple = use_extension("@cucina_platforms//apple:extensions.bzl", "apple")
use_repo(cucina_apple, "cucina_macos_exec_sdk")
override_repo(osx, macos_sdk = "cucina_macos_exec_sdk")

# hermetic-llvm toolchains for the supported (exec, target) pairs: copy the register_toolchains()
# block of tools/xplat/cucina_platforms.MODULE.bazel. Do not register @llvm//toolchain:all.
```

`.bazelrc` essentials (Cucina's own `.bazelrc` has them): `--experimental_platform_in_output_dir`,
`--noexperimental_use_platforms_in_output_dir_legacy_heuristic`,
`--repo_env=BAZEL_DO_NOT_DETECT_CPP_TOOLCHAIN=1`, `--repo_env=BAZEL_NO_APPLE_CPP_TOOLCHAIN=1` and, with
rules_rs, `--@rules_cc//cc/toolchains/args/archiver_flags:use_libtool_on_macos=False`.

### 2. One configuration per target

`cucinactl bazelrc --cross --target <name> [--exec-pool <pool>]` prints the configuration from
`platforms/targets.json` (endpoint, credentials and transfer flags omitted here; the chosen exec
platform's `flags`, e.g. the macOS SDK version, are added too). The `--override_repository` line is
machine-local: `tools/xplat/windows-test-overlay.sh` prints it. For `x86_64-windows-msvc` from a Linux
client:

```
common --extra_execution_platforms=@cucina_platforms//exec:linux-x86-64-native,@cucina_platforms//exec:linux-aarch64-native,@cucina_platforms//exec:windows-x86-64-native,@cucina_platforms//exec:macos-arm64-xcode27.0-xcode,@cucina_platforms//test:test_on_windows_x86_64_msvc
common --host_platform=@cucina_platforms//exec:linux-x86-64-native
common --platforms=@llvm//platforms:windows_x86_64_msvc
common --experimental_platform_in_output_dir
common --repo_env=BAZEL_MSVC_RUNTIME_VISUAL_STUDIO_EULA=1   # MSVC targets only (accepted 2026-10-01)
common --repo_env=BAZEL_WINDOWS_SDK_EULA=1
common --test_env=SYSTEMROOT=C:\Windows                      # Windows targets (R-XPLAT-4)
common --test_env=PATH=C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0
common --override_repository=bazel_tools=/home/me/.cache/cucina/bazel_tools_overlay/9.2.0   # Linux/macOS clients, Windows tests
```

Then `bazel test //...` (or `//absl/...`). Every target runs one invocation per target platform;
matrix runs may go in parallel (separate output bases). The equivalent for the Cucina repository is
`--config=cucina` (Linux compiles first) and `--config=cucina-macos` (macOS lane: everything on
macOS), generated into `bazel/platforms/cucina.bazelrc`.

| Client | macOS targets | Linux, wasm, BPF targets | Windows targets |
| --- | --- | --- | --- |
| Linux | compile, link, test on macOS VMs; SDK never on the client | as configured | needs the `@bazel_tools` overlay (below) |
| macOS | same (identical action keys) | same | needs the overlay |
| Windows | same | same | nothing extra: Windows Bazel embeds the test wrapper |

## Schema

`platforms/targets.json` describes every hermetic-llvm target Cucina supports, the exec platforms
that compile it and the runner that tests it. It is read by `tools/xplat` (which generates the
`@cucina_platforms` module in `bazel/platforms`), by `cucinactl bazelrc --cross`, by the e2e matrix
runner and by the structural test. Runner property sets are **not** repeated here: they live in
`platforms/pools.json`, and `targets.json` references pools and runners by name.

Compatibility rules: consumers ignore unknown fields; fields are only added, never renamed or
retyped, within `schemaVersion` 1. A breaking change bumps `schemaVersion`.

### Top level

| Field | Type | Meaning |
| --- | --- | --- |
| `schemaVersion` | int | `1`. |
| `hermeticLlvm` | object | `{module, version, llvmVersion}`: the pinned `llvm` Bazel module (0.8.24, LLVM 23.1.2) whose platform labels the targets use. |
| `excludedPlatforms` | list | `{os, cpu, reason}`: OS/CPU pairs that are neither targets nor exec platforms (macOS x86_64, Windows arm64). The generator rejects any pool, exec platform or target with such a pair. |
| `execPlatforms` | list | Compile exec platforms (below). |
| `targets` | list | Target rows (below), including excluded ones. |

### `execPlatforms[]` — where compile and link actions run

One per compile-capable pool runner: native runners of the Linux and Windows pools and the Xcode
runner of each macOS pool. qemu and macOS "generic" runners are test-only and appear only as test
exec platforms.

| Field | Type | Meaning |
| --- | --- | --- |
| `name` | string | Unique id; equals the pool name. `cucinactl bazelrc --cross --exec-pool <name>` selects it. |
| `label` | string | Bazel `platform()` label, `@cucina_platforms//exec:<pool>-<runner>`. |
| `pool` | string | `platforms/pools.json` platform name. |
| `runner` | string | Runner name within that pool; the platform's `exec_properties` are exactly that runner's `properties`. |
| `os`, `cpu` | string | Bazel `@platforms//os:<os>` / `@platforms//cpu:<cpu>` names (`linux`, `macos`, `windows`; `x86_64`, `aarch64`). |
| `constraints` | list of labels | Extra constraint values besides OS/CPU, following hermetic-llvm's `rbe.bzl` exec pattern (`@llvm//constraints/libc:gnu.2.28` on Linux, `@llvm//constraints/windows/abi:gnullvm` on Windows). Exec-configured tools are built for these. |
| `flags` | list of strings | Extra Bazel flags required whenever this platform compiles (the macOS SDK version: `--@cucina_platforms//apple:sdk_version=27.0`). `cucinactl bazelrc` emits them. |

### `targets[]` — one row per hermetic-llvm target

| Field | Type | Meaning |
| --- | --- | --- |
| `name` | string | Canonical id (hermetic-llvm's README row name, e.g. `x86_64-windows-msvc`); `--target` value. |
| `aliases` | list of strings | Other accepted `--target` values (Rust triples, hermetic-llvm platform names). The platform label's target name is always accepted too. |
| `platform` | label | The hermetic-llvm target platform for `--platforms`, verified with `bazel query` against the pinned `llvm` module. |
| `os`, `cpu` | string | Bazel OS/CPU names (`none` for freestanding targets; `wasm32`, `bpfel`, …). |
| `libc` | string or null | `gnu.<version>` (glibc version selected by the platform), `musl`, or null. |
| `abi` | string or null | `gnu`, `musl`, `msvc` or null. `msvc` means the MSVC EULA `--repo_env` flags are required. |
| `execPlatforms` | list of names | Compile exec platforms allowed for this target, **default first** (UC25). macOS targets list only macOS pools. |
| `test` | object or null | The test placement: `{label, pool, runner, mode}`. `label` is the test exec platform `@cucina_platforms//test:test_on_<P>` (P = the platform's target name); `mode` is `native` or `qemu-user`. `null` for build-only and excluded targets. |
| `testMode` | string | `native`, `qemu-user` or `none` (no OS to run on). |
| `testSkip` | string | Expected test-step semantics: `incompatible` (the test step runs; Bazel reports tests whose `target_compatible_with` excludes the target as SKIPPED, nothing else is skipped) or `not-applicable` (no test step; the harness builds only and reports the test step as "not applicable", UC27). |
| `coverage` | string | Campaign coverage (R-XPLAT-1): `full` (Abseil `//absl/...` build + test), `build-full-test-smoke` (full build; smoke test subset MUST, full suite SHOULD, with scaled timeouts), `build-example` (a hermetic-llvm e2e example, build only). |
| `testTimeoutScale` | number, optional | Multiplier for Bazel's test timeouts under emulation (`--test_timeout=60s×k,300s×k,900s×k,3600s×k`). Absent means 1. |
| `notes` | string | Free text for humans. |
| `excluded` | bool, optional | `true` for targets the user excluded; such rows have `reason` and empty `execPlatforms`, and every consumer must reject them with `reason`. |
| `reason` | string | Why the target is excluded (only with `excluded`). |

### Invariants (checked by `//tools/xplat:structural_test`)

* Every `execPlatforms[].pool`/`runner` and every `test.pool`/`test.runner` exists in
  `platforms/pools.json`, and the generated platform's `exec_properties` equal that runner's
  `properties` exactly (Buildbarn matches the entire set).
* Every runnable target has a generated `test_on_<P>` whose parent is its hermetic-llvm platform.
* Excluded targets and `excludedPlatforms` pairs appear nowhere in the generated module.
* `execPlatforms` of macOS targets contain only macOS exec platforms (macOS compiles on macOS).

## How routing works

* **Compile exec platforms** (`@cucina_platforms//exec:<pool>-<runner>`): one per native Linux/Windows
  runner and per macOS Xcode runner. Constraints: OS and CPU plus hermetic-llvm's exec constraints
  (`@llvm//constraints/libc:gnu.2.28` on Linux, as in hermetic-llvm's `rbe.bzl`;
  `@llvm//constraints/windows/abi:gnullvm` on Windows, so exec-configured C++ tools build with MinGW
  and Rust exec tools with the gnullvm toolchain, without the MSVC EULA). `exec_properties` are exactly
  the runner's `properties` from `platforms/pools.json`.
* **Test exec platforms** (`@cucina_platforms//test:test_on_<P>`): `parents = ["@llvm//platforms:<P>"]`
  plus the test runner's properties. Bazel 9's default test toolchain runs a test on the **first**
  execution platform that satisfies every constraint of the target platform (libc and glibc version,
  Windows ABI and CRT, C++ library; constraint defaults count). That is either the twin or, when a
  compile platform listed earlier already satisfies the target, that compile platform; the resolution
  check below verifies that this only happens where both advertise the same runner (x86_64/aarch64
  glibc on their native Linux pools, and macOS, see below). Your own target platforms (e.g. libstdc++) get twins from
  `cucina_test_exec_platform(name, target_platform, pool, runner)` in `@cucina_platforms//:defs.bzl`.
* **Order**: `--extra_execution_platforms` is set once per configuration: the compile platforms in
  preference order, then the twins. Toolchain-less actions (genrules, hermetic-llvm's `llvm-ar`
  run_binary steps) take the first platform, so twins must never come first. Bazel appends the host
  platform last, and an action that resolves to it would reach Buildbarn without platform properties
  and be rejected, so every list is complete (R-BUILD-4); `--host_platform` is the first compile
  platform.
* **macOS targets compile on macOS only** (user decision): the root module registers hermetic-llvm's
  toolchains for every supported exec → non-macOS target and macOS → macOS only (ADR 0902). Even a
  Linux-first list puts every C/C++ action of a macOS target on `macos-arm64-xcode27.0-xcode`;
  `--config=cucina-macos`/`cucinactl` additionally put macOS first so genrules stay there too.
  macOS-target tests therefore run on the same Xcode runner, not on the generic one (ADR 0903).
* **Verification**: `bazel run //tools/xplat/cmd/xplatcheck -- -workdir /tmp/xplatcheck -exec-pools`
  writes a scratch workspace (Abseil-style `cc_library` + `cc_test`, a freestanding C library for
  wasm/BPF), runs `bazel aquery` with each row's `cucinactl bazelrc --cross` flags and checks the
  execution platform of every CppCompile/CppLink/TestRunner action. 2026-10-02 on the dev Mac: all 20
  configurations (17 targets + x86_64-linux-gnu compiled on each other pool) resolve as specified.
  The tier-unit `//tools/xplat:xplat_test` checks the checked-in platforms against pools.json (exact
  property sets = the chart's predeclared queue keys), the twins, the exclusions and the toolchain pairs.

## Choosing the compile pool (UC25)

`cucinactl bazelrc --cross --target x86_64-linux-gnu --exec-pool linux-aarch64` moves that pool's exec
platform to the front; the target and the test placement do not change (tests of x86_64 still run on
`linux-x86-64`). Any pool in the target's `execPlatforms` is allowed (Linux x86_64, Graviton, Windows,
macOS for non-macOS targets; macOS targets only macOS). Exec-configured tools are rebuilt for the
chosen pool, so the first build on a new pool is slower.

**Default order by measured $/build (R-XPLAT-7).** `linux-x86-64` is first until measured. Benchmark
(campaign T16/T17, scripted in the e2e harness): for each candidate compile pool (linux-x86-64 on its
default type, linux-aarch64 on c8g), cold-build `//absl/...` for `x86_64-linux-gnu` with an empty
cache, the pool at max 4 and the test runner pool warm, three times. Cost per build = Σ(instance
seconds × on-demand price) + EBS GB-seconds + data transfer, from the controller's
`cucina_instance_seconds_total` and the price list (`internal/cost`); report median $/build and wall
time. Choose the cheapest pool whose median wall time is within 20 % of the fastest, put it first in
`execPlatforms` of every non-macOS row, regenerate. A second, smaller run with `aarch64-linux-gnu`
checks the choice does not depend on the target architecture.

## Apple SDK on the exec machine (R-XPLAT-8)

hermetic-llvm normally downloads Apple's SDK on the client (`@macos_sdk`); `osx.from_host()` also
resolves it on the client. Cucina overrides `@macos_sdk` with `@cucina_macos_exec_sdk` (ADR 0901):

* compile and link command lines carry `-isysroot`/`--sysroot=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX<version>.sdk`,
  the fixed path of the worker images (docs/operations/macos-images.md);
* the only SDK-related input is one symlink node (`…/sysroot/MacOSX<version>.sdk -> <that path>`), which
  rules_rs uses as SDKROOT; the SDK never enters the CAS and no client downloads it;
* `--@cucina_platforms//apple:sdk_version=27.0` (emitted by `cucinactl` and the `:cucina*` configs from
  the pool's Xcode version) puts the SDK version into every action key; the default (unset) uses Xcode's
  unversioned `MacOSX.sdk`, so local builds work on any Mac with `/Applications/Xcode.app`;
  `--@cucina_platforms//apple:sdk_path=/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk` covers Macs
  with only the Command Line Tools (local builds only);
* Mac and Linux clients send identical actions (`--experimental_platform_in_output_dir` keeps output
  paths host-independent), so they share the cache (NFR-X3).

Verified 2026-10-02 on the dev Mac (exec == dev Mac, Xcode 27.0 27A266a): a hermetic-llvm `cc_binary`
using CoreFoundation and `//tools/hello/cc:greet_test` build and pass; `cucina-hostd` with cgo
(Foundation, CoreFoundation, Security, libresolv) and `cucinactl` (Rust, rules_rs) link and run; in a
fresh output base only `@cucina_macos_exec_sdk` is materialised, never `@macos_sdk`.

Failure modes and messages:

| Situation | What you see |
| --- | --- |
| Worker or Mac without that SDK (wrong image, Xcode elsewhere) | `clang: error: no such sysroot directory: '/Applications/Xcode.app/…/MacOSX27.0.sdk'` followed by missing-header errors (`'stdio.h' file not found`) |
| macOS target with no macOS exec platform listed | analysis fails: `No matching toolchains found for types @bazel_tools//tools/cpp:toolchain_type` |
| `--features=layering_check` on a macOS target | `module … does not depend on a module exporting 'cstdio'`: SDK headers are not in hermetic-llvm's crosstool module map (unsupported) |

## Windows tests from Linux and macOS clients (R-XPLAT-4)

Non-Windows Bazel embeds `dummy.sh` where Windows Bazel embeds the test wrapper `tw.exe` and the XML
writer `xml.exe` ([bazel#19209](https://github.com/bazelbuild/bazel/issues/19209)), so a test on a
Windows exec platform fails with `missing input file '@@bazel_tools//tools/test:tw.exe'`. Bazel 9.2.0
accepts an overlay of `@bazel_tools` under Bzlmod (ADR 0904):

```sh
tools/xplat/windows-test-overlay.sh          # in a workspace; prints the line below
# common --override_repository=bazel_tools=$HOME/.cache/cucina/bazel_tools_overlay/9.2.0
```

The script copies this Bazel's own `embedded_tools` (unchanged) and adds `tw.exe`/`xml.exe` from the
official `bazel_nojdk-9.2.0-windows-x86_64.exe` (SHA-256 pinned per Bazel version). Put the printed
line in `user.bazelrc` (machine-local). Test environment for Windows targets (Bazel's strict action
environment is the *client's* `PATH=/bin:/usr/bin:/usr/local/bin`):

```
common '--test_env=SYSTEMROOT=C:\Windows'
common '--test_env=PATH=C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem;C:\Windows\System32\WindowsPowerShell\v1.0'
```

Do not set `TMP`/`TEMP`: bb_runner gives every action its own (`setTmpdirEnvironmentVariable`) and an
action value would override it. No `--action_env` is needed (hermetic-llvm's Windows tools do not use
PATH; §10.2 forbids PATH/INCLUDE/LIB there). Emit the same two lines from Windows clients too, so test
actions are identical across client OSes (cache sharing).

* `sh_test`/`py_test` need `launcher.exe`, which non-Windows Bazel builds from source with the target's
  C++ toolchain. With hermetic-llvm's MinGW toolchain add
  `--per_file_copt=external/bazel_tools/src/tools/launcher/util/launcher_util\.cc@-D_CRT_RAND_S`
  (upstream fix drafted). Running them also needs bash or Python on the workers, which the Windows
  image does not ship: use `cc_test` (Abseil does).
* **Windows client alternative** (openai/codex#20585, campaign T19): a Windows client compiles on Linux
  workers and tests on Windows workers with no overlay, because Windows Bazel embeds `tw.exe`
  (`cucinactl bazelrc --cross --target x86_64-windows-gnu` on Windows, plus the Windows-client lines of
  [Hermeticity and cache hygiene](#hermeticity-and-cache-hygiene) to share the cache with Linux
  clients). Tests can instead stay on the client with `--strategy=TestRunner=local`. A Windows client
  can also test *Linux* targets on Linux workers; Bazel then picks the test shell from the exec
  platform (`/bin/bash`) unless `--shell_executable` or `BAZEL_SH` is set, so leave both unset there.
* Upstream: `docs/upstream/bazel-19209-windows-test-wrapper/` (patches against 9.2.0, PR text, evidence).

## qemu-user runners

riscv64, s390x and armv7 tests run on the `linux-x86-64` pool's emulated runners (`cucina-emulation:
qemu`, concurrency 2 per VM). The x86_64 image registers `qemu-user-binfmt` (fix-binary) and exposes
the cross glibc runtimes at the paths their own `ld.so` searches (ADR 0305), so tests need no
`QEMU_LD_PREFIX` and no per-action environment; musl targets are fully static. Expect 5–20× slower
CPU-bound tests; `testTimeoutScale` (10) means `--test_timeout=600,3000,9000,36000` for these targets.
The campaign runs a smoke subset (MUST) and the full suite where time allows (SHOULD).

## Build-only targets (wasm, BPF)

wasm32/wasm64 and bpfeb/bpfel have no OS (`os: none`, `testMode: none`, `testSkip: not-applicable`):
build only (Abseil does not support them; the campaign builds a hermetic-llvm e2e example). The e2e
harness reports their test step as "not applicable" (UC27). Running `bazel test` for them would find
no test exec platform (`By default, tests are executed on the first execution platform that matches all
constraints specified by the target platform…`), which is the intended fast failure.

## Fast failure when no runner exists (UC27)

| Case | Where it fails | Message |
| --- | --- | --- |
| Excluded target (macOS x86_64, Windows arm64) | `cucinactl bazelrc --cross` (row has `excluded: true`) and `tools/xplat` validation | the row's `reason`, e.g. "macOS x86_64 is excluded by the user (PROMPT.md section 13)." |
| Excluded or unknown platform used directly with Bazel | analysis | no toolchain registered (`No matching toolchains found …`) |
| Test target platform without a twin (custom platform) | analysis | Bazel's `default_test_toolchain_type` message: "By default, tests are executed on the first execution platform that matches all constraints specified by the target platform…" |
| Pool disabled, at `max: 0`, image missing or failing to provision | controller, within one scaling cycle of the Execute | the controller fails the queue's waiting operations (`KillOperations{size_class_queue_without_workers}`, R-RE-2) with a status naming the pool and the reason; Bazel prints it and stops instead of waiting |
| Properties no queue is declared for (pool not in `values.pools`, stale `@cucina_platforms`) | scheduler, immediately | `FAILED_PRECONDITION: No workers exist for instance name prefix … platform …` |

## Hermeticity and cache hygiene

* **Repo contents cache (R-DATA-2).** Checked 2026-10-02 with every row fetched: all heavy hermetic-llvm
  repositories are reproducible and land in the contents cache — LLVM prebuilts (darwin-arm64,
  linux-amd64/arm64, windows-amd64) and extras, `llvm-project`, glibc headers and stubs per
  architecture/version, kernel headers, musl, MinGW, `msvc_runtime`, `windows_sdk`, plus
  `cucina_macos_exec_sdk`. They contain only relative symlinks that stay inside the repository (1,139
  checked; none absolute or escaping). Not cached: the two selector hubs `@glibc` and `@kernel_headers`
  (BUILD files only, a few KB, no download). No runtime `getenv`/`watch` in the cached repositories.
* **One toolchain upload per version (R-XPLAT-7, NFR-X5).** The same repositories serve every
  configuration; inputs are content-addressed, so a toolchain blob crosses P1 once per version and host
  OS (the repo contents cache key includes the client OS).
* **MSVC EULA flags** appear only in `--config=msvc` (`.bazelrc`) and in `cucinactl bazelrc --cross` output
  for targets with `abi: msvc`. Listing the MSVC twin in `:cucina` fetches nothing unless an MSVC target
  is built.
* **Identical action keys across clients (UC26, NFR-X3)**: `--experimental_platform_in_output_dir` with
  hashed names, the exec-side Apple SDK, the Windows test environment above and the byte-identical
  overlay make Linux and macOS clients send identical actions. Bazel derives the strict action
  environment from the *client* OS (Linux/macOS: `PATH=/bin:/usr/bin:/usr/local/bin`; Windows: MSYS
  and `C:\Windows` directories) and Windows clients without runfiles add `RUNFILES_MANIFEST_ONLY=1`,
  so a Windows client shares the cache of cross configurations only with these lines (hermetic-llvm's
  tools do not read PATH; this does not apply to the native MSVC lane of §10.2):

  ```
  # Windows clients, cross configurations only
  common --action_env=PATH=/bin:/usr/bin:/usr/local/bin
  common --host_action_env=PATH=/bin:/usr/bin:/usr/local/bin
  common --enable_runfiles
  startup --windows_enable_symlinks
  ```

  Checked offline from macOS (`--execution_log_json_file`): the Windows test spawn is
  `external\bazel_tools\tools\test\tw.exe ./lib_test.exe` with platform `{ISA: x86-64, OSFamily:
  windows}` and environment exactly `PATH` (Windows), `SYSTEMROOT`, `TEST_SRCDIR`, `TEST_TMPDIR`
  (relative) and `TZ=UTC`.

## Maintaining the module

* Edit `platforms/targets.json` (or `platforms/pools.json`), then `bazel run //tools/xplat:update`
  (also run by `bazel run //:update_goldens`); `//tools/xplat:fresh_*_test` fail on drift.
* `bazel/platforms` is a separate module (`.bazelignore`d in this repository, consumed through
  `local_path_override`); the root `MODULE.bazel` includes `tools/xplat/cucina_platforms.MODULE.bazel`.
* New Xcode version: add the pool platform to `pools.json` (`xcodeVersion`), an `execPlatforms` row with
  `flags: ["--@cucina_platforms//apple:sdk_version=<v>"]`, regenerate.
* New Bazel version: add the Windows release's SHA-256 to `tools/xplat/windows-test-overlay.sh`.

## Known limitations

* macOS-target tests run on the Xcode runner, not the generic one (ADR 0903).
* `layering_check` is not supported for macOS targets (SDK headers are outside the module map).
* `sh_test`/`py_test` on Windows workers need an interpreter the image does not have.
* hermetic-llvm's bootstrap (stage1+) toolchains are not registered.
* Windows tests from Linux/macOS clients were verified up to the test spawn on the dev Mac; the run on
  Windows workers is campaign scenario T16/T18.
