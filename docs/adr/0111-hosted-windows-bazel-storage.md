<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0111 — Avoid duplicate build caches on hosted Windows

* Status: accepted (2026-10-02); native hosted validation pending

## Context

Run 37067506820 at `93e4411` reached Go linking on the standard Windows runner and failed with
“There is not enough space on the disk” for `reconcile_test.exe` and `static_test.exe`.
The log records a cold cache miss, a checkout on `D:`, and the output root, repository-download
cache, shared extracted-repository cache and disk action cache all on `C:`. It does not record
starting free space, so no particular volume capacity is assumed.

Bazel 9.2.0's `help build --long` confirms that `experimental_disk_cache_gc_max_size=4G` applies
only after the server is idle (five minutes by default). It does not limit peak disk-cache
usage during the build. An action-cache copy of every output can therefore accumulate next
to the output tree while the command is running.

## Decision

Keep `windows-latest` and all build/test targets. Only Windows storage preparation changes:

* Require `GITHUB_ACTIONS=true`, `RUNNER_OS=Windows` and `RUNNER_ENVIRONMENT=github-hosted`.
* Inventory the actual fixed system/workspace volumes and choose the one with most free bytes;
  do not assume `D:` exists or is larger. Use the short `<drive>:/b/o` output root and
  `<drive>:/b/r` repository cache on that same volume. Refuse an already-existing root.
* Preserve cached repository downloads using a separate versioned `actions/cache` key and
  `--experimental_repository_cache_hardlinks`. Do not restore the former combined action cache.
* Set `--disk_cache=` and `--repo_contents_cache=` explicitly. Keep Bazel's normal output-base
  action cache, so the subsequent test command reuses the build. One ephemeral workspace does
  not need another directory of every action output or shareable extracted repositories.
* Record free bytes before setup, after cache restore and after the build/test lane, including
  failure. Report logical output/cache file sizes separately from allocated disk space.

There is no cleanup command: no Visual Studio, Windows SDK, .NET, Git, preinstalled tool or
caller-owned directory is removed. Linux/macOS settings, test assertions, target selection and
timeouts stay unchanged. A larger paid runner is permitted only as a later measured fallback;
this change allocates none and adds no paid capacity.

## Validation and consequences

The failed hosted run is the red evidence. Pinned Bazel help verified the cache flags and their
semantics. A controlled PowerShell fixture executes the workflow's actual setup body against
stateful fake volumes/files: local, wrong-OS and self-hosted contexts cannot mutate state;
two-volume and single-volume selection works; unrelated disks are ignored; zero capacity and
pre-existing roots are rejected. Actionlint and shellcheck pass. No host storage was changed
while validating these cases.

The next native Windows run must demonstrate that the unchanged build and tests finish with
remaining free space. Its measurements, not the local fixture or a successful cross-build,
will determine whether additional capacity is required. Cold builds may take longer without a
persisted disk action cache, but repository downloads and within-job incremental reuse remain.
