<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Testing policy

Tests exist to give you, and the people and agents after you, a fast and trustworthy signal that the
requirements hold. They do not exist to maximise the number of tests or the coverage percentage.
Coding agents are known to over-test, to mirror the implementation in their assertions, to over-mock, and to
edit or delete tests until the build is green. This policy is binding for everyone, and it is built so
that doing the right thing is also the cheapest thing. Section 11 has the recipes to copy.

Requirement IDs (`R-AUTH-8`, `UC15`, `NFR-P1`, `T10`, …) are the stable labels this repository uses in code
comments, tests, ADRs and docs to say which requirement something serves.

## 0. The rules on one screen

1. Every check is a permanent test, an ephemeral check or a production guard (§1). Only the first is committed.
2. Every test belongs to exactly one tier (§2) and is declared with the Bazel macro for that tier.
3. A permanent test passes the admission rule (§3), or it is not committed.
4. The don't-test list (§4) is not tested. Table rows beat new tests; one property test beats a pile of examples.
5. The banned patterns (§5) are enforced by tools wherever a tool can enforce them.
6. A bug fix starts with a failing test (§6.1). Deleting, weakening or skipping a test, or regenerating a golden,
   needs a `Test-Change: <reason>` line in the commit message (§6.2).
7. Flakes are bugs: no `flaky` attribute, `--runs_per_test=20` for new tests, quarantine expires (§7).
8. Mutation testing, not coverage, judges whether the tests are strong (§8).
9. Stay inside the time budgets (§9).

## 1. Three kinds of verification

Everything you run to check your work is exactly one of these.

| Kind | What | What you commit |
| --- | --- | --- |
| **Permanent test** | Guards a requirement, a bug or an invariant for the life of the code. | The test, if it passes the admission rule (§3). |
| **Ephemeral check** | Scratch tests, `curl`/`grpcurl` calls, PTY sessions, one-off scripts used while building. | Nothing. Put the evidence (the command and its output) in the commit or pull request description. Do not commit the code. |
| **Production guard** | Validation, invariants, SLOs and canaries that catch behaviour better caught at runtime than before merge (§12). | The guard, with one test of its failure path. |

Explore with ephemeral checks and commit only what earns a place. Turn every real failure you hit into a red-first
regression test.

## 2. Tiers

Every test has exactly one tier. The tier names are plain words so that they never collide with the acceptance
scenario IDs (`T0`…`T22`). The macros in [`bazel/tiers.bzl`](bazel/tiers.bzl) set the Bazel `size`, the tags and the
`tier-<name>` tag; a test without a `tier-*` tag fails analysis ([`bazel/tier_check.bzl`](bazel/tier_check.bzl), ADR 0104).

| Tier | Bazel size and tags | Holds | Runs | Declare with |
| --- | --- | --- | --- | --- |
| `static` | `small`, `tier-static` | As Bazel tests: compile, `go vet`/nogo, rustfmt, buildifier, `buf` lint, `kubeconform -strict`, `values.schema.json`, type-checked Buildbarn config rendering, drift checks of generated code. Beside Bazel: golangci-lint, cargo-deny, gitleaks (full history) and actionlint in the CI `lint` job; clippy with `bazelisk build --config=clippy`; `tofu validate` and tflint in `deploy/aws-e2e/tests/run.sh`. Planned: `buf` breaking checks, and clippy and the OpenTofu checks as CI jobs | Every change (the Bazel tests and the `lint` job) | `cucina_sh_test(tier = "static")`, or `tags = ["tier-static"]` on third-party test macros |
| `unit` | `small`, `tier-unit`: under 1 s, no I/O, no sleeps, one process | Pure cores: the autoscaler `Plan`, VM and instance state machines, trust-policy evaluation, token mint and verify, config rendering (goldens), platform and `bazelrc` generation, the cost model, the credential-helper protocol, managed-preferences parsing. Property-based where the input space is large | `bazel test //...` | `cucina_go_test(tier = "unit")`, `cucina_rust_test(tier = "unit")` |
| `integration` | `medium`, `tier-integration`: under 30 s, localhost only | Real components at our boundaries against **fakes**: envtest reconcilers with fake Compute, VMRuntime and BuildQueue; the STS against the local `internal/auth/oidctest` issuer fixtures; the pinned Buildbarn binaries booted with rendered configuration (an action round trip); `cucinactl` against fake management and REAPI servers; hostd against a fake `tart`; network faults; conformance suites against the fakes | `bazel test //...` (cheap on RBE) | `cucina_go_test(tier = "integration", envtest = True)` |
| `simulation` | `large`, `manual`, `tier-simulation` | Deterministic simulation of the controller: scenario files, a nightly 1,000-seed sweep, trace replay | Nightly (the CI `nightly` job); planned: also on pull requests that touch scaling or lifecycle code | `cucina_go_test(tier = "simulation")` |
| `system` | `large`, `manual`, `requires-docker`, `no-remote-exec` | Planned, no target exists yet: kind with Helm install and upgrade from the previous release, RBAC, leader election, `ct install`. The only tier that may use Docker, in its own CI lane, never on RBE | Planned: nightly, and on pull requests that touch the chart or RBAC | `cucina_go_test(tier = "system")` |
| `acceptance` | `enormous`, `manual`, `no-remote-exec` | The real AWS and Mac campaign (T0–T22) as scenarios in the e2e harness | Campaign, release, on demand | `cucina_scenario(...)` |
| `production` | not a Bazel tier | The guards in §12: fail-fast configuration, invariants, SLO burn-rate alerts, canaries | Always, in every deployment | code, rules and dashboards |
| `manual` | not a Bazel tier | Human checklists, [`docs/testing/manual/`](docs/testing/manual/README.md) | Releases, OS and Xcode changes, hardware changes | `docs/testing/manual/MT-NNN.md` |

