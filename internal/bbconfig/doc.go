// SPDX-License-Identifier: FSL-1.1-ALv2

// Package bbconfig renders the Buildbarn configuration that runs outside the
// cluster: bb_worker and bb_runner on every worker VM (EC2 Linux/Windows, Tart
// macOS VMs) and the host-level L2 bb_storage cache on Mac hosts (R-CACHE-2/-3/-4,
// R-DATA-3, R-RE-1/-4, R-SEC-2/-5). The cluster-side configuration (frontend,
// storage shards, scheduler) is rendered by the Helm chart.
//
// Inputs are split in two (docs/contracts.md §4):
//
//   - cucinav1.WorkerSettings: pool-level settings the controller hands out at
//     enrollment (EnrollWorker for EC2, IssueVMIdentity for Tart VMs).
//   - Machine: facts only the machine knows (vCPUs, memory, volumes, paths, the
//     CA bundle), filled in by cucina-worker-agent (EC2) or cucina-hostd (Tart).
//
// # Two Buildbarn schemas (ADR 0001)
//
// bb_worker, bb_runner and bb_scheduler are built against bb-storage
// ae61334ea798, whose `local` blob access uses the OLD flat schema
// (keyLocationMapOnBlockDevice, keyLocationMapMaximumGetAttempts, …). The
// repository's go.mod pins exactly that bb-storage, so RenderWorker and
// RenderRunner build the typed Go protobuf messages of the release binaries and
// marshal them with protojson: a field the binaries do not know cannot compile.
//
// bb_storage (frontend, shards, host L2) is pinned at 086b011, whose `local`
// blob access uses the NEW nested keyLocationMap (lossymap) schema. No Go types
// for it can coexist with the old ones in one binary, so RenderHostL2 renders a
// text/template from a typed input struct; the result is validated by unit tests
// (everything except the `local` block type-checks against the old types, which
// are identical elsewhere) and by booting the pinned bb_storage binary
// (internal/bbconfig/boottest).
//
// Every Buildbarn binary parses its configuration as Jsonnet and then strict
// protojson: an unknown field aborts start-up. docs/dev/buildbarn.md lists every
// field this package sets, with the proto file and line at the pinned tags.
package bbconfig
