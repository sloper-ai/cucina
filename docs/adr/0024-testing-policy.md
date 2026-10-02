<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0024 — D14: Tiered, admission-gated tests; fakes behind owned ports; production guards

* Status: accepted (2026-10-02)

## Context
Coding agents over-test, mirror the implementation in assertions, over-mock, and edit or delete tests to get green. Coverage
numbers reward exactly that. At the same time the failure modes that matter here (auth bypass, cost runaway, orphaned
instances, config drift against Buildbarn) need deep, trustworthy tests, and future contributors must be able to add a
test without inventing infrastructure.

## Decision
The policy in [`TESTING.md`](../../TESTING.md) is binding: eight tiers with Bazel size and tags; an admission rule for
every permanent test; a don't-test list; banned patterns enforced by tooling; red-first fixes; a `Test-Change:` trailer
for any weakening, backed by a git hook, a CI range check and CODEOWNERS; flaky tests are bugs (quarantine expires);
mutation testing instead of a coverage target. Architecture follows: consumer-owned ports with stateful fakes and
conformance suites, a deterministic simulation of the controller, one scenario harness shared by the acceptance
campaign and the canaries, invariants and SLOs that production code and tests share, and formal manual checklists.

## Consequences
* Fewer, stronger tests; a failing test is evidence about the code first, and the test is edited only with a recorded reason.
* The enforcement details are in ADR 0025 (trailer and hooks) and ADR 0026 (flake policy).
