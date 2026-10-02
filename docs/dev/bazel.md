<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Bazel in Cucina

Cucina is one Bzlmod workspace on **Bazel 9.2.0** (`.bazelversion`, run through Bazelisk).
`bazel build //... && bazel test //...` is the canonical build (R-BUILD-1). `go.mod` and
`Cargo.toml`/`Cargo.lock` stay the dependency sources of truth, so `go test ./...` and
`cargo test` keep working for IDEs.

> On the dev Mac the `bazel` mise shim has no version; use `bazelisk` (same thing) or
> `mise use -g bazelisk` and alias `bazel=bazelisk`.

## First-time setup

```sh
tools/setup-user-bazelrc.sh           # writes the gitignored user.bazelrc
tools/bazel-warmup.sh                 # optional: prefetch every external repository
```

`user.bazelrc` puts the output user root, the disk cache (GC'd at 100 GB / 21 days) and the
repository cache on `$CUCINA_DEV_STORAGE/bazel/` (`/Volumes/Code/Sources/.dev-storage/cucina/bazel`).
Never put machine paths in the committed `.bazelrc`.

## Everyday commands

| What | Command |
| --- | --- |
| Build / test everything | `bazel build //... && bazel test //...` |
| One tier | `bazel test //... --test_tag_filters=tier-unit` (`tier-static`, `tier-integration`) |
| A Go package | `bazel test //internal/scaling/...` |
| A Rust target | `bazel build //cli/cucinactl` · `bazel test //cli/...` |
| Regenerate Go BUILD files | `bazel run //:gazelle -- <your dirs>` (whole repo: `bazel run //:gazelle`) |
| Regenerate checked-in protobuf code / goldens | `bazel run //:update_goldens` (or `bazel run //<pkg>:<name>`) |
| Format / lint Starlark | `bazel run //:buildifier` · check: `bazel test //:buildifier_check` |
| Clippy | `bazel build --config=clippy //cli/...` |
| Flake check for new tests | `bazel test //<pkg>:<test> --runs_per_test=20` |
| Cross-compile `cucinactl` | `bazel build //cli/cucinactl --platforms=@rules_rs//rs/platforms:x86_64-unknown-linux-musl` (also `aarch64-unknown-linux-musl`; Windows: `x86_64-pc-windows-gnullvm`, ADR 0103) |
| Cross-compile C/C++ | `bazel build //tools/hello/cc:hello --platforms=@llvm//platforms:linux_x86_64_gnu.2.28` |
| OCI image (no Docker) | `bazel build //tools/hello/image:hello` → `bazel-bin/tools/hello/image/hello/` (OCI layout) |

## Declaring tests: tiers (R-TEST-2, R-TEST-8h)

Every test has exactly one tier. Use the symbolic macros from `//bazel:tiers.bzl`; they set
`size`, the `tier-<name>` tag and the tier's extra tags:

| Tier | Macro argument | Bazel size / tags | Runs in |
| --- | --- | --- | --- |
| static | `tier = "static"` | small | `bazel test //...` |
| unit | `tier = "unit"` | small (< 1 s, no I/O, no sleeps) | `bazel test //...` |
| integration | `tier = "integration"` (+ `envtest = True` for Go) | medium (< 30 s, localhost) | `bazel test //...` |
| simulation | `tier = "simulation"` | large, `manual` | nightly (`--config=nightly`) |
| system | `tier = "system"` | large, `manual`, `requires-docker`, `no-remote-exec` | dedicated kind lane |
| acceptance | `cucina_scenario(...)` | enormous, `manual`, `no-remote-exec` | campaign (`--config=e2e`) |

```starlark
load("//bazel:tiers.bzl", "cucina_go_test", "cucina_rust_test", "cucina_sh_test", "cucina_scenario")

cucina_go_test(name = "plan_test", srcs = ["plan_test.go"], embed = [":scaling"], tier = "unit")
cucina_go_test(name = "reconcile_test", srcs = [...], deps = [...], tier = "integration", envtest = True)
cucina_go_test(name = "boot_test", srcs = [...], tier = "integration",
               runfiles_env = {"BB_STORAGE": "@bb_release//:bb_storage"})
cucina_rust_test(name = "cli_test", crate = ":cucinactl_lib", tier = "unit")
cucina_sh_test(name = "chart_lint_test", srcs = ["lint.sh"], data = ["//tools:helm"], tier = "static")
```

* Gazelle emits `cucina_go_test` (`# gazelle:map_kind` in the root `BUILD.bazel`); add
  `tier = "..."` once — Gazelle keeps it. A missing tier is an analysis error.
* Any other `*_test` rule (third-party macros such as `write_source_files`, `cc_test`,
  `buildifier_test`) takes `tags = ["tier-<name>"]`.
* The guard: `.bazelrc` applies `//bazel:tier_check.bzl%tier_tags_aspect` to every top-level
  target, so `bazel build/test //...` fails on a test without a `tier-*` tag
  (`//tools:tier_tags_test` proves the guard rejects one). See ADR 0104.
* Never tag `exclusive` (it disables remote execution); use `exclusive-if-local`.
* `envtest = True` adds the pinned envtest-v1.36.2 assets and `KUBEBUILDER_ASSETS`
  (`//tools/envtest:assets`, built from `@envtest`; darwin/linux/windows).
* `runfiles_env = {"BB_STORAGE": "@bb_release//:bb_storage"}` (cucina_go_test) adds the label
  to `data` and sets the variable to a path valid from the test's working directory. Don't
  put `$(rootpath ...)` in a go_test's `env`: rules_go runs the test from its package
  directory, so a runfiles-root-relative path doesn't resolve (this is why the
  `BB_STORAGE`/`BB_RUNNER` boot tests fail today).
