# SPDX-License-Identifier: FSL-1.1-ALv2
"""Checked-in protobuf code generation (R-BUILD-1 "Protobuf").

Generated Go code is committed next to the .proto files so IDEs, `go build` and
`go test ./...` see it. The output is byte-identical to `buf generate` with
paths=source_relative (the protoc version in headers is normalised to
"(unknown)", as buf writes it; plugin versions come from go.mod `tool` lines); Bazel regenerates it with the hermetic toolchain and a
`write_source_files` diff test (tier-static) fails when the checked-in copy is
stale. Gazelle (`# gazelle:go_generate_proto false`) compiles the checked-in
*.pb.go files as an ordinary go_library.

    load("//bazel:proto.bzl", "cucina_proto_go")

    proto_library(name = "v1_proto", srcs = [...])   # Gazelle-generated

    cucina_proto_go(
        name = "v1_go",
        proto = ":v1_proto",
        importpath = "github.com/sloper-ai/cucina/api/proto/cucina/v1",
        grpc = True,
    )

Regenerate: `bazel run //api/proto/cucina/v1:v1_go` (or `bazel run //:update_goldens`).
"""

load("@bazel_lib//lib:write_source_files.bzl", "write_source_files")
load("@bazel_skylib//rules:select_file.bzl", "select_file")
load("@protobuf//bazel/common:proto_info.bzl", "ProtoInfo")
load("@rules_go//proto:def.bzl", "go_proto_library")

def _protoc_version_normalized_impl(ctx):
    out = ctx.actions.declare_file(ctx.attr.out)
    ctx.actions.run(
        executable = ctx.executable._protonorm,
        arguments = [ctx.file.src.path, out.path],
        inputs = [ctx.file.src],
        outputs = [out],
        mnemonic = "ProtoGoNormalize",
        progress_message = "Normalising protoc version in %{output}",
    )
    return [DefaultInfo(files = depset([out]))]

_protoc_version_normalized = rule(
    implementation = _protoc_version_normalized_impl,
    doc = "Rewrites the protoc version in generated Go headers to `(unknown)` (as buf does).",
    attrs = {
        "out": attr.string(mandatory = True),
        "src": attr.label(allow_single_file = [".go"], mandatory = True),
        "_protonorm": attr.label(
            default = Label("//tools/protonorm"),
            executable = True,
            cfg = "exec",
        ),
    },
)

def cucina_proto_go(name, proto, importpath, srcs = None, grpc = False, deps = [], visibility = None):
    """Generates and checks in `<file>.pb.go` (+ `<file>_grpc.pb.go`) for `proto`.

    Args:
      name: name of the update target (`bazel run :<name>`); the freshness tests
        are `<name>_*_test` (tier-static).
      proto: the proto_library to generate from.
      importpath: Go import path of the generated package.
      srcs: .proto file names in this package (defaults to glob(["*.proto"])).
      grpc: True to also run protoc-gen-go-grpc (go_grpc_v2) on every source, or
        a list of the .proto files that declare services.
      deps: go_proto_library deps for imported (non well-known) protos.
      visibility: visibility of the update target.
    """
    if srcs == None:
        srcs = native.glob(["*.proto"])
    if grpc == True:
        grpc_srcs = srcs
    elif grpc:
        grpc_srcs = grpc
    else:
        grpc_srcs = []

    gen = name + "_gen"
    go_proto_library(
        name = gen,
        compilers = ["@rules_go//proto:go_proto"] + ([Label("//bazel/toolchains/go_proto:go_grpc")] if grpc_srcs else []),
        importpath = importpath,
        proto = proto,
        deps = deps,
        # Only the generated sources are consumed; Gazelle's go_library compiles
        # the checked-in copies.
        tags = ["manual"],
        visibility = ["//visibility:private"],
    )
    native.filegroup(
        name = gen + "_srcs",
        srcs = [":" + gen],
        output_group = "go_generated_srcs",
        tags = ["manual"],
        visibility = ["//visibility:private"],
    )

    files = {}
    for src in srcs:
        if not src.endswith(".proto") or "/" in src:
            fail("cucina_proto_go: srcs must be .proto files in this package, got %r" % src)
        base = src[:-len(".proto")]
        outs = [base + ".pb.go"]
        if src in grpc_srcs:
            outs.append(base + "_grpc.pb.go")
        for out in outs:
            selected = "%s_%s" % (gen, out.replace(".", "_"))
            select_file(
                name = selected,
                srcs = ":" + gen + "_srcs",
                subpath = "/" + out,
                tags = ["manual"],
                visibility = ["//visibility:private"],
            )
            _protoc_version_normalized(
                name = selected + "_norm",
                src = ":" + selected,
                out = "%s_norm/%s" % (gen, out),
                tags = ["manual"],
                visibility = ["//visibility:private"],
            )
            files[out] = ":" + selected + "_norm"

    write_source_files(
        name = name,
        files = files,
        tags = ["tier-static"],
        visibility = visibility,
    )

_PROTO_TOOLCHAIN = "@protobuf//bazel/private:proto_toolchain_type"

def _import_path(proto_info, src):
    """The import path protoc knows `src` by (relative to its proto_source_root)."""
    root = proto_info.proto_source_root
    if root not in ("", ".") and src.path.startswith(root + "/"):
        return src.path[len(root) + 1:]
    return src.short_path

def _out(opts, directory):
    return "%s:%s" % (",".join(opts), directory.path) if opts else directory.path

