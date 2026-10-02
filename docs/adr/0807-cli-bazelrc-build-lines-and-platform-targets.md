<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0807 — `cucinactl bazelrc` emits `build` lines and reads `platforms/targets.json`

* Status: accepted (2026-10-02); supersedes the flag scope and the targets fixture of ADR 0802

## Context
ADR 0802 emitted every flag under `common`/`common:<name>` and embedded a fixture targets
catalog until the cross agent published `platforms/targets.json` (schema in
docs/cross-compilation.md, ADR 0900). Two things changed:

* Bazel expands a config's `common:` lines before its `build:` lines, whatever file they are
  in. The Cucina repository's `.bazelrc` sets `build:cucina --remote_instance_name=main` and
  imports `bazel/platforms/cucina.bazelrc` (`build:cucina --extra_execution_platforms=…`), so a
  `common:cucina` line from `cucinactl bazelrc --config-name cucina` in `user.bazelrc` lost
  against them (observed by the cross agent with `--announce_rc`, ADR 0900). The same holds
  for unscoped lines against `build` lines of other rc files.
* `targets.json` adds exec-platform `flags` (the macOS SDK version), `testTimeoutScale` for
  qemu-user runners, `excluded` rows with a `reason`, and the decisions of ADRs 0903/0904:
  the Windows test environment from every client OS, Windows-client action environment
  lines, and a `@bazel_tools` overlay instead of a patched Bazel for Windows tests.

## Decision
* Every non-startup line is `build` (`build:<name>` with `--config-name`); `startup` lines
  first. `bazel query`/`bazel mod` therefore do not see them (documented).
* `catalog.rs` embeds `platforms/targets.json` and `platforms/pools.json`, ignores unknown
  fields and validates on load (pools/runners exist, macOS targets compile on macOS only,
  excluded rows have no placement and are the only ones using excluded OS/CPU pairs, flags are
  single `--` tokens). `cli/cucinactl/data/targets.json` stays as a minimal schema-1 test
  fixture (first-version fields plus unknown ones) for compatibility.
* Cross configurations add: the compile exec platforms' `flags` (chosen pool first, first
  occurrence of a flag name wins; native configurations with the module's labels add the
  chosen platform's), `--test_timeout` = Bazel's 60/300/900/3600 s × `testTimeoutScale`
  (rounded), the two Windows `--test_env` lines for Windows targets from every client, the
  Windows-client lines (`--action_env`/`--host_action_env=PATH=/bin:/usr/bin:/usr/local/bin`,
  `--enable_runfiles`, `startup --windows_enable_symlinks`), and for Linux/macOS clients
  targeting Windows a note (comment + stderr, `notes` in `bazelrc.v1`) pointing at
  `tools/xplat/windows-test-overlay.sh`. Excluded targets fail with their `reason` (exit 2);
  `--cache-only` with `--cross` is refused.
* `--list-targets` (`target-list.v1`) gains `test_timeout_scale`, `excluded`, `reason` and
  lists excluded rows.

## Consequences
cucinactl output composes with the repository's `:cucina` configs and other `build:` lines in
file order. A new `targets.json` field needs no cucinactl change unless it should be emitted;
a catalog the validation rejects breaks `bazelrc` at run time, so `//tools/xplat` and
`cucinactl`'s unit tests must agree. The property test allows build settings (`--@…`) only
verbatim from the catalog's exec platforms.
