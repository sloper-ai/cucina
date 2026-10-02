<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0027 — Third-party notices are generated from `go list` and cargo-about, not from a licence-scanning service

* Status: accepted (2026-10-02)

## Context
The licence requires `THIRD_PARTY_NOTICES.md` to cover everything bundled or redistributed, and the file ships inside the macOS package and the
images. It must be reproducible, reviewable and regenerated before each release. `go-licenses` is not on the pinned tool list, its last stable
release is old, and adding a tool needs justification.

## Decision
* `tools/notices/generate.sh` assembles the file from: a hand-maintained `components.json` (Buildbarn, Tart, WinFsp, shawl, hermetic-llvm, Go, musl, ...)
  with their licence texts under `tools/notices/texts/`; the Go modules that `go list -deps` finds for the shipped binaries on every release platform, with
  the licence files from the module cache; and `cargo about generate` (pinned to the version in the script, configured by `about.toml` and `about.hbs`)
  for the Rust workspace.
* A small classifier (`lib.sh`) recognises the licence of each Go module from distinctive sentences. It decides the table entry and the policy check
  (`policy.json`); the full text is always reproduced, so a misclassification cannot drop an attribution. An unknown or disallowed licence, or a module with
  no licence file, fails the run.
* Apache-2.0 text is reproduced once; NOTICE files are reproduced per module; every other distinct text is reproduced as is.
* The output has no timestamps. `generate.sh --check` fails when the committed file is stale.

## Consequences
* No new tool dependency beyond cargo-about, which is already installed.
* The classifier is heuristic and tested (`tools/notices/test.sh`); a new licence family needs a pattern, a test and a `policy.json` decision.
* WinFsp (GPLv3 with FLOSS exception) is listed as installed, not redistributed: worker images are built per operator and must not be published.
