<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Run tests on Windows execution platforms from Linux and macOS hosts

Upstream: bazelbuild/bazel#19209 ("Running a test on a Windows executor from a Linux host is not
possible"). Patches against tag `9.2.0`:

* `0001-Fetch-the-Windows-test-wrapper-on-non-Windows-hosts.patch`
* `0002-launcher-define-_CRT_RAND_S-before-including-windows.h.patch`

## Problem

Every test rule depends on `@bazel_tools//tools/test:test_wrapper` and `:xml_writer`. When the test's
execution platform is Windows, these select `tw.exe` and `xml.exe`, which only Windows builds of
Bazel embed in `@bazel_tools`. On a Linux or macOS host the files do not exist, so a test that is
meant to run on a remote Windows worker cannot run at all:

```
ERROR: BUILD.bazel:6:8: Testing //:lib_test failed: missing input file '@@bazel_tools//tools/test:tw.exe'
ERROR: BUILD.bazel:6:8: Testing //:lib_test failed: missing input file '@@bazel_tools//tools/test:xml.exe'
```

Cross-compiling for Windows on Linux workers and testing on Windows workers is otherwise fully
supported by Bazel 9 (the default test toolchain sends the test action to an execution platform that
satisfies the Windows target platform), so this is the only missing piece. Users work around it by
copying the two binaries into the install base or by driving such builds from a Windows host.

A second, smaller problem shows up once the test wrapper exists: `sh_test`/`py_test` for Windows need
`launcher.exe`, which non-Windows hosts build from `//src/tools/launcher`. With a MinGW-w64 cross
toolchain (`*-windows-gnu`) that fails:

```
src/tools/launcher/util/launcher_util.cc:209:5: error: use of undeclared identifier 'rand_s'
```

## Reproduction

Bazel 9.2.0 on macOS arm64 (also Linux x86_64), a C++ cross toolchain for Windows (here
hermetic-llvm 0.8.24), and an execution platform for the test that carries the Windows target's
constraints:

```starlark
# BUILD.bazel
cc_test(name = "lib_test", srcs = ["lib_test.cc"])
platform(
    name = "test_on_windows",
    parents = ["@llvm//platforms:windows_x86_64"],
    exec_properties = {"OSFamily": "windows", "ISA": "x86-64"},
)
```

```sh
bazel test //:lib_test --platforms=@llvm//platforms:windows_x86_64 \
  --extra_execution_platforms=@platforms//host,//:test_on_windows
# -> missing input file '@@bazel_tools//tools/test:tw.exe' (and xml.exe)
```

With a Windows remote executor the same happens before any upload. `bazel aquery` shows the action
Bazel wants to run: `external\bazel_tools\tools\test\tw.exe ./lib_test.exe` with inputs
`external/bazel_tools/tools/test/tw.exe` and `xml.exe`.

## Change

Patch 1 follows the precedent of `@remote_coverage_tools`: the Windows test tools become a lazily
fetched external repository for hosts that do not embed them.

* `tools/test/extensions.bzl`: a `remote_windows_test_tools_extension` declares
  `@remote_windows_test_tools`, an `http_archive` with `tw.exe` and `xml.exe` at its root.
* `src/MODULE.tools`: `use_repo` of that extension (the default lockfile needs regenerating).
* `tools/test/BUILD.tools`: in the Windows branch of `test_wrapper` and `xml_writer`, use the embedded
  binary when Bazel runs on Windows (`IS_HOST_WINDOWS`, already used by `tools/launcher/BUILD.tools`)
  and `@remote_windows_test_tools//:tw.exe` / `:xml.exe` otherwise.

Nothing changes on Windows hosts, and nothing is downloaded unless a test actually runs on a Windows
execution platform: the `select()` branch is only taken in that configuration.

The archive has to be published once by a maintainer (and again only when `tw.exe` or `xml.exe`
change): build `//tools/test:tw` and `//tools/test:xml` on Windows, zip `tw.exe` and `xml.exe`, upload
it to `https://mirror.bazel.build/bazel_windows_test_tools/releases/windows_test_tools-v1.0.zip`, and
replace the placeholder checksum in `extensions.bzl` (the same workflow as the coverage output
generator). A follow-up can publish `launcher.exe` in the same archive and register it as a prebuilt
`launcher_toolchain_type` toolchain for non-Windows hosts, so that `sh_test`/`py_test` for Windows no
longer need a C++ cross toolchain.

Patch 2 moves `#define _CRT_RAND_S` in front of `#include <windows.h>` in `launcher_util.cc`.
MinGW-w64's `<windows.h>` includes `<stdlib.h>`, which declares `rand_s` only if the macro is already
defined; MSVC's `<windows.h>` does not, which is why only `-windows-gnu` targets break.

## Testing

* With the two binaries made available at those labels (an `--override_repository=bazel_tools=`
  copy of the 9.2.0 install base's `embedded_tools` plus `tw.exe`/`xml.exe` from the official
  `bazel_nojdk-9.2.0-windows-x86_64.exe`), the reproduction above gets past input checking and
  executes the test spawn (`tw.exe ./lib_test.exe`, then `xml.exe`); without them it fails with the
  "missing input file" errors. End-to-end runs against a Windows remote executor (Buildbarn) are
  pending and will be added here.
* Patch 2: `sh_test` for `@llvm//platforms:windows_x86_64` (MinGW-w64, UCRT) builds `launcher.exe` from
  source on macOS arm64 with the define in place, and fails with the `rand_s` error without it.
* Suggested upstream test: a shell integration test in `src/test/shell/bazel/` that registers an
  execution platform with `@platforms//os:windows`, runs `bazel aquery` for a `sh_test` on that
  platform from a Linux host, and asserts that the TestRunner inputs contain
  `external/remote_windows_test_tools/tw.exe` (served from a local test archive through
  `--override_repository=remote_windows_test_tools=...`).

## Compatibility

* Windows hosts: unchanged (embedded binaries, no new repository fetched).
* Linux/macOS hosts: only builds that run tests on Windows execution platforms fetch the archive;
  the action keys of such tests contain the archive's binaries, which are the same files Windows
  hosts embed, so Linux, macOS and Windows clients can share remote cache entries.
* `--override_repository=remote_windows_test_tools=...` lets air-gapped users provide the archive.
