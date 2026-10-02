<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0353 — macOS VMs: Xcode and generic runners each offer vCPU slots

* Status: accepted (2026-10-02)

## Context
Every macOS worker VM advertises two runner platforms (R-MAC-7, R-XPLAT-3): `xcode`
(`OSFamily=macos, ISA=arm-a64, xcode-version=<ver>`) for Xcode-dependent actions and `generic`
(`OSFamily=macos, ISA=arm-a64`) for hermetic-llvm builds and cross-built tests. Buildbarn matches properties
exactly, so they are two scheduler queues served by one `bb_worker`, whose runner entries each have a fixed
`concurrency`; Buildbarn has no slot pool shared between runners. Options:
(a) split the vCPUs between the runners (e.g. half each), (b) give each runner all vCPUs, (c) one runner per VM
and two pools. `platforms/pools.json` defaults to (b) (`vcpusFactor: 1.0` on both runners).

## Decision
Keep (b): each runner offers `vCPUs` slots; both share the VM's CPUs.
* In practice a VM sees one kind of work at a time: a hermetic macOS-target build sends compile, link and test actions
  to `generic` (R-XPLAT-1 default placement); an Xcode/rules_apple build sends almost everything to `xcode`. A split (a)
  would leave half of every VM idle in both common cases; (c) doubles VMs for the same hardware and Apple's 2-VM cap.
* When both kinds overlap, the VM runs up to 2 x vCPUs actions: the macOS scheduler time-slices them (throughput stays
  about the same, per-action latency rises), and the autoscaler sizes VM count from the **sum** of demand over vCPUs
  (`docs/contracts.md` §3: `ceil(sum_r D_r / N)`), so sustained overlap adds VMs rather than queueing behind
  oversubscription, up to the pool's `max` and the host slots.
* Memory headroom: a VM gets (host RAM - 8 GB) / slots (20 GB on a 48 GB host with 2 slots, 28 GB on the recommended
  64 GB Mac mini); 2 x vCPUs typical clang/swiftc actions fit. Pools with memory-heavy actions lower one runner's
  factor in `platforms.extra` (Helm) without an image change.

## Consequences
* Worst-case per-action latency doubles while both runners are saturated; acceptable for build actions, documented
  in the operations guide.
* The concurrency is rendered from `WorkerSettings.runners[].concurrency` (0 = vCPUs), so the policy lives in
  `platforms/pools.json` and the image needs no change to alter it.