* Quarantine: tag `quarantine` (+ issue, owner, 14-day expiry); default runs skip it, nightly runs it.

## Go

* Add a package: write the code, then `bazel run //:gazelle -- path/to/pkg`. Binaries are pure
  Go (`.bazelrc` sets `--@rules_go//go/config:pure`); a cgo target (only `cucina-hostd`) sets
  `pure = "off"`.
* Add a dependency (go.mod is shared — serialise with the lock):

  ```sh
  lockf $CUCINA_DEV_STORAGE/gomod.lock go get example.com/mod@vX.Y.Z
  # once code imports it, make it a direct requirement (do NOT run `go mod tidy`: it drops
  # requirements other agents added but haven't used yet):
  lockf $CUCINA_DEV_STORAGE/gomod.lock go mod edit -droprequire=example.com/mod -require=example.com/mod@vX.Y.Z
  bazel mod tidy        # updates use_repo(go_deps, ...) in MODULE.bazel
  bazel run //:gazelle -- path/to/pkg
  ```

  `bazel mod tidy` only exposes **direct** (`// indirect`-free) requirements as repositories.
* nogo runs the `go vet` analyzers (+ nilness) on every Go compile (`tools/nogo`);
  golangci-lint (`.golangci.yml`, v2) runs in CI: depguard bans gomock/mockery/testify-mock,
  testcontainers, zap and deprecated packages; forbidigo bans `time.Sleep` in `_test.go`.

## Protobuf (Go + Rust)

Generated code is checked in next to (Go) or inside (Rust) the consuming package; Bazel
regenerates it hermetically and a `tier-static` diff test fails on drift.

```starlark
load("@protobuf//bazel:proto_library.bzl", "proto_library")    # Gazelle writes this
load("//bazel:proto.bzl", "cucina_proto_go", "cucina_proto_rust")

cucina_proto_go(name = "v1_go", proto = ":v1_proto", importpath = "...", grpc = ["svc.proto"])
cucina_proto_rust(name = "gen_update", proto = "//api/proto/cucina/v1:cucinav1_proto")  # -> src/gen/{buffa,connect}
```

* Go output is byte-identical to `buf generate` (`buf.gen.yaml`, `paths=source_relative`): the
  plugin versions come from the go.mod `tool` lines (protoc-gen-go v1.36.12, protoc-gen-go-grpc
  v1.6.2, run with its default options), the protoc version in headers is normalised to
  `(unknown)`, and descriptor sets keep comments (ADR 0105). `//api/proto/cucina/v1` proves it.
* Rust uses connect-rust + buffa (ADR 0003, ADR 0106): protoc with the pinned protoc-gen-buffa /
  protoc-gen-buffa-packaging v0.9.2 and protoc-gen-connect-rust v0.9.1. Mount the checked-in trees
  with `#[path = "gen/buffa/mod.rs"] pub mod proto;` and `#[path = "gen/connect/mod.rs"] pub mod
  connect;`; the crate needs `buffa`, `buffa-types`, `connectrpc`, `serde`, `serde_json`.
  `//tools/hello/rust` is the working example. tonic/prost are banned by `deny.toml`.
