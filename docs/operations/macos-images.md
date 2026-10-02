<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->

# macOS worker images (Tart)

How the golden macOS VM images that Mac hosts run are built, versioned, tested, stored and published
(R-MAC-3/4/7, R-VER-1..3, R-OPS-2/-7, R-DATA-5, R-CACHE-2/-3). Source: [`workers/macos`](../../workers/macos/README.md).
The contract between the image and `cucina-hostd` is [docs/dev/hostd.md §1](../dev/hostd.md). Decisions:
ADR [0350](../adr/0350-macos-worker-image.md) (image design), [0351](../adr/0351-macos-build-directory.md) (build
directory), [0352](../adr/0352-xcode-source-for-worker-images.md) (where Xcode comes from),
[0353](../adr/0353-macos-runner-concurrency.md) (runner concurrency).

## 1. What an image is

* A Tart VM built by Packer (`packer-plugin-tart` 1.21.0) from Cirrus Labs' `ghcr.io/cirruslabs/macos-<release>-xcode:<tag>`
  (Golden Gate = macOS 27, Tahoe = 26), pinned by digest in `workers/macos/versions.json`.
* Name and version: `cucina-worker-macos:<xcode>-<cucina_version>`, e.g. `cucina-worker-macos:27.0-0.1.0`. The tag is
  the **image version**: it is written to `/etc/cucina/image-version` and `/usr/local/cucina/image.json` inside the guest
  and to OCI labels (`ai.sloper.cucina.image-version`, `…xcode-version`, `…xcode-build`, `…buildbarn`) when pushed.
* A pool's generation is its image reference (R-POOL-8, R-OPS-2): pointing a `WorkerPool` at a new tag starts a new
  generation; hostd pre-pulls it and re-clones each VM at its next idle stop (VMs are persistent otherwise).
* Contents (details in the workers/macos README): exactly one Xcode at `/Applications/Xcode.app`, Buildbarn
  `bb_worker`/`bb_runner` (`20260930T173749Z-1a3be95`), optional `cucina-worker-agent`, launchd plists that hostd
  bootstraps, the unprivileged auto-login build user `builder`, the case-sensitive data volume `/Volumes/cucina`,
  Spotlight/Software Update/sleep/Screen Sharing off. No credentials.

### Fixed toolchain paths (R-XPLAT-8)

| What | Path / value |
| --- | --- |
| Xcode | `/Applications/Xcode.app` (real directory; the only Xcode; no Command Line Tools) |
| `DEVELOPER_DIR` / `xcode-select -p` | `/Applications/Xcode.app/Contents/Developer` |
| macOS SDK (`-isysroot`) | `/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk` (`MacOSX<ver>.sdk` links to it) |
| Toolchain | `/Applications/Xcode.app/Contents/Developer/Toolchains/XcodeDefault.xctoolchain/usr/bin/{clang,ld,…}`; `/usr/bin/clang` forwards there |
| `XCODE_VERSION_OVERRIDE` key | `<major>.<minor>.<patch>.<build>`, e.g. `27.0.0.27A266a` (`image.json` → `xcode.xcodeVersionOverride`), mapped to the developer dir by bb_runner's `appleXcodeDeveloperDirectories` |

## 2. Building an image

Prerequisites on the build Mac: macOS ≥ the guest's macOS, Tart 2.40.1, Packer 1.16.1 (the plugin is installed by
`packer init`), `jq`, the base image already in `TART_HOME` (the Makefile refuses to let Packer pull ~70 GB
implicitly: `tart pull <base>` first), and, if the Cirrus image ships a different Xcode build than the one pinned, that
Xcode in `/Applications/Xcode.app` on the build Mac (§3).

```sh
source .work/env.sh                                    # TART_HOME etc. (dev Mac); or export TART_HOME yourself
make -C workers/macos image-macos XCODE=27.0 CUCINA_VERSION=0.1.0
```

