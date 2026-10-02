# 0003 — Rust RPC and protobuf stack: connect-rust + buffa (not tonic/prost)

* Status: accepted — user decision, 2026-10-02 (supersedes the tonic/prost rows of R-LIB-3 and the `rust_prost_library` row of R-BUILD-1)

## Context
PROMPT.md R-LIB-3 specified `tonic` 0.14.6 + `tonic-prost(-build)` + `prost` 0.14.4 for `cucinactl`. The user instructed
to use **connect-rust** (`connectrpc`, github.com/connectrpc/connect-rust, Apache-2.0, 522 stars, MSRV 1.88, passes the full
ConnectRPC conformance suite across Connect, gRPC and gRPC-Web) and **buffa** (github.com/anthropics/buffa, Apache-2.0,
902 stars: protobuf with editions, JSON, zero-copy views) instead.

## Decision
* Rust protobuf messages come from `buffa`/`buffa-types`; Rust service stubs and clients come from `connectrpc`
  (`protoc-gen-connect-rust`, `protoc-gen-buffa`, `protoc-gen-buffa-packaging`), generated with `buf generate` into
  `cli/cucina-api/src/gen/{buffa,connect}/` and checked in (drift check in CI; Bazel reproduces it with pinned plugin binaries).
* The controller and Buildbarn stay gRPC servers (grpc-go). The Rust client speaks the **gRPC protocol over HTTP/2 + TLS**:
  `ClientConfig::new(uri).with_protocol(Protocol::Grpc)` over `Http2Connection`/`SharedHttp2Connection::connect_tls(uri, rustls::ClientConfig)`.
  The Connect protocol is unused on the wire to Cucina servers.
* TLS stays rustls with a **single crypto provider, aws-lc-rs** (connectrpc's rustls dependencies already default to aws-lc-rs; CI checks
  `cargo tree -i ring` is empty). Fake management/REAPI servers in tests use connectrpc's server (replacing the "tonic server as dev-dependency").
* REAPI and googleapis protos (bytestream, longrunning, rpc) are generated with the same plugins.

## Consequences
* connect-rust and buffa are pre-1.0: pin exact versions, review on every bump, keep the generated-code drift check.
* `tonic`, `tonic-prost`, `prost` are banned in `deny.toml` for the workspace (keeps one protobuf runtime in the tree).
* R-LIB-1's star bar is met (> 300 stars) and both are user-mandated, so no further exemption is needed.
