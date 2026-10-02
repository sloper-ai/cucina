# SPDX-License-Identifier: FSL-1.1-ALv2
"""Multi-arch OCI images for pure-Go binaries, built without Docker (R-BUILD-1, R-BUILD-3).

    load("//bazel:oci.bzl", "cucina_go_image")

    cucina_go_image(
        name = "image",
        binary = "//cmd/cucina-controller",
        repository = "ghcr.io/sloper-ai/cucina-controller",
    )

Targets:
  :<name>        oci_image_index for linux/amd64 + linux/arm64 (an OCI image layout
                 directory: bazel-bin/<pkg>/<name>/{oci-layout,index.json,blobs/})
  :<name>_image  the single-platform image (built once per index platform)
  :<name>_layer  the binary layer: /usr/local/bin/<binary name>, root:root 0755
  :<name>_doc_layer  LICENSE.md + THIRD_PARTY_NOTICES.md in /usr/share/doc/cucina
  :<name>_push   oci_push to `repository` (only when `repository` is set; never
                 run from tests; credentials come from the registry's docker config)

The base is gcr.io/distroless/static-debian13:nonroot pinned by digest in
MODULE.bazel (`@distroless_static`); the binary must be pure Go (`pure = "on"`).
"""

load("@rules_oci//oci:defs.bzl", "oci_image", "oci_image_index", "oci_push")
load("@tar.bzl", "mutate", "tar")

# Target platforms of the index. TODO(cross-platform agent): switch to the
# @cucina_platforms Linux targets once bazel/platforms exists.
LINUX_IMAGE_PLATFORMS = [
    "@llvm//platforms:linux_x86_64",
    "@llvm//platforms:linux_aarch64",
]

def cucina_go_image(
        name,
        binary,
        repository = None,
        remote_tags = None,
        base = "@distroless_static",
        platforms = LINUX_IMAGE_PLATFORMS,
        visibility = None,
        tags = None):
    """Wraps a pure-Go `go_binary` into a linux/amd64 + linux/arm64 image index.

    Args:
      name: name of the oci_image_index.
      binary: the go_binary label (set `pure = "on"`).
      repository: registry repository for `:<name>_push` (omit for no push target).
      remote_tags: tags applied by `:<name>_push` (default ["latest"]).
      base: base image (an `oci.pull` with linux/amd64 + linux/arm64).
      platforms: target platforms of the index.
      visibility: visibility of the index and push targets.
      tags: tags for all generated targets.
    """
    label = native.package_relative_label(binary)
    tar(
        name = name + "_layer",
        srcs = [binary],
        # A pure-Go binary needs no runfiles tree (it would duplicate the binary).
        include_runfiles = False,
        # rules_go places the executable at <package>/<name>_/<name>.
        mutate = mutate(
            strip_prefix = "%s/%s_" % (label.package, label.name) if label.package else "%s_" % label.name,
            package_dir = "usr/local/bin",
            owner = "0",
            ownername = "root",
            tags = tags,
        ),
        tags = tags,
        visibility = ["//visibility:private"],
    )
    tar(
        name = name + "_doc_layer",
        srcs = [
            Label("//:LICENSE.md"),
            Label("//:THIRD_PARTY_NOTICES.md"),
        ],
        mutate = mutate(
            package_dir = "usr/share/doc/cucina",
            owner = "0",
            ownername = "root",
            tags = tags,
        ),
        tags = tags,
        visibility = ["//visibility:private"],
    )
    oci_image(
        name = name + "_image",
        base = base,
        entrypoint = ["/usr/local/bin/" + label.name],
        tars = [":" + name + "_layer", ":" + name + "_doc_layer"],
        tags = tags,
        visibility = ["//visibility:private"],
    )
    oci_image_index(
        name = name,
        images = [":" + name + "_image"],
        platforms = platforms,
        tags = tags,
        visibility = visibility,
    )
    if repository:
        oci_push(
            name = name + "_push",
            image = ":" + name,
            remote_tags = remote_tags or ["latest"],
            repository = repository,
            tags = tags,
            visibility = visibility,
        )