Choosing the tier: if the code under test can be a pure function, make it one and use `unit`. If it needs a socket, a
process or the Kubernetes API server, it is `integration` and talks to fakes and local binaries only. Anything that needs
real AWS, a real Mac or a real identity provider is `acceptance` or `manual`.

Running them:

```sh
bazelisk test //...                                       # everything that gates a merge
bazelisk test //... --test_tag_filters=tier-unit,-quarantine   # one tier (tier-static, tier-integration); keep -quarantine, a command-line filter replaces the one in .bazelrc
bazelisk test --config=nightly <simulation or quarantined targets>   # nightly lane
bazelisk run //test/e2e:scenario -- --env=<env> --id=<id>    # explicit real-environment run; see §11.7
```

## 3. The admission rule

A permanent test is committed only if all of this is true.

* **It cites what it guards**: an acceptance criterion or requirement ID, a bug ID (written red-first), or a named
  invariant. Put it in a one-line comment on the test or its table: `// Guards: R-SCALE-3 — no scale-in before idleTimeout`.
* **It asserts observable behaviour through a public interface**, not internal calls or private helpers.
* **It uses fakes, not interaction mocks.** The fakes live in [`internal/fakes`](internal/fakes) and are stateful simulators.
* **It is deterministic**: no sleeps, no wall-clock dependence, no ordering luck; randomness comes from a seed.
* **It fits its tier's budget** (§9).
* **It duplicates nothing.** Add a table row instead of a new test, and never cover the same behaviour at several tiers.
* **Budget: about one test per acceptance criterion.** One property test replaces a pile of examples.

Go deep where the risk is high: STS and auth (bypass), the autoscaler (cost runaway, orphans, starvation), VM and instance
lifecycle, config rendering. Stay shallow where the risk is low: CLI formatting, plumbing.

## 4. The don't-test list

Do not write tests for:

* third-party behaviour (Buildbarn, Kubernetes, AWS, Tart, Bazel), beyond the conformance suites at our own ports;
* generated code, framework wiring, getters and constants, private helpers;
* log text, impossible branches, every permutation of a configuration (the schema covers it);
* exact call sequences;
* the same behaviour at several tiers;
* TUI pixels beyond a handful of key screens.

## 5. Banned patterns and how they are enforced

| Pattern | Enforced by | State |
| --- | --- | --- |
| `time.Sleep` in Go tests | `forbidigo` in [`.golangci.yml`](.golangci.yml) (type-aware, `_test.go` only). Use a fake clock (`fakes.NewClock`) or `testing/synctest` and wait with `<-time.After(d)` | enforced by the CI `lint` job; applies inside `synctest` bubbles too |
| Sleeps in Rust tests | review. Use `tokio::time::pause` / `#[tokio::test(start_paused = true)]`, which needs tokio's `test-util` feature in the crate's `[dev-dependencies]` (no crate enables it yet) | review |
| Mock frameworks on owned ports | `depguard` in [`.golangci.yml`](.golangci.yml) bans `gomock` (both import paths), `mockery` and `testify/mock`; [`deny.toml`](deny.toml) bans `mockall` | active |
| Docker in the unit, integration and simulation tiers | `depguard` bans `testcontainers-go`; only the `system` tier carries `requires-docker` | active |
| A test without a tier | the `tier_tags_aspect` in [`.bazelrc`](.bazelrc) fails `bazel build //...` | active |
| `flaky = True`, the `exclusive` tag, an expired or oversized quarantine | [`tools/ci/check-quarantine.sh`](tools/ci/check-quarantine.sh) (§7) | script ready, CI wiring planned |
| Deleted, weakened or skipped tests, regenerated goldens, without a reason | [`.githooks/commit-msg`](.githooks/commit-msg), the same check over every commit in CI, CODEOWNERS (§6.2) | hook active (opt-in per clone), CI wiring planned |
| Secrets and environment identifiers in the public repository | [`.githooks/pre-push`](.githooks/pre-push) and gitleaks in CI ([`.gitleaks.toml`](.gitleaks.toml)) (§6.3) | active |
| Assertions on log text | review. Assert on state the fake exposes, or on metrics | review |
| Snapshots longer than about 50 lines | review. A screen snapshot is one terminal screen; split goldens by component | review |
| Tests with no assertions | review and mutation testing: a test that asserts nothing kills no mutant (§8) | review |
| Network access outside localhost | tests run sandboxed; `requires-network` is allowed only with an ADR | convention |