* protoc is the prebuilt one from protobuf 36.2 (`--@protobuf//bazel/toolchains:prefer_prebuilt_protoc`).
* Imports are relative to `api/proto` (`# gazelle:proto_strip_import_prefix /api/proto`).

## Rust (rules_rs 0.0.112, rustc 1.98.1)

* Workspace root: `Cargo.toml`/`Cargo.lock` at the repo root, members under `cli/` (owned by
  the CLI agent: everything in `cli/**`; workspace plumbing — root `Cargo.toml`, `Cargo.lock`,
  `MODULE.bazel` crate config, `third_party/rust/`, `deny.toml`, `rustfmt.toml` — stays with the
  bazel agent).
* BUILD files are hand-written:

  ```starlark
  load("@crates//:defs.bzl", "aliases", "all_crate_deps")
  load("@rules_rs//rs:rust_library.bzl", "rust_library")
  rust_library(name = "lib", srcs = glob(["src/**/*.rs"]), aliases = aliases(),
               deps = all_crate_deps(normal = True))   # proc macros included; tests: normal_dev = True
  ```
* Add a dependency: edit `Cargo.toml` (prefer `[workspace.dependencies]`), then
  `cargo update -p <crate>` (or `cargo generate-lockfile`), then build with Bazel — `crate.from_cargo`
  re-resolves from the lockfile (no Bazel-specific lockfile). Keep `ring` out (cargo-deny bans it;
  aws-lc-rs is the only crypto provider).
* Crates with C code build from BCR modules through rules_rs overlays in `third_party/rust/`
  (`aws-lc-sys` → BCR `aws-lc` + bindgen, `zstd-sys` → BCR `zstd`). See ADR 0101 for the
  aws-lc-rs/aws-lc-sys pins.
* `cargo deny --all-features check` (CI) enforces `deny.toml`; `rustfmt.toml` is shared by
  `cargo fmt` and Bazel's `rustfmt_test`.

## C/C++

hermetic-llvm 0.8.24 (LLVM 23.1.2) is the only C/C++ toolchain (`.bazelrc` sets
`BAZEL_DO_NOT_DETECT_CPP_TOOLCHAIN=1`, `BAZEL_NO_APPLE_CPP_TOOLCHAIN=1` and an empty
`--xcode_version_config`). Load cc rules from `@rules_cc//cc:*.bzl` (Bazel 9 has no autoloads).
macOS targets resolve the SDK on the executing Mac through `@cucina_platforms` (R-XPLAT-8;
this supersedes the downloaded-SDK arrangement recorded in ADR 0102). Native builds use the
unversioned Xcode SDK; remote pool configurations pin the SDK version.

## Pinned tool binaries

`tools/pinned.bzl` pins helm v4.3.0, helm-unittest v1.2.0, kubeconform v0.8.0, buf v1.73.0,
controller-gen v0.21.0, gitleaks v8.30.1, promtool (Prometheus v3.15.0), tflint v0.64.0,
OpenTofu v1.13.1, envtest-v1.36.2 and the Buildbarn release binaries (bb-storage
`20260930T153215Z-086b011`, bb-remote-execution `20260930T173749Z-1a3be95`) for darwin_arm64,
linux_amd64, linux_arm64 and windows_amd64, each by checksum. Use the platform-selected labels:

| Label | Notes |
| --- | --- |
| `//tools:helm`, `//tools:kubeconform`, `//tools:buf`, `//tools:gitleaks`, `//tools:promtool`, `//tools:tflint`, `//tools:tofu`, `//tools:controller-gen` | single binaries |
| `//tools:helm_unittest_plugin` | the plugin files; `HELM_PLUGINS=$(dirname $(dirname <rootpath of unittest/plugin.yaml>))` |
| `@bb_release//:bb_storage` `:bb_scheduler` `:bb_worker` `:bb_runner` (also `//tools:bb_*`) | boot pinned Buildbarn in integration tests |
| `@envtest//:binaries` / `//tools/envtest:assets` | envtest assets; prefer `envtest = True` |

