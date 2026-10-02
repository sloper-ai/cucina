# SPDX-License-Identifier: FSL-1.1-ALv2
"""Rules for Cucina's release artifacts (R-BUILD-3, R-OPS-7; ADR 0150, ADR 0151).

The release graph lives in //release (`bazel build //release:all`). Every rule here gets the
version from one `cucina_buildinfo` target, which reads Bazel's stable workspace status in
stamped builds (`--stamp --workspace_status_command=release/workspace-status.sh`) and falls
back to DEV_VERSION otherwise. The heavy lifting is done by //bazel/release/tool.
"""

load("@bazel_lib//lib:stamping.bzl", "STAMP_ATTRS", "maybe_stamp")
load("@bazel_lib//lib:transitions.bzl", "platform_transition_filegroup")
load("@rules_oci//oci:defs.bzl", "oci_image", "oci_image_index", "oci_load")
load("@tar.bzl", "mutate", "tar")

CucinaBuildInfo = provider(
    doc = "The release version information of a build.",
    fields = {
        "json": "File: build info JSON (tool input)",
        "env": "File: shell-sourceable VERSION/CORE/COMMIT/EPOCH/REPOSITORY/RELEASE_URL",
    },
)

_TOOL = attr.label(default = Label("//bazel/release/tool"), executable = True, cfg = "exec")
_BUILDINFO = attr.label(default = Label("//release:buildinfo"), providers = [CucinaBuildInfo])

# Linux platforms of the multi-arch images (same as //bazel:oci.bzl).
LINUX_IMAGE_PLATFORMS = [
    Label("@llvm//platforms:linux_x86_64"),
    Label("@llvm//platforms:linux_aarch64"),
]

# --- cucina_buildinfo ---------------------------------------------------------------------

def _buildinfo_impl(ctx):
    json = ctx.actions.declare_file(ctx.label.name + ".json")
    env = ctx.actions.declare_file(ctx.label.name + ".env")
    args = ctx.actions.args()
    args.add("buildinfo")
    inputs = []
    stamp = maybe_stamp(ctx)
    if stamp:
        args.add("--stable-status", stamp.stable_status_file)
        inputs.append(stamp.stable_status_file)
    args.add("--out-json", json)
    args.add("--out-env", env)
    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = inputs,
        outputs = [json, env],
        mnemonic = "CucinaBuildInfo",
        progress_message = "Resolving the release version (%{label})",
    )
    return [
        DefaultInfo(files = depset([json, env])),
        CucinaBuildInfo(json = json, env = env),
        OutputGroupInfo(json = depset([json]), env = depset([env])),
    ]

cucina_buildinfo = rule(
    implementation = _buildinfo_impl,
    doc = "The release version: STABLE_CUCINA_* of a stamped build, else the dev version.",
    attrs = dict({"_tool": _TOOL}, **STAMP_ATTRS),
)

# --- release_binary: one binary built for one platform ------------------------------------

_PLATFORMS = "//command_line_option:platforms"
_GO_PURE = str(Label("@rules_go//go/config:pure"))

def _release_transition_impl(settings, attr):
    return {
        _PLATFORMS: [str(attr.platform)] if attr.platform else settings[_PLATFORMS],
        _GO_PURE: False if attr.cgo else settings[_GO_PURE],
    }

_release_transition = transition(
    implementation = _release_transition_impl,
    inputs = [_PLATFORMS, _GO_PURE],
    outputs = [_PLATFORMS, _GO_PURE],
)

def _release_binary_impl(ctx):
    dep = ctx.attr.binary[0] if type(ctx.attr.binary) == "list" else ctx.attr.binary
    exe = dep[DefaultInfo].files_to_run.executable
    if not exe:
        fail("%s is not executable" % ctx.attr.binary)
    out = ctx.actions.declare_file(ctx.attr.out or (ctx.label.name + "/" + exe.basename))
    ctx.actions.symlink(output = out, target_file = exe, is_executable = True)
    return [DefaultInfo(files = depset([out]), executable = out)]

release_binary = rule(
    implementation = _release_binary_impl,
    doc = "A binary built for `platform` (default: the target platform), optionally with cgo.",
    attrs = {
        "binary": attr.label(mandatory = True, cfg = _release_transition, executable = True),
        "platform": attr.label(doc = "Target platform; default: keep the current one."),
        "cgo": attr.bool(doc = "Build Go with cgo (cucina-hostd: managed preferences, keychain)."),
        "out": attr.string(doc = "Output path below the package; default <name>/<binary basename>."),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist",
        ),
    },
    executable = True,
)

# --- cucina_release_archive -----------------------------------------------------------------