## 6. Discipline

### 6.1 Red first

A bug fix starts with a test that reproduces the bug. Run it, watch it fail for the right reason, then fix the code.
Record that once in the commit message so a reviewer can see the test would have caught it:

```text
Fix the scale-in timer reset on controller restart

Red: TestIdleTimerSurvivesRestart failed before the fix (instance terminated 4 m early)
```

### 6.2 The `Test-Change` trailer

*Tests verify correctness; they do not define the solution.* If a test is wrong, say so and fix it in the open. Never work
around a test, and never edit a test only to turn a red build green: a failing test is evidence about the code first.

A commit needs a `Test-Change: <reason>` line in its message when its diff does any of the following (the exact rules are
in [`.githooks/lib/classify-diff.awk`](.githooks/lib/classify-diff.awk) and [`classify-build.awk`](.githooks/lib/classify-build.awk), ADR 0025):

1. **Deletes a test source file.** Test sources are `*_test.{go,rs,sh,py,cc,cpp,c,ts,js}`, `test.sh`, `test-*.sh`, `*.tftest.hcl` and anything
   under a `test/` or `tests/` directory. A rename is not a deletion.
2. **Deletes or rewrites a golden or scenario file**: anything under `testdata/` or `goldens/`, `*.golden*`, `*.snap`,
   `__snapshots__/`, `sim/scenarios/`. Appending is fine; removing or changing a line is not.
3. **Adds a skip marker to a test that already exists**: `t.Skip`, `t.Skipf` or `t.SkipNow` in a `_test.go` file (the scenario harness under `test/e2e` skips on unmet
   prerequisites by design and is exempt); `#[ignore]` in Rust; `"manual"`, `"quarantine"`, `@platforms//:incompatible` or
   `flaky = True` on a test target in a BUILD file. A skip or a platform restriction in a new file, in a test added by the same
   change, or on a new BUILD target is not judged here: the admission rule (§3) and review judge it. Moving a marker is not adding one;
   a file renamed in the same commit is compared with its old self.
4. **Removes more test cases than it adds**: `func Test…`, `func Fuzz…`, `t.Run(`, `rapid.Check(` and `rapid.MakeCheck(`, `#[test]`, `#[tokio::test]`, `#[rstest]`, `proptest!`.
5. **Removes more than ten net lines from test sources** (`CUCINA_TEST_SHRINK_LINES` changes the limit).
6. **Deletes a manual checklist** (`docs/testing/manual/MT-NNN.md`).
7. **Moves a test target to another tier** (a changed `tier =`; the `simulation`, `system` and `acceptance` tiers are tagged `manual`, so the test stops
   running in a plain `bazelisk test //...`).
8. **Removes a test target** from a BUILD file without adding another (the targets are matched by name over all changed BUILD files, so moving one is fine).

An unknown revision or a failure while obtaining the diff, reading BUILD blobs or running the classifier makes the hook refuse the commit even with a trailer (exit 2 for these verification failures). The classifiers remain heuristics, not full Go, Rust or Starlark parsers. User configuration (`diff.noprefix`, colours, an external diff driver, textconv) does not change what the check sees.

Write the reason as one sentence that says why this is not a weaker test, in the last paragraph of the message
(`git commit --trailer "Test-Change: …"` adds it):

```text
Merge the three idle-timeout cases into one table

Test-Change: consolidated into TestIdleTimeouts; every input is still a row
```

The reason must be at least eight characters; an empty reason is refused.

How it is enforced, in layers (each layer is cheap; none is the only one):

* **`.githooks/commit-msg`** runs the check on the commit being created. Enable the hooks once per clone:
  `git config core.hooksPath .githooks`. `git commit --no-verify` skips it, so:
* **CI** repeats the check on every commit of a pull request
  (`.githooks/lib/test-change.sh range origin/main..HEAD`; wiring into the workflow is planned). This also covers `--amend`, which the hook can
  only check against the commit being amended. Merge commits are not judged: the commits they bring in are.
* **CODEOWNERS** ([`.github/CODEOWNERS`](.github/CODEOWNERS)) routes changes to tests, testdata, goldens, scenarios, manual
  checklists, BUILD declarations and policy files (hooks, lint configuration, tier macros, workflows) to a maintainer.
  Repository administrators must require code-owner review in branch protection for this to block merging; the file alone only requests review.