Kubernetes JSON schemas for kubeconform are vendored in `tools/kubeconform/schemas/` (k8s
v1.36.5, strict); run kubeconform with
`-schema-location 'tools/kubeconform/schemas/{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json' -kubernetes-version 1.36.5 -strict -skip CustomResourceDefinition`
(add kinds with `tools/kubeconform/vendor-schemas.sh`). `controller-gen` needs `go` on `PATH`
(it loads packages with `go list`): run it via `bazel run //tools:controller-gen -- ...` from a
shell with mise's Go, or wrap it in a `genrule` that puts `@rules_go//go` first on `PATH`.

## Containers (no Docker)

`cucina_go_image(name, binary, repository)` from `//bazel:oci.bzl` wraps a pure-Go binary at
`/usr/local/bin/<name>` on `gcr.io/distroless/static-debian13:nonroot` (pinned by digest in
`MODULE.bazel`) as a linux/amd64 + linux/arm64 `oci_image_index`; `:<name>_push` is an
`oci_push` (never run by tests). Build output: `bazel-bin/<pkg>/<name>/` is an OCI image layout
(`oci-layout`, `index.json`, `blobs/sha256/…`). Both architectures include `LICENSE.md` and
`THIRD_PARTY_NOTICES.md` under `/usr/share/doc/cucina` (R-ARTIFACT). The rules_oci 2.3.0
Windows launcher correction is in `third_party/oci/windows-launchers.patch`; it resolves source
scripts relative to their generated batch launchers and shares one descriptor launcher per image.

## Remote execution configs

* `--config=cucina` — Cucina RBE client defaults (R-DATA-1); the endpoint and credential
  helper come from `cucinactl bazelrc` (user.bazelrc). `--config=cucina-ci` adds
  `--remote_download_outputs=minimal`; `--config=cucina-readonly` stops uploads.
* `--config=cucina-macos` — macOS exec platform first. Execution-platform lists are imported
  from `bazel/platforms/cucina.bazelrc` (cross-platform agent).
* `--config=msvc` — the MSVC/Windows SDK EULA `--repo_env`s (accepted 2026-10-01); implied on
  Windows hosts.

## CI (.github/workflows/ci.yml)

The repository only allows GitHub-owned actions plus an allow-list (jdx/mise-action, ...),
pinned to full commit SHAs. Jobs install Bazelisk and other tools with `jdx/mise-action`
(`install_args`, versions from `mise.toml`) and keep Bazel's repository and disk caches with
`actions/cache` (the repo contents cache stays out of the saved cache). `--config=ci` sets
`--lockfile_mode=error`: after changing `MODULE.bazel`, `go.mod` or `Cargo.lock`, run a build
(or `bazel mod deps --lockfile_mode=update`, which records every extension) and commit
`MODULE.bazel.lock`. Windows runs use `--output_user_root=C:/b` and long paths. `.gitattributes`
keeps source text LF even with `core.autocrlf=true`, while preserving verbatim upstream licence
files and the notices aggregate. All three OS lanes build and test `//...`, including the Windows CLI. The pinned LLVM/rules_rust
compatibility patches are documented in [ADR 0107](../adr/0107-windows-native-toolchain-compatibility.md).

Linux frees unused preinstalled SDKs before building. `.bazelrc` clears repository
`ANDROID_HOME`: Cucina has no Android targets, and a dangling runner SDK path must not affect
C/C++ toolchain resolution. Each build/test lane has a four-hour timeout (below the hosted
six-hour limit). The observed macOS runner in run 37000869610 was `macos-26-arm64`; native builds
use the runner's unversioned Xcode SDK symlink, not the production pool's Xcode 27 pin. Docker
is not needed by these lanes.

The RBE lane runs only for trusted pushes/dispatches with `CUCINA_ENDPOINT` set to a TLS
hostname[:port] (optionally `grpcs://`-prefixed). It first builds `cucinactl` locally and installs
its credential-helper personality outside the checkout, then scopes that helper to the endpoint
host. GitHub OIDC is exchanged on demand; tokens are never written into `.bazelrc`. Optional
`CUCINA_URL` selects a separate HTTPS STS discovery URL. A configured deployment and matching
GitHub TrustPolicy are still required; a skipped RBE lane is not remote-execution evidence.

## Several agents on one machine

