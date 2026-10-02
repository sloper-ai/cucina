<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0025 — Weakening a test needs a `Test-Change:` trailer, enforced in layers by cheap heuristics

* Status: accepted (2026-10-02)

## Context
`TESTING.md` requires that deleting, weakening or skipping a test, or regenerating a golden, is never silent: the commit
says why (`Test-Change: <reason>`). Whether a diff "weakens" a test is not decidable in general, a single hook can be
bypassed (`--no-verify`, `git commit --amend`, a clone without the hooks), and coding agents work in many clones.

## Decision
* **One implementation, three call sites.** `.githooks/lib/test-change.sh` reads the change and a commit message. `.githooks/commit-msg` runs it on the commit
  being created; CI runs `test-change.sh range origin/main..HEAD` on every commit of a pull request, which also covers bypassed hooks and amended commits; reviewers
  can run `commit <rev>`.
* **Two classifiers, both in awk.** `classify-diff.awk` reads `git diff -U0` for Go, Rust, goldens, scenarios and checklists; `classify-build.awk` reads the old and the
  new content of every changed BUILD file, statement by statement, so a test target is compared with its old self (a marker added, a tier changed, a target removed)
  wherever it sits, list comprehensions included; targets are matched by name over all changed BUILD files, so moving a target or a whole file neither removes it nor hides a marker added on the way. Both are heuristics, listed in `TESTING.md` section 6.2: a test source file deleted (a rename is not a deletion);
  a golden or scenario file deleted or with any line removed (appending is fine); a skip marker added to a test that already exists (`t.Skip*` outside `test/e2e`,
  `#[ignore]`, `"manual"`/`"quarantine"`/`@platforms//:incompatible`/`flaky = True` on a test rule); a test target moved to another tier or removed; more test cases removed than
  added; more than ten net lines removed from test sources (`CUCINA_TEST_SHRINK_LINES`); a manual checklist deleted.
* **New code is not judged for skips.** An environment guard in a new test file or in a test added by the same change, and a platform restriction on a new BUILD target, are ordinary
  engineering; the admission rule and review judge them. Only a skip that can silence a test that already exists needs a reason. A file renamed in the same commit is compared with its old self.
* **A check that cannot run never passes.** An unknown revision, a failing `git` or `awk`, or a diff too large for an argument list ends in exit status 2 and a refused commit.
  Inside `cmd || status=...` errexit is off, so each step is checked explicitly; the diff goes through files, not arguments. Path prefixes, quoting, colour, external diff, textconv and
  `diff.relative` are pinned, so a developer's configuration cannot blind the check.
* **The trailer** is any line `Test-Change: <reason>` of at least eight characters, anywhere in the message (squash merges do not keep it last).
  Merge commits and `fixup!`/`squash!`/`amend!` commits are not judged: the commits they bring in are, and a fixup is folded into a commit that carries its own message.
* **CODEOWNERS** puts tests, goldens, scenarios, manual checklists and the policy files themselves (hooks, lint configuration, tier macros) under
  a maintainer's review, so the last line of defence is a human reading the diff.
* **Every rule is proved load-bearing.** `tools/ci/test-githooks.sh` has one case per pattern; `tools/ci/mutate-hooks.sh` deletes each pattern and condition of the hooks in turn,
  runs the suites against the mutant and fails if one survives.
* The pre-push hook is separate and is about hygiene (secrets and environment identifiers), not about tests. It scans the commits no remote-tracking branch has, with
  gitleaks and `hygiene.sh`, and judges each identifier on its own: documentation placeholders (account `123456789012` and the like, the RFC 5737 addresses) are allowed, and one
  does not shield a real identifier on the same line.

## Consequences
* A refactor that merely shrinks a test costs one trailer line; semantic weakening that keeps the line count (loosening an assertion) is not
  detected. Review, CODEOWNERS and mutation testing cover that.
* A skip added in a new file is not caught by the hook. That is deliberate (it would block every legitimate platform guard) and is covered by the admission rule in review.
* The hook is opt-in per clone (`git config core.hooksPath .githooks`); CI is the enforcing layer once the range check is wired into the workflow (planned).
* `--amend` is checked against the amended commit, not its parent; the CI range check is the complete one.
* The heuristics are tested by `tools/ci/test-githooks.sh` and mutation-tested by `tools/ci/mutate-hooks.sh` (a few minutes; run it when the hooks change).
