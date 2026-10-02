<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0106 — Rust protobuf/RPC code generation in Bazel: protoc + buffa/connect-rust plugins

* Status: accepted (2026-10-02); implements ADR 0003 (connect-rust + buffa replace tonic/prost)

## Context
R-BUILD-1 originally called for `rust_prost_library` with Cucina's own `rust_prost_toolchain`.
The user decided (ADR 0003) that `cucinactl` uses connect-rust (`connectrpc`) and buffa instead of
tonic/prost. rules_rust has no buffa/connect rules; upstream connect-rust ships a Bazel example that
drives protoc with three plugins from a genrule.

## Decision
* `cucina_proto_rust(name, proto, out_dir = "src/gen")` (`//bazel:proto.bzl`) runs the prebuilt
  protoc from protobuf's proto toolchain with the pinned release binaries (`tools/pinned.bzl`,
  SHA-256 from each release's `checksums-sha256.txt`, cross-checked with GitHub's asset digests):
  protoc-gen-buffa v0.9.2 (`views=true,json=true`), protoc-gen-buffa-packaging v0.9.2 (twice; the
  second run with `filter=services`, passed inline per output directive because protoc applies an
  `--x_opt` to every run of a plugin) and protoc-gen-connect-rust v0.9.1
  (`buffa_module=crate::proto`). Inputs are the proto_library's descriptor sets with source info,
  so comments are kept.
* Output trees `<out_dir>/buffa/**` and `<out_dir>/connect/**` are checked in with
  `write_source_files` (tier-static diff tests); crates mount them with
  `#[path = "gen/buffa/mod.rs"] pub mod proto;` / `#[path = "gen/connect/mod.rs"] pub mod connect;`.
* The prost toolchain was removed; `deny.toml` bans `tonic`, `tonic-prost` and `prost`.

## Consequences
Bazel and `buf generate` produce the same files as long as plugin versions and options match the
CLI's buf.gen.yaml (owner: CLI agent); the diff test proves it. Plugins are exec tools only, so
their Rust dependencies never mix with `@crates`. `//tools/hello/rust` is the working example.
