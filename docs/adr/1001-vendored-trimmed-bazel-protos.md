<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 1001 — e2e collectors decode Bazel's BEP and compact execution log with vendored protos

* Status: accepted (2026-10-02)

## Context
§10.4 asks for scripted collectors over the Build Event Protocol (JSON and binary) and the compact execution log
(`--execution_log_compact_file`: zstd-compressed, length-delimited `tools.protos.ExecLogEntry`). No Go module publishes these
protos: they live in Bazel's source tree. `build_event_stream.proto` imports `failure_details.proto` (≈ 20k lines of
generated Go), `command_line.proto`, `action_cache.proto`, `invocation_policy.proto` and `package_load_metrics.proto`;
`spawn.proto` only imports well-known types.

## Decision
* Vendor `spawn.proto` from Bazel 9.2.0 verbatim (plus `go_package`) under `test/e2e/third_party/bazel/spawnpb/`.
* Vendor a **trimmed** `build_event_stream.proto` under `test/e2e/third_party/bazel/bespb/`: only the messages the collector
  reads, with Bazel's field numbers, names and types unchanged; fields typed by the other Bazel protos are dropped. Protobuf
  ignores unknown fields, so real binary streams decode, and protojson with `DiscardUnknown` decodes Bazel's JSON.
* Generate Go with `protoc-gen-go` v1.36.12 through `buf generate` (module root `test/e2e/third_party/bazel`) and check the
  `.pb.go` files in, like the rest of the repository's generated code.
* Prove the trimming with real fixtures: a Bazel 9.2.0 build's JSON stream and its binary stream (scrubbed of user, host and
  paths) must summarise identically (`collect/bep` test); the compact execution log fixtures come straight from Bazel 9.2.0.
* Parse BEP from the **binary** file in the campaign (smaller to move off the client VMs); the JSON path stays for humans.

## Consequences
* A Bazel bump re-vendors two files; field-number drift would make the JSON/binary equality test fail.
* The unstructured command line (it carries `--client_env`, i.e. the client environment) is deliberately not collected, and
  Bazel runs with a minimal environment on the hosts so raw BEP files on the dev-storage volume hold no credentials.
* Regeneration: `cd test/e2e/third_party/bazel && buf generate` with a local `protoc-gen-go` v1.36.12 (docs/dev/e2e.md);
  not wired to `cucina_proto_go` because Gazelle's `proto_library` import path differs from the buf module root.
