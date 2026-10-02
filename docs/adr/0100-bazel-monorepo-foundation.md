<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0100 — Bazel monorepo foundation

* Status: accepted (2026-10-02)

## Context
R-BUILD-1..6 require one Bzlmod workspace on Bazel 9.2.0 that builds Go, Rust, C/C++, protobuf,
OCI images and chart/infra checks hermetically, locally first and later on Cucina RBE, while
`go.mod` and `Cargo.toml`/`Cargo.lock` stay the dependency sources of truth. About ten agents
add code concurrently.

## Decision
* Modules (BCR, 2026-10-01 versions): rules_go 0.64.1 + gazelle 0.54.0 (Go SDK 1.27.1,
  `go_deps.from_file`), rules_rs 0.0.112 (rustc 1.98.1, `crate.from_cargo` over the root
  workspace), llvm 0.8.24 (the only C/C++ toolchain), protobuf 36.2 (prebuilt protoc through
  toolchain resolution), rules_cc 0.2.25, rules_oci 2.3.0 + tar.bzl 0.10.9, rules_shell 0.8.0,
  bazel_lib 3.7.2, bazel_skylib 1.9.2, platforms 1.1.0, apple_support 2.9.1 (only for an
  empty `xcode_config`), buildifier_prebuilt 10.1.0.
* Pure Go globally (`--@rules_go//go/config:pure`); cgo targets opt in with `pure = "off"`.
* Generated code (Go protobuf, Rust prost) is checked in via `write_source_files` with
  tier-static diff tests; `bazel run //:update_goldens` refreshes every such target.
* Machine-local paths (output user root, disk/repository cache) live only in the gitignored
  `user.bazelrc` written by `tools/setup-user-bazelrc.sh`.
* Third-party binaries are pinned by checksum per OS/arch in a reproducible module extension
  (`tools/pinned.bzl`) with platform-selecting hub repositories.
* Gazelle maps `go_test` to the tier macro `cucina_go_test`; generated `.pb.go` files are
  ordinary Go sources for Gazelle (`go_generate_proto false`).
* Output directories carry the target platform (`--experimental_platform_in_output_dir`) named
  by a hash of its label (`--noexperimental_use_platforms_in_output_dir_legacy_heuristic`), so
  paths are host-independent and same-named platforms (host `darwin_arm64` vs
  `@rules_go//go/toolchain:darwin_arm64`) can't produce conflicting actions.

## Consequences
`bazel build //... && bazel test //...` is the canonical build on macOS, Linux and Windows hosts.
The first cold build is slow (hermetic-llvm compiles libc++/compiler-rt/libc per target); disk and
repository caches make it a one-time cost per machine. Agents must run `bazel mod tidy` after
editing `go.mod`, and only mark modules direct with `go mod edit` (never a repo-wide
`go mod tidy` while others are mid-change).
