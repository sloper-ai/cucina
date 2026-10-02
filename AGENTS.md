<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Instructions for coding agents

Cucina is a managed Buildbarn distribution: Bazel remote execution and caching with zero-idle-cost EC2 pools and macOS Tart pools,
installed with one Helm chart. This file is for coding agents and contributors alike; `CLAUDE.md` imports it.

## Read first

1. [`TESTING.md`](TESTING.md) is binding. Read it before you write or change a test.
2. [`docs/architecture.md`](docs/architecture.md) and [`docs/contracts.md`](docs/contracts.md): how the parts fit, and the contracts between them.
3. [`docs/dev/getting-started.md`](docs/dev/getting-started.md): tools and everyday commands.

## Rules

1. **Bazel is canonical.** `bazelisk build //... && bazelisk test //...` must pass. `go test ./...` and `cargo test` also work for IDEs, because `go.mod` and
   `Cargo.toml`/`Cargo.lock` stay the dependency sources of truth. Declare every test with a tier macro from `bazel/tiers.bzl`.
2. **Red first.** A bug fix starts with a failing test. Never weaken, skip or delete a test, and never regenerate a golden, to make a build green. If a test is
   wrong, say so and change it with a `Test-Change: <reason>` line in the commit message (TESTING.md §6.2). Enable the hooks once: `git config core.hooksPath .githooks`.
3. **Tests are admitted, not accumulated**: cite what you guard, assert behaviour through a public interface, use the fakes in `internal/fakes` (no mock frameworks), no sleeps
   (fake clocks), one test per acceptance criterion, table rows over new tests. Do not test third-party behaviour, generated code, getters, log text or private helpers.
4. **This repository is public.** Never commit secrets, private keys, kubeconfigs, OpenTofu state, certificates, test credentials, AWS account IDs, IPs or
   hostnames of a test environment, or personal data. Those live under `~/.config/cucina/` (mode 0700). Bulk data (caches, VM images) lives outside the repository.
5. **SPDX header** on every source file you write: `SPDX-License-Identifier: FSL-1.1-ALv2` in the file's comment syntax (not needed in generated code, JSON or lockfiles).
6. **Contracts change deliberately.** `api/proto`, `api/v1alpha1`, `platforms/pools.json`, `internal/ports` and `docs/contracts.md` are shared by many components. Change the contract,
   the code and the docs together. Generated code is checked in: regenerate it (`bazelisk run //:update_goldens`), never edit it by hand.
7. **Record deviations.** If you cannot meet a requirement as written, pick the closest alternative that keeps its intent and write an ADR (`docs/adr/README.md` has the format and the numbering ranges).
8. **Stack.** Go 1.27: `log/slog` JSON logging (no zap), `testify`, `rapid`, `testing/synctest`. Rust 1.98 (`rust-version = "1.88"`): connectrpc and buffa for RPC and protobuf, a single
   crypto provider (aws-lc-rs, never `ring`), no YAML (JSON or TOML only), no `mockall`. Buildbarn parses its configuration strictly: an unknown field aborts startup, so check the pinned protos.
9. **Commits.** Imperative subject, the why in the body, `Red: …` for bug fixes, `Test-Change: …` where TESTING.md requires it. Do not commit, push or open pull requests unless you were asked to.
10. **Docs.** Concise, with commands you have run. Mark anything that does not exist yet as planned. Never document a command you have not checked.
11. **Dependencies.** Prefer a modern, widely used, maintained library to hand-rolled code. Its licence must be on the allow-list (`tools/notices/policy.json`, checked when the notices are regenerated),
    `go.mod` and `Cargo.toml` pin the versions, and `.golangci.yml` (depguard) and `deny.toml` list the banned ones. A choice that is not the obvious one gets an ADR.
12. **Cloud resources.** Anything created in AWS for a test carries the `cucina:env`, `cucina:run` and `cucina:expires` tags and is destroyed afterwards (`deploy/aws-e2e`). Never touch account-level settings.

## Where things live

| Path | What |
| --- | --- |
| `api/` | CRDs (`v1alpha1`, `crds/`) and the protobuf API (`proto/cucina/v1`) |
| `cmd/`, `internal/` | Go: `cucina-controller`, `cucina-hostd`, `cucina-worker-agent`; ports, fakes, scaling, providers, auth, PKI, config rendering |
| `cli/` | Rust: `cucinactl` (CLI and TUI) and its generated API crate |
| `charts/cucina/` | The Helm chart and the Buildbarn configuration it renders |
| `workers/{linux,windows,macos}/` | Packer templates and provisioning for the worker images |
| `macos/` | Host package (`pkg/`), MDM profiles (`profiles/`) |
| `deploy/aws-e2e/` | OpenTofu for the temporary acceptance environment, and its scripts |
| `sim/`, `invariants/`, `slo/`, `test/e2e/` | Simulation, shared invariants and SLOs, the scenario harness |
| `platforms/`, `bazel/` | The platform catalog; Bazel macros and generated platforms |
| `docs/` | Architecture, operations runbooks, ADRs, manual checklists, upstream patches |
| `.githooks/`, `tools/ci/`, `tools/notices/` | Git hooks, policy checks, third-party notices generator |

## Everyday commands

```sh
bazelisk build //... && bazelisk test //...             # the canonical build
bazelisk test //path/to/pkg:name_test --runs_per_test=20   # flake check for a new test
bazelisk run //:gazelle -- path/to/pkg                  # regenerate Go BUILD files
bazelisk run //:update_goldens                          # regenerate goldens (needs Test-Change)
tools/ci/test-githooks.sh && tools/ci/test-ci-scripts.sh   # tests of the policy tooling
tools/ci/mutate-hooks.sh                                # mutation sweep of the git hooks (minutes; run when you change them)
tools/notices/generate.sh                               # regenerate THIRD_PARTY_NOTICES.md
```
