<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# macOS worker image

Packer + `packer-plugin-tart` **1.21.0** turn a digest-pinned Cirrus macOS/Xcode image into
`cucina-worker-macos:<xcode>-<cucina_version>` in `TART_HOME`. Nothing is published by the build.
The guest contract is [hostd §1](../../docs/dev/hostd.md); the [operations guide](../../docs/operations/macos-images.md)
covers versions, disk budgets, credentials and private GHCR publishing.

```sh
make -C workers/macos image-macos XCODE=27.0   # build -> smoke/three boots -> report
make -C workers/macos validate               # Packer fmt/validate + shellcheck
# Delete only explicitly named clones you created; never a fleet-wide prefix sweep:
make -C workers/macos clean-test-vms VMS='cucina-imgtest-<owned-name>'
```

| Path | Purpose |
| --- | --- |
| `versions.json` | Base digest, Xcode build, Buildbarn release/checksums and account layout. |
| `packer/` | Plugin/source template, variables and per-Xcode vars files. |
| `provision/` | Guest provisioning: Xcode, system settings, standard build user, APFS, payload, reboot, finalization and smoke. |
| `files/` | launchd, SSH and newsyslog configuration; `cucina-render`. |
| `scripts/` | Verified downloads, in-guest smoke, host-side boot/render checks and guarded private publishing. |
| `bench/` | Real remote C++ build/test comparison of native and NFSv4 build directories (ADR 0351). |

## Payload and contract

* **Xcode 27.0 (27A266a)** is already in the pinned Golden Gate base. The build moves the matching bundle to
  `/Applications/Xcode.app`, removes other Xcodes and Command Line Tools, accepts the licence and completes first launch.
  A caller-supplied `XCODE_APP` is the fallback for a base without the pinned build (ADR 0352).
* Fixed exec-side paths (R-XPLAT-8):
  * developer directory: `/Applications/Xcode.app/Contents/Developer`;
  * SDK: `/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk`;
  * the generic SDK alias and `xcrun`'s versioned path must resolve to the same directory. Their spelling may differ.
  `image.json` also records `xcodeVersionOverride`, e.g. `27.0.0.27A266a`, for bb_runner's Xcode mapping.
* **Buildbarn** `bb_worker` and `bb_runner` are the unmodified darwin/arm64 release
  `20260930T173749Z-1a3be95`, checked against both the release `sha256` asset and the repository pins, then checked
  again in the guest. `cucina-worker-agent` is required and SHA-256 checked. All three live in `/usr/local/cucina/bin`.
  `LICENSE.md` and `THIRD_PARTY_NOTICES.md` are installed in `/usr/local/cucina`.
* **Accounts and launchd** (ADR 0350): `builder` (UID 600) is a standard, non-admin account with no sudo, auto-login
  and a usable login keychain. bb_runner runs in `gui/600`; bb_worker runs as root (`workerUser` in the manifest).
  Root owns `/var/log/cucina` (0755); worker/runner logs are owned by their respective users (0644), including rotation.
  The plists live in `/usr/local/cucina/launchd`, **not** `/Library/Launch*`: hostd bootstraps them only after injection.
  Labels are `ai.sloper.cucina.bb-worker` and `ai.sloper.cucina.bb-runner`; the socket is `/var/run/cucina/runner.sock`.
* **Storage:** case-sensitive APFS `cucina` at `/Volumes/cucina` (root-owned), with build directories and the native
  input cache. Persistent **40 GiB L1** and file pool live under `/var/db/cucina` (root 0700), never inside the native
  input cache that bb_worker wipes at startup. The virtual disk is **250 GB**; hosts must not request a smaller clone.
* **Boot:** the Tart Guest Agent RPC runs in its root daemon (`--run-daemon --run-rpc`), independent of GUI login.
  Spotlight and guest Software Update are disabled; no sleep, Screen Sharing or SSH password authentication.
  Inherited Rosetta payload and supplemental receipts are removed. The Cirrus admin password is rotated after setup.
* **Version:** `/usr/local/cucina/image.json` (schema 1) and `/etc/cucina/image-version` record the image tag.
  A new image reference causes a pool generation rollout; pushing adds OCI version labels (not exercised here).

No Cucina identity, registry token or controller credential is baked in (`/etc/cucina/pki` is empty). Auto-login
necessarily retains the local account credential in macOS's reversible `/etc/kcpassword` format, root 0600.
The shared pkg codec is used **at build time only** to verify the supported GUI-context `sysadminctl` operation.

## Runners and concurrency

One bb_worker advertises two platforms: `xcode` = `{OSFamily: macos, ISA: arm-a64, xcode-version: <ver>}` and
`generic` = `{OSFamily: macos, ISA: arm-a64}`. Each offers **vCPU slots**, sharing the same CPUs and runner process
(ADR 0353; unchanged `platforms/pools.json` policy). Mixed workloads can oversubscribe CPUs/memory; lower factors for
memory-heavy pools. The image supplies layout and tools, not runtime endpoint/credential configurations.

`cucina-render` accepts WorkerSettings protojson on stdin, derives the current machine document, reads the injected
CA bundle and calls `cucina-worker-agent render --settings FILE --machine FILE --out DIR`. It creates the returned
plan's directories and writes `worker.json`, `runner.json`, and `env`; hostd normally performs rendering on the host.
