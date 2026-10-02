#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Provisions a Cucina Linux worker image (R-POOL-4) on Ubuntu 26.04 LTS or Amazon Linux 2023, x86_64 or arm64.
# Run as root by Packer with ARCH, VARIANT, IMAGE_VERSION, GENERATION in the environment and the repository's
# workers/linux/files tree + versions.json staged in /var/tmp/cucina.
#
#  * FUSE 3 (user_allow_other), chrony -> Amazon Time Sync Service, SSM Agent as a .deb (not the snap)
#  * pinned bb_worker/bb_runner (SHA-256) in /usr/local/bin, systemd units (bb-runner -> bb-worker), bootstrap hook,
#    unprivileged bbrunner user for run_commands_as, instance-store formatter, dead-man switch timer
#  * boot trims: no snapd, no unattended-upgrades/apt timers, cloud-init reduced to what a worker needs, volatile
#    journal, noatime, no fsck, masked units, networkd-wait-online returns on the first interface
#  * x86_64 + Ubuntu only: qemu-user (static) with binfmt_misc (fix-binary) and cross glibc runtimes for
#    riscv64, s390x and armhf, usable without QEMU_LD_PREFIX (R-XPLAT-3)
set -euxo pipefail

: "${ARCH:?}" "${VARIANT:?}" "${IMAGE_VERSION:?}" "${GENERATION:?}"
stage=/var/tmp/cucina
files=$stage/files
pin() { python3 -c 'import json,sys; v=json.load(open(sys.argv[1]))["pins"]
for k in sys.argv[2:]: v=v[k]
print(v)' "$stage/versions.json" "$@"; }

case "$ARCH" in x86_64) debarch=amd64 ;; arm64) debarch=arm64 ;; *) echo "bad ARCH $ARCH" >&2; exit 2 ;; esac

fetch() { # fetch URL SHA256 DEST
  local url=$1 sum=$2 dest=$3 i
  for i in 1 2 3 4 5; do
    if curl -fsSL --retry 3 -o "$dest.part" "$url"; then break; fi
    sleep $((i * 3))
  done
  echo "$sum  $dest.part" | sha256sum -c -
  mv "$dest.part" "$dest"
}

cloud-init status --wait >/dev/null 2>&1 || true

# R-ARTIFACT: fail closed before installing components when their legal payload is absent.
bash "$stage/install-notices.sh" "$stage/notices" /usr/share/doc/cucina

