<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0350 — macOS worker image: Cirrus base, unprivileged auto-login build user, hostd-started services

* Status: accepted (2026-10-02)

## Context
R-MAC-7 asks for a Packer (`packer-plugin-tart` >= 1.21) image built from Cirrus Labs' macOS + Xcode images with
`bb_worker` as a LaunchDaemon and `bb_runner` as a LaunchAgent in the auto-logged-in user's GUI session (R-MAC-4),
exactly one Xcode, no baked secrets, and a fast boot (NFR-P1). The Cirrus base auto-logs-in `admin` (password
`admin`, passwordless sudo, SSH and Screen Sharing on) and runs the Tart Guest Agent's `tart exec` RPC inside that
GUI session, so every action would run as a root-capable user. R-SEC-5 wants actions unprivileged ("on macOS, the VM
user session"). Hostd's published in-VM contract (docs/dev/hostd.md §1) supports the Cirrus default (A) and a
hardened variant (B), and installs the Buildbarn launchd jobs itself at every start.

## Decision
* **Base**: `ghcr.io/cirruslabs/macos-<release>-xcode:<tag>`, digest-pinned in `workers/macos/versions.json`, cloned by
  Packer (plugin pinned `= 1.21.0`); output `cucina-worker-macos:<xcode>-<cucina_version>` in `TART_HOME`, published
  separately (`push.sh`, private GHCR). Never let Packer pull implicitly (the Makefile checks `tart list`).
* **Hostd contract option B**: a dedicated *standard* user `builder` (uid 600, `_developer` group, no sudo) is the
  auto-login user and the `buildUser` of `image.json`; the Guest Agent RPC moves into the root LaunchDaemon
  (`tart-guest-agent --run-daemon --run-rpc`) and the per-user agent is removed, so `tart exec` runs as root (hostd's
  `sudo -n` needs no sudoers entry) and answers before the GUI login. `builder`'s password is random, generated in the
  guest and never printed; it exists only for local login and the root-0600 `/etc/kcpassword` representation.
  Golden Gate's `sysadminctl` exits zero while reporting `SACSetAutoLoginPassword error:22` in system/SSH bootstrap
  context. A real A/B check showed `launchctl asuser 0` leaving the stale base credential while `asuser 501` (the
  active base administrator GUI) updated it. Provisioning uses that GUI context and verifies the encoded password
  plus NUL against the pkg's shared codec, ignoring padding (ADR 0755). It precreates the builder's login keychain
  with the same password; smoke tests noninteractive add/delete, not just console ownership or process names.
* **bb_worker runs as root** (`image.json` `workerUser: "root"`, which hostd's contract supports; the plist has no
  `UserName`), bb_runner and therefore every action as `builder`. Root keeps the worker key, the L1 blocks, the file
  pool and bb_worker itself (signals, debugger attach) out of the actions' reach, and macOS only lets root mount the
  NFSv4 build directory (ADR 0351). Native build directories are shared across the two users the upstream way
  (bbconfig renders `setUmask: 0`; state directories stay 0700 so world-writable state files remain unreachable).
* **launchd**: the plists live in `/usr/local/cucina/launchd/` and hostd bootstraps them (system domain for bb_worker,
  `gui/600` for bb_runner) after pushing configs and credentials. This replaces the task's
  "loaded but not started" wording with the stronger "not loaded at boot" that hostd's contract defines: nothing
  Buildbarn-related runs before hostd has configured the VM, and `KeepAlive` restarts crashed services afterwards.
  `ProcessType=Interactive` keeps launchd from throttling build work.
* **Build-time credentials**: Cirrus' `admin`/`admin` is used by Packer only, on Tart's host-only NAT network. The last
  provisioning step disables SSH password and keyboard-interactive logins (`/etc/ssh/sshd_config.d/000-cucina.conf`),
  Screen Sharing is disabled, and `admin`'s password becomes an unrecorded random value. Hostd therefore does not need
  to rotate passwords per VM.
* **Data volume**: a case-sensitive APFS volume `cucina` in the boot container (shares and grows with the disk),
  owners enabled, Spotlight and FSEvents journaling off. Disk 250 GB logical (sparse) with the recovery partition removed
  so `tart set --disk-size` + the guest agent's resize can grow clones; images are replaced, never updated in place.
* **Background activity**: Spotlight off on all volumes, Software Update checks/downloads off, no sleep/screensaver,
  no Bonjour advertisements; no Cucina daemon runs in the guest besides the two Buildbarn jobs (hostd enforces the
  dead-man switch from outside, hostd §1.6). One reboot during the build performs `builder`'s first login so a VM's
  first start from the image is an ordinary boot.
* **Rosetta** is inherited from the Cirrus base, whose SIP is already disabled (verified in the guest). Provisioning
  disables `com.apple.oahd`, removes only Rosetta's installed payload/cache and receipt, and smoke requires its runtime
  and receipt to be absent. macOS x86_64 is not advertised. This does not change SIP on the development Mac.
* **Guest logs**: `/var/log/cucina` is root:wheel 0755; the worker log belongs to `workerUser`, the runner log to the
  build user (both 0644), including after newsyslog rotation. Red-first image smoke reproduced the old builder-owned
  directory vulnerability: an action could replace the root worker's log path. The strengthened smoke checks actual
  permissions as `builder`, rejects passwordless sudo, and verifies protected service/config/state directories.
* **Boot ordering**: staging is `/private/var/tmp/cucina-stage`, not `/private/tmp` (the latter was cleared during the
  provisioning reboot). Finalization waits for the build user's GUI session before closing build-time access.
  Golden Gate deletes legacy Software Update preferences at boot; disable/unload the guest's update daemon, and
  smoke-test that enforced state rather than assuming a persistent preference value.

## Consequences
* Two users share the native build directory; hostd writes the PKI files owned by `workerUser` (root) and creates
  bbconfig's state directories with the worker's ownership. Setting `layout.workerUser` to `builder` in
  `versions.json` gives the single-user variant of hostd's contract (no NFSv4, key readable by actions).
* The image is larger than the Cirrus base by the Cucina payload only when the base's Xcode is kept; replacing Xcode
  (ADR 0352) rewrites ~10 GB of the disk, which costs one larger first push/pull per Xcode version.
* Changing the base's auto-login user means hostd must use the console user/`buildUser` from `image.json`, never `admin`.
