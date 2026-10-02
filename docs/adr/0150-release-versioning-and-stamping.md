<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0150 — One release version, stamped by Bazel from the VERSION file

* Status: accepted (2026-10-02)

## Context
R-OPS-7 asks for one SemVer version (from 0.1.0) across the chart, images, CLI, macOS package and
host agent, with release artifacts built by Bazel (R-BUILD-3). The binaries had no common
mechanism: the Go mains default to `dev` (`-X` was planned but unwired), `cucinactl` reported
`env!("CARGO_PKG_VERSION")`, which is `0.0.0` both under Bazel (rules_rust's default `version`)
and Cargo (workspace version). A configuration flag (`--//...:version=`) was rejected: it puts
the version into the configuration, so every version rebuilds all Go and Rust code, and
global `extra_rustc_env` would recompile every crate. Installer compares dotted numbers, so the
macOS package cannot carry a SemVer pre-release (ADR 0753).

## Decision
* `VERSION` (repo root) holds `MAJOR.MINOR.PATCH[-PRERELEASE]` (SemVer 2.0.0, no build metadata:
  OCI tags reject `+`). A release tag is `v` + VERSION; the workflow refuses a mismatch.
* Release builds run `bazel build --stamp --workspace_status_command=release/workspace-status.sh`.
  It publishes `STABLE_CUCINA_VERSION` (VERSION, or `$CUCINA_VERSION` for dry runs),
  `STABLE_CUCINA_COMMIT`, `STABLE_CUCINA_SOURCE_DATE_EPOCH` (commit time),
  `STABLE_CUCINA_REPOSITORY` and `STABLE_CUCINA_DIRTY`. Build metadata and OCI labels preserve
  the dirty bit; publishing rejects dirty, unstamped or unknown-source builds. A frozen,
  git-less validation copy may supply `CUCINA_SOURCE_COMMIT`, `CUCINA_SOURCE_DIRTY` and
  `SOURCE_DATE_EPOCH` explicitly. It must never share the mutable checkout's `.git` link.
* Binaries: `//bazel/release:stamp.bzl`. Go uses `x_defs = go_version_x_defs(...)` (rules_go
  stamping; only the link actions rerun). Rust reads `option_env!("CUCINA_VERSION")` from
  `rustc_env_files = RUST_VERSION_ENV_FILES` with `stamp = -1` (only `cucinactl` recompiles),
  falling back to `CARGO_PKG_VERSION` for Cargo builds.
* Everything else reads one `cucina_buildinfo` target (`//release:buildinfo`, stamp-aware via
  bazel_lib): `helm package --version/--app-version` on an untouched source chart, OCI labels
  `org.opencontainers.image.{version,revision,created,…}` and the tag, archive names and the
  macOS package, whose bundle version is the numeric core (`0.2.0` for `0.2.0-rc.1`).
* Unstamped builds report `0.0.0-dev` everywhere (Go, Rust, chart, images), so a dev build can
  never pass for a release and version agreement holds in every build. Timestamps in archives,
  images and the chart package are the commit time, so builds are reproducible.
* `cucina-release verify` (the release-lane tests, `release/assemble.sh`) asserts that every
  artifact of one build reports the same version: binaries by content and, where the host can
  run them, by their `--version`/`version` output.

## Consequences
The owners' BUILD files load `stamp.bzl` (cmd/*, cli/cucinactl); a binary without it fails
`verify`. Stamped builds rebuild the stamped link/compile actions whenever the commit changes,
nothing else. Pre-release packages share the core version with the final release; do not deploy
them through MDM to fleets that will receive the final version.
