<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0306 — Linux worker boot trims (no SSH, reduced cloud-init, volatile journal)

* Status: accepted (2026-10-02)

## Context
NFR-P1 needs Linux cold start p50 <= 60 s including registration; the image's share is boot-to-ready (R-POOL-2/4:
minimal first-boot work, no reboot, masked units, SSM agent as a deb).

## Decision
* No snapd (the SSM agent is the pinned `.deb`), no unattended-upgrades/apt/man-db/motd/fwupd timers, no
  apport/ModemManager/multipathd/udisks2/iscsi/lvm monitors, no rsyslog/cron/atd, no hibernation agent or sysstat.
* **No SSH daemon on workers** (masked; host-key generation masked): access is SSM Session Manager only, which also
  shrinks the attack surface. The image build itself uses SSH and masks it last.
* cloud-init limited to the EC2 datasource and the modules a worker needs (network, hostname, default user/keys,
  write_files/runcmd/user scripts); no package/apt/locale/timezone/ntp/snap/Pro work at boot.
* journald `Storage=volatile`; `noatime`, fsck disabled (fstab pass 0 + `fsck.mode=skip`), GRUB timeout 0,
  `systemd-networkd-wait-online --any`.
* Ubuntu's initrd-less boot (`GRUB_FORCE_PARTUUID`) stays on: `grub-initrd-fallback.service` must not be masked, or
  GRUB keeps its `initrdfail` flag and every launch loads the 43–58 MB generic initrd from the cold EBS snapshot
  (measured: about 3 s of initrd plus up to 10 s of slower pre-kernel time). A host-only dracut image is not
  produced by Ubuntu's `update-initramfs`, so shrinking the initrd is not pursued.
* Ubuntu chrony uses only the Amazon Time Sync Service (169.254.169.123 / fd00:ec2::123); the AL2023 comparison
  image prefers that source but retains its stock fallback pool. Both SSM agents use dual-stack endpoints
  (workers may only have IPv6 egress, R-DATA-4).

## Consequences
Measured (docs/operations/images.md): launch to `systemctl is-system-running` p50 15.8 s (Ubuntu x86_64), 16.3 s
(arm64), 12.0 s (AL2023). Debugging requires SSM (the worker instance profile has AmazonSSMManagedInstanceCore). Logs live in memory and are
lost at termination unless shipped (R-OBS-4 reaches them via SSM while running). Measured effects are in
docs/operations/images.md.
