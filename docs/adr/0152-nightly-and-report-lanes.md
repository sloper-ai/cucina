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
`.github/workflows/nightly.yml` holds scheduled/reporting lanes and the selected PR gates below; the filename does not make a PR gate advisory. Each job is selected by event:
* nightly (03:41 UTC): `//sim:sim_sweep_test` (logs uploaded on failure), the system tier (kind
  v0.33.0; the controller image comes from Bazel as `//release:controller_image_load`, loaded with
  `kind load image-archive`, no registry; any `tier-system` Bazel tests; `ct install` v3.14.0 over
  `release/kind-values.yaml` (file-backed `standard` storage, generated TLS, no AWS), not the
  static `ci/` render fixtures that require external issuers/monitoring/EBS), security scans
  with fresh advisory data (`govulncheck` v1.1.4,
  `cargo deny` 0.20.2, `golangci-lint`), and `tools/ci/check-quarantine.sh`;
* pull requests: the quarantine check (cheap, it gates), the existing simulation sweep for scaling/lifecycle inputs, and the existing kind/ct lane for chart/RBAC inputs. A read-only selector verifies exact event commits and complete history, then uses the merge-base delta (including deletions/renames); shared build/CI inputs select both. Selector errors fail rather than masquerade as no changes. Mutation testing of changed lines remains a non-blocking report;
* weekly (Sunday): full mutation runs. gremlins v0.6.0 (`go install` at the tag, binary cached)
  over the R-TEST-5.6 scope `internal/{sts,auth,keys,scaling,hostd/lifecycle,bbconfig}`; cargo-mutants v27.1.0
  (`cargo install --locked`, cached) over the credential helper. Both `continue-on-error`, results
  uploaded as artifacts, no score target.
All actions are pinned by SHA (pinact), use only GitHub-owned actions or the allow-listed
`jdx/mise-action`, and get `contents: read` only. Bazelisk and chart-testing are installed through
mise; do not broaden the repository's selected-actions policy. The kind node image is pinned
by digest, and the dedicated cluster is deleted on success or failure. PRs use `pull_request`, never elevated `pull_request_target` credentials. Administrators must configure required checks and CODEOWNER review separately; no workflow changes repository settings.

## Consequences
ci.yml's `nightly` job also runs the simulation tier; keep one of the two sweeps (this one keeps
the logs). The system lane needs its explicit installable kind profile; render-only fixtures
must not be mistaken for install tests. Mutation reports need a human to triage survivors.
