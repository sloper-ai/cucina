<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Campaign report template

`docs/reports/e2e-<YYYY-MM-DD>.md` is generated, not hand-written:

```sh
go run ./test/e2e/cmd/e2e report --env aws-e2e [--extra-spend spend.json]   # renders and redacts
go run ./test/e2e/cmd/e2e redact-check docs/reports/e2e-*.md               # must print nothing before a commit
```

The generator reads the scenario results (`<artifactsDir>/<runId>/results/result-*.json`), this repository's ADRs and the
lead's issue ledger, and writes the sections below. Edit the inputs, not the output; regenerate after every rerun.

## Sections (in order)

1. **Headline** — run ID, scenario counts (pass/fail/skip/error), NFR counts (pass/fail/not measured), spend against the
   $300 budget.
2. **NFR results** — one row per PROMPT §8 row (NFR-P1…P4, M1…M3, T1…T9, C1…C4, R1…R4, X1…X5): target, status, every
   measurement with its subject, and the scenarios that measured it. A row with no measurement is **NOT MEASURED**, never a pass.
3. **Scenarios** — summary table, then per scenario: status, duration, cost class and measured dollars, skip reason or error,
   NFR measurements, post-condition checks, metrics, steps, timeline (instance lifecycle, scale-in) and every PromQL query used.
4. **Cost** — itemised spend (compute by instance type, EBS, public IPv4, snapshots, transfer) plus `--extra-spend` items
   (standing environment, image builds, Fast Launch), and NFR-C2's monthly standing cost for the test topology and the small
   production topology (from T15). Mentions the `AWSServiceRoleForEC2FastLaunch` service-linked role left in place.
5. **Issues found and fixed** — `docs/reports/issues.md`, maintained by the lead (format below), included verbatim.
6. **ADRs** — index of `docs/adr/*.md`.
7. **Limitations** — skipped scenarios with reasons, checks that could not run, NFRs not measured.
8. **Tag sweep** — the output of `deploy/aws-e2e/scripts/sweep.sh --report` recorded by T15, with its clean/leftover verdict.

## Issue ledger format (`docs/reports/issues.md`)

```markdown
# Issues found during the campaign
- **<short title>** (scenario, NFR) — symptom with numbers · root cause · fix (commit) · red-first test (name) · status
```

## Redaction (§12)

The generator replaces the descriptor's identifiers (instance and image IDs, endpoint, security groups, host names, home
directory) and every match of the patterns in `test/e2e/report/redact.go`: AWS account IDs in ARNs and ECR hosts, EC2 resource
IDs, IPv4/IPv6 addresses (except loopback and documentation ranges), EC2 and `.local` host names, home directories, e-mail
addresses (except `example.com`), pre-signed URL query strings, JWTs and `cuc_sk_` service keys. `redact-check` fails on any
match; it runs before every commit of a report.