`image-macos` = `build` → `smoke` → `report`:
1. `fetch`: Buildbarn binaries downloaded and verified against the release `sha256` asset **and** the pins.
2. `agent`: `go build ./cmd/cucina-worker-agent` for darwin/arm64 (or `WORKER_AGENT=<path>`).
3. `packer build`: clone the base, grow the disk (`disk_size_gb`, recovery partition removed), then in the guest:
   Xcode (keep or copy, §3), system settings, build user + auto-login, data volume, Cucina payload, one reboot (first
   automatic login of `builder`), finalize (SSH passwords off, `admin` password rotated, staging removed), verify (the
   in-guest smoke test must pass). Log: `$CUCINA_DEV_STORAGE/logs/macimage-packer-<xcode>-<stamp>.log`.
4. `smoke` (`scripts/test-image.sh`): clone to a throwaway `cucina-imgtest-*` VM sized like hostd would, boot it 3×
   with hostd's flags, time boot-to-ready, run `cucina-smoke` through `tart exec`, record sizes, **delete** the clone.
   Report JSON: `$CUCINA_DEV_STORAGE/macimage/reports/`.

`FORCE=1` replaces an existing local image of the same tag. A Packer failure leaves no VM behind (the plugin deletes
its VM on error); `make clean-test-vms` removes leftover test clones.

## 3. Where Xcode comes from

Each image carries **exactly one** Xcode, matching the pool's `xcode-version` (R-MAC-7):

1. **A Cirrus `-xcode:<tag>` image** whose Xcode has the pinned build: nothing else to do. Cirrus publishes
   `ghcr.io/cirruslabs/macos-golden-gate-xcode:<ver>` (and `macos-tahoe-xcode` for side-by-side 26.x pools) shortly after
   Xcode releases; they may contain several Xcodes, simulator runtimes and CI tooling. The build removes every Xcode
   but the pinned one.
