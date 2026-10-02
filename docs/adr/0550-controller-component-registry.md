<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0550 — cucina-controller is assembled from build-time optional components

* Status: accepted (2026-10-02)

## Context
One binary serves the reconcilers, the autoscaler, the STS, enrollment, the host stream and the management API
(contracts §1), but those packages are written in parallel by different agents and land at different times. A
monolithic `main` that imports all of them breaks the build whenever one of them is mid-edit, and makes it hard to see
which pieces a process actually runs. Alternatives: plugin binaries (no: one image, one version), a dependency-injection
framework (no: not on the R-LIB lists, and init-order magic is harder to read than plain constructors).

## Decision
`internal/controller` exposes a small registry. Each foreign package is wired by exactly one file
`internal/controller/components_<name>.go` whose `init()` calls `RegisterComponent` (servers and background jobs),
`ProvideCompute`/`ProvideBuildQueue` (port adapters) or `RegisterBootstrapStep` (Helm hook steps). A factory receives
`*Deps` (configuration, clients, TLS, metrics, ports, shared objects) and returns a value whose optional interfaces decide
how it runs: gRPC service on a named listener, HTTPS handler, runner (leader-only or every replica), readiness check.
Construction runs in `Order`; `Order < 0` before the reconcilers (port providers such as the host stream, shared objects
such as the PKI issuer and the key manager), the rest after them. `cucina-controller version` lists what is compiled in.

## Consequences
Deleting one file removes a feature without touching anything else; a broken package is isolated to its file. The
registry is process-global (`init()`), so tests construct reconcilers and servers directly instead of going through it.
Wiring is plain Go in one directory, reviewed with the controller; the table in docs/dev/controller.md lists every file.
