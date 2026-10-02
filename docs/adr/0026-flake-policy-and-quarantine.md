<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0026 — Quarantine is a set of BUILD tags with an expiry that CI enforces

* Status: accepted (2026-10-02)

## Context
A flaky test is a bug, and the usual failure of flake policy is that quarantine becomes permanent. We need a mechanism that
cannot silently rot, works without running Bazel (it must run first in CI, cheaply), and keeps the metadata next to the test.

## Decision
* No `flaky` attribute anywhere (gating runs use `--flaky_test_attempts=1`; new or changed tests must pass `--runs_per_test=20`).
* A quarantined test carries literal tags in its BUILD file: `quarantine`, `quarantine-until-YYYY-MM-DD` (at most 14 days away),
  `quarantine-issue-<number>`, `quarantine-owner-<handle>`. `.bazelrc` excludes `quarantine` from gating runs (`test --test_tag_filters=-quarantine`);
  the nightly configuration runs them anyway.
* `tools/ci/check-quarantine.sh` parses the BUILD files as text (no Bazel, no Starlark evaluation) and fails on an expired quarantine,
  an expiry more than 14 days away, missing issue or owner, more than five quarantined tests, `flaky = True`, or the `exclusive` tag
  (which disables remote execution; use `exclusive-if-local`).
* A test that fails only under Cucina's own remote execution is a Cucina bug: it is tracked and fixed, never quarantined to hide it.

## Consequences
* Extending a quarantine is a visible BUILD change that CODEOWNERS routes to a maintainer.
* Tags must be literals at the target (a macro that computes tags is not seen by the check). The tier macros pass `tags` through unchanged.
* The check is cheap enough to run on every push, and is tested by `tools/ci/test-ci-scripts.sh`.
