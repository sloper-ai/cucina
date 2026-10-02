<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0352 — Where a worker image's Xcode comes from

* Status: accepted (2026-10-02)

## Context
Each macOS worker image carries exactly one Xcode whose version is the pool's `xcode-version` (R-MAC-7), at fixed
paths that the exec-side SDK toolchain relies on (R-XPLAT-8). Action keys of macOS-target actions contain the
toolchain paths and, through Bazel's `XCODE_VERSION_OVERRIDE`, the exact Xcode build, so mixing builds across
clients and workers costs cache hits or correctness. For the acceptance campaign the image must carry the dev Mac's
Xcode 27.0 **27A266a** exactly. Cirrus Labs publishes `macos-<release>-xcode:<tag>` images built with `xcodes` from
Apple's downloads; a tag can carry several Xcodes and may lag or lead Apple's builds. Fetching Xcode from Apple needs
an Apple ID (developer.apple.com downloads), which must never be stored by Cucina.

## Decision
1. Prefer the Cirrus `-xcode:<tag>` image when it contains the pinned build (`versions.json`: `source: cirrus`); the
   build keeps that bundle (moved to `/Applications/Xcode.app`) and deletes every other Xcode and the Command Line Tools.
2. Otherwise copy an operator-provided `Xcode.app` with the pinned build (`source: local-xcode-app`): the Packer build
   shares it read-only into the build VM (`--dir=cucina-xcode:<path>:ro`, build time only) and `ditto`s it to
   `/Applications/Xcode.app`, then `xcode-select -s`, `xcodebuild -license accept`, `xcodebuild -runFirstLaunch`, and
   verifies `xcodebuild -version` and the SDK path. Obtaining that `Xcode.app` (download of the `.xip` with the user's
   Apple ID, expansion) is a documented manual step (docs/operations/macos-images.md §3); no Apple credentials enter
   the build, the repository or the image.
3. The build fails, rather than silently using another build, when neither source has the pinned build.

Rejected: downloading Xcode inside the build VM (`xcodes`/`xcodebuild -downloadPlatform` need Apple ID credentials in the
build); uploading a `.xip` with Packer's file provisioner and expanding it in the guest (~3x slower than copying the
expanded bundle over virtiofs, and needs twice the guest disk during the build).

## Consequences
* A copied Xcode rewrites ~10 GB of the guest disk, so the first push/pull of such an image moves that much more than
  the Cirrus base; later Cucina-only versions on the same Xcode reuse the chunks (R-DATA-5).
* The base's simulator runtimes stay (they are independent of the Xcode bundle); images do not add more.
* Campaign outcome (base `ghcr.io/cirruslabs/macos-golden-gate-xcode:27`): recorded in the campaign notes below.

## Campaign notes
* Filled in at build time: the Xcode build(s) found in the base and which source the image used.
