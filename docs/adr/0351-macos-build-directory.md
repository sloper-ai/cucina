<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0351 — macOS VMs: build directory mode (NFSv4 virtual vs native)

* Status: proposed — measurement pending (2026-10-02)

## Context
R-CACHE-3 asks for the NFSv4 virtual build directory in Tart guests (macOS >= 15) if it works reliably, else native,
measured both ways. A virtual build directory instantiates input roots lazily (fewer bytes fetched, no per-action
hardlinking, NFR-T8) but every file access of an action goes through bb_worker's in-process NFSv4 server; a native
one hardlinks every input file from a local cache before the action starts. The pool default `buildDirectory: auto`
means "the measured choice for the OS".

## Method
`workers/macos/bench/bench-build-dir.sh <image>`: a throwaway clone of the worker image (7 vCPUs, 20 GiB on the
16-core/48 GB dev Mac = hostd's sizing for 2 VMs), the pinned `bb_worker`/`bb_runner` configured exactly like hostd
does (configs under `/etc/cucina/bb`, launchd jobs bootstrapped from the image's plists, L1 on the VM disk), a local
`bb_storage` + `bb_scheduler` on the host over the vmnet gateway, and Bazel 9.2.0 driving a C++ workload of 200
compile actions whose input roots each hold 2,000 extra headers (a stand-in for toolchain/sysroot trees in hermetic
input roots), one link, one test-binary run and one action reading all 2,000 headers. Per mode: one cold run (empty
L1 and caches) and four warm runs with `--noremote_accept_cached`, plus bb_worker's per-stage metrics.

## Results
To be recorded from the campaign run.

## Decision
To be recorded from the campaign run.