* The heuristics cannot see a semantic weakening that keeps the line count (loosening an assertion). Review and mutation testing (§8) cover that.

The hooks are tested by [`tools/ci/test-githooks.sh`](tools/ci/test-githooks.sh): the `rules` suite has one case per pattern, and
[`tools/ci/mutate-hooks.sh`](tools/ci/mutate-hooks.sh) checks the listed detection patterns and decision conditions with explicit mutants.
It rejects stale mutations, incomplete runs and setup failures, and a comment-only control must survive. Run it when you change the hooks; this finite sweep is evidence for those mutations, not proof that every possible weakening is detected.

### 6.3 Public-repository hygiene

This repository is public. Never commit secrets, private keys, kubeconfigs, OpenTofu state, generated certificates, test
credentials, the AWS account ID, or the IPs and hostnames of a test environment. Those live under `~/.config/cucina/`
(mode 0700). `.githooks/pre-push` scans every commit about to be pushed with gitleaks and with
[`.githooks/lib/hygiene.sh`](.githooks/lib/hygiene.sh) (AWS account IDs in ARNs and registry hosts, EC2 public DNS names, IAM Identity
Center URLs) and refuses the push if either finds something, or if gitleaks is not installed
(`mise install gitleaks`) or cannot read the commits. Each identifier is judged on its own: the documentation placeholders (account `123456789012` and the
other well-known ones, and the RFC 5737 addresses such as `ec2-203-0-113-7.<region>.compute.amazonaws.com`) are allowed, but one on the same line does not hide a real
identifier. Only commits that no remote-tracking branch has are scanned, so a push to a fork is not blocked by old history. A secret that reached a commit must be removed
from history and rotated.

## 7. Flaky tests and quarantine

A flaky test is a bug in the test, the code or the infrastructure.

* There is no `flaky` attribute. Gating runs use `--flaky_test_attempts=1` (set in `.bazelrc`).
* A new or changed test must pass `bazelisk test //path:name_test --runs_per_test=20` before merge.
* **Quarantine** is the exception, for a test you cannot fix today. A quarantined test carries literal tags in its BUILD file:

  ```starlark
  cucina_go_test(
      name = "reconcile_test",
      tags = [
          "quarantine",                      # excluded from gating runs
          "quarantine-until-2026-10-16",     # at most 14 days from today
          "quarantine-issue-123",            # the tracking issue
          "quarantine-owner-someone",        # who fixes it
      ],
      tier = "integration",
  )
  ```

  At most five tests may be quarantined at once, and an expired quarantine fails the check: fix the test or delete it (the script is ready; wiring it into CI is planned, §5).
  [`tools/ci/check-quarantine.sh`](tools/ci/check-quarantine.sh) enforces this (and bans `flaky = True` and the `exclusive` tag) by reading the
  BUILD files; run it locally before you push. Quarantined tests still run in the nightly lane (`--config=nightly`).
* A test that fails only under Cucina's own remote execution is a **Cucina bug**. Track it and fix it; never retry it away.
* Extending a quarantine is a visible BUILD change that CODEOWNERS routes to a maintainer. Say why on the issue.

## 8. Mutation testing

Mutation testing, not coverage, judges whether tests are strong. Coverage is diagnostic only: you may report changed-line
coverage, but never add a test just to raise it.

* **Tools**: `gremlins` v0.6.0 for Go (`go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0`) and `cargo-mutants` 27.1.0 for Rust
  (`cargo install cargo-mutants --version 27.1.0 --locked`). Both run through the native toolchains, so they apply to pure,
  unit-tier code.
* **Scope**: STS and auth (`internal/auth`, `internal/sts`, `internal/keys`), the autoscaler core (`internal/scaling`), VM state machines
  (`internal/hostd/lifecycle`), config rendering (`internal/bbconfig`) and the Rust credential helper. A pull request that touches none of
  them needs no mutation run.
* **When**: diff mode on pull requests, a full run weekly. No CI job runs `gremlins` or `cargo-mutants` yet (planned): until then you run them yourself for a change that touches the scope.
  The git hooks have their own sweep, [`tools/ci/mutate-hooks.sh`](tools/ci/mutate-hooks.sh).

  ```sh
  gremlins unleash --diff origin/main                                       # Go, from the repository root; changed lines only
  git diff origin/main... > pr.diff && cargo mutants --in-diff pr.diff -p cucinactl   # Rust; changed lines only
  gremlins unleash                                                          # weekly: the whole module; read the in-scope packages
  ```

* **Surviving mutants on changed lines** must be killed (strengthen the test) or annotated with a reason in the pull request
  (an equivalent mutant, unreachable code, covered at another tier). There is no global score target.
* Note that diff mode mutates the code under test, not the tests: a diff that only deletes tests runs no mutants. That is
  what the `Test-Change` trailer is for.

## 9. Time budgets