2. **Otherwise, an `Xcode.app` supplied by the operator** (ADR 0352): the build shares it read-only into the build VM
   (`--dir`, build time only) and copies it to `/Applications/Xcode.app` with `ditto`. This is how the campaign image
   gets the dev Mac's exact 27.0 (27A266a) build when Cirrus' image differs. To obtain a specific build:
   * **Manual step (needs the user's Apple ID; never automated, never stored):** sign in at
     <https://developer.apple.com/download/all/>, download `Xcode_<ver>.xip`, expand it (`xip --expand` or `unxip`),
     move the result to `/Applications/Xcode.app` (or point `XCODE_APP=` at it), run it once or
     `sudo xcodebuild -license accept`. Tools such as `xcodes` can download with an interactive Apple ID login; do not
     put Apple ID credentials, session cookies or app-specific passwords into the repository, CI variables or images.
   * Xcode's licence forbids redistribution outside the organization: images containing it go only to the **private**
     GHCR package (§6).

## 4. A new Xcode, macOS or Cucina release (R-VER-3)

1. New Xcode: add `xcode."<ver>"` to `versions.json` (build, base image + digest after `tart pull`) and
   `packer/xcode-<ver>.pkrvars.hcl`; new macOS: a `macosReleases` entry (Cirrus release name).
2. `make -C workers/macos image-macos XCODE=<ver> CUCINA_VERSION=<semver>` (build + smoke test).
3. Publish (release workflow or `make push`, §6), then bump the pool: a new `WorkerPool` (new `xcode-version` pool, kept
   side by side with the old one, R-VER-2) or a new image reference on the existing pool (generation bump, R-OPS-2).
4. Hosts must run a macOS version **≥ the guest's** (Virtualization.framework cannot boot newer guests): update hosts
   through MDM first (docs/macos/mac-mini-setup.md), then roll out the image.

Beta Xcode/macOS images are built the same way with a distinct tag and used only by an opt-in pool (R-VER-2 SHOULD).

## 5. Disk budget on hosts (R-DATA-5)

| Item | Size (this campaign, measured) |
| --- | --- |
| Base image `macos-golden-gate-xcode:27` in the OCI cache | see the campaign report |
| Worker image (local VM, sparse disk) | see the campaign report |
| Per VM on top of the image (clone = copy-on-write) | L1 up to 40 GiB (default) + native build-dir input cache (≤ 16 GiB) + file pool high-water mark + build directories |
| Host L2 cache (hostd) | 200 GiB default (R-CACHE-4) |

* Tart clones are APFS copy-on-write: a VM costs only what it writes. On macOS 27 hosts hostd clones with
  `tart clone --stacked` from a pulled OCI image (immutable base + writable overlay); images built locally (the
  campaign) are cloned without `--stacked`.
* Several images side by side (R-VER-2) share nothing at the file level, but **Tart's chunked disk layers** let a
  host that already holds one version download only the changed chunks of the next one (same for the registry on
  push). Keep image changes small and in one place (the Cucina payload) to keep deltas small.
* Hostd keeps the cache inside a budget with `tart prune --space-budget=<n>GB` (R-MAC-5); budget at least
  2 images + 2 VMs (≈ 2 x image + 2 x 70 GB) + L2 on a 1 TB Mac mini.

## 6. Publishing (private GHCR package, R-OPS-7)

* Package: `ghcr.io/sloper-ai/cucina-worker-macos` (**private**: Xcode may not be redistributed publicly).
* `CUCINA_PUBLISH=1 TART_REGISTRY_USERNAME=… TART_REGISTRY_PASSWORD=… make -C workers/macos push XCODE=27.0` runs
  `scripts/push.sh` (`tart push` with the version labels). Credentials come from the environment only (a token with
  `write:packages`), never from files or the keychain.
* Hosts pull with a **read-only** package credential that the controller holds and hands to hostd over its mTLS channel
  at pull time (`TART_REGISTRY_*` environment variables for that `tart pull` only). It is never in images, profiles
  or the pkg. Site mirrors (zot) sync from the same package.
* GHCR storage and transfer are currently free for private packages; GitHub may change this with a month's notice.
* **Nothing is published during the acceptance campaign**: the campaign uses the locally built image; `push.sh`
  refuses to run without `CUCINA_PUBLISH=1`.

## 7. Security notes

* Build time: the Cirrus base has `admin`/`admin` with passwordless sudo, SSH and Screen Sharing on. These defaults
  are usable only while Packer builds the image, and only from the build Mac (Tart's shared NAT network). The build
  turns SSH password logins off, disables Screen Sharing and replaces `admin`'s password with an unrecorded random
  value; hostd needs no password (`tart exec` runs as root through the Tart Guest Agent daemon).
* Actions run as the standard user `builder` (R-SEC-5); bb_worker runs as root (`image.json` `workerUser`), so the
  worker key hostd pushes at each start (`/etc/cucina/pki`, 0700 root), the L1 and the file pool are out of the
  actions' reach. A VM is still shared by the actions of one pool over its lifetime (persistent VMs, ≤ 7 days), so a
  pool remains the trust boundary ("one pool per trust level").
* Rosetta is present in the Cirrus base (SIP-protected, cannot be removed); no x86_64 runner is advertised and macOS
  x86_64 is out of scope.

## 8. Troubleshooting

| Symptom | Check |
| --- | --- |
| `tart exec` never answers | `tart run` log; the guest agent daemon: `launchctl print system/org.cirruslabs.tart-guest-daemon` (via VNC/recovery) |
| `builder` not logged in after boot | `defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser`; `/etc/kcpassword` present; FileVault must be off in the guest |
| `/Volumes/cucina` missing | `diskutil apfs list`; the volume is in the boot container and mounts at boot |
| `xcodebuild` asks for the licence | image built without `xcodebuild -license accept`; rebuild (the verify step checks it) |
| Wrong Xcode build in a pool | `tart exec <vm> cat /usr/local/cucina/image.json`; the pool's `xcode-version` must equal `xcode.version` |
| Smoke test details | `tart exec <vm> sudo -n -- /usr/local/cucina/libexec/cucina-smoke` |
