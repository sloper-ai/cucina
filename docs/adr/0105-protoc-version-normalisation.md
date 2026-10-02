<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0105 — Checked-in Go protobuf code is identical from Bazel and `buf generate`

* Status: accepted (2026-10-02)

## Context
The management/enrollment/host API (`api/proto/cucina/v1`) is generated with `buf generate`
(`buf.gen.yaml`, protoc-gen-go v1.36.12 + protoc-gen-go-grpc v1.6.2, `paths=source_relative`)
and committed. R-BUILD-1 also wants Bazel to regenerate it with a freshness `diff_test`. The two
paths differed in two ways: rules_go's `go_grpc_v2` resolved protoc-gen-go-grpc v1.5.1, and protoc
records its own version in the header (`protoc v7.36.2`) while buf records `(unknown)`.

## Decision
* Pin both plugins in `go.mod` as Go `tool` directives (`go get -tool ...@vX`), so Gazelle's
  `go_deps` resolves rules_go's plugin repositories to exactly those versions (MVS). buf.gen.yaml
  can run the same versions with `go tool protoc-gen-go(-grpc)`.
* `cucina_proto_go` post-processes each generated file with `//tools/protonorm`, rewriting the
  protoc version line to `(unknown)` — what buf writes — so the output no longer depends on the
  protoc release.
* `api/proto` sets `# gazelle:proto_strip_import_prefix /api/proto` so `// source:` lines and
  embedded descriptors use `cucina/v1/*.proto`, as buf does.

## Consequences
Bumping protobuf/protoc never churns checked-in code; bumping a plugin is a `go get -tool` plus
`bazel run //:update_goldens`. The diff test proves both generators agree.
