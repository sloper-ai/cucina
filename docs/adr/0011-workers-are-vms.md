<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0011 — D1: Workers are VMs running bb_worker and bb_runner natively

* Status: accepted (2026-10-02)

## Context
Remote-execution workers need native disk and CPU performance, and they must run on the
operating systems the builds target. A Kubernetes pod cannot be a Windows worker on k3s (no
Windows nodes), and macOS cannot be a Kubernetes node at all. Running `bb_worker` in a pod on a
Linux node would also hide the machine lifecycle (boot, image, termination) that Cucina has to control to
reach zero idle cost.

## Decision
Every worker is a virtual machine that runs `bb_worker` and `bb_runner` as ordinary processes:
an EC2 instance (Linux x86_64/arm64, Windows x86_64) or a Tart VM on a Mac mini. Cucina owns the
VM lifecycle. The `Compute` and `VMRuntime` ports (`internal/ports`) isolate the providers, so a
pod provider can be added later without touching the autoscaler.

## Consequences
* Images are first-class artifacts (AMIs, Tart images) with their own build, version and rollout
  story (see `docs/operations/images.md`, `docs/operations/macos-images.md`).
* Cucina, not Kubernetes, schedules workers: the controller launches VMs and drains them.
* Kubernetes-pod workers, EC2 Mac instances and Linux VMs on Mac hosts are out of scope for v1;
  the ports must not preclude them.