All agents share one workspace. A Bazel server serialises commands per output base
("Another command is running"), so concurrent agents should use their own output base and
share the repository cache (which also holds Bazel's repo contents cache) and the disk cache:

```sh
bazel --output_base=$CUCINA_DEV_STORAGE/bazel/ob-$CUCINA_AGENT test //your/pkg/...
```

Run Gazelle only on your own directories (`bazel run //:gazelle -- internal/scaling`), and
`bazel mod tidy` after changing `go.mod`.

## Measurements (dev Mac, M4 Max, 2026-10-02)

Wall-clock times from `$CUCINA_DEV_STORAGE/logs/`:

| Scenario | Time |
| --- | --- |
| Cold, empty caches: first `bazel run //tools/hello/hellopb:hellopb_go` (Go SDK, prebuilt protoc, protoc-gen-go/-grpc built) | 134 s |
| Cold: first `bazel test //tools/hello/...` adding C++/Rust (LLVM 23 + llvm-project sources, Rust 1.98.1, crates, libc++/compiler-rt and AWS-LC compiled) | 407 s |
| `bazel build //...`, whole repo, warm repository cache, disk cache cold for the new output paths (5,762 actions) | 230 s |
| Warm `bazel build //...` / `bazel test //...` (89 tests) | 21 s / 14 s |
| New output base (`--output_base=…/ob-<agent>`), shared repository + disk cache, `//tools/... //api/proto/...` (21 tests) | 27 s, 0.44 GB |
| Same, but without the disk cache (everything compiled) | 167 s, 3.1 GB |
| Cross: C++ hello → Linux gnu/musl x86_64/aarch64 | 10–43 s each |
| Cross: Rust `hello_rs` → x86_64/aarch64 musl (cold for the target) · → x86_64 Windows gnullvm | 167/193 s · 92 s |

Per-agent output bases are therefore cheap as long as the repository cache (which holds
Bazel's repo contents cache) and the disk cache are shared, as `user.bazelrc` sets up: a fresh
output base reaches green in ~30 s instead of ~3 min, and costs ~0.5 GB plus one Bazel server.

## Known gotchas

* Bazel 9 has no autoloads: load `sh_test` from `@rules_shell`, `proto_library` from
  `@protobuf//bazel:proto_library.bzl`, cc rules from `@rules_cc`.
* `--experimental_remote_merkle_tree_cache` is gone in Bazel 9; don't pass it.
* rules_go enables cgo whenever a C toolchain resolves — that's why `.bazelrc` forces pure Go.
* Starlark flags in `.bazelrc` reference `@rules_cc`, `@protobuf`, `@rules_go`: keep those
  `bazel_dep`s in `MODULE.bazel`.
* The first build compiles hermetic-llvm's runtimes (libc++, compiler-rt, musl, glibc stubs) per
  target from source: expect a slow cold build; the disk cache makes it a one-time cost.
* Output directories are named `bazel-out/platform-<hash>-fastbuild/` (hash of the platform
  label; `--noexperimental_use_platforms_in_output_dir_legacy_heuristic`). The legacy names made
  the host platform (`darwin_arm64`) collide with `@rules_go//go/toolchain:darwin_arm64`
  (`go_cross_binary`) → "conflicting actions". Use `bazel-bin/...` or `bazel cquery --output=files`.
* rules_go runs a `go_test` from its package directory, so paths passed through `env` must be
  package-relative or resolved with runfiles (`envtest = True` handles `KUBEBUILDER_ASSETS`).
* Go modules that ship their own Bazel BUILD files (bb-storage, bb-remote-execution,
  remote-apis, cel-go, go-jsonnet) get their BUILD files regenerated by Gazelle
  (`go_deps.gazelle_override(build_file_generation = "clean")` in MODULE.bazel). Add a module
  there if `bazel build` reports `unknown repo 'rules_go'/'protobuf'/'grpc'` inside `@com_github_...`.
* gitleaks (`//tools:gitleaks_test`) flags fake tokens/keys in tests too: build them at run time
  (e.g. sign a JWT in the test) instead of committing literals; never allowlist secret-like values.
* Windows release binaries still use `x86_64-pc-windows-gnullvm` (ADR 0103). Native Windows
  builds use MSVC and execute the CLI tests, with the narrowly scoped toolchain patches in
  ADR 0107; do not restore the former CI path exclusions.
* `bazel/platforms` is its own Bzlmod module (`@cucina_platforms`), listed in `.bazelignore`
  so `//...` doesn't load it as main-repo packages; wire it with `bazel_dep` +
  `local_path_override(path = "bazel/platforms")`.