def _archive_impl(ctx):
    bi = ctx.attr.buildinfo[CucinaBuildInfo]
    out = ctx.actions.declare_file(ctx.label.name + (".zip" if ctx.attr.format == "zip" else ".tar.gz"))
    args = ctx.actions.args()
    args.add("archive")
    args.add("--buildinfo", bi.json)
    args.add("--format", ctx.attr.format)
    args.add("--top", ctx.attr.top)
    args.add("--exe", "%s=%s" % (ctx.attr.exe_name, ctx.file.binary.path))
    args.add_all(ctx.attr.links, before_each = "--link")
    for doc in ctx.files.docs:
        args.add("--doc", "%s=%s" % (doc.basename, doc.path))
    args.add("--out", out)
    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = [bi.json, ctx.file.binary] + ctx.files.docs,
        outputs = [out],
        mnemonic = "CucinaArchive",
        progress_message = "Archiving %{label}",
    )
    return [DefaultInfo(files = depset([out]))]

cucina_release_archive = rule(
    implementation = _archive_impl,
    doc = "A deterministic tar.gz/zip of a CLI binary, its argv[0] aliases and licence documents.",
    attrs = {
        "binary": attr.label(mandatory = True, allow_single_file = True),
        "exe_name": attr.string(mandatory = True),
        "links": attr.string_list(doc = "argv[0] aliases: hard links in tar.gz, copies in zip (R-AUTH-8)."),
        "docs": attr.label_list(allow_files = True),
        "format": attr.string(mandatory = True, values = ["tar.gz", "zip"]),
        "top": attr.string(mandatory = True, doc = "Top-level directory template, e.g. cucinactl-{version}-linux-amd64."),
        "buildinfo": _BUILDINFO,
        "_tool": _TOOL,
    },
)

# --- images -------------------------------------------------------------------------------

def _oci_meta_impl(ctx):
    bi = ctx.attr.buildinfo[CucinaBuildInfo]
    args = ctx.actions.args()
    args.add("oci-meta")
    args.add("--buildinfo", bi.json)
    args.add("--title", ctx.attr.title)
    args.add("--description", ctx.attr.description)
    args.add("--repository", ctx.attr.repository)
    inputs = [bi.json]
    if ctx.attr.base:
        args.add("--base-name", ctx.attr.base_name)
        args.add("--base-layout", ctx.files.base[0].path)
        inputs.extend(ctx.files.base)
    args.add("--out-labels", ctx.outputs.labels_out)
    args.add("--out-created", ctx.outputs.created_out)
    args.add("--out-tags", ctx.outputs.tags_out)
    args.add("--out-repo-tags", ctx.outputs.repo_tags_out)
    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = inputs,
        outputs = [ctx.outputs.labels_out, ctx.outputs.created_out, ctx.outputs.tags_out, ctx.outputs.repo_tags_out],
        mnemonic = "CucinaOCIMeta",
        progress_message = "Writing image metadata for %{label}",
    )

cucina_oci_meta = rule(
    implementation = _oci_meta_impl,
    doc = "OCI labels (org.opencontainers.image.*), creation time and tags of a release image.",
    attrs = {
        "title": attr.string(mandatory = True),
        "description": attr.string(mandatory = True),
        "repository": attr.string(mandatory = True, doc = "Repository name below ghcr.io/<owner>/."),
        "base": attr.label(doc = "The base image (its manifest digest becomes base.digest)."),
        "base_name": attr.string(),
        "labels_out": attr.output(mandatory = True),
        "created_out": attr.output(mandatory = True),
        "tags_out": attr.output(mandatory = True),
        "repo_tags_out": attr.output(mandatory = True),
        "buildinfo": _BUILDINFO,
        "_tool": _TOOL,
    },
)

DISTROLESS_STATIC = "gcr.io/distroless/static-debian13"

