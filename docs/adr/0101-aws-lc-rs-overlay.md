<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0101 — aws-lc-rs 1.18.1 on the rules_rs aws-lc-sys overlay (BCR AWS-LC 5.1.0)

* Status: accepted (2026-10-02)

## Context
R-LIB-3 makes aws-lc-rs the only crypto provider (aws-lc-rs 1.18.1). R-BUILD-1 requires
`aws-lc-sys` to build through the rules_rs overlay (BCR `aws-lc` + `rust_bindgen`, no build
script), which rules_rs 0.0.112 tests with aws-lc-rs 1.17.1 / aws-lc-sys 0.42.0. Findings
(2026-10-02):
* aws-lc-sys 0.42.0 bundles AWS-LC 5.1.0 (= BCR `aws-lc` 5.1.0.bcr.3); aws-lc-rs 1.18.1 needs
  aws-lc-sys ^0.45.0, which bundles AWS-LC 5.7.0 (not on the BCR).
* rustls 0.23.44+ requires aws-lc-rs ^1.18, so pinning aws-lc-rs 1.17.1 would also force
  rustls ≤ 0.23.43, below R-LIB-3's 0.23.45.

## Decision
Keep the R-LIB-3 versions (aws-lc-rs 1.18.1, aws-lc-sys 0.45.0, rustls 0.23.45) on the overlay
copied into `third_party/rust/aws-lc-sys.MODULE.bazel` (BCR `aws-lc` 5.1.0.bcr.3). The overlay
generates aws-lc-sys's bindings with bindgen from the BCR headers, so the Rust declarations
always match the linked C library; aws-lc-rs 1.18.1 compiles against them (no missing symbols)
and `//tools/hello/rust:aws_lc_test` proves it at run time (SHA-256 known answer, ECDSA P-256
sign/verify, rustls `aws_lc_rs` provider install, zstd via the `zstd-sys` overlay).

## Consequences
At run time the Bazel-built binaries use AWS-LC 5.1.0, while `cargo build` (aws-lc-sys's own
build script) uses the bundled 5.7.0; both are supported AWS-LC releases with the same API
surface that aws-lc-rs uses. When the BCR publishes `aws-lc` ≥ 5.7, bump the overlay. Fallback if a
future aws-lc-rs needs newer symbols: pin aws-lc-rs =1.17.1 + aws-lc-sys =0.42.0 + rustls =0.23.43
(record it here), or `gen_build_script = "on"` for aws-lc-sys (cc-rs with the hermetic LLVM).
`deny.toml` bans `ring`/OpenSSL so the single-provider rule cannot regress.
