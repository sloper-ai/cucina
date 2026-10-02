<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
## Summary

<!-- What does this change do, and why? One to three sentences. -->

## Requirements covered

<!-- Requirement IDs (R-…, UC…, NFR-…, T…), issues, or ADRs this serves. -->

## Tests added or changed

<!-- One row per test. The tier is the Bazel tier (static, unit, integration, simulation, system, acceptance) or manual (MT-NNN). See TESTING.md. -->

| Test | Tier | Guards (requirement, bug or invariant) |
| --- | --- | --- |
|  |  |  |

- [ ] Bug fix: a test failed first, and the commit message has a `Red:` line.
- [ ] `bazelisk test //...` passes, and every new or changed test passes `--runs_per_test=20`.
- [ ] Mutation testing (STS and auth, autoscaler, VM state machines, config rendering, credential helper): surviving mutants on changed lines are killed or explained here: <!-- or "not applicable" -->

## Evidence

<!-- Commands you ran and their (redacted) output: ephemeral checks live here, not in the repository. Real-environment results, measurements, screenshots with names blurred. -->

## Test-Change notes

- [ ] No test was deleted, weakened or skipped, and no golden was regenerated.
- [ ] Or: every commit that does so carries a `Test-Change:` line, and this is why it is not a weaker test: <!-- reasons -->

## Public-repository hygiene

- [ ] No secrets, keys, kubeconfigs, OpenTofu state, certificates, test credentials, AWS account IDs, or IPs and hostnames of a test environment, in code, docs, tests or evidence.
- [ ] The pre-push hook or CI gitleaks run is clean.

## Docs, ADRs, contracts

- [ ] Docs are updated (README, architecture, runbooks), and anything not built yet is marked as planned.
- [ ] A deviation from the plan has an ADR.
- [ ] A change to a shared contract (`api/`, `platforms/pools.json`, `internal/ports`, `docs/contracts.md`) changes the contract, the code and the docs together.
- [ ] `THIRD_PARTY_NOTICES.md` is regenerated if dependencies changed (`tools/notices/generate.sh`).

<!-- ===== First pull request only: delete this section afterwards ===== -->

## Licence change

The original repository contained an AGPL-3.0 `LICENSE` file. This change removes it and adds `LICENSE.md`, the Functional Source License, Version 1.1, ALv2 Future License (FSL-1.1-ALv2), with Brwse Co. as licensor, and puts `SPDX-License-Identifier: FSL-1.1-ALv2` in Cucina's own source files.