def _rust_buffa_connect_impl(ctx):
    proto_info = ctx.attr.proto[ProtoInfo]
    protoc = ctx.toolchains[_PROTO_TOOLCHAIN].proto.proto_compiler
    buffa = ctx.actions.declare_directory(ctx.label.name + "/buffa")
    outputs = [buffa]
    tools = [ctx.file._buffa, ctx.file._buffa_packaging]

    args = ctx.actions.args()
    args.add(ctx.file._buffa, format = "--plugin=protoc-gen-buffa=%s")
    args.add(ctx.file._buffa_packaging, format = "--plugin=protoc-gen-buffa-packaging=%s")

    # buffa-packaging runs twice, so its parameters go inline per directive
    # (`--x_out=params:dir`; protoc would apply an `--x_opt` to both runs).
    # Single-run plugins use `--x_opt`, since `crate::proto` contains colons.
    args.add("--buffa_out=" + buffa.path)
    if ctx.attr.buffa_opts:
        args.add("--buffa_opt=" + ",".join(ctx.attr.buffa_opts))
    args.add("--buffa-packaging_out=" + buffa.path)
    if ctx.attr.services:
        connect = ctx.actions.declare_directory(ctx.label.name + "/connect")
        outputs.append(connect)
        tools.append(ctx.file._connect)
        args.add(ctx.file._connect, format = "--plugin=protoc-gen-connect-rust=%s")
        args.add("--connect-rust_out=" + connect.path)
        if ctx.attr.connect_opts:
            args.add("--connect-rust_opt=" + ",".join(ctx.attr.connect_opts))
        args.add("--buffa-packaging_out=" + _out(["filter=services"], connect))
    args.add_joined(
        proto_info.transitive_descriptor_sets,
        join_with = ctx.configuration.host_path_separator,
        format_joined = "--descriptor_set_in=%s",
    )
    args.add_all([_import_path(proto_info, src) for src in proto_info.direct_sources])

    ctx.actions.run(
        executable = protoc,
        arguments = [args],
        inputs = proto_info.transitive_descriptor_sets,
        outputs = outputs,
        tools = tools,
        mnemonic = "RustBuffaConnectGen",
        progress_message = "Generating buffa/connect-rust code for %{label}",
    )
    return [
        DefaultInfo(files = depset(outputs)),
        OutputGroupInfo(buffa = depset([buffa]), connect = depset(outputs[1:])),
    ]

_rust_buffa_connect = rule(
    implementation = _rust_buffa_connect_impl,
    doc = "Runs protoc with protoc-gen-buffa(+packaging) and protoc-gen-connect-rust.",
    attrs = {
        "buffa_opts": attr.string_list(),
        "connect_opts": attr.string_list(),
        "proto": attr.label(mandatory = True, providers = [ProtoInfo]),
        "services": attr.bool(default = True),
        "_buffa": attr.label(
            default = Label("@protoc_gen_buffa//:protoc-gen-buffa"),
            cfg = "exec",
            allow_single_file = True,
        ),
        "_buffa_packaging": attr.label(
            default = Label("@protoc_gen_buffa_packaging//:protoc-gen-buffa-packaging"),
            cfg = "exec",
            allow_single_file = True,
        ),
        "_connect": attr.label(
            default = Label("@protoc_gen_connect_rust//:protoc-gen-connect-rust"),
            cfg = "exec",
            allow_single_file = True,
        ),
    },
    toolchains = [config_common.toolchain_type(_PROTO_TOOLCHAIN, mandatory = True)],
)

def cucina_proto_rust(
        name,
        proto,
        out_dir = "src/gen",
        services = True,
        buffa_opts = ["views=true", "json=true"],
        connect_opts = ["buffa_module=crate::proto"],
        visibility = None):
    """Checked-in Rust protobuf messages (buffa) and RPC stubs (connect-rust) for `proto`.

    Runs the pinned protoc-gen-buffa v0.9.2, protoc-gen-buffa-packaging v0.9.2 and
    protoc-gen-connect-rust v0.9.1 (tools/pinned.bzl) exactly like the upstream
    connect-rust Bazel example and `buf generate`: `<out_dir>/buffa/**` holds the
    message types (+ packaging `mod.rs`), `<out_dir>/connect/**` the service stubs
    (+ packaging with `filter=services`). The crate compiles the checked-in files
    (`#[path = "gen/buffa/mod.rs"] pub mod proto;` and
    `#[path = "gen/connect/mod.rs"] pub mod connect;` in the crate root, matching
    `buffa_module=crate::proto`; `include!` won't do: the files carry inner
    attributes). Generated code needs `buffa`, `buffa-types`, `connectrpc`,
    `serde` and `serde_json` (json=true) as crate deps. `bazel run :<name>` refreshes them; tier-static
    diff tests catch drift. The defaults must match the buf.gen.yaml the Cargo
    path uses (ADR 0106).

    Args:
      name: update target (`bazel run :<name>`); diff tests are `<name>_*_test`.
      proto: the proto_library to generate from (its direct sources).
      out_dir: package-relative output directory.
      services: also generate connect-rust stubs (protos with services).
      buffa_opts: protoc-gen-buffa options.
      connect_opts: protoc-gen-connect-rust options.
      visibility: visibility of the update target.
    """
    gen = name + "_gen"
    _rust_buffa_connect(
        name = gen,
        buffa_opts = buffa_opts,
        connect_opts = connect_opts,
        proto = proto,
        services = services,
        tags = ["manual"],
        visibility = ["//visibility:private"],
    )
    files = {}
    for group in ["buffa", "connect"] if services else ["buffa"]:
        native.filegroup(
            name = "%s_%s" % (gen, group),
            srcs = [":" + gen],
            output_group = group,
            tags = ["manual"],
            visibility = ["//visibility:private"],
        )
        files["%s/%s" % (out_dir, group)] = ":%s_%s" % (gen, group)
    write_source_files(
        name = name,
        files = files,
        tags = ["tier-static"],
        visibility = visibility,
    )
