<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0113 — Update govulncheck without reducing source analysis

* Status: accepted (2026-10-03)

## Context

The security lane at `f6a3e94b31f2` ran govulncheck v1.1.4 with Go 1.27.1 and
panicked in x/tools v0.29.0 SSA construction: `unexpected expr: *ast.KeyValueExpr`.
This is an incomplete scan, not a vulnerability finding or a clean result.

Go 1.27 permits promoted-field keys in struct literals. The old SSA builder only
resolves direct field names; [upstream fix 8e51a5fb67f9](https://go.googlesource.com/tools/+/8e51a5fb67f9b3e2b32792f21e727664ca6561e2)
resolves embedded field paths before lowering their values. This explains the
observed failure path, although the stack does not identify the offending literal.

## Decision

Supersede only the govulncheck pin in [ADR 0152](0152-nightly-and-report-lanes.md).
Use official stable **v1.8.0**, commit
`709015412431dd2b5b28a53c06c70bc02d49074c`, in the existing security lane.
[Official release metadata](https://proxy.golang.org/golang.org/x/vuln/@v/v1.8.0.info)
and [module metadata](https://proxy.golang.org/golang.org/x/vuln/@v/v1.8.0.mod)
identify the release and its x/tools v0.50.0 dependency, which contains the fix.
Its Go directive is 1.26.0; Cucina's Go 1.27.1 pin is unchanged.

Keep source-mode reachability analysis over `./...`, fresh advisory data and the
30-minute job deadline. Do not change product dependencies, exclude packages,
reduce analysis depth or suppress findings to accommodate the scanner. An official
stable release is preferred to a floating version, fork or unreleased commit;
metadata alone does not prove the earliest working version or a completed scan.

## Consequences

Scanner dependencies remain separate from Cucina's product module. A focused
compatibility check must distinguish completed clean analysis, vulnerability
findings and scanner/infrastructure failure. Qualification on an immutable source
snapshot does not qualify later source changes or replace the normal security lane.

The focused check on immutable `f6a3e94b31f2` used a Go 1.27.1 host-built scanner,
then analyzed `./...` for Linux/amd64 with `CGO_ENABLED=0`, `GOWORK=off` and
`GOFLAGS=-mod=readonly`. The scanner completed without the SSA panic, but exited 3
and reported reachable [GO-2026-6443](https://pkg.go.dev/vuln/GO-2026-6443) in gRPC
v1.84.0; this is **not a clean automated scan**. Scanner output and independently
verified applicability are distinct; [ADR 0114](0114-grpc-advisory-applicability.md)
records the version-data discrepancy and the scoped Buildbarn assessment. The scan
also reported four vulnerabilities in
required modules that the code does not appear to call; the default output did
not enumerate their IDs. The advisory database reported an update of
2026-10-01 20:24:15 UTC.

The frozen source's module-file SHA-256 hashes were identical before and after:

* `go.mod`: `36179b932323d7968c3903ed8b06063ff15f6462b900872e4050463cdb3e36c0`
* `go.sum`: `99babb205669cac63da4c46acc07f98a1220380be8a13ff705f8d05a1e4d6601`

These identify the checked module files, not a hash of the entire source tree.
Product dependency remediation is separate from this scanner-only decision.
