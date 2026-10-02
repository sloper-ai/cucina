<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Getting started: a development machine

How to set up a machine to work on Cucina and the commands you will use every day. Bazel is the canonical build; Go and Cargo workflows also work, so IDEs behave. For Bazel details see [`bazel.md`](bazel.md); for the test policy see [`TESTING.md`](../../TESTING.md).

## 1. Tools

Install [mise](https://mise.jdx.dev) and let it install the pinned developer tools from [`mise.toml`](../../mise.toml):

```sh
mise install            # bazelisk, go, rust, helm, kubectl, packer, opentofu, golangci-lint, gitleaks, buf, kubeconform, tflint, cargo-deny
```

The mise file also pins `shellcheck` for shell scripts and `actionlint` for workflows. Notices use cargo-about 0.9.2 (`mise exec cargo-about@0.9.2 -- tools/notices/generate.sh`); mutation testing uses `gremlins` and `cargo-mutants` ([`TESTING.md`](../../TESTING.md) section 8). Run the checks rather than assuming a checked-in script is lint-clean.

Bazel runs through **Bazelisk**, which reads the version from [`.bazelversion`](../../.bazelversion) (9.2.0). Use `bazelisk`; if you prefer `bazel`, alias it. Go is 1.27.1 and Rust is the stable 1.98 toolchain pinned by `rust-toolchain.toml`. You do not need a system C or C++ compiler or a Docker daemon:
the toolchains are hermetic, and images are built without Docker.

Hosts and platforms: development works on macOS (Apple silicon) and Linux. Windows-only and macOS-only pieces (the Windows worker image, the macOS host agent's cgo parts) are built on their own OS or through Cucina itself.

## 2. Machine-local settings

Bulk data (Bazel's output base, the disk cache and repository cache, Go and Cargo caches, Tart VM images) should not live in your home directory or in the repository. Pick a volume with plenty of space and no secrets on it, then let one script write the gitignored `user.bazelrc` that points Bazel at it:

```sh
export CUCINA_DEV_STORAGE=/path/to/big/volume/cucina      # optional; the default is ~/.cache/cucina
tools/setup-user-bazelrc.sh                                 # or: tools/setup-user-bazelrc.sh /path/to/dir
tools/bazel-warmup.sh                                       # optional: prefetch every external repository
```

Point the other tools at the same place if you like (`GOMODCACHE`, `GOCACHE`, `CARGO_HOME`, `CARGO_TARGET_DIR`, `TART_HOME`). **Secrets are the opposite:** kubeconfigs, OpenTofu state, generated keys and test credentials live under `~/.config/cucina/` (mode 0700), on an encrypted disk, never in the repository and never on the shared bulk volume.
This repository is public ([`AGENTS.md`](../../AGENTS.md) lists what must never be committed).

Enable the git hooks once per clone:

```sh
git config core.hooksPath .githooks        # commit-msg: the Test-Change rule. pre-push: gitleaks and a scan for environment identifiers
```

## 3. Build and test

```sh
bazelisk build //... && bazelisk test //...             # the canonical build; the first run compiles toolchains, later runs are cached
bazelisk test //path/to/pkg/...                         # one package
bazelisk test //... --test_tag_filters=tier-unit,-quarantine   # one tier (tier-static, tier-integration); keep -quarantine
bazelisk test //path:name_test --runs_per_test=20       # the flake check every new or changed test must pass
```

Several people or agents sharing a machine should give each checkout its own output base and share the caches: `bazelisk --output_base=<dir>/ob-<name> test //...`.

## 4. Working in each language

### Go (controller, host agent, worker agent, libraries)

* `go test ./internal/scaling/...` works for quick IDE feedback; Bazel runs the same tests with their tiers.
* **Add a package**: write the code, then `bazelisk run //:gazelle -- path/to/pkg` (Gazelle generates the BUILD files and emits `cucina_go_test`; add `tier = "..."` to each new test target once).
* **Add a dependency**: `go get example.com/mod@vX.Y.Z`, import it from code, then `bazelisk mod tidy` and re-run Gazelle for the package. New dependencies follow the dependency rule in [`AGENTS.md`](../../AGENTS.md) (modern, well known, maintained, licence on the allow-list; an ADR otherwise).
* Style and bans are in `.golangci.yml` (`golangci-lint run ./...`): no `time.Sleep` in tests, no mock frameworks, no zap, `log/slog` for logging.
* Pure Go is the default (`pure = "on"`); only the macOS host agent uses cgo, and it builds on macOS.

### Rust (`cucinactl`)

* `cargo test`, `cargo clippy` and `cargo fmt` work from the repository root (the workspace is under `cli/`). Bazel builds and tests the same crates: `bazelisk build //cli/cucinactl`, `bazelisk test //cli/...`.
* RPC and protobuf use connectrpc and buffa ([ADR 0003](../adr/0003-rust-rpc-stack-connect-rust-buffa.md)); the generated code is checked in. TLS uses one crypto provider (aws-lc-rs), and `cargo deny --all-features check` enforces the dependency rules in `deny.toml`.
* Run the CLI from source: `cargo run -p cucinactl -- --help`, or `bazelisk run //cli/cucinactl -- --help`.

### Protobuf and generated code

Generated code is checked in next to its source, and a test fails when it drifts. Regenerate it with `bazelisk run //:update_goldens` (the `buf generate` path in `buf.gen.yaml` is the IDE equivalent for Go). Never edit generated files by hand.

### Helm chart

```sh
helm lint --strict charts/cucina
bazelisk test //charts/cucina/...        # schema checks, kubeconform, helm-unittest, and the config-render check that boots the pinned Buildbarn binaries
```

### Local controller image for kind (pre-release)

Until a release image is published, kind needs an operator-built image loaded into its nodes. This is the local smoke-test path; the canonical release build remains `bazelisk build //cmd/cucina-controller:image`. It needs Docker and kind in addition to the tools above. Use a **dedicated local cluster**, not a production kubeconfig.

```sh
# Run from the repository root; the kind nodes and image use the host architecture.
IMAGE_DIR=$(mktemp -d)
IMAGE_ARCH=$(go env GOARCH)
GOOS=linux GOARCH="$IMAGE_ARCH" CGO_ENABLED=0 go build -o "$IMAGE_DIR/cucina-controller" ./cmd/cucina-controller
cp THIRD_PARTY_NOTICES.md "$IMAGE_DIR/"
docker build --platform "linux/$IMAGE_ARCH" -t cucina-controller:dev -f - "$IMAGE_DIR" <<'DOCKERFILE'
# SPDX-License-Identifier: FSL-1.1-ALv2
FROM gcr.io/distroless/static-debian13@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --chmod=0555 cucina-controller /usr/local/bin/cucina-controller
COPY THIRD_PARTY_NOTICES.md /usr/share/doc/cucina/THIRD_PARTY_NOTICES.md
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/cucina-controller"]
DOCKERFILE
mise exec kind@0.30.0 -- kind create cluster --name cucina-dev
mise exec kind@0.30.0 -- kind load docker-image cucina-controller:dev --name cucina-dev
rm -rf "$IMAGE_DIR"
cargo build --locked -p cucinactl --bin cucinactl
export PATH="${CARGO_TARGET_DIR:-$PWD/target}/debug:$PATH"
```

The distroless digest is the one pinned in `MODULE.bazel`; keep them together when updating it. The [README quickstart](../../README.md#quickstart) supplies the matching chart values and local port-forwards. For k3s or EKS, make the canonical image available through your own registry (or your runtime's local OCI import) and set `images.controller.repository`, `tag` or `digest` accordingly; the default pre-release GHCR tag is not available yet.

### Infrastructure and images

OpenTofu for the acceptance environment is under `deploy/aws-e2e` ([`docs/operations/aws-e2e.md`](../operations/aws-e2e.md)); Packer templates for the worker images are under `workers/` ([`images.md`](../operations/images.md), [`macos-images.md`](../operations/macos-images.md)). Both create real, billable cloud resources:
read their docs before running anything, and tag and destroy everything you create.

## 5. Docs and policy tooling

| Task | Command |
| --- | --- |
| Check the git hooks and policy scripts | `tools/ci/test-githooks.sh && tools/ci/test-ci-scripts.sh && tools/notices/test.sh` |
| Quarantine and flake policy | `tools/ci/check-quarantine.sh` |
| Manual-check structure | `tools/ci/check-manual-tests.sh` |
| Every chart alert has its row in the operations README | `tools/ci/check-alert-runbooks.sh` |
| Mutation sweep of the git hooks (a few minutes) | `tools/ci/mutate-hooks.sh`, when you change `.githooks/` |
| ADR index (generated) | `tools/ci/adr-index.sh` after adding or retitling an ADR |
| Third-party notices (generated) | `tools/notices/generate.sh`, before releases and when dependencies change |
| Test-Change check over your branch | `.githooks/lib/test-change.sh range origin/main..HEAD` |

## 6. Before you open a pull request

1. `bazelisk build //... && bazelisk test //...` passes, and new or changed tests pass `--runs_per_test=20`.
2. You read your own diff for secrets and environment identifiers (the pre-push hook checks, but you are the first line).
3. Docs are updated, anything unbuilt is marked as planned, and a deviation from the plan has an ADR ([`docs/adr/README.md`](../adr/README.md)).
4. Commit messages: an imperative subject, the reason in the body, `Red:` for bug fixes, `Test-Change:` where [`TESTING.md`](../../TESTING.md) requires it.
5. The pull request template asks for the requirements covered, the tests and their tiers, and the evidence.

## Troubleshooting

* **"Another command is running"**: one Bazel server serialises commands per output base; use a separate `--output_base`.
* **Slow first build**: the first build compiles hermetic-llvm's runtimes per target; the disk cache makes it a one-time cost. `tools/bazel-warmup.sh` prefetches external repositories.
* **`bazel` is not found, or a mise shim says no version is set**: use `bazelisk`, or `mise use -g bazelisk`.
* **A hook blocks a commit**: read its message; it names the finding and the line to add ([`TESTING.md`](../../TESTING.md) section 6.2).
