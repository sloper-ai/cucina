<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0351 — macOS VMs use NFSv4 virtual build directories

* Status: accepted (2026-10-02)

## Context
R-CACHE-3 prefers NFSv4 in Tart guests when it is reliable; otherwise it requires a measured native fallback.
Native mode materializes every input file as a hardlink before execution. NFSv4 instantiates the input root lazily,
fetching files as actions access them. Both use the image's case-sensitive APFS mount point; persistent L1 is a
separate file-backed store, not the native input cache cleared at worker startup.

## Measurement
The reproducible driver is `workers/macos/bench/bench-build-dir.sh`. It used the locally built
`cucina-worker-macos:27.0-0.1.0-dev`: macOS 27.0 (`26A428`), Xcode 27.0 (`27A266a`), 7 vCPUs/20 GiB on an Apple M4 Max
host. bb_worker/bb_runner were release `20260930T173749Z-1a3be95`; host-side bb_storage was
`20260930T153215Z-086b011` and bb_scheduler matched the worker. Test servers bound only loopback and the vmnet bridge.

Bazel 9.2.0 remotely compiled 200 C++ translation units, linked and ran their test binary, and read a 2,000-header
input tree. Every compile action declared that tree, although it used only a subset. Both modes used an 8 GiB
persistent L1 for this small fixture and identical runner concurrency. `--noremote_accept_cached`, an empty local
incremental state before each run, and disabled local fallback forced actual execution. Separate smoke actions
proved **both** exact platform queues execute as a non-root user.

| Measurement | Native | NFSv4 |
| --- | ---: | ---: |
| Successful build/test runs | 5/5 | 5/5 |
| First run, worker caches cold | 172.6 s | 19.9 s |
| Warm runs, individually | 106.4, 85.4, 48.9, 33.8 s | 18.0, 16.7, 16.7, 16.9 s |
| Warm median (middle two averaged) | 67.15 s | 16.80 s |
| Start/configure worker → first trivial action complete | 4.3 s | 0.9 s |
| Mean FetchingInputs stage, all measured actions | 805.5 ms | 1.5 ms |
| Mean Running stage | 1,420.4 ms | 540.3 ms |
| Mean UploadingOutputs stage | 712.0 ms | 15.2 ms |
| Input-root files read/action metric | not emitted for native | 20.5 |

A smaller 20-unit/200-header pilot also passed both modes and both queues (native 17.5 s; NFSv4 4.9 s).
No test, compiler, linker or executable-loading failure occurred in either measured mode.

**Caveats:** modes ran sequentially in one VM, native first. Worker caches were reset between modes, but the OS/SDK
page cache and host load were not held constant; the declining native timings show warm-up. These are operational
measurements, not a causal estimate of the entire speedup. The input-stage difference is the relevant evidence for
avoiding eager materialization. This experiment does not measure full Abseil, WAN savings, or post-shutdown L1 hit
ratio (T13/NFR-T3 remain campaign measurements). The initial driver's lower-middle statistic was corrected to an
arithmetic median without changing raw timings; the original and corrected JSON reports are retained locally.

## Decision
* Resolve macOS `buildDirectory: auto` to **`nfsv4`** in the shared pool/config renderers; until that integration lands,
  send `buildDirectory=nfsv4` explicitly. The image itself carries layout/tools, not endpoint or worker configuration.
* bb_worker runs as **root** to mount the NFS filesystem; bb_runner and actions stay in the standard builder user's
  GUI session. Root-private state/PKI and builder-private runner socket permissions are preserved (ADR 0350).
* Use the pinned worker's Darwin NFS implementation with its OS-supported minor-version selection, 120 s enforced /
  60 s announced leases, and filesystem-access-cache prefetching. No FUSE extension or host filesystem mount is needed.
* Retain native as an explicit diagnostic fallback, on case-sensitive APFS, not the default. Record a new failure and
  reason before reverting a pool to it.

## Consequences
* Virtual inputs avoid repeated hardlink setup and reduce input-stage overhead for large declared trees.
* NFS mount cleanup is asynchronous. Same-boot worker replacement must let the old daemon release resources or use the
  pinned worker's stale-mount handling; normal persistent VM shutdown/restart creates a fresh kernel mount.
* The benchmark is acceptance-tier work on a real Mac, not a default unit/integration test. Its raw logs/results live
  outside the public repository; only redacted measurements are recorded here.
