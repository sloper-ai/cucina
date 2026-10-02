<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0801 — cucinactl: token-cache locking and the few dependencies beyond R-LIB-3

* Status: accepted (2026-10-02)

## Context
Bazel starts the credential helper once per gRPC service, concurrently (R-AUTH-8), so renewals of the 0600 JWT cache need
a cross-process lock. `std::fs::File::lock` needs Rust 1.89 but the workspace declares `rust-version = "1.88"`
(clippy `incompatible_msrv`), the workspace denies `unsafe_code`, and `fs2`/`fs4`/`fd-lock` are below R-LIB-1's bar.

## Decision
* Unix: `flock(2)` through **rustix** (`fs` feature; bytecodealliance, already in the graph via crossterm/terminal_size).
  Windows: an exclusive **share-mode open** of the lock file (std only; no other handle can open it while held). Writers
  retry with a 5 ms poll up to 9 s (Bazel's helper timeout is 10 s), re-read the cache after acquiring the lock, and replace
  the cache atomically (temp file + rename), so readers never lock and never see a torn file.
* Other direct additions, all already in the lock graph and well-known: `http` (hyperium; `Uri` for connect-rust client
  configs). Tests use no new crates: temporary directories are a 20-line helper, and the `--output json` contract test uses a
  small validator for exactly the JSON Schema keywords our schemas use (the `jsonschema` crate would pull a large tree).
* Removed from the stub's dependency set: `tonic`, `tonic-prost`, `prost`, `prost-types` (ADR 0003), `tokio-rustls` and
  `aws-lc-rs` as direct normal deps (rustls/reqwest/connectrpc carry aws-lc-rs; tests use it to generate keys),
  `proptest-state-machine` (unused).

## Consequences
One code path per OS family, no unsafe, no MSRV bump. Cargo.lock changed (rules_rs repin needed).