def cucina_release_image(
        name,
        binary,
        title,
        description,
        docs,
        base = "@distroless_static",
        base_name = DISTROLESS_STATIC,
        platforms = LINUX_IMAGE_PLATFORMS,
        tags = None,
        visibility = None):
    """A linux/amd64 + linux/arm64 release image of a pure-Go binary (R-BUILD-3).

    The binary is /usr/local/bin/<binary name> (the entrypoint; charts pass subcommands as
    args), the licence documents are in /usr/share/doc/cucina (R-ARTIFACT), and the config
    carries org.opencontainers.image.* labels and the commit time as `created`.

    Targets: <name> (oci_image_index, an OCI layout directory), <name>_load (the linux/amd64
    image as a docker tarball for kind: build it with --output_groups=+tarball).

    Args:
      name: name of the image index.
      binary: the pure-Go go_binary.
      title: image title, also the repository name (cucina-controller, cucina-sts).
      description: image description label.
      docs: licence documents for /usr/share/doc/cucina.
      base: base image (an oci.pull with linux/amd64 and linux/arm64).
      base_name: reference of `base` for the base.name label.
      platforms: index platforms.
      tags: tags for all targets (release targets are `manual`).
      visibility: visibility of the index.
    """
    label = native.package_relative_label(binary)
    tar(
        name = name + "_bin_layer",
        srcs = [binary],
        include_runfiles = False,
        # rules_go places the executable at <package>/<name>_/<name>.
        mutate = mutate(
            strip_prefix = "%s/%s_" % (label.package, label.name) if label.package else "%s_" % label.name,
            package_dir = "usr/local/bin",
            owner = "0",
            ownername = "root",
            tags = tags,  # the macro does not pass its tags to the mutate target
        ),
        tags = tags,
    )
    tar(
        name = name + "_doc_layer",
        srcs = docs,
        mutate = mutate(
            package_dir = "usr/share/doc/cucina",
            owner = "0",
            ownername = "root",
            tags = tags,
        ),
        tags = tags,
    )
    cucina_oci_meta(
        name = name + "_meta",
        title = title,
        description = description,
        repository = title,
        base = base,
        base_name = base_name,
        labels_out = name + ".labels.txt",
        created_out = name + ".created.txt",
        tags_out = name + ".tags.txt",
        repo_tags_out = name + ".repo_tags.txt",
        tags = tags,
    )
    oci_image(
        name = name + "_image",
        base = base,
        entrypoint = ["/usr/local/bin/" + label.name],
        tars = [
            ":" + name + "_bin_layer",
            ":" + name + "_doc_layer",
        ],
        labels = ":" + name + ".labels.txt",
        created = ":" + name + ".created.txt",
        tags = tags,
    )
    oci_image_index(
        name = name,
        images = [":" + name + "_image"],
        platforms = platforms,
        tags = tags,
        visibility = visibility,
    )
    platform_transition_filegroup(
        name = name + "_first_platform",
        srcs = [":" + name + "_image"],
        target_platform = platforms[0],
        tags = tags,
    )
    oci_load(
        name = name + "_load",
        image = ":" + name + "_first_platform",
        repo_tags = ":" + name + ".repo_tags.txt",
        tags = tags,
    )

# --- cucina_chart_package -----------------------------------------------------------------

def _chart_impl(ctx):
    bi = ctx.attr.buildinfo[CucinaBuildInfo]
    out = ctx.actions.declare_file(ctx.label.name + ".tgz")
    args = ctx.actions.args()
    args.add("chart")
    args.add("--buildinfo", bi.json)
    args.add("--helm", ctx.file._helm)
    args.add("--chart-root", ctx.attr.chart_root)
    args.add("--image-layout", ctx.files.image[0].path)
    args.add("--out", out)
    args.add_all(ctx.files.chart)
    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = [bi.json] + ctx.files.chart + ctx.files.image,
        tools = [ctx.file._helm],
        outputs = [out],
        mnemonic = "CucinaChartPackage",
        progress_message = "Packaging the Helm chart (%{label})",
    )
    return [DefaultInfo(files = depset([out]))]

cucina_chart_package = rule(
    implementation = _chart_impl,
    doc = "`helm package --version/--app-version <release>` with the controller image pinned by digest.",
    attrs = {
        "chart": attr.label(mandatory = True, allow_files = True, doc = "The chart's files."),
        "chart_root": attr.string(mandatory = True),
        "image": attr.label(mandatory = True, doc = "The controller image index (OCI layout)."),
        "buildinfo": _BUILDINFO,
        "_helm": attr.label(default = "@helm//:helm", allow_single_file = True, cfg = "exec"),
        "_tool": _TOOL,
    },
)

# --- cucina_host_pkg ------------------------------------------------------------------------

def _host_pkg_impl(ctx):
    bi = ctx.attr.buildinfo[CucinaBuildInfo]
    out = ctx.actions.declare_directory(ctx.label.name)
    args = ctx.actions.args()
    args.add(out.path)
    args.add(bi.env)
    args.add(ctx.file._build_pkg)
    args.add(ctx.file._make_manifest)
    args.add(ctx.file.hostd)
    args.add(ctx.file.bb_storage)
    args.add(ctx.file.tart)
    args.add(ctx.file.license)
    args.add(ctx.file.notices)
    ctx.actions.run(
        executable = ctx.executable._wrapper,
        arguments = [args],
        inputs = [bi.env, ctx.file.hostd, ctx.file.bb_storage, ctx.file.tart, ctx.file.license, ctx.file.notices] +
                 ctx.files._pkg_inputs,
        outputs = [out],
        mnemonic = "CucinaHostPkg",
        progress_message = "Building the unsigned macOS host package (%{label})",
        # pkgbuild writes through system helpers that the darwin sandbox denies (ADR 0750).
        execution_requirements = {"no-sandbox": "1"},
    )
    return [DefaultInfo(files = depset([out]))]

