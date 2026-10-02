# SPDX-License-Identifier: FSL-1.1-ALv2
"""Multi-arch OCI images built with rules_img, without Docker (R-BUILD-3, ADR 0110).

`cucina_go_image` preserves the public image target and its single OCI-layout
TreeArtifact at bazel-bin/<pkg>/<name>. The index, manifests and layers are built
by rules_img. Its official exporter materializes every blob; a native, offline
helper retains the historical single-index envelope without changing those bytes.
No shell, registry write or custom image format is involved.

Targets: :<name> (complete layout), :<name>_image (single-platform provider),
:<name>_layer and :<name>_doc_layer (native layers), and optional :<name>_push and
:<name>_load. Push targets never publish during `bazel build`.
"""

load("@bazel_lib//lib:transitions.bzl", "platform_transition_filegroup")
load("@rules_img//img:image.bzl", "image_index", "image_manifest")
load("@rules_img//img:layer.bzl", "file_metadata", "image_layer")
load("@rules_img//img:load.bzl", "image_load")
load("@rules_img//img:providers.bzl", "ImageIndexInfo")
load("@rules_img//img:push.bzl", "image_push")

LINUX_IMAGE_PLATFORMS = [
    "@llvm//platforms:linux_x86_64",
    "@llvm//platforms:linux_aarch64",
]

_IMG_TOOLCHAIN = "@rules_img//img:toolchain_type"
_EPOCH = "1970-01-01T00:00:00Z"

def _image_layout_impl(ctx):
    image = ctx.attr.image
    out = ctx.actions.declare_directory(ctx.label.name)
    flat = ctx.actions.declare_directory(ctx.label.name + "_flat_oci")
    args = ctx.actions.args()
    args.add("oci-layout")
    args.add("--format", "directory")
    args.add("--output", flat.path)
    info = image[ImageIndexInfo]
    args.add("--index", info.index.path)
    inputs = [info.index]
    for manifest in info.manifests:
        args.add("--manifest-path", manifest.manifest.path)
        args.add("--config-path", manifest.config.path)
        inputs.extend([manifest.manifest, manifest.config])
        for layer in manifest.layers:
            if layer.blob == None:
                fail("%s: OCI export requires eager base layers, not a shallow image" % ctx.label)
            args.add("--layer", "%s=%s" % (layer.metadata.path, layer.blob.path))
            inputs.extend([layer.metadata, layer.blob])
    ctx.actions.run(
        executable = ctx.toolchains[_IMG_TOOLCHAIN].imgtoolchaininfo.tool_exe,
        arguments = [args],
        inputs = depset(inputs),
        outputs = [flat],
        env = {"RULES_IMG": "1"},
        mnemonic = "CucinaOCIExport",
        progress_message = "Exporting complete OCI index %{label}",
        toolchain = _IMG_TOOLCHAIN,
    )
    wrap = ctx.actions.args()
    wrap.add_all(["wrap-oci", "--layout", flat.path, "--out", out.path])
    ctx.actions.run(
        executable = ctx.executable._layout_tool,
        arguments = [wrap],
        inputs = [flat],
        outputs = [out],
        mnemonic = "CucinaOCILayout",
        progress_message = "Preserving OCI layout interface %{label}",
    )
    return [
        DefaultInfo(files = depset([out]), runfiles = ctx.runfiles([out])),
        info,
    ]

cucina_image_layout = rule(
    implementation = _image_layout_impl,
    doc = "Export a rules_img image as one complete, self-contained OCI-layout directory.",
    attrs = {
        "image": attr.label(mandatory = True, providers = [ImageIndexInfo]),
        "_layout_tool": attr.label(default = Label("//bazel/release/tool"), executable = True, cfg = "exec"),
    },
    toolchains = [_IMG_TOOLCHAIN],
)