| What | Budget |
| --- | --- |
| A `unit` test | under 1 s, no I/O |
| An `integration` test | under 30 s, localhost only |
| A non-AWS e2e scenario (kind) | 2 min each |
| Warm pre-merge `bazel test //...` | p95 of 10 min (5 min on Cucina RBE) |

Tighten timeouts with `--test_verbose_timeout_warnings` (set in `.bazelrc`), which names tests that finish far below their
timeout. A test that needs more than its tier's budget is in the wrong tier or does too much.

## 10. The architecture that tests build on

Anyone can add a test without inventing infrastructure because the following exists. Read the code, not just this list.

* **Ports with first-class fakes.** Every external dependency (EC2, Tart, the Buildbarn scheduler API, the identity provider, the secret store,
  process execution, the file system, the clock, randomness) sits behind a narrow, consumer-owned interface in
  [`internal/ports`](internal/ports). Each has one maintained fake in [`internal/fakes`](internal/fakes): a stateful simulator (pending to running
  latency, eventual consistency, quotas, capacity errors, throttling, the two-VM cap, disk full, crashes) with `FailNext(op, err)`-style
  knobs, reproducible from a seed. The port's owner owns and tests its fake. There is no LocalStack.
* **Conformance suites.** [`internal/ports/porttest`](internal/ports/porttest) has one suite per port (`RunCompute`, `RunVMRuntime`, `RunBuildQueue`,
  `RunHostFleet`, …). It runs against the fakes in the integration tier and against the real adapters in acceptance. When a real adapter fails a
  suite, **fix the fake**. A future provider inherits the suite for free.
* **Deterministic simulation** of the controller ([`sim/`](sim)): every port faked, one manual clock, one seeded random source, scenario YAML
  (fleet, workload, fault schedule, seed, expectations), the shared invariants checked after every step, a nightly seed sweep, failing seeds
  minimised into regression scenarios.
* **One scenario harness** ([`test/e2e`](test/e2e)) runs the same scenarios in the temporary AWS environment; planned: on kind with fakes, as a safe
  production smoke subset and as a canary CronJob. A scenario declares its ID, prerequisites, cost class, timeout and post-conditions; an unmet
  prerequisite is an explicit SKIP with a reason.
* **Fixtures and goldens.** Per-domain test-data builders with valid defaults, shared across tiers. One golden mechanism: checked-in files
  updated with `bazelisk run //:update_goldens`. CI never writes goldens.
* **Telemetry as oracle.** The `slo/` and `invariants/` packages are shared by scenarios, canaries and alerts, so tests, canaries and
  alerts cannot drift apart. Use `testutil.CollectAndLint` for metric hygiene.
* **Network faults.** An in-process, in-memory network with per-address faults (down, reset peer, blackhole) between hostd and the controller:
  [`internal/hostlink/hostlinktest`](internal/hostlink/hostlinktest), used inside `testing/synctest` bubbles with a fake clock. There is no external proxy. Other links get the same
  kind of fake network when a test needs it; chaos tooling is for game days only ([runbook](docs/operations/game-days-fis.md)).

## 11. Recipes

The snippets are scaffolds: replace the names, keep the shape.

### 11.1 A unit test (Go)

```go
// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// Guards: R-SCALE-2 - the VMs wanted before clamping are
// max(ceil(sum of D_r / N), max over runners of ceil(D_r / S_r)).
func TestDesiredVMs(t *testing.T) {
	spec := scaling.Spec{
		PoolSpec: domain.PoolSpec{Runners: []domain.Runner{{Name: "native", Concurrency: 8}}},
		VCPUs:    8,
	}
	cases := []struct {
		name   string
		queued int
		want   int
	}{
		{"an empty queue needs no VM", 0, 0},
		{"the first queued action starts a VM", 1, 1},
		{"rounds up", 9, 2},
		{"a full VM is not rounded up", 8, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			byTotal, byRunner := scaling.DesiredVMs(spec, map[string]int{"native": tc.queued})
			require.Equal(t, tc.want, max(byTotal, byRunner))
		})
	}
}
```

```starlark
load("//bazel:tiers.bzl", "cucina_go_test")

cucina_go_test(
    name = "scaling_test",
    srcs = ["scaling_test.go"],
    embed = [":scaling"],
    tier = "unit",
)
```

Write the test, run `bazelisk run //:gazelle -- path/to/pkg` (Gazelle emits `cucina_go_test` through `map_kind`), add `tier = "unit"` once, then run it
20 times: `bazelisk test //path:scaling_test --runs_per_test=20`.

**Time without sleeping.** Inject the clock port and advance it: `clock := fakes.NewClock(t0)` … `clock.Advance(5 * time.Minute)`. For code that really
uses goroutines and timers, run the test inside `synctest.Test(t, func(t *testing.T) { … })` from `testing/synctest`, where time is virtual, and wait with
`<-time.After(d)` (a call to `time.Sleep` is banned in tests).