cucina_host_pkg = rule(
    implementation = _host_pkg_impl,
    doc = """The UNSIGNED macOS host package and its MDM manifests (R-MAC-8, ADR 0753), named
cucina-host-<MAJOR>-<MINOR>-<PATCH>.* and pointing at the GitHub Release URL. Signing is a
local-only step (//release:pkg_sign; R-MAC-9).""",
    attrs = {
        "hostd": attr.label(mandatory = True, allow_single_file = True),
        "bb_storage": attr.label(mandatory = True, allow_single_file = True),
        "tart": attr.label(mandatory = True, allow_single_file = True, doc = "Tart's release tarball."),
        "license": attr.label(mandatory = True, allow_single_file = True),
        "notices": attr.label(mandatory = True, allow_single_file = True),
        "buildinfo": _BUILDINFO,
        "_wrapper": attr.label(default = Label("//bazel/release:build_pkg.sh"), executable = True, cfg = "exec", allow_single_file = True),
        "_build_pkg": attr.label(default = Label("//macos/pkg:scripts/build-pkg.sh"), allow_single_file = True),
        "_make_manifest": attr.label(default = Label("//macos/pkg:scripts/make-manifest.sh"), allow_single_file = True),
        "_pkg_inputs": attr.label(default = Label("//macos/pkg:build_inputs")),
    },
)

# --- dist and finalize ----------------------------------------------------------------------

def _dist_impl(ctx):
    bi = ctx.attr.buildinfo[CucinaBuildInfo]
    out = ctx.actions.declare_directory(ctx.label.name)
    args = ctx.actions.args()
    args.add("dist")
    args.add("--buildinfo", bi.json)
    args.add("--out", out.path)
    inputs = [bi.json]
    for target, template in ctx.attr.files.items():
        files = target[DefaultInfo].files.to_list()
        if len(files) != 1:
            fail("%s must produce exactly one file" % target.label)
        args.add("--file", "%s=%s" % (template, files[0].path))
        inputs.extend(files)
    for target in ctx.attr.trees:
        for f in target[DefaultInfo].files.to_list():
            args.add("--tree", f.path)
            inputs.append(f)
    for target, image in ctx.attr.images.items():
        layout = target[DefaultInfo].files.to_list()
        args.add("--image", "%s=%s" % (image, layout[0].path))
        inputs.extend(layout)
    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = inputs,
        outputs = [out],
        mnemonic = "CucinaDist",
        progress_message = "Staging release assets (%{label})",
    )
    return [DefaultInfo(files = depset([out]))]

cucina_release_dist = rule(
    implementation = _dist_impl,
    doc = "Release assets under their published names (assets/) plus meta/ (build info, image digests).",
    attrs = {
        "files": attr.label_keyed_string_dict(allow_files = True, doc = "Asset -> published name template."),
        "trees": attr.label_list(doc = "Directories whose files are assets under their own names."),
        "images": attr.label_keyed_string_dict(doc = "Image index -> name, recorded in meta/images.json."),
        "buildinfo": _BUILDINFO,
        "_tool": _TOOL,
    },
)

def _finalize_impl(ctx):
    out = ctx.actions.declare_directory(ctx.label.name)
    args = ctx.actions.args()
    args.add("finalize")
    args.add("--out", out.path)
    args.add("--formula-template", ctx.file.formula_template)
    inputs = [ctx.file.formula_template]
    for d in ctx.attr.dists:
        for f in d[DefaultInfo].files.to_list():
            args.add(f.path)
            inputs.append(f)
    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = inputs,
        outputs = [out],
        mnemonic = "CucinaFinalize",
        progress_message = "Writing SHA256SUMS and the Homebrew formula (%{label})",
    )
    return [DefaultInfo(files = depset([out]))]

cucina_release_finalize = rule(
    implementation = _finalize_impl,
    doc = "Merges dists of one build, writes assets/SHA256SUMS and meta/cucinactl.rb.",
    attrs = {
        "dists": attr.label_list(mandatory = True),
        "formula_template": attr.label(mandatory = True, allow_single_file = True),
        "_tool": _TOOL,
    },
)
