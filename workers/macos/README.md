<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# macOS worker image (`workers/macos`)

Packer + [`packer-plugin-tart`](https://github.com/cirruslabs/packer-plugin-tart) (pinned `= 1.21.0`) template that turns
Cirrus Labs' `ghcr.io/cirruslabs/macos-<release>-xcode:<tag>` into the Cucina worker image
`cucina-worker-macos:<xcode>-<cucina_version>` (R-MAC-7). Hosts run it with `cucina-hostd`, which configures every VM
at every start through the Tart Guest Agent; the in-VM contract is [docs/dev/hostd.md §1](../../docs/dev/hostd.md).
Operations (versions, Xcode sources, disk budget, publishing): [docs/operations/macos-images.md](../../docs/operations/macos-images.md).

```sh
make -C workers/macos image-macos XCODE=27.0   # build -> smoke test (throwaway clone) -> report
make -C workers/macos validate                 # packer init/fmt/validate + shellcheck, no VM
make -C workers/macos clean-test-vms           # delete leftover cucina-imgtest-* VMs
```

| Path | Purpose |
| --- | --- |
| `versions.json` | Pins: plugin, base image per Xcode (+ digest), Xcode build, Buildbarn release + SHA-256, layout. |
| `packer/` | `plugins.pkr.hcl`, `variables.pkr.hcl`, `macos.pkr.hcl`, one `xcode-<ver>.pkrvars.hcl` per Xcode. |
| `provision/` | Guest scripts (root, in order): inspect, Xcode, system, build user, data volume, Cucina payload, finalize, verify. |
| `files/` | launchd plists, newsyslog and sshd drop-ins, the in-VM render call site. |
| `scripts/` | Host side: `fetch-buildbarn.sh`, `test-image.sh` (boot-to-ready + smoke), `smoke.sh` (in-guest, installed as `cucina-smoke`), `push.sh` (private GHCR, guarded). |
| `bench/` | R-CACHE-3 measurement harness (NFSv4 vs native build directory in a Tart guest), see ADR 0351. |

## What the image contains

* **Exactly one Xcode** at `/Applications/Xcode.app` (a real directory), selected with `xcode-select`, licence accepted,
  first launch done; no other Xcode bundle and no Command Line Tools. Fixed paths for the exec-side SDK (R-XPLAT-8):
  `DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer`,
  `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk`
  (`MacOSX<ver>.sdk` is a symlink to it), `/usr/bin/clang` resolves through `xcode-select` to Xcode's toolchain.
  `image.json` records `xcode.xcodeVersionOverride` (e.g. `27.0.0.27A266a`), the key of bb_runner's
  `appleXcodeDeveloperDirectories` for Bazel's `XCODE_VERSION_OVERRIDE`.
* **Buildbarn** `bb_worker`/`bb_runner` (bb-remote-execution `20260930T173749Z-1a3be95`, darwin/arm64, SHA-256 checked
  against the release's `sha256` asset and `versions.json`, again inside the guest) in `/usr/local/cucina/bin`, plus
  `cucina-worker-agent` when built (`/usr/local/cucina/libexec/cucina-render` is the in-VM render call site).
* **launchd**: `/usr/local/cucina/launchd/ai.sloper.cucina.bb-worker.plist` (system domain = LaunchDaemon, runs as
  root: `image.json` `workerUser: "root"`) and `ai.sloper.cucina.bb-runner.plist` (LaunchAgent, `gui/<uid>` of the
  build user, `LimitLoadToSessionType=Aqua`); both
  `ProcessType=Interactive`, `KeepAlive`. They are *not* in `/Library/Launch*`: nothing Buildbarn-related runs until
  hostd has pushed configs and credentials and bootstraps them (hostd §1.2). They share bb_runner's UNIX socket
  `/var/run/cucina/runner.sock`.
* **Build user** `builder` (uid 600): a standard account (no sudo, not admin; `_developer` group) that is logged in
  automatically, so bb_runner and every action run unprivileged in a GUI session (R-MAC-4, R-SEC-5), while bb_worker
  (root) keeps the worker key, the L1 and the file pool out of the actions' reach and can mount NFSv4. The Tart Guest
  Agent's RPC (`tart exec`) runs in its root LaunchDaemon (`--run-daemon --run-rpc`): answers before the GUI login and
  gives hostd root through `sudo -n` without any sudoers entry (hostd §1.1 option B).
* **Case-sensitive APFS volume** `cucina` at `/Volumes/cucina` (`build/`, `tmp/` for the build user; `cache/`,
  `state/` for bb_worker; owners enabled, no Spotlight, no FSEvents journal) in the boot container, so it grows with
  the disk. The persistent L1 (40 GiB default, rendered by hostd; suggested under `state/`) lives on the VM disk and
  survives VM shutdown (R-CACHE-2).
* **Headless settings**: Spotlight off on all volumes, no Software Update checks/downloads, no sleep or screensaver,
  Screen Sharing disabled, no Bonjour advertisements, SSH without passwords; the Cirrus `admin` password is rotated
  to an unrecorded random value at the end of the build.
* **Version label**: `/usr/local/cucina/image.json` (schema 1, hostd reads it) and `/etc/cucina/image-version`
  (`<xcode>-<cucina_version>` = the OCI tag = the pool generation's image version); `push.sh` adds the same as OCI labels.

Nothing secret is baked in: `/etc/cucina/pki` is empty in the image.

## Runners and concurrency

One bb_worker per VM advertises two runner platforms (R-MAC-7, R-XPLAT-3):
`xcode` = `{OSFamily: macos, ISA: arm-a64, xcode-version: <ver>}` and `generic` = `{OSFamily: macos, ISA: arm-a64}`
(hermetic-llvm builds and cross-built tests). Each offers **vCPUs slots** and both share the VM's CPUs
(`platforms/pools.json` default); ADR 0353 explains why this oversubscription-on-mix policy is kept.
