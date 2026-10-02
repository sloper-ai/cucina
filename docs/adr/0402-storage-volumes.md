<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0402 — Storage volumes: one filesystem PVC (file mode) or raw Block for the CAS plus a small filesystem PVC

* Status: accepted (2026-10-02)

## Context
R-CP-1/-3 ask for one PVC per shard holding CAS, AC, ISCC and FSAC with persistent state, file-backed on local-path and
raw `volumeMode: Block` on EBS CSI. A raw block PVC is one device; bb_storage needs eight block devices per shard (blocks
and key-location map of four stores) plus a directory per store for its persistent state.

## Decision
* `storage.mode: file` (default, k3s): one filesystem PVC; every store's blocks and key-location map are sparse files.
* `storage.mode: block` (EKS): the CAS blocks — the only large, hot region — use a raw Block PVC; the CAS key-location map,
  the other stores and the persistent state use a second, small filesystem PVC.
* The persistent-state directories are `subPath` mounts, which kubelet creates with the volume root's mode; no init
  container (and no extra image) is needed.
* The PVC size derives from the stores (+10 % and 1 GiB) or `storage.persistence.size`; StatefulSet claim templates are
  immutable, so growing a volume is a documented procedure (docs/operations/chart.md).

## Consequences
Block mode deviates from "one PVC each" by one small volume per shard. Changing a store's size re-lays its blocks and
empties that store (cold cache), which the operations guide calls out.
