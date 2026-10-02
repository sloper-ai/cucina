<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0570 — Worker agent picks and mounts the L1 volume; internal/bbconfig sizes it

* Status: accepted (2026-10-02)

## Context
The worker-agent task asked the agent for "L1 placement per `WorkerSettings.l1_placement`, L1 size derivation, `filePool`
location". `internal/bbconfig` (ADR 0411) independently implements L1 placement resolution and sizing from machine facts
(`InstanceStorePath/Bytes`, `DataVolumePath/Bytes`, `StateRoot/Bytes`, `MemoryBytes`): block tiers, key-location-map size,
file-pool quota per runner thread. Two sizing implementations would drift, and only bbconfig's is boot-tested against the
pinned `bb_worker`. Meanwhile the images already format the instance store themselves (Linux
`cucina-format-instance-store.service` → `/var/lib/cucina/ephemeral`, RAID 0 across several devices; Windows `cucina-boot.ps1`
→ `D:`), before bootstrap runs.

## Decision
* The agent owns the **device**: it lists disks (Linux sysfs model strings `Amazon EC2 NVMe Instance Storage` /
  `Amazon Elastic Block Store`, root disk by the major:minor of `/`; Windows `Get-Disk`), resolves `l1_placement`
  (`auto`: instance store → ephemeral EBS data volume → no device), and makes the volume usable: it **reuses** a volume that is
  already mounted (the disk itself, or an array the image mounted at the mount point), otherwise formats a blank device
  (ext4 without journal / NTFS quick) and mounts it (`/var/lib/cucina/{ephemeral,data}`, `C:\bb\{ephemeral,data}`). It never
  touches the OS disk, partitioned disks or RAID members.
* An explicit `instance-store`/`ebs` request without such a device degrades along the `auto` order with a WARN line
  (`event=l1.fallback`) instead of failing the worker: a misconfigured pool keeps building and the log says why.
* The agent passes the **resolved** placement and the facts to bbconfig, which does all sizing. "No device" leaves `auto`, so
  bbconfig chooses memory (≥ 16 GiB RAM) or the root disk.

## Consequences
* One sizing implementation (bbconfig), validated by booting `bb_worker`; the agent's tests cover detection and reuse only.
* The images' format units are optional: the agent covers images without them and EBS data volumes.
* Several instance-store devices are striped only when the image does it (mdadm); the agent alone uses the largest device.
