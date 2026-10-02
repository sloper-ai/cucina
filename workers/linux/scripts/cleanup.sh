#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Last provisioning step of the Linux worker image: no SSH on workers (SSM only; masked now because the build
# itself used SSH), no build-time state (package lists, Packer's key, host keys, cloud-init instance data,
# machine-id, logs) in the AMI.
set -euxo pipefail
: "${VARIANT:?}"

if [[ "$VARIANT" == ubuntu ]]; then
  systemctl mask ssh.service ssh.socket
  apt-get clean
  rm -rf /var/lib/apt/lists/*
else
  systemctl mask sshd.service
  dnf clean all
fi
rm -f /etc/systemd/system/bb-runner.service.d/99-image-build.conf /etc/systemd/system/cucina-worker-agent.service.d/99-image-build.conf
rmdir /etc/systemd/system/bb-runner.service.d /etc/systemd/system/cucina-worker-agent.service.d 2>/dev/null || true
rm -rf /var/tmp/cucina /var/tmp/* /var/cache/man/*
rm -f /etc/cucina/bb/* /etc/ssh/ssh_host_* /root/.ssh/authorized_keys /home/*/.ssh/authorized_keys
if ! cloud-init clean --logs --seed --machine-id 2>/dev/null; then
  # cloud-init < 23.1 (Amazon Linux 2023) has no --machine-id
  cloud-init clean --logs --seed
  truncate -s 0 /etc/machine-id
fi
find /var/log -type f \( -name '*.gz' -o -name '*.[0-9]' \) -delete
find /var/log -type f -exec truncate -s 0 {} +
journalctl --rotate >/dev/null 2>&1 || true
journalctl --vacuum-time=1s >/dev/null 2>&1 || true
rm -f /root/.bash_history /home/*/.bash_history
sync
