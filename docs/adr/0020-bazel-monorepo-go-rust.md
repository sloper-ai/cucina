<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0020 — D10: A Bazel monorepo with Go and Rust, one protobuf API

* Status: accepted (2026-10-02)

## Context
Cucina has a Go controller and host agent, a Rust CLI and TUI, Helm charts, images, Packer templates and OpenTofu, all
built for several operating systems. Go matches controller-runtime and Buildbarn; Rust for the CLI was a product decision.
One hermetic build graph lets the repository build and test itself locally or through Cucina (dogfooding).

## Decision
A Bzlmod workspace on Bazel 9.2 (rules_go and Gazelle, rules_rs, hermetic-llvm, protobuf, rules_oci) builds everything;
`go.mod` and `Cargo.toml`/`Cargo.lock` stay valid for IDEs. One protobuf API (`api/proto/cucina/v1`) is shared by the
Go controller and the Rust CLI, with generated code checked in. OpenTofu provides the e2e infrastructure, Packer the
images, and the Helm chart is `apiVersion: v2`.

## Consequences
* `bazel build //... && bazel test //...` is the canonical build; Go and Cargo workflows exist for IDEs (`docs/dev/getting-started.md`).
* Release artifacts are built by Bazel, not by goreleaser, ko or cargo-zigbuild.
