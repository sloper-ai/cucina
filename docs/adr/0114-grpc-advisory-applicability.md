<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0114 — Record grpc advisory applicability without rewriting scan results

* Status: accepted (2026-10-03)

## Context

On 2026-10-03, govulncheck **v1.8.0**, using Go **1.27.1**, `linux/amd64`, `CGO_ENABLED=0`, analyzed immutable Cucina
`f6a3e94b31f2cccce3b11a6609891c0cae52c8c2` and **exited 3**. The raw report remains external evidence at
`$CUCINA_DEV_STORAGE/validation/f6a3e94b31f2/govulncheck-v1.8.0/scan.log`; its finding is not removed or rewritten:

```text
Vulnerability #1: GO-2026-6443
    Server panic via missing authority or Host headers in google.golang.org/grpc
  More info: https://pkg.go.dev/vuln/GO-2026-6443
  Module: google.golang.org/grpc
    Found in: google.golang.org/grpc@v1.84.0
    Fixed in: google.golang.org/grpc@v1.85.0-dev.0.20260825072537-93e31b48545e
```

**This is not a clean scan.** Scanner-tool compatibility is a separate decision in
[ADR 0113](0113-govulncheck-go127-compatibility.md). The subsequently supplied verbose diagnostic also **exited 3**, with the
same grpc symbol finding. Its retained `scan-verbose.log` in the same evidence directory has verified SHA-256
`f466808a74cb99e5e2452a970aab04c3b5f57be0872974482be9c167a848b8df` and reports **zero additional imported-package findings**
and these **four module-only findings**, all in `golang.org/x/crypto@v0.54.0`:

| Reported advisory | Reported issue | Reported fixed version |
| --- | --- | --- |
| GO-2026-6355 | SSH established-channel deadlock DoS | `v0.56.0` |
| GO-2026-6354 | SSH undecided-channel deadlock DoS | `v0.56.0` |
| GO-2026-6303 | SSH source-address critical option not enforced for non-public-key authentication callbacks | `v0.55.0` |
| GO-2026-5932 | Unmaintained, unsafe OpenPGP package | `N/A` |

The scanner's "doesn't appear to call" classification is reported only for this **Linux/amd64, CGO-disabled source graph**,
not proof of unreachability across platforms/configurations or a blanket waiver. No broader assessment of these four findings
is made here.

### Cucina's grpc 1.84.0: fixed source, inconsistent database range

