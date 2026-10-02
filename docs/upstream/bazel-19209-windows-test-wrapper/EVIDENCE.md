<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Evidence (2026-10-02, dev Mac: macOS 27 arm64, Bazel 9.2.0, hermetic-llvm 0.8.24)

Workspace: an Abseil-style `cc_test` (`//:lib_test`, depends on `@abseil-cpp//absl/strings`) and an
`sh_test`, with the execution platform `@cucina_platforms//test:test_on_windows_x86_64`
(`parents = ["@llvm//platforms:windows_x86_64"]`, `exec_properties = {OSFamily: windows, ISA: x86-64}`).

| Experiment | Command (abridged) | Result |
| --- | --- | --- |
| aquery, no overlay | `aquery --platforms=@llvm//platforms:windows_x86_64 --extra_execution_platforms=<linux exec>,<test_on_windows> 'mnemonic(TestRunner, //:lib_test)'` | TestRunner on `test_on_windows_x86_64`; argv `external\bazel_tools\tools\test\tw.exe ./lib_test.exe`; inputs `external/bazel_tools/tools/test/{tw,xml}.exe`; env `PATH=/bin:/usr/bin:/usr/local/bin` (the client OS's strict PATH) |
| test, no overlay | `test --platforms=… --extra_execution_platforms=@platforms//host,<test_on_windows> //:lib_test` | `missing input file '@@bazel_tools//tools/test:tw.exe'` and `…:xml.exe`, "2 input file(s) do not exist" |
| test, overlay | same + `--override_repository=bazel_tools=<install_base/embedded_tools + tw.exe + xml.exe>` | Bazel 9.2.0 accepts the override under Bzlmod (`external/bazel_tools` → overlay); `lib_test.exe` is cross-compiled for MinGW and the test spawn runs (locally it cannot execute a Windows binary: `xml.exe … Exit 1`), i.e. nothing is missing any more |
| test spawn, overlay + Windows test env | same + `--test_env=SYSTEMROOT=C:\Windows --test_env=PATH=C:\Windows\System32;…` + `--execution_log_json_file` | the logged spawn: args `external\bazel_tools\tools\test\tw.exe ./lib_test.exe`, platform `{ISA: x86-64, OSFamily: windows}`, env `PATH` (Windows), `SYSTEMROOT`, `TEST_SRCDIR`, `TEST_TMPDIR`, `TZ=UTC` |
| sh_test, overlay | `build --platforms=@llvm//platforms:windows_x86_64 … //:sh_t` | Bazel builds `launcher.exe` from `@bazel_tools//src/tools/launcher` with the MinGW toolchain and fails: `launcher_util.cc:209:5: error: use of undeclared identifier 'rand_s'` |
| sh_test, overlay + define | same + `--per_file_copt=external/bazel_tools/src/tools/launcher/util/launcher_util\.cc@-D_CRT_RAND_S` | builds (10 actions) |

Release binary used for `tw.exe`/`xml.exe`: `bazel_nojdk-9.2.0-windows-x86_64.exe`, SHA-256
`d86a8241cfd5c0ce56ec6a61c08c381ff4178dc74d17dd5349b6636ccc5f1dcb` (matches the release's `.sha256`
asset). Its `embedded_tools` differs from the macOS 9.2.0 install base only in host binaries
(`tw.exe`, `xml.exe`, `launcher.exe`, `launcher_maker.exe`, `def_parser.exe`, `zipper.exe`,
modules tools) and the embedded JDK; all BUILD and `.bzl` files are identical.