def cucina_go_image(
        name,
        binary,
        repository = None,
        remote_tags = None,
        base = "@distroless_static",
        platforms = LINUX_IMAGE_PLATFORMS,
        visibility = None,
        tags = None,
        docs = None,
        label_files = None,
        created = None,
        load_tags_file = None):
    """Wrap a pure-Go executable and licence documents in a reproducible OCI index.

    Args:
      name: public image target and output directory name.
      binary: go_binary label; must build with pure = "on".
      repository: optional registry/repository for :<name>_push.
      remote_tags: tags for an explicit push (default ["latest"]).
      base: digest-pinned, eagerly pulled rules_img base image.
      platforms: target platforms of the image index.
      visibility: visibility of the public layout and optional push/load targets.
      tags: tags propagated to all generated targets.
      docs: licence files (default: repository LICENSE.md and THIRD_PARTY_NOTICES.md).
      label_files: release metadata files in KEY=VALUE form.
      created: optional RFC3339 timestamp file from release buildinfo.
      load_tags_file: optional full-reference tag file; creates :<name>_load for the first platform.
    """
    label = native.package_relative_label(binary)
    executable = "/usr/local/bin/" + label.name
    image_layer(
        name = name + "_layer",
        srcs = {executable: binary},
        include_runfiles = False,
        compress = "gzip",
        default_metadata = file_metadata(uid = 0, gid = 0, mtime = _EPOCH),
        file_metadata = {executable: file_metadata(mode = "0755")},
        tags = tags,
        visibility = ["//visibility:private"],
    )
    documents = docs if docs != None else [Label("//:LICENSE.md"), Label("//:THIRD_PARTY_NOTICES.md")]
    doc_files = {
        "/usr/share/doc/cucina/" + native.package_relative_label(doc).name: doc
        for doc in documents
    }
    image_layer(
        name = name + "_doc_layer",
        srcs = doc_files,
        include_runfiles = False,
        compress = "gzip",
        default_metadata = file_metadata(uid = 0, gid = 0, mtime = _EPOCH),
        file_metadata = {path: file_metadata(mode = "0644") for path in doc_files},
        tags = tags,
        visibility = ["//visibility:private"],
    )
    image_manifest(
        name = name + "_image",
        base = base,
        user = "65532",
        entrypoint = [executable],
        cmd = [],
        layers = [":" + name + "_layer", ":" + name + "_doc_layer"],
        label_files = label_files or [],
        created = created,
        stamp = "disabled",
        stamp_created = "disabled",
        tags = tags,
        visibility = ["//visibility:private"],
    )
    image_index(
        name = name + "_index",
        manifests = [":" + name + "_image"],
        platforms = platforms,
        stamp = "disabled",
        tags = tags,
        visibility = ["//visibility:private"],
    )
    cucina_image_layout(
        name = name,
        image = ":" + name + "_index",
        tags = tags,
        visibility = visibility,
    )
    if repository:
        registry, separator, path = repository.partition("/")
        if not separator or not path:
            fail("repository must be a registry-qualified path, got %r" % repository)
        image_push(
            name = name + "_push",
            image = ":" + name + "_index",
            registry = registry,
            repository = path,
            tag_list = remote_tags if remote_tags != None else ["latest"],
            strategy = "eager",
            push_at_build_time = "disabled",
            tags = tags,
            visibility = visibility,
        )
    if load_tags_file:
        image_index(
            name = name + "_first_platform",
            manifests = [":" + name + "_image"],
            platforms = [platforms[0]],
            stamp = "disabled",
            tags = tags,
            visibility = ["//visibility:private"],
        )
        platform_transition_filegroup(
            name = name + "_load_tags",
            srcs = [load_tags_file],
            target_platform = platforms[0],
            tags = tags,
            visibility = ["//visibility:private"],
        )
        image_load(
            name = name + "_load",
            image = ":" + name + "_first_platform",
            tag_file = ":" + name + "_load_tags",
            strategy = "eager",
            tags = tags,
            visibility = visibility,
        )
