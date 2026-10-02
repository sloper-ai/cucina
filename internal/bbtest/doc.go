// SPDX-License-Identifier: FSL-1.1-ALv2

// Package bbtest boots the pinned Buildbarn release binaries (bb_storage,
// bb_scheduler, bb_worker, bb_runner) as child processes on loopback for
// integration-tier tests (R-TEST-2): no Docker, no network beyond localhost.
//
// It provides:
//
//   - Binary: locates a pinned binary from BB_STORAGE, BB_SCHEDULER, BB_WORKER
//     or BB_RUNNER (absolute, relative to the working directory, or a Bazel
//     runfiles path), falling back to $CUCINA_DEV_STORAGE/bb-release on a
//     developer machine; skips under `go test` when absent, fails under Bazel.
//   - Boot/BootStorage/BootScheduler/BootWorker/BootRunner: start a binary with a
//     configuration, capture its output, wait for readiness, stop it (SIGTERM,
//     then SIGKILL) at test cleanup and print its log tail when the test failed.
//   - NewPKI: a throwaway CA with server and SPIFFE-style client leaves
//     (crypto/x509, test only: never production PKI, never committed).
//   - StorageConfig/SchedulerConfig: minimal frontend/storage and scheduler
//     configurations for the pinned schemas (bb_storage NEW, bb_scheduler OLD).
//   - REClient: a small REAPI v2 client (CAS upload/download, Execute).
//   - FakeWorker: a hand-driven Synchronize client that registers runner threads
//     with a real bb_scheduler and executes tasks on command.
//
// Waiting is event driven where an event exists (gRPC connectivity changes, the
// process exiting, a Synchronize call arriving); nothing here sleeps.
package bbtest
