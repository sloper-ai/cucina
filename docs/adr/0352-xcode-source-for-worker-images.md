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

Rejected: automating the Apple ID login inside the build VM. An operator-supplied, already expanded bundle avoids
handling Apple credentials and avoids holding both the `.xip` and the expanded Xcode in the guest. Copy-path speed
has not been benchmarked; the campaign uses the exact matching Xcode already in the Cirrus base.

## Consequences
* A copied Xcode rewrites ~10 GB of the guest disk, so the first push/pull of such an image moves that much more than
  the Cirrus base; later Cucina-only versions on the same Xcode reuse the chunks (R-DATA-5).
* The base's simulator runtimes stay (they are independent of the Xcode bundle); images do not add more.
* Campaign outcome (base `ghcr.io/cirruslabs/macos-golden-gate-xcode:27`): recorded in the campaign notes below.

## Campaign notes
* Base digest `sha256:324ea5656dee8ab9b0a0df70fda2cfed8912051ad0eca3b1b883bf6ddac88fab` was inspected in a disposable Tart clone: macOS 27.0 (`26A428`), exactly one Xcode 27.0 (`27A266a`) at `/Applications/Xcode_27.app`.
* Keep that matching bundle and move it to `/Applications/Xcode.app`; no host Xcode transfer or shared directory is needed for this build.
* Red-first image build caught an overly strict SDK-path test: Cirrus' `xcrun` returns `MacOSX27.0.sdk`, with `MacOSX.sdk` resolving to that directory. The fixed toolchain path is unchanged; provisioning and smoke now compare canonical directories and require SDK headers, instead of asserting identical path spelling.
