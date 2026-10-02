<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0904 — Windows tests from Linux/macOS clients: a `@bazel_tools` overlay (no patched Bazel)

* Status: accepted (2026-10-02) — option 1 of R-XPLAT-4 / ADR 0023 works

## Context
Non-Windows Bazel embeds `dummy.sh` where Windows Bazel embeds `tools/test/tw.exe` and `xml.exe`, so a
test whose exec platform is Windows fails with `missing input file '@@bazel_tools//tools/test:tw.exe'`
(bazelbuild/bazel#19209; reproduced with Bazel 9.2.0 on macOS). R-XPLAT-4 asks to try an overlay of
`@bazel_tools` first and to build a patched Bazel only if Bzlmod rejects it.

## Decision
* Bazel 9.2.0 accepts `--override_repository=bazel_tools=<dir>` under Bzlmod. The overlay is the
  client's own install base `embedded_tools` (unchanged) plus `tw.exe` and `xml.exe` extracted from the
  official `bazel_nojdk-9.2.0-windows-x86_64.exe` (SHA-256 `d86a8241…f1dcb`, pinned per Bazel version in
  `tools/xplat/windows-test-overlay.sh`, which builds it and prints the flag for `user.bazelrc`). Its
  BUILD/.bzl files are byte-identical to every 9.2.0 host's, so action keys of all other actions are
  unchanged and Windows-exec test actions match a Windows client's.
* Windows-target configurations from non-Windows clients add
  `--test_env=SYSTEMROOT=C:\Windows` and a Windows `--test_env=PATH=…` (Bazel's strict action env is the
  client OS's `/bin:/usr/bin:/usr/local/bin`). No `TMP`/`TEMP`: bb_runner sets them per action
  (`setTmpdirEnvironmentVariable`), and an action value would override its per-action directory. No
  `--action_env`: hermetic-llvm's Windows tools need no PATH, and §10.2 forbids PATH/INCLUDE/LIB there.
* `sh_test`/`py_test` need `launcher.exe`, which non-Windows Bazel builds from source; with
  hermetic-llvm's MinGW toolchain it needs `-D_CRT_RAND_S` for `launcher_util.cc`
  (`--per_file_copt=external/bazel_tools/src/tools/launcher/util/launcher_util\.cc@-D_CRT_RAND_S`).
  Running them also needs bash/Python on the Windows workers, which the image does not ship: out of
  scope; Abseil uses `cc_test`.
* The upstream fix (lazily fetched `@remote_windows_test_tools`, and the launcher include order) is in
  `docs/upstream/bazel-19209-windows-test-wrapper/`; Cucina does not carry it.
* The Windows-client pattern (openai/codex#20585: compile on Linux workers, test on Windows) needs
  nothing extra: Windows Bazel embeds `tw.exe`.

## Consequences
No patched Bazel to build, pin or ship through Bazelisk. Each Bazel upgrade adds the new release's
SHA-256 to the script. The overlay path is machine-local (user.bazelrc). Verified offline (missing
input → test spawn runs); the run on Windows workers is the campaign's T16/T18.