### 11.2 A unit test (Rust)

Use the public pure core with an explicit time input rather than waiting for a file-backed cache or a timer:

```rust
// SPDX-License-Identifier: FSL-1.1-ALv2
use cucinactl::auth::token::{CachedToken, RENEW_BEFORE_SECS};
use cucinactl::config::AuthMethod;

// Guards: R-AUTH-6/8 — the renewal margin is applied at the five-minute boundary.
#[test]
fn cached_token_renewal_boundary() {
    let token = CachedToken::from_access_token(
        "synthetic-not-a-jwt".into(), Some(600), 1_000, AuthMethod::Oidc,
    );
    for (now, reusable) in [(1_000, true), (1_300, true), (1_301, false)] {
        assert_eq!(token.valid_for(now, RENEW_BEFORE_SECS), reusable);
    }
}
```

For genuinely asynchronous code, `#[tokio::test(start_paused = true)]` and `tokio::time::advance` require tokio's `test-util` feature in the crate's `[dev-dependencies]`; it is not enabled in the current manifests. Do not add sleeps instead.

```starlark
load("//bazel:tiers.bzl", "cucina_rust_test")

cucina_rust_test(name = "cucinactl_unit_test", crate = ":cucinactl_lib", tier = "unit")
```

Contract tests on `--output json` and on the credential-helper protocol use `assert_cmd` and `predicates` against the built binary; the JSON schemas live in
`cli/cucinactl/schemas/`. Fake management and REAPI servers in tests are built on connectrpc's server and listen on localhost only.

### 11.3 A property test

Use a property test where the input space is large and the invariants are crisp. A pure function checked against a rule that is obvious from its contract is the model case, and
[`TestPlanAgainstReference`](internal/scaling/plan_test.go) does the same for the whole autoscaler against a naive reference model:

```go
// Guards: R-SCALE-2 - the VMs wanted hold the whole queue, and not one VM more.
func TestDesiredVMsAreNeitherTooFewNorTooMany(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		slots := rapid.IntRange(1, 16).Draw(t, "slots")
		queued := rapid.IntRange(0, 500).Draw(t, "queued")
		spec := scaling.Spec{
			PoolSpec: domain.PoolSpec{Runners: []domain.Runner{{Name: "native", Concurrency: slots}}},
			VCPUs:    slots,
		}
		byTotal, byRunner := scaling.DesiredVMs(spec, map[string]int{"native": queued})
		vms := max(byTotal, byRunner)
		if vms*slots < queued {
			t.Fatalf("%d VM(s) of %d slots cannot hold %d queued actions", vms, slots, queued)
		}
		if vms > 0 && (vms-1)*slots >= queued {
			t.Fatalf("%d VM(s) is one more than %d actions need at %d slots each", vms, queued, slots)
		}
	})
}
```

`pgregory.net/rapid` has state-machine testing for the lifecycle machines (`rapid.Check(t, rapid.Run[*machine]())`). In Rust use `proptest` (and
`proptest-state-machine`). A failing seed is a regression test: keep the minimised case as a table row.

### 11.4 An integration test

Real components, fakes at the edges, localhost only. Use the macro with `envtest = True` for a reconciler test (the pinned `etcd`, `kube-apiserver` and
`kubectl` come from runfiles; `KUBEBUILDER_ASSETS` is set), and the fakes for everything outside the process:

```starlark
cucina_go_test(
    name = "workerpool_reconcile_test",
    srcs = ["workerpool_reconcile_test.go"],
    deps = [":reconcile", "//internal/fakes"],
    tier = "integration",
    envtest = True,
)
```

To boot a pinned Buildbarn binary against a rendered configuration use `internal/bbtest` (binaries come from `@bb_release`, no Docker). Inject network faults with
an in-memory network between your two real components (see `internal/hostlink/hostlinktest`: down, reset peer, blackhole per address), not between a component and a fake.

### 11.5 A conformance suite for a port

A new adapter (another cloud, a pod provider, static workers) proves itself with the suite of its port:

```go
func TestMyComputeConformance(t *testing.T) {
	porttest.RunCompute(t, func(t *testing.T) porttest.ComputeHarness {
		return porttest.ComputeHarness{ /* the adapter, the cluster tag, a launch request, optional capabilities */ }
	})
}
```

See [`internal/fakes/conformance_test.go`](internal/fakes/conformance_test.go) for the real harnesses. A nil capability skips only what a real backend cannot do deterministically
(forcing a capacity error), with an explicit reason. If the real adapter fails a case the fake accepted, fix the fake.

### 11.6 A simulation scenario

A scenario is a YAML file in [`sim/scenarios/`](sim/scenarios) (schema: [`sim/scenario.go`](sim/scenario.go)): `fleet`, `workload` (arrivals or a JSONL trace of arrival, platform,
size class and duration), `faults` (ICE, throttling, API errors, controller restarts, worker deaths, network cuts), a `seed` and `expectations`
(`allSucceed`, `endAtZero`, `maxInstances`, `queueWaitMax`, …). Copy the closest existing file:

```yaml
name: ice
seed: 6
duration: 40m
fleet:
  pools:
    - name: linux-x86-64
      provider: ec2
      platform: linux-x86-64
      runners: [{name: native, properties: {OSFamily: linux, ISA: x86-64}, concurrency: 8}]
      vcpus: 8
      max: 4
      idleTimeout: 5m
      startupTimeout: 5m
      instanceTypes: [c8i.2xlarge, c7i.2xlarge]
workload:
  arrivals: [{at: 30s, pool: linux-x86-64, count: 200, duration: 20s}]
faults: [{at: 0s, kind: ice, type: c8i.2xlarge, duration: 20m}]
expectations: {allSucceed: true, endAtZero: true, maxInstances: {linux-x86-64: 4}}
```

(This is a trimmed [`ice.yaml`](sim/scenarios/ice.yaml); the parser rejects unknown fields and requires the runners, the instance types and the timeouts.)

The fixed-seed scenarios run in `go test ./sim/...` (and under Bazel). The nightly lane runs the seed sweep, which gives every scenario a random fault schedule per seed:

```sh
go test ./sim -run TestSweep -sweep.seeds=1000                  # the nightly sweep
go test ./sim -run TestSweep -sweep.seeds=1000 -sweep.write     # also minimise failing seeds into sim/scenarios/regressions/
go run ./sim/cmd/simctl run -scenario sim/scenarios/ice.yaml -seed <n> -chaos -log 100   # reproduce one seed
```

A minimised failing seed is committed under `sim/scenarios/regressions/` as a regression scenario (a new file, so no `Test-Change` is needed). Every scenario checks the shared invariants after every step. Details: [`docs/dev/scaling.md`](docs/dev/scaling.md).

### 11.7 An end-to-end scenario

A scenario is a `*harness.Scenario` ([`test/e2e/harness/scenario.go`](test/e2e/harness/scenario.go)) built in [`test/e2e/scenarios/`](test/e2e/scenarios) and added to `Register` in `registry.go`. One Bazel target runs any of them by ID, so adding a scenario needs no BUILD change:

```go
func scaleIn() *harness.Scenario {
	return &harness.Scenario{
		ID:       "T8",
		Title:    "Scale-in",
		Requires: []harness.Requirement{harness.RequiresAWS, harness.RequiresKubernetes},
		Cost:     harness.CostLow,
		Timeout:  20 * time.Minute,
		Run:      func(c *harness.Context) error { /* drive the system, record c.Metric(...) */ return nil },
		Post:     []harness.Check{ /* SLI and invariant queries over slo/ and invariants/ */ },
	}
}
```

```sh
bazelisk run //test/e2e:scenario -- --env=<env> --id=T8         # --id=all runs the campaign order
```

The target is a `cucina_scenario` (tier `acceptance`: `enormous`, `manual`, `no-remote-exec`). `Envs` limits where a scenario may run (today the AWS environment; planned: kind with fakes, a read-only production smoke subset, a canary CronJob); a scenario that runs on kind must finish within two minutes.
Post-conditions are the same queries that alerts and canaries use. A scenario whose prerequisites the environment lacks is skipped with a reason, never failed. The harness validates every declaration (ID, timeout, cost class) in its own tests.

### 11.8 A manual checklist

Create `docs/testing/manual/MT-NNN.md` from an existing file: front matter (`id`, `title`, `risks`, `trigger`, `owner`, `last_run: never`, `sign_off: pending`) and the
sections Preconditions, Steps, Expected results and Required evidence. Mark every step `[agent]` (an agent may pre-run it) or `[human]`.
`tools/ci/check-manual-tests.sh` checks the structure. A human signs off; a manual failure that can be automated becomes a regression test.
See [`docs/testing/manual/README.md`](docs/testing/manual/README.md).

### 11.9 A golden

A golden is a checked-in file that a test compares exactly against generated output (config rendering per size profile, `bazelrc` output, generated code). Keep goldens
small and per component. Regenerate them in one place:

```sh
bazelisk run //:update_goldens
git diff                    # read every changed line
```

A regenerated golden changes lines, so the commit needs `Test-Change: <why the output changed>`. CI never writes goldens: it only runs the diff test.

### 11.10 A TUI snapshot and a VHS flow

Snapshot four to six key screens with `insta` and ratatui's `TestBackend` (one terminal screen each, 80x24 by default):

```rust
#[test]
fn overview_screen() {
    let mut terminal = ratatui::Terminal::new(ratatui::backend::TestBackend::new(80, 24)).unwrap();
    terminal.draw(|frame| overview::render(frame, &fixture_state())).unwrap();
    insta::assert_snapshot!(terminal.backend());
}
```

