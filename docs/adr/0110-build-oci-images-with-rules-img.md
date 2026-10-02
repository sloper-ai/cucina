<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0110 — Build OCI images with rules_img

* Status: accepted (2026-10-02); supersedes ADR 0100's container-builder choice

## Context

The user explicitly selected **rules_img**, overriding the earlier rules_oci preference in
R-BUILD-1. This changes the builder, not the OCI format or the artifact contracts. Existing
release, chart and offline upload tools depend on a complete OCI layout at public image targets,
with a single image-index descriptor in `index.json`, linux/amd64 and linux/arm64 manifests,
nonroot execution, exact licence documents and stable digest/provenance handling.

## Decision

* Pin **rules_img 0.3.22**, the latest official release and BCR version checked on 2026-10-02.
  The [official release](https://github.com/bazel-contrib/rules_img/releases/tag/v0.3.22) was
  published on 2026-09-23. Its source archive SHA-256 is
  `35ab7cf2ddef71329dacc0b7879c8beebcc05a9f4c56916a1f256f86fa6ed879`, matching the BCR integrity.
  Use the documented native `image_layer`, `image_manifest`, `image_index` and exporter APIs.
* Keep the existing distroless static-debian13 nonroot index digest. Pull its two required
  platforms eagerly, so every layer is available before build actions and exports are complete.
* Keep `//cmd/cucina-controller:image`, release image targets and their output directory names.
  The native exporter emits the rules_img index and blobs; a small offline Go `wrap-oci` adapter
  adds the historical single-index envelope. It never changes the index bytes or any payload
  digest. It materializes sandbox input symlinks into ordinary files, validating every blob hash.
* Keep image user `65532`, the executable entrypoint and an empty default command. Native gzip
  layers carry root-owned binaries at mode 0755, documents at 0644, and fixed epoch timestamps.
  Both architectures include exact `LICENSE.md` and `THIRD_PARTY_NOTICES.md` bytes.
* Release metadata still comes from one buildinfo input: version, revision, dirty-source status,
  commit time, base-platform manifest digest and tags. The native metadata tool also accepts a
  raw base manifest while retaining its existing layout input. Existing release assertions stay.
* Preserve `//release:controller_image_load` and its single-amd64 `tarball` output group for kind.
  Building a load target does not invoke a daemon. Explicit push targets disable build-time
  publication, regardless of global defaults.
* Remove the rules_oci dependency, lock references and compatibility patches. Containers no
  longer use tar.bzl/gawk. Retain their pins/Windows adapter only for hermetic-llvm's independent
  `prebuilt/llvm/llvm_release.bzl` and `prebuilt/extras/extra_bins_release.bzl` packaging rules.

## Verification and consequences

The previous release verification passed before migration. New public-CLI compatibility tests
first failed for the missing manifest/envelope inputs, then reproduced the immutable sandbox
symlink failure. They now preserve digest identity and leave original inputs untouched.

Verified commands (using a private output base, no publishing):

```sh
bazelisk build --config=ci //tools/hello/image:hello //cmd/cucina-controller:image //release:all //release:controller_image_load --output_groups=+tarball
bazelisk test --config=ci //bazel/release/tool:files_test //bazel/release/tool:tool_test //release:verify_test --runs_per_test=20 --nocache_test_results
```

All three release/tool test targets passed 20 uncached runs. Direct inspection of both canonical
images verified their two architectures, ELF payloads, ownership/modes/timestamps, exact licence
bytes, nonroot config, every blob hash and the nested index digest. No external symlinks remain.
An independent output base with the disk action cache disabled rebuilt both canonical images in
164 seconds (3,729 actions) and reproduced both index digests exactly. The Windows execution
graph also passed analysis for canonical images and the kind archive. Hosted native Windows
execution remains required; cross-build and analysis results are not execution evidence.

New builder serialization may change image digests; consumers must pin the newly verified digest.
The current deployment is not changed by this migration. No image is pushed or deployed as part
of these checks, and no assertion, test platform or timeout is relaxed.
