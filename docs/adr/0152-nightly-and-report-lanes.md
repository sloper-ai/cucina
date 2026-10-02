<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0152 — Nightly, system and mutation lanes outside the gating CI

* Status: accepted (2026-10-02)

## Context
R-TEST-2 puts the 1,000-seed simulation sweep and the kind/`ct install` system tier into nightly
lanes (Docker only there), R-TEST-5.6 wants mutation testing in diff mode on PRs and a weekly full
run as reports, R-BUILD-1 runs `golangci-lint`/`cargo deny` outside Bazel and R-TEST-5.4 needs the
14-day quarantine expiry enforced even when nobody pushes. ci.yml (the gating lanes) belongs to
the Bazel agent.

## Decision
`.github/workflows/nightly.yml` holds every non-gating lane, each job selected by event:
* nightly (03:41 UTC): `//sim:sim_sweep_test` (logs uploaded on failure), the system tier (kind
  v0.33.0; the controller image comes from Bazel as `//release:controller_image_load`, loaded with
  `kind load image-archive`, no registry; any `tier-system` Bazel tests; `ct install` v3.14.0 over
  the `ci/` profiles), security scans with fresh advisory data (`govulncheck` v1.1.4,
  `cargo deny` 0.20.2, `golangci-lint`), and `tools/ci/check-quarantine.sh`;
* pull requests: the quarantine check (cheap, it gates) and mutation testing of changed lines;
* weekly (Sunday): full mutation runs. gremlins v0.6.0 (`go install` at the tag) over the
  R-TEST-5.6 scope `internal/{sts,auth,scaling,hostd/vmm,bbconfig}`; cargo-mutants v27.1.0
  (`cargo install --locked`, cached) over the credential helper. Both `continue-on-error`, results
  uploaded as artifacts, no score target.
All actions are pinned by SHA (pinact); jobs get `contents: read` only.

## Consequences
ci.yml's `nightly` job also runs the simulation tier; keep one of the two sweeps (this one keeps
the logs). The system lane depends on the chart installing on kind with defaults; failures there
are chart/controller bugs, not CI noise. Mutation reports need a human to triage survivors.