# --- Packages ------------------------------------------------------------------------------------------------
if [[ "$VARIANT" == ubuntu ]]; then
  export DEBIAN_FRONTEND=noninteractive
  # The stock AMI ships the SSM agent as a snap; snapd seeding is one of the slowest boot steps.
  if command -v snap >/dev/null; then
    for s in $(snap list 2>/dev/null | awk 'NR>1 {print $1}' | grep -v -E '^(core|core[0-9]+|snapd|bare)$' || true); do snap remove --purge "$s" || true; done
    for s in $(snap list 2>/dev/null | awk 'NR>1 {print $1}' || true); do snap remove --purge "$s" || true; done
  fi
  purge=()
  for p in snapd unattended-upgrades apport apport-core-dump-handler apport-symptoms landscape-common popularity-contest \
    ubuntu-advantage-tools ubuntu-pro-client ubuntu-pro-client-l10n fwupd fwupd-signed modemmanager udisks2 multipath-tools \
    lxd-agent-loader lxd-installer motd-news-config update-notifier-common update-manager-core byobu needrestart pollinate \
    sosreport open-iscsi plymouth plymouth-theme-ubuntu-text networkd-dispatcher kerneloops whoopsie; do
    if dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q 'ok installed'; then purge+=("$p"); fi
  done
  # No autoremove: purging can take metapackages (ubuntu-server/-minimal) with it, and autoremove would then
  # strip their dependencies.
  if ((${#purge[@]})); then apt-get purge -y "${purge[@]}"; fi
  rm -rf /snap /var/snap /var/lib/snapd /var/cache/snapd /root/snap /home/*/snap
  printf 'Package: snapd\nPin: release *\nPin-Priority: -10\n' >/etc/apt/preferences.d/cucina-no-snapd

  apt-get update -y
  pkgs=(fuse3 chrony jq ca-certificates curl mdadm)
  if [[ "$ARCH" == x86_64 ]]; then
    mapfile -t qemu_pkgs < <(python3 -c 'import json; print("\n".join(json.load(open("/var/tmp/cucina/versions.json"))["pins"]["qemu_cross"]["packages"]))')
    pkgs+=("${qemu_pkgs[@]}")
  fi
  apt-get install -y --no-install-recommends "${pkgs[@]}"

  # SSM Agent as a pinned .deb (R-POOL-4).
  fetch "$(pin ssm_agent deb "$debarch" url)" "$(pin ssm_agent deb "$debarch" sha256)" /tmp/amazon-ssm-agent.deb
  dpkg -i /tmp/amazon-ssm-agent.deb
  rm -f /tmp/amazon-ssm-agent.deb
  systemctl enable amazon-ssm-agent.service
else
  dnf -y install fuse3 jq mdadm
  systemctl enable amazon-ssm-agent.service
fi

# --- FUSE, time, SSM ---------------------------------------------------------------------------------------
touch /etc/fuse.conf
if grep -qE '^\s*#\s*user_allow_other' /etc/fuse.conf; then
  sed -i -E 's/^\s*#\s*user_allow_other/user_allow_other/' /etc/fuse.conf
elif ! grep -qE '^\s*user_allow_other' /etc/fuse.conf; then
  echo user_allow_other >>/etc/fuse.conf
fi

if [[ "$VARIANT" == ubuntu ]]; then
  install -d /etc/chrony/sources.d
  rm -f /etc/chrony/sources.d/*.sources
  install -m 0644 "$files/etc/chrony-amazon.sources" /etc/chrony/sources.d/cucina-amazon.sources
  # Drop the default internet pools (NTS to public servers): the Amazon Time Sync Service is local and faster.
  sed -i -E 's/^(pool |server )/# cucina: \1/' /etc/chrony/chrony.conf
  if systemctl list-unit-files systemd-timesyncd.service >/dev/null 2>&1; then systemctl mask systemd-timesyncd.service; fi
  systemctl enable chrony.service
else
  # AL2023's chrony already prefers 169.254.169.123 (link-local source); keep it.
  systemctl enable chronyd.service
fi

install -d /etc/amazon/ssm
install -m 0644 "$files/etc/amazon-ssm-agent.json" /etc/amazon/ssm/amazon-ssm-agent.json

# --- Buildbarn binaries, users, directories ------------------------------------------------------------------
fetch "$(pin buildbarn bb_worker "$debarch" url)" "$(pin buildbarn bb_worker "$debarch" sha256)" /usr/local/bin/bb_worker
fetch "$(pin buildbarn bb_runner "$debarch" url)" "$(pin buildbarn bb_runner "$debarch" sha256)" /usr/local/bin/bb_runner
chmod 0755 /usr/local/bin/bb_worker /usr/local/bin/bb_runner

uid=$(pin bbrunner uid)
gid=$(pin bbrunner gid)
getent group bbrunner >/dev/null || groupadd --system --gid "$gid" bbrunner
getent passwd bbrunner >/dev/null || useradd --system --uid "$uid" --gid "$gid" --no-create-home \
  --home-dir /nonexistent --shell /usr/sbin/nologin bbrunner

# Layout shared with cucina-worker-agent / internal/bbconfig (docs/dev/worker-agent.md): /var/lib/cucina is the
# build root (0755, actions traverse it as bbrunner); the agent creates its private state root and plan directories.
install -d -m 0755 /etc/cucina /etc/cucina/bb /opt/cucina /opt/cucina/bin /var/lib/cucina \
  /var/lib/cucina/build /var/lib/cucina/ephemeral /var/lib/cucina/data
install -d -m 0700 /etc/cucina/pki
install -m 0755 "$files"/bin/* /opt/cucina/bin/
agent_src=$(find "$stage/agent" -maxdepth 1 -type f -name 'cucina-worker-agent*' | head -n 1)
if [[ -n "$agent_src" ]]; then
  install -m 0755 "$agent_src" /opt/cucina/bin/cucina-worker-agent
  /opt/cucina/bin/cucina-worker-agent version
fi
install -m 0644 "$files/etc/deadman.env" /etc/cucina/deadman.env
install -m 0644 "$files/etc/tmpfiles-cucina.conf" /etc/tmpfiles.d/cucina.conf
install -m 0644 "$files"/systemd/*.service "$files"/systemd/*.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable bb-runner.service bb-worker.service cucina-worker-agent.service \
  cucina-format-instance-store.service cucina-deadman.timer

# --- qemu-user + cross runtimes (x86_64 Ubuntu): foreign dynamic binaries run without QEMU_LD_PREFIX ---------
if [[ "$ARCH" == x86_64 && "$VARIANT" == ubuntu ]]; then
  # The cross runtimes live in /usr/<triplet>/lib; expose them at the multiarch paths the foreign ld.so searches
  # (/usr/lib/<triplet>) and the ELF interpreters at /lib/<ld.so> (merged /usr). Nothing amd64 uses these paths.
  for t in riscv64-linux-gnu:ld-linux-riscv64-lp64d.so.1 s390x-linux-gnu:ld64.so.1 arm-linux-gnueabihf:ld-linux-armhf.so.3; do
    triplet=${t%%:*} ldso=${t#*:}
    test -e "/usr/$triplet/lib/$ldso"
    if [[ -e "/usr/lib/$triplet" && ! -L "/usr/lib/$triplet" ]]; then
      echo "/usr/lib/$triplet exists and is not a symlink" >&2
      exit 1
    fi
    ln -sfn "/usr/$triplet/lib" "/usr/lib/$triplet"
    ln -sfn "/usr/$triplet/lib/$ldso" "/usr/lib/$ldso"
  done
  # Registrations with the fix-binary flag (F) survive mount namespaces/chroots; systemd-binfmt loads them at boot.
  ls /usr/lib/binfmt.d/ || true
  systemctl restart systemd-binfmt.service || true
  if [[ ! -e /proc/sys/fs/binfmt_misc/qemu-riscv64 ]] && command -v update-binfmts >/dev/null; then
    update-binfmts --enable || true
  fi
  for f in qemu-riscv64 qemu-s390x qemu-arm; do
    test -e "/proc/sys/fs/binfmt_misc/$f"
    grep -E '^(interpreter|flags)' "/proc/sys/fs/binfmt_misc/$f"
  done
fi

# --- Boot trims ----------------------------------------------------------------------------------------------
install -d /etc/systemd/journald.conf.d
install -m 0644 "$files/etc/journald-cucina.conf" /etc/systemd/journald.conf.d/90-cucina.conf

# noatime and no fsck pass for every local file system in fstab; fsck.mode=skip on the kernel command line.
awk 'BEGIN { OFS = "\t" }
  /^[[:space:]]*#/ || NF < 4 { print; next }
  $3 ~ /^(ext4|xfs|vfat|btrfs)$/ {
    if ($4 !~ /(^|,)noatime(,|$)/) $4 = $4 ",noatime"
    if (NF < 5) $5 = 0
    $6 = 0
  }
  { print }' /etc/fstab >/etc/fstab.cucina
mv /etc/fstab.cucina /etc/fstab
cat /etc/fstab
if [[ "$VARIANT" == ubuntu ]]; then
  install -d /etc/default/grub.d
  cat >/etc/default/grub.d/90-cucina.cfg <<'GRUB'
# SPDX-License-Identifier: FSL-1.1-ALv2
GRUB_TIMEOUT=0
GRUB_TIMEOUT_STYLE=hidden
GRUB_RECORDFAIL_TIMEOUT=0
GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT fsck.mode=skip"
GRUB
  update-grub
  # Ubuntu cloud images boot without an initramfs (GRUB_FORCE_PARTUUID) and fall back to it only after a failed
  # attempt; grub-initrd-fallback.service clears GRUB's initrdfail flag after a good boot, so it must stay enabled
  # (masking it made every launch load the 43-58 MB initrd from the cold EBS snapshot: about 3 s more).
  grep -h GRUB_FORCE_PARTUUID /etc/default/grub.d/*.cfg || true
  install -d /etc/cloud/cloud.cfg.d
  install -m 0644 "$files/etc/cloud-cucina.cfg" /etc/cloud/cloud.cfg.d/90-cucina.cfg
  install -d /etc/systemd/system/systemd-networkd-wait-online.service.d
  install -m 0644 "$files/etc/networkd-wait-online-any.conf" /etc/systemd/system/systemd-networkd-wait-online.service.d/90-cucina.conf
  mask_units=(apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service man-db.timer
    man-db.service motd-news.timer motd-news.service fwupd-refresh.timer e2scrub_all.timer e2scrub_reap.service fstrim.timer
    dpkg-db-backup.timer update-notifier-download.timer update-notifier-motd.timer ua-timer.timer ua-reboot-cmds.service
    esm-cache.service apport.service ModemManager.service multipathd.service multipathd.socket udisks2.service
    lxd-agent.service pollinate.service secureboot-db.service
    iscsid.socket iscsid.service open-iscsi.service lvm2-monitor.service lvm2-lvmpolld.socket keyboard-setup.service
    console-setup.service setvtrgb.service rsyslog.service cron.service atd.service networkd-dispatcher.service
    snapd.service snapd.socket snapd.seeded.service ubuntu-advantage.service
    systemd-journal-flush.service ec2-instance-connect-harvest-hostkeys.service
    hibinit-agent.service sysstat.service sysstat-collect.timer sysstat-summary.timer sshd-keygen.service
    systemd-firstboot.service)
else
  grubby --update-kernel=ALL --args="fsck.mode=skip"
  install -d /etc/cloud/cloud.cfg.d
  install -m 0644 "$files/etc/cloud-cucina.cfg" /etc/cloud/cloud.cfg.d/90-cucina.cfg
  mask_units=(dnf-makecache.timer dnf-makecache.service update-motd.service hibinit-agent.service sysstat.service
    sysstat-collect.timer sysstat-summary.timer sshd-keygen.service sshd-keygen@.service systemd-firstboot.service
    systemd-boot-update.service
    rsyslog.service atd.service crond.service systemd-journal-flush.service)
fi
for u in "${mask_units[@]}"; do
  if systemctl list-unit-files "$u" --no-legend 2>/dev/null | grep -q .; then systemctl mask "$u"; fi
done
cloud-init schema --system 2>&1 | tail -n 3 || true

# --- Image metadata --------------------------------------------------------------------------------------------
kernel=$(find /lib/modules -mindepth 1 -maxdepth 1 -printf "%f\n" | sort -V | tail -n 1)
qemu=""
for q in qemu-riscv64-static qemu-riscv64; do
  if command -v "$q" >/dev/null; then qemu="$($q --version | head -n 1) ($(command -v "$q"))"; break; fi
done
pkgver() {
  if [[ "$VARIANT" == ubuntu ]]; then
    dpkg-query -W -f='${Version}' "$1" 2>/dev/null || true
  elif rpm -q "$1" >/dev/null 2>&1; then
    rpm -q --qf '%{VERSION}-%{RELEASE}' "$1"
  fi
}
python3 - <<PY
import json, os
img = {
  "family": "linux-${VARIANT}-${ARCH}",
  "image_version": "${IMAGE_VERSION}",
  "generation": "${GENERATION}",
  "os": dict(l.rstrip().split("=", 1) for l in open("/etc/os-release") if "=" in l),
  "kernel": "${kernel}",
  "buildbarn": {"bb_remote_execution": "$(pin buildbarn bb_remote_execution)",
                "bb_worker_sha256": "$(pin buildbarn bb_worker "$debarch" sha256)",
                "bb_runner_sha256": "$(pin buildbarn bb_runner "$debarch" sha256)"},
  "packages": {"ssm_agent": "$(pkgver amazon-ssm-agent)", "chrony": "$(pkgver chrony)", "fuse3": "$(pkgver fuse3)",
               "qemu_user": "$(pkgver qemu-user)", "qemu_user_binfmt": "$(pkgver qemu-user-binfmt)", "libc6_riscv64_cross": "$(pkgver libc6-riscv64-cross)",
               "libc6_s390x_cross": "$(pkgver libc6-s390x-cross)", "libc6_armhf_cross": "$(pkgver libc6-armhf-cross)",
               "systemd": "$(pkgver systemd)", "cloud_init": "$(pkgver cloud-init)"},
  "qemu": "${qemu}",
  "bbrunner": {"user": "bbrunner", "uid": $(pin bbrunner uid), "gid": $(pin bbrunner gid)},
  "worker_agent": os.path.exists("/opt/cucina/bin/cucina-worker-agent"),
  "worker_agent_version": "$( [[ -x /opt/cucina/bin/cucina-worker-agent ]] && /opt/cucina/bin/cucina-worker-agent version 2>/dev/null || true )",
  "paths": {"bb_worker_config": "/etc/cucina/bb/worker.json", "bb_runner_config": "/etc/cucina/bb/runner.json",
            "env_file": "/etc/cucina/env", "pki_dir": "/etc/cucina/pki", "run_dir": "/run/cucina",
            "build_root": "/var/lib/cucina", "instance_store_mount": "/var/lib/cucina/ephemeral",
            "data_volume_mount": "/var/lib/cucina/data", "instance_store_info": "/run/cucina/instance-store.json"},
}
img["os"] = {k: v.strip('"') for k, v in img["os"].items() if k in ("NAME", "VERSION", "VERSION_ID", "VERSION_CODENAME", "PRETTY_NAME")}
json.dump(img, open("/etc/cucina/image.json", "w"), indent=2)
print(json.dumps(img, indent=2))
PY