The [Go vulnerability record][go-db] includes `1.84.0` in its affected interval, while the [upstream advisory][advisory]
explicitly lists `1.84.0` as patched. The [official proxy tag metadata][grpc-tag] identifies commit
`e84aa5ab15d1d2b29d54f838312ad490cb7551a8`. It contains security backport
[`d5a41119e0e3189ea913cf839586ce34a44f1a3f` / PR #9370][backport] of [PR #9365][fix]; the ancestry comparison was
three commits ahead, zero behind. Both the [tagged transport source, lines 525–530][fixed-source] and the inspected immutable
Bazel source reject a request without either header before routing. The [checksum database][checksums] matches `go.sum`:

```text
google.golang.org/grpc v1.84.0 h1:soMyaPJ8pAak5PIQ0DGBUir0XRo2fRoMqhNWMLlLxO0=
google.golang.org/grpc v1.84.0/go.mod h1:ljCht0DrxQrXBDRTZp52Qxh3Ffk8CdYm2sj4O2QN2C0=
```

Source containment is verified; reconciliation of the official database record remains outstanding. Moving to a development
pin, or downgrading to another stable backport solely to change scanner output, does not address missing code here.

### Buildbarn's grpc 1.81.1: affected version, inactive xDS server path

This is independent of Cucina's Go module. Four existing **darwin/arm64** release binaries were inspected with `go version -m`
without executing them. Each reports **Go 1.27.1 / grpc 1.81.1**. SHA-256 measurements match [the binary pins](../../tools/pinned.bzl):

| Binary | Release | Measured SHA-256 |
| --- | --- | --- |
| `bb_storage` | `20260930T153215Z-086b011` | `ca879430c4383cb723144eef3e3069ff8cf58422bd7f0a54c7e4290603139bc3` |
| `bb_scheduler` | `20260930T173749Z-1a3be95` | `6d28e3852ca8ece61c23c7146690ca6313fb01f62b836b2120771bb49812c7ce` |
| `bb_worker` | `20260930T173749Z-1a3be95` | `67fbcf00951b7e1ca90aa07781b496795b3d9f7b588c8f03e59ecbe3298656ff` |
| `bb_runner` | `20260930T173749Z-1a3be95` | `0c8ffbf17da304808a1580d430e0eaeea4dba47fd04b5f2434348980ec181105` |

The advisory requires server-side xDS routing after transport establishment. At grpc 1.81.1 commit
`caf0772c2bcb8bc15d43eb53448e921f34f0b7e8`, [`xds.NewGRPCServer` installs the routing interceptors][xds-factory];
[`RouteAndProcess` requires an xDS connection wrapper and usable routing configuration before indexing `authority[0]`][xds-routing].
TLS or JWT alone is not the basis of this assessment.

The inspected Buildbarn [storage server factory][storage-factory] at `086b011866fe9dc69c1172a209ddb78fb5ca3127` is byte-identical
to the [execution dependency's factory][execution-factory] at `ae61334ea7982155ceb317e286e2a462d875477d`. Both unconditionally
call ordinary `grpc.NewServer` at line 178; neither the factory nor its [server configuration proto][server-proto] offers an
xDS selection. No xDS server constructor/import or direct routing call was found in the three pinned Buildbarn production
source trees. Execution source is `1a3be95748727e2f03660925916ab39ee6c79d17`:

| Runtime source | Inspected Cucina configuration |
| --- | --- |
| [Storage main:192][storage-main], ordinary server factory | [Chart](../../charts/cucina/templates/_configs.tpl): frontend/shard `grpcServers`; [host L2](../../internal/bbconfig/hostl2.json.tmpl): `grpcServers`, TLS and client-certificate policy. |
| [Scheduler main:190–214][scheduler-main], ordinary server factory | Chart: `clientGrpcServers`, `workerGrpcServers`, `buildQueueStateGrpcServers`. |
| [Runner main:160][runner-main], ordinary server factory | [Runner renderer](../../internal/bbconfig/runner.go): `grpcServers[].listenPaths`, including Linux, Windows and Darwin fixtures. |
| [Worker main:74,368][worker-main], outbound scheduler/runner clients | [Worker renderer](../../internal/bbconfig/worker.go): scheduler/storage/runner client endpoints; `global.diagnosticsHttpServer` is HTTP, not an xDS gRPC listener. |

Buildbarn's service-name relays and instance-name scheduler demultiplexing are not xDS virtual-host routing. **The inspected
runtime/configuration paths do not activate this advisory's xDS precondition; grpc 1.81.1 is not thereby patched.**

## Decision

Retain the existing product dependency and binary pins. Record these two distinct applicability findings without an allowlist,
suppression, output filter, configuration-schema change or gate-status rewrite. Preserve the raw exit **3** and report; this ADR
does not turn them into a pass. No fixed unmodified upstream Buildbarn replacement was verified, so no replacement pin is asserted.

## Consequences

This is a static, exact-pin/source/configuration assessment, not a live deployment attestation, all-platform binary verification
or blanket vulnerability waiver. Reassess changed pins, binary provenance, server factories/imports and rendered configurations.
**Future xDS server activation invalidates the Buildbarn assessment** and requires a fresh remediation decision before activation.
Keep the upstream-fixed-binary upgrade path under review, without inventing releases or changing schemas. The four module-only
advisories above are retained as scoped scanner reports, not independently established nonapplicability. The bounded source
review did not execute release binaries or send exploit requests.

[go-db]: https://vuln.go.dev/ID/GO-2026-6443.json
[advisory]: https://github.com/grpc/grpc-go/security/advisories/GHSA-2v4p-qf9q-27wj
[grpc-tag]: https://proxy.golang.org/google.golang.org/grpc/@v/v1.84.0.info
[fix]: https://github.com/grpc/grpc-go/pull/9365
[backport]: https://github.com/grpc/grpc-go/commit/d5a41119e0e3189ea913cf839586ce34a44f1a3f
[fixed-source]: https://github.com/grpc/grpc-go/blob/e84aa5ab15d1d2b29d54f838312ad490cb7551a8/internal/transport/http2_server.go#L525-L530
[checksums]: https://sum.golang.org/lookup/google.golang.org/grpc@v1.84.0
[xds-factory]: https://github.com/grpc/grpc-go/blob/caf0772c2bcb8bc15d43eb53448e921f34f0b7e8/xds/server.go#L75-L79
[xds-routing]: https://github.com/grpc/grpc-go/blob/caf0772c2bcb8bc15d43eb53448e921f34f0b7e8/internal/xds/server/routing.go#L39-L70
[storage-factory]: https://github.com/buildbarn/bb-storage/blob/086b011866fe9dc69c1172a209ddb78fb5ca3127/pkg/grpc/server.go#L46-L178
[execution-factory]: https://github.com/buildbarn/bb-storage/blob/ae61334ea7982155ceb317e286e2a462d875477d/pkg/grpc/server.go#L46-L178
[server-proto]: https://github.com/buildbarn/bb-storage/blob/086b011866fe9dc69c1172a209ddb78fb5ca3127/pkg/proto/configuration/grpc/grpc.proto#L150-L253
[storage-main]: https://github.com/buildbarn/bb-storage/blob/086b011866fe9dc69c1172a209ddb78fb5ca3127/cmd/bb_storage/main.go#L192
[scheduler-main]: https://github.com/buildbarn/bb-remote-execution/blob/1a3be95748727e2f03660925916ab39ee6c79d17/cmd/bb_scheduler/main.go#L190-L214
[runner-main]: https://github.com/buildbarn/bb-remote-execution/blob/1a3be95748727e2f03660925916ab39ee6c79d17/cmd/bb_runner/main.go#L160
[worker-main]: https://github.com/buildbarn/bb-remote-execution/blob/1a3be95748727e2f03660925916ab39ee6c79d17/cmd/bb_worker/main.go#L74
