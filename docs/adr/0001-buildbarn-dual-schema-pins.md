# 0001 — Buildbarn version pins: worker and storage schemas differ

* Status: accepted (2026-10-02)

## Context
R-CP-2 requires one matched set of upstream date-tags. On 2026-10-02 the newest `bb-remote-execution` release
(`20260930T173749Z-1a3be95`) has a `go.mod` that pins `bb-storage` at `ae61334ea798` (2026-09-06). `bb-storage` changed the `local`
blob-access schema on 2026-09-29 (`keyLocationMap` lossymap replaces the flat `keyLocationMapOnBlockDevice`,
`keyLocationMapMaximumGetAttempts`, …). The release binaries of `bb_worker`/`bb_runner`/`bb_scheduler` therefore parse the **old**
schema for their embedded blobstore configuration, while any `bb_storage` built after 2026-09-29 parses the **new** one. The 2026-09-30
`bb-storage` fix ("discard buffers in BlobAccess.Put()") stops zstd decoders from leaking when an authorizer denies a write; with the
bounded `zstdPool` that R-DATA-3 requires, an older build can deadlock the frontend when a read-only client attempts an upload.

## Decision
Use release binaries (R-CP-2) from: bb-remote-execution `20260930T173749Z-1a3be95` for `bb_scheduler`, `bb_worker`, `bb_runner`; bb-storage
`20260930T153215Z-086b011` for every `bb_storage` use (frontend, storage shards, host L2). Render the worker L1 `local` block in the old flat
schema and every `bb_storage` `local` block in the new nested schema. `go.mod` pins `bb-storage` at `ae61334ea798` and `bb-remote-execution`
at `1a3be95`, so worker/runner/scheduler configuration is type-checked at compile time against exactly the schema of the release binaries;
`bb_storage` configuration is template-rendered and validated by booting the pinned `bb_storage` binary (strict protojson).

## Consequences
* Two schemas in one repository; `internal/bbconfig` documents which is which.
* When bb-remote-execution publishes a release depending on a post-2026-09-29 bb-storage, re-pin both and delete this skew (one-line ADR update).
* Config-render tests boot every pinned binary against every rendered profile, so drift fails CI.
