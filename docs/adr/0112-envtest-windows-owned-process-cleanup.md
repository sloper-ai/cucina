<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0112 — Reap envtest children before removing Windows state

* Status: accepted (2026-10-02); actual native Windows validation pending

## Context

Windows run 37074297279 at `217b0c1` started etcd and kube-apiserver successfully. The public
`TestControlPlaneStarts` failed after 2.97 seconds: both `Environment.Stop()` calls reported
“not supported by windows”. The server continued running after its certificate directory was
removed; Bazel eventually reported the enclosing test's 300-second timeout. This was a process
lifetime failure, not slow startup, a disk-capacity problem or the earlier macOS hostname issue.

Pinned controller-runtime v0.24.1 sends `SIGTERM` through `os.Process.Signal` on Windows,
returns on that unsupported operation, and still runs deferred directory removal. Go supports
`Process.Kill` through an owned Windows process handle, not those Unix signals. Upstream
[PR 3519](https://github.com/kubernetes-sigs/controller-runtime/pull/3519) uses `Process.Kill`;
there is no v0.24.x backport. The later v0.25.2 requires Kubernetes v0.37 and has a separate
Windows redeclaration regression ([PR 3586](https://github.com/kubernetes-sigs/controller-runtime/pull/3586)).
A public wrapper cannot repair the old lifetime: the started command/handle is private, and
calling the old Stop first already removes state. No unsafe private-field access is acceptable.

## Decision

Maintain a **test-only generated source closure**, not a fork of the controller implementation:

* `internal/envtest` contains the 25 Go files (about 137 KiB) needed from v0.24.1's public envtest
  package and six private support packages. Public client/log/webhook imports still use the
  original module. Original Apache-2.0 copyright notices and the exact upstream LICENSE remain.
  The two additional local platform adapters and their Windows lifecycle regression are
  identified as Cucina-authored FSL code.
* `tools/envtest/upstream.json` pins the source module zip SHA-256
  `8b0383d6842b4dc0bdd35e5417e2fe997417066fc59439bfe1ec0972b8df29b0` and every copied file hash.
  The generator verifies these, performs deterministic import relocation, applies exact
  replacements from `lifecycle.json` and adds the three reviewed templates. Do not edit the
  generated Go files. `//tools/envtest:drift_test` reconstructs and compares all outputs and
  the complete actual/expected `.go` inventory, including obsolete or new-package sources.
  It is an explicitly **local, uncached static gate** (`local`, `external` tags): a sandbox or
  remote action cannot see undeclared checkout sources, which native Go would still compile.
  A local repository rule supplies only this checkout's `internal/envtest` path directly to
  the test environment; no source-root lookup depends on Unix symlinks or Windows runfiles/
  manifest layout. Native and Bazel checks invoke the same generator `--check`. Other inputs
  remain declared runfiles; no global sandbox setting changes or other source trees are scanned.
* Only Windows Stop is intercepted. Terminate the directly owned `*os.Process` handle, wait
  for the existing `cmd.Wait` goroutine within the unchanged stop budget, then remove generated
  data/certificate directories. Resolve termination errors only after that bounded join:
  Go 1.27 can release a reaped Windows handle before State publishes exit, making `Kill`
  return `EINVAL`, not `ErrProcessDone`. Either is benign only after the join. Other kill
  errors remain errors, and a timeout preserves both the deadline and any kill error while
  retaining state. No blanket error suppression is used. Never discover processes by name,
  kill global process trees, or assume a successful terminate call means the child has exited.
* The original non-Windows Stop body, process-group attributes and TERM/KILL signaling remain
  byte-for-byte unchanged. No timeout, skip, retry or assertion is relaxed.
* Four test consumers import this local public Environment API. All generated libraries are
  `testonly` in Bazel; native Go and Bazel compile the identical checked-in files. The production
  command dependency graph does not include the closure. `go.mod` has no replacement and the
  Go module cache is never modified.

## Verification and removal

The existing native Windows failure is the original red evidence. The real integration test
checks Start, ServerVersion, Stop, closed API/etcd ports, and removal of generated state. It
explicitly chooses `UseExistingCluster=false`, tested against an ambient
`USE_EXISTING_CLUSTER=true` and a deliberately absent kubeconfig, so it always owns the children.
The existing drift table rejects extra root and nested Windows-only sources; both inventory
rows and the ambient-cluster case failed before their fixes. A Windows-only lifecycle table
uses a genuinely reaped child and `testing/synctest` to hold exit publication back, checking
joined cleanup versus an unjoined timeout with state retained. It calls public `State.Stop`;
there is no production timing hook. The race is source-verified and its pre-fix binary was
cross-compiled, but that is **not an observed native Windows red/green run**. The real control
plane test still executes on all three supported hosts; no simulated POSIX signal, relaxed
deadline, or platform skip replaces Windows execution evidence.

Verified regeneration (after the pinned original dependency is in the Go module cache):

```sh
go run ./tools/envtest/cmd/sync --archive "$GOMODCACHE/cache/download/sigs.k8s.io/controller-runtime/@v/v0.24.1.zip" --manifest tools/envtest/upstream.json --patches tools/envtest/lifecycle.json --out internal/envtest
bazelisk test //tools/envtest:envtest_test //tools/envtest:drift_test //internal/controller:controller_test //internal/reconcile:reconcile_test --runs_per_test=20 --nocache_test_results
```

The cost is seven small test-only packages, provenance/templates and a generator/drift gate.
Remove the closure and restore the four upstream imports when a compatible, maintained upstream
release compiles on Windows and proves owned child termination/reaping before state removal.
Retain the public lifecycle regression when doing so. A Kubernetes minor upgrade is a separate
contract/dependency decision, not a side effect of fixing test cleanup.