Review changes with `cargo insta review`; commit the `.snap` file. Keep two or three key flows as VHS goldens: a `.tape` with `Output flow.txt` writes the terminal text, which is
compared to the committed golden. Prefer `Wait+Screen /regex/` over fixed delays. VHS needs `ttyd` and `ffmpeg`, so those tests carry `no-remote-exec` and run in a dedicated lane.
Feel, colour and terminal compatibility are manual checks (MT-006).

## 12. Production guards

Some behaviour is better guarded at runtime than before merge. Each guard gets **one** test of its failure path.

| Guard | How |
| --- | --- |
| Fail fast on configuration | Parse into types at startup and exit non-zero with a precise message; CRD CEL rules; `values.schema.json`; permission self-checks at startup; a protocol-version handshake between hostd and the controller |
| Invariants stay on in production | Predicates in [`invariants/`](invariants) (instances never exceed max, at most two macOS VMs per host, never terminate a busy worker, every launched resource is tagged, no duplicate launch for one idempotency token) are called by the controller *and* the simulation. A violation aborts the operation, increments `cucina_invariant_violations_total{invariant}`, logs, alerts, and crashes the component if its state is suspect |
| Crash-only components | Restarts are the recovery mechanism (level-triggered reconcilers, launchd `KeepAlive`); acceptance scenario T9 proves it once |
| Alerts and SLO budgets | The chart ships threshold alerts for queue time, worker failures, leaks, orphans, egress, cache-hit drops, retention, host offline and certificate expiry, plus Sloth-generated burn-rate rules. See the exact [alert/runbook table](docs/operations/README.md#find-the-runbook-from-an-alert); do not assume every threshold alert is a multi-window SLO burn-rate alert. Keep the generated rules aligned with [`slo/`](slo) and test them with `promtool test rules` |
| Canaries | The leader runs the cache canary every five minutes in-process (`internal/controller/components_canary.go`); `helm test` runs it on demand. It exchanges a token and checks CAS/AC without starting workers. Both need a usable service key (`hooks.test.credentialSecret` after retiring break-glass). Execution probes exist in `internal/canary`, but daily/per-deploy scheduling and a chart-managed CronJob are still planned; do not claim those cadences are active |
| Shadow mode | A change of autoscaler policy first decides without acting and is diffed against the current policy |
| Game days | AWS FIS injects real capacity errors and throttling into the controller's role: [`docs/operations/game-days-fis.md`](docs/operations/game-days-fis.md) |

Adding an invariant: write the predicate in `invariants/` next to the others, call it from the code path it guards, add it to the simulation's step check, and write the one test
that makes it fire. Metric names are API: they are listed in [`docs/contracts.md`](docs/contracts.md) and pass `testutil.CollectAndLint`.

## 13. Review checklist for a new or changed test

Review with fresh eyes (a person, or an agent that did not write it).

* Would it fail if the feature broke? Delete the feature in your head, or flip a condition and watch it go red.
* Would it survive a harmless refactor (renamed helper, reordered calls, different internal structure)?
* Does it cite what it guards, sit in the right tier, and fit the budget?
* Is it a table row that someone made into a new test?
* Does it assert behaviour, or does it restate the implementation?
* Did the commit that changes or removes a test carry `Test-Change`, and is the reason true?

## 14. Where things live

| What | Where |
| --- | --- |
| Tier macros, tier guard | [`bazel/tiers.bzl`](bazel/tiers.bzl), [`bazel/tier_check.bzl`](bazel/tier_check.bzl); how-to in [`docs/dev/bazel.md`](docs/dev/bazel.md) |
| Fakes, conformance suites | [`internal/fakes`](internal/fakes), [`internal/ports/porttest`](internal/ports/porttest) |
| Simulation | [`sim/`](sim), [`docs/dev/scaling.md`](docs/dev/scaling.md) |
| Invariants, SLOs | [`invariants/`](invariants), [`slo/`](slo) |
| Scenario harness, canaries | [`test/e2e`](test/e2e), [`docs/dev/e2e.md`](docs/dev/e2e.md) |
| Manual checklists | [`docs/testing/manual/`](docs/testing/manual/README.md) |
| Lint and supply-chain configuration | [`.golangci.yml`](.golangci.yml), [`deny.toml`](deny.toml), [`.gitleaks.toml`](.gitleaks.toml), [`rustfmt.toml`](rustfmt.toml) |
| Hooks and policy checks | [`.githooks/`](.githooks), [`tools/ci/`](tools/ci), [`.github/CODEOWNERS`](.github/CODEOWNERS) |

The checks in `tools/ci/` have their own tests: `tools/ci/test-githooks.sh` (suites `commit-msg`, `rules` and `pre-push`) and `tools/ci/test-ci-scripts.sh`
(the quarantine, manual-check, alert-table and ADR-index checks); `tools/ci/mutate-hooks.sh` mutation-tests the hooks.
