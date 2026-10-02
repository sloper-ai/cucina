<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: a new Xcode, Visual Studio or OS release

**Use when** Apple ships a new Xcode or macOS, Microsoft ships a new Visual Studio Build Tools, MSVC or Windows SDK, or a new Ubuntu LTS appears, and you want builds to use it. Adopting a new release is meant to be **routine: one scripted image build, a smoke test, and a generation bump**, with no code changes. Several versions
(for example two Xcode versions) run side by side as separate pools. **Severity:** Planned. **Time:** an image build (macOS: an hour or two; Windows: over an hour) plus a rollout.

## The shape of every adoption

1. Build a new image version with one command; it builds, smoke-tests (a tiny remote build) and reports.
2. Publish it where the pool can use it (an AMI in your account, or a Tart image in the private registry package).
3. Add a pool for it, or bump the existing pool's image (a generation bump), following the matching rollout runbook.
4. Verify with a real build, keep the old version until nobody needs it, then retire it.

## macOS and Xcode

Hosts must run a macOS version **at least as new as their guests**, so a new guest macOS needs host updates first ([macOS update](macos-update.md)).

1. **Find where the Xcode comes from.** Preferably a Cirrus Labs `ghcr.io/cirruslabs/macos-<release>-xcode:<version>` image when one exists. Otherwise it comes from a `.xip` downloaded from Apple Developer, which needs **the operator's Apple ID** at download time. Do that step by hand; never store the Apple ID, a password, an app-specific password or a session in the repository, in CI variables or in an image. Xcode's
   licence forbids redistribution outside your organisation, so images with Xcode go only to the **private** registry package.
2. **Describe the version.** Add an `xcode` entry (and its base image digest after `tart pull`) to `workers/macos/versions.json` and a variables file `workers/macos/packer/xcode-<version>.pkrvars.hcl`; a new macOS adds a release entry. Details and the exact keys: [macOS images](macos-images.md), section "A new Xcode, macOS or Cucina release".
3. **Build, smoke-test and report:**

   ```sh
   make -C workers/macos image-macos XCODE=<version> CUCINA_VERSION=<semver>
   ```

   Each image carries exactly one Xcode and two runners (an Xcode runner with `xcode-version`, and a generic arm64 runner). The smoke test checks `xcodebuild -version`, the SDK path, the Buildbarn binaries, the case-sensitive build volume, Spotlight and the guest agent.
4. **Publish** to the private package. Publishing is deliberately guarded: the push names the local image from the same `XCODE` and `CUCINA_VERSION` as the build (the defaults would push a different image), it refuses to run without `CUCINA_PUBLISH=1`, and the package credential comes from the environment only (hosts later pull with a separate read-only credential delivered at pull time):

   ```sh
   read -rs TART_REGISTRY_PASSWORD && export TART_REGISTRY_PASSWORD      # a token with write access to the package: never in a file, a variable file or the shell history
   export TART_REGISTRY_USERNAME=<user> CUCINA_PUBLISH=1
   make -C workers/macos push XCODE=<version> CUCINA_VERSION=<semver>
   ```
5. **Add a pool next to the old one.** Add the new platform (it differs only in `xcode-version`) to the chart values (`platforms.extra`) and a `WorkerPool` for it, then `helm upgrade`. **Adding a platform adds a queue, which restarts the scheduler once** ([ADR 0002](../adr/0002-queue-declaration-from-values.md)); clients ride it out with retries.
   To move an existing pool instead (no new platform), change its image reference: that is a generation bump with no scheduler restart ([AMI rollout and rollback](ami-rollout-rollback.md) describes the logic).
6. **Point clients at it.** `cucinactl bazelrc --platform macos --xcode <version>` prints the configuration that selects that Xcode.
7. **Verify with a real build** that selects the new Xcode and prints `xcodebuild -version` from inside an action; confirm the old pool still serves the old version. Hosts pre-pull the new image during idle hours; the first VM clones it ([MT-005](../testing/manual/MT-005.md) is the full check).
8. **Retire the old version** when nobody uses it: set its pool's `max` to 0, let it idle out, delete the pool, remove its platform from the values (another scheduler restart), and `tart prune` reclaims host disk.

Beta Xcode or macOS images are built the same way under a distinct tag and used only by an opt-in, isolated pool.

## Windows and Visual Studio

1. **Update the pins.** Visual Studio Build Tools, the MSVC toolset and the Windows SDK are pinned (`workers/windows/versions.json` and the shared `workers/windows/scripts/install-vs.ps1`), identically for the worker image and the Windows client image, so paths and versions match. Bump the pin; do not float.
2. **Build the base layer, then the worker layer:**

   ```sh
   make -C workers/windows image-windows STAGE=base
   make -C workers/windows verify-bazel          # Bazel's MSVC autodetection must work with the new toolset
   make -C workers/windows image-windows STAGE=worker FAST_LAUNCH=1
   ```

   If Bazel cannot autodetect the new Visual Studio even with the `BAZEL_VC`, `BAZEL_VC_FULL_VERSION` and `BAZEL_WINSDK_FULL_VERSION` pins, stay on the previous Visual Studio and write an ADR.
3. **Roll it out** with the AMI procedure: enable Fast Launch on the new AMI, then change the pool's image ([AMI rollout and rollback](ami-rollout-rollback.md)). Update the clients' pins to the same versions (the generated Windows configuration carries them).
4. **Two toolchain versions at once** need an extra platform property (for example `msvc-version`) and a pool each; add it only when more than one version really runs concurrently (`platforms.extra`, then `helm upgrade`).

## Linux and a new Ubuntu LTS

1. Change the release or variant in the Packer variables and build both architectures:

   ```sh
   make -C workers/linux image-linux ARCH=x86_64 VARIANT=ubuntu
   make -C workers/linux image-linux ARCH=arm64 VARIANT=ubuntu
   ```

   `VARIANT=al2023` builds the Amazon Linux 2023 alternative; `make -C workers/linux measure-boot` measures boot to ready, which is what cold start depends on.
2. The x86_64 image also carries the qemu-user runtimes for riscv64, s390x and armv7 (the image build smoke-tests them); confirm they still work on the new base.
3. Roll out with [AMI rollout and rollback](ami-rollout-rollback.md).

## Verify

* The new image's smoke test passed; `cucinactl images` lists the new version and generation for the pool.
* A real build on the new version succeeded, and the old version still works.
* Cold-start numbers are still within target (Linux about a minute; Windows with Fast Launch about two minutes; macOS under a minute and a half).

## Roll back

Point the pool at the previous image or remove the new pool; nothing about the old version was changed ([AMI rollout and rollback](ami-rollout-rollback.md), [Xcode](macos-images.md)).

## Escalate

Attach the image build log summary, the smoke-test output, `cucinactl images`, and the versions involved.
