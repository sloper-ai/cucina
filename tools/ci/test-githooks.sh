#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Tests for the git hooks in .githooks/ (commit-msg, pre-push) and the checks
# behind them. Every case runs in a throwaway repository; the real repository is
# never touched. Needs: git, gitleaks (CUCINA_GITLEAKS or PATH), awk, sed.
#
# What this guards (TESTING.md, "The Test-Change trailer" and "Public-repo hygiene"):
#   * a commit that deletes, weakens or skips tests, or rewrites goldens, is
#     refused unless its message has "Test-Change: <reason>";
#   * a push containing a secret or an AWS account ID is refused.
#
# Suites: commit-msg (the hook end to end: trailer rules, history check, failing closed),
# rules (one case per pattern of the classifiers, so that every rule is load-bearing:
# tools/ci/mutate-hooks.sh proves it by deleting each pattern in turn) and pre-push.
#
# Usage: tools/ci/test-githooks.sh [commit-msg | rules | pre-push]   (exit 0 = all cases passed; no argument runs all suites)
# Under Bazel, point CUCINA_HOOKS_DIR at the runfiles copy of .githooks.
set -euo pipefail

suite="${1:-all}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -n "${CUCINA_HOOKS_DIR:-}" ]; then
    hooks="$CUCINA_HOOKS_DIR"
elif [ -n "${TEST_SRCDIR:-}" ] && [ -d "${TEST_SRCDIR}/${TEST_WORKSPACE:-_main}/.githooks" ]; then
    hooks="${TEST_SRCDIR}/${TEST_WORKSPACE:-_main}/.githooks"
else
    hooks="$(cd "$here/../../.githooks" && pwd)"
fi

# The hooks run in throwaway repositories outside this checkout, where a
# project-scoped mise shim would not resolve: use the real binary.
gitleaks_bin="${CUCINA_GITLEAKS:-gitleaks}"
case "$gitleaks_bin" in
*/*) gitleaks_bin="$(cd "$(dirname "$gitleaks_bin")" && pwd -P)/$(basename "$gitleaks_bin")" ;; # Bazel passes a relative path
esac
if command -v mise >/dev/null 2>&1; then
    gitleaks_bin="$(cd "$here" && mise which "$gitleaks_bin" 2>/dev/null || echo "$gitleaks_bin")"
fi
if ! "$gitleaks_bin" version >/dev/null 2>&1; then
    echo "test-githooks: gitleaks is required (set CUCINA_GITLEAKS or install it)" >&2
    exit 1
fi
export CUCINA_GITLEAKS="$gitleaks_bin"

work="$(mktemp -d "${TMPDIR:-/tmp}/cucina-githooks.XXXXXX")"
work="$(cd "$work" && pwd)"
trap 'rm -rf "$work"' EXIT
# Hermetic git: no user or system configuration, no prompts, no editors.
export HOME="$work/home"
mkdir -p "$HOME"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_TERMINAL_PROMPT=0 GIT_EDITOR=true GIT_PAGER=cat

# On macOS /usr/bin/git is a launcher. Avoid its repeated toolchain lookup in
# fixture setup and direct staged checks, just as Git does for hook children.
# Preserve explicitly selected Git wrappers and custom (possibly relative) helper
# paths; other installations keep their existing command selection unchanged.
if [ "$(command -v git)" = /usr/bin/git ] && [ -z "${GIT_EXEC_PATH+x}" ] && [ "$(uname -s)" = Darwin ]; then
    git_exec_path="$(git --exec-path)"
    if [ -x "$git_exec_path/git" ]; then
        export PATH="$git_exec_path:$PATH"
    fi
fi

passed=0
failed=0
out="$work/out.txt"
: >"$out"

pass() {
    passed=$((passed + 1))
    echo "ok - $1"
}

fail() {
    failed=$((failed + 1))
    echo "not ok - $1"
    if [ -s "$out" ]; then sed 's/^/    | /' "$out"; fi
    # tools/ci/mutate-hooks.sh only needs to know that a mutant was caught.
    if [ -n "${CUCINA_TEST_FAIL_FAST:-}" ]; then exit 1; fi
}

in_sandbox() {
    case "$PWD" in
    "$work"/*) ;;
    *)
        echo "test-githooks: refusing to run outside the sandbox ($PWD)" >&2
        exit 2
        ;;
    esac
}

# new_repo <name>: an empty repository with the hooks enabled.
new_repo() {
    rm -rf "${work:?}/$1"
    git init -q "$work/$1"
    cd "$work/$1"
    git symbolic-ref HEAD refs/heads/main
    git config user.name "Hook Test"
    git config user.email "hook-test@example.invalid"
    git config commit.gpgsign false
    git config core.hooksPath "$hooks"
}

put() { # put <relative path>: write stdin to <path>, creating directories
    local dir=.
    case "$1" in */*) dir="${1%/*}" ;; esac
    [ -d "$dir" ] || mkdir -p "$dir"
    cat >"$1"
}

# ------------------------------------------------------------------ fixtures

# plan_test_go <variant>: a Go test file with three test functions, each with
# three assertions (3 lines apiece).
#   full | skip | comment-skip | hollow | lean | no-scale-in
check() { # check <got-expr> <want> : one 3-line assertion
    printf '\tif got := %s; got != %s {\n\t\tt.Fatalf("got %%d", got)\n\t}\n' "$1" "$2"
}

plan_test_go() {
    local variant="$1"
    printf 'package plan\n\nimport "testing"\n\n'
    printf 'func TestScaleOut(t *testing.T) {\n'
    if [ "$variant" = hollow ]; then
        printf '\tt.Log("todo")\n'
    else
        check 'Desired(3, 9)' 3
        check 'Desired(5, 9)' 5
        check 'Desired(7, 9)' 7
    fi
    printf '}\n\n'
    if [ "$variant" != no-scale-in ]; then
        printf 'func TestScaleIn(t *testing.T) {\n'
        [ "$variant" = skip ] && printf '\tt.Skip("flaky, fix later")\n'
        [ "$variant" = comment-skip ] && printf '\t// t.Skip("not needed")\n'
        case "$variant" in
        hollow) printf '\tt.Log("todo")\n' ;;
        lean)
            check 'Desired(0, 1)' 0
            check 'Desired(1, 1)' 1
            ;;
        *)
            check 'Desired(0, 1)' 0
            check 'Desired(1, 1)' 1
            check 'Desired(2, 1)' 1
            ;;
        esac
        printf '}\n\n'
    fi
    printf 'func TestMax(t *testing.T) {\n\tt.Run("capped", func(t *testing.T) {\n'
    if [ "$variant" = hollow ]; then
        printf '\t\tt.Log("todo")\n'
    else
        printf '\t\tif got := Desired(99, 4); got != 4 {\n\t\t\tt.Fatalf("got %%d", got)\n\t\t}\n'
    fi
    printf '\t})\n}\n'
}

# build_file <variant>: a BUILD file with a test target and a binary.
#   plain | manual-test | manual-binary | flaky-test
build_file() {
    printf 'load("//bazel:tiers.bzl", "cucina_go_test")\n\n'
    printf 'cucina_go_test(\n    name = "plan_test",\n    srcs = ["plan_test.go"],\n'
    [ "$1" = manual-test ] && printf '    tags = ["manual"],\n'
    [ "$1" = flaky-test ] && printf '    flaky = True,\n'
    printf '    tier = "unit",\n)\n\ncc_binary(\n    name = "tool",\n    srcs = ["tool.cc"],\n'
    [ "$1" = manual-binary ] && printf '    tags = ["manual"],\n'
    printf ')\n'
}

# base_repo <name>: a repository whose first commit holds tests, a golden, a
# Rust test, a BUILD file and a manual check.
base_repo() {
    new_repo "$1"
    plan_test_go full | put internal/plan/plan_test.go
    printf 'package plan\n\nfunc Desired(queued, max int) int {\n\tif queued > max {\n\t\treturn max\n\t}\n\treturn queued\n}\n' |
        put internal/plan/plan.go
    printf 'line one\nline two\nline three\nline four\n' | put internal/render/testdata/small.golden
    printf '#[test]\nfn rejects_wrong_state() {\n    assert!(true);\n}\n' | put cli/cucinactl/tests/login.rs
    build_file plain | put BUILD.bazel
    printf '# MT-001\n' | put docs/testing/manual/MT-001.md
    git add -A
    # Fixture setup, not the change being judged. Root-commit behavior has its
    # own explicit case below; avoid running both hooks for every fixture.
    git -c core.hooksPath=/dev/null commit -q -m "base: tests, golden and BUILD file"
}

# try_commit <message> [<second paragraph>]: stage everything, commit through the hook.
try_commit() {
    in_sandbox
    git add -A
    if [ "$#" -ge 2 ]; then
        git commit -q -m "$1" -m "$2" >"$out" 2>&1
    else
        git commit -q -m "$1" >"$out" 2>&1
    fi
}

# expect_commit <name> <ok|fail> <message> [<second paragraph>]: commit the
# pending change, compare the outcome, then return the repository to its base.
expect_commit() {
    local name="$1" expected="$2" got=ok
    shift 2
    try_commit "$@" || got=fail
    if [ "$got" = "$expected" ]; then
        pass "$name"
    else
        fail "$name (expected the commit to be ${expected}ed, it was ${got}ed)"
    fi
    in_sandbox
    if [ "$got" = ok ]; then git reset -q --hard HEAD~1; else git reset -q --hard HEAD; fi
}

# expect_staged <name> <ok|fail>: judge the staged change the way the commit-msg hook does, without
# committing it (a case costs a fraction of a commit), compare the outcome, then return the repository
# to its base. Further arguments are ignored, so a call can keep the commit message it would have used.
expect_staged() {
    local name="$1" expected="$2" got=ok
    in_sandbox
    git add -A
    printf 'change for: %s\n' "$name" >"$work/message.txt"
    "$hooks/lib/test-change.sh" staged "$work/message.txt" >"$out" 2>&1 || got=fail
    if [ "$got" = "$expected" ]; then
        pass "$name"
    else
        fail "$name (expected the change to be judged ${expected}, it was judged ${got})"
    fi
    git reset -q --hard HEAD
}

expect_message() { # expect_message <name> <text>: the last hook output mentions <text>
    if grep -q -- "$2" "$out"; then pass "$1"; else fail "$1 (output lacks: $2)"; fi
}

expect_status() { # expect_status <name> <wanted exit status> <command...>
    local name="$1" want="$2" got=0
    shift 2
    "$@" >"$out" 2>&1 || got=$?
    if [ "$got" -eq "$want" ]; then pass "$name"; else fail "$name (exit status $got, wanted $want)"; fi
}

# rule <name> <ok|fail> <path> <before> <after> [<finding code>]: commit <before> at
# <path> without the hook (the fixture; "-" = the file does not exist yet), then try
# to commit <after> ("-" = delete the file) through the hook. A refusal must name
# <finding code> when one is given. Every case uses its own paths.
rule() {
    local name="$1" expect="$2" file="$3" before="$4" after="$5" code="${6:-}"
    in_sandbox
    if [ "$before" != "-" ]; then
        printf '%s\n' "$before" | put "$file"
        git add -A
        git -c core.hooksPath=/dev/null commit -q -m "fixture for: $name"
    fi
    if [ "$after" = "-" ]; then
        git rm -q "$file"
    else
        printf '%s\n' "$after" | put "$file"
    fi
    expect_staged "$name" "$expect"
    if [ "$expect" = fail ] && [ -n "$code" ]; then
        expect_message "$name: the refusal says $code" "$code"
    fi
}

# go_test <body line>...: a Go test file whose one test has the arguments as its body.
go_test() {
    local line
    printf 'package a\n\nimport "testing"\n\nfunc TestA(t *testing.T) {\n'
    for line in "$@"; do printf '\t%s\n' "$line"; done
    printf '}\n'
}

# bt <rule> <name> [<attribute line>...]: a BUILD rule with a tier.
bt() {
    local kind="$1" name="$2" line
    shift 2
    printf '%s(\n    name = "%s",\n    srcs = ["%s.go"],\n' "$kind" "$name" "$name"
    for line in "$@"; do printf '    %s\n' "$line"; done
    printf '    tier = "unit",\n)\n'
}

# make_remote <name>: a base repository pushed once to a bare "remote".
make_remote() {
    base_repo "$1"
    git init -q --bare "$work/$1-remote.git"
    git remote add origin "$work/$1-remote.git"
    git push -q origin main >"$out" 2>&1
}

remote_head() { # remote_head <name> <ref>
    git --git-dir="$work/$1-remote.git" rev-parse "$2" 2>/dev/null || echo none
}

# A fixture that looks like a GitHub personal access token to gitleaks, built at
# run time so that this file itself contains no complete token.
fixture_tail='aB3dE5fG7hJ9kL1mN3pQ5rS7tU9vW1xY3zA5'
leaky="ghp_${fixture_tail}"
# An ARN with a (made-up) account ID, assembled the same way.
account_digits='345678901234'

suite_commit_msg() {
    suite_commit_msg_policy
    suite_commit_msg_robustness
    suite_commit_msg_environment
}

suite_commit_msg_policy() {
    # ---------------------------------------------------------------- commit-msg

    base_repo c1
    printf 'package plan\n\nimport "testing"\n\nfunc TestExtra(t *testing.T) { t.Log("x") }\n' | put internal/plan/extra_test.go
    printf 'package plan\n' | put internal/plan/extra.go
    expect_commit "adding tests and code needs no trailer" ok "add extra"

    git rm -q internal/plan/plan_test.go
    expect_commit "deleting a test file is refused without Test-Change" fail "drop plan tests"
    expect_message "the refusal names the deleted file" "plan_test.go"
    git rm -q internal/plan/plan_test.go
    expect_commit "deleting a test file passes with Test-Change" ok "drop plan tests" \
        "Test-Change: behaviour moved to the scaling package, covered there"

    git rm -q internal/plan/plan_test.go
    expect_commit "an empty reason is not a reason" fail "drop plan tests" "Test-Change: ok"
    expect_message "the refusal asks for a real reason" "needs a real reason"

    git mv internal/plan/plan_test.go internal/plan/planner_test.go
    expect_commit "renaming a test file is not a deletion" ok "rename plan tests"

    printf 'line one\nline 2\nline three\nline four\n' | put internal/render/testdata/small.golden
    expect_commit "rewriting a golden is refused without Test-Change" fail "regenerate golden"
    expect_message "the refusal says golden-changed" "golden-changed"
    printf 'line one\nline 2\nline three\nline four\n' | put internal/render/testdata/small.golden
    expect_commit "rewriting a golden passes with Test-Change" ok "regenerate golden" \
        "Test-Change: renderer now numbers lines; output intentionally changed"

    printf 'line one\nline two\nline three\nline four\nline five\n' | put internal/render/testdata/small.golden
    expect_commit "appending to a golden needs no trailer" ok "extend golden"

    git rm -q internal/render/testdata/small.golden
    expect_commit "deleting a golden is refused without Test-Change" fail "drop golden"

    git mv internal/render/testdata/small.golden internal/render/testdata/renamed.golden
    expect_commit "renaming a golden unchanged is allowed" ok "rename golden"

    plan_test_go skip | put internal/plan/plan_test.go
    expect_commit "adding t.Skip is refused without Test-Change" fail "silence a test"
    expect_message "the refusal names the skip" "skip-added"
    plan_test_go comment-skip | put internal/plan/plan_test.go
    expect_commit "a comment mentioning t.Skip is fine" ok "comment only"

    printf 'package e2e\n\nimport "testing"\n\nfunc TestNeedsAWS(t *testing.T) {\n\tt.Skip("environment has no aws capability")\n}\n' |
        put test/e2e/scenario_test.go
    expect_commit "scenario SKIP for an unmet prerequisite is allowed" ok "add scenario"

    printf '#[test]\n#[ignore]\nfn rejects_wrong_state() {\n    assert!(true);\n}\n' | put cli/cucinactl/tests/login.rs
    expect_commit "adding #[ignore] is refused without Test-Change" fail "ignore a rust test"

    build_file manual-test | put BUILD.bazel
    expect_commit "tagging a test target manual is refused without Test-Change" fail "hide a test"
    expect_message "the refusal names the rule" "cucina_go_test"
    build_file manual-binary | put BUILD.bazel
    expect_commit "tagging a non-test target manual is fine" ok "manual binary"
    build_file flaky-test | put BUILD.bazel
    expect_commit "flaky = True on a test is refused" fail "mark flaky"

    plan_test_go hollow | put internal/plan/plan_test.go
    expect_commit "hollowing out tests is refused" fail "tidy tests"
    expect_message "the refusal reports the shrink" "tests-shrunk"

    plan_test_go lean | put internal/plan/plan_test.go
    expect_commit "a small deletion within the limit passes" ok "drop one check"
    plan_test_go lean | put internal/plan/plan_test.go
    if CUCINA_TEST_SHRINK_LINES=2 try_commit "drop one check"; then
        fail "CUCINA_TEST_SHRINK_LINES tightens the limit"
    else
        pass "CUCINA_TEST_SHRINK_LINES tightens the limit"
    fi
    git reset -q --hard HEAD

    plan_test_go no-scale-in | put internal/plan/plan_test.go
    expect_commit "removing a test function is refused" fail "drop a test"
    expect_message "the refusal counts removed cases" "tests-removed"

    git rm -q internal/plan/plan_test.go
    expect_commit "fixup commits are folded later and skipped here" ok "fixup! base: tests, golden and BUILD file"

    git rm -q docs/testing/manual/MT-001.md
    expect_commit "deleting a manual check is refused" fail "drop manual check"

    # A merge brings in commits that were already checked; the merge itself is not judged.
    git switch -q -c feature
    git rm -q internal/plan/plan_test.go
    git commit -q -m "drop plan tests" -m "Test-Change: obsolete; the unit moved to internal/scaling" >"$out" 2>&1
    git switch -q main
    printf 'package plan\n' | put internal/plan/other.go
    git add -A && git commit -q -m "unrelated change on main"
    if git merge --no-ff -q feature -m "merge feature" >"$out" 2>&1; then
        pass "a merge commit is not judged by the Test-Change rules"
    else
        fail "a merge commit is not judged by the Test-Change rules"
    fi
    expect_status "the commit check does not judge a merge commit either" 0 "$hooks/lib/test-change.sh" commit HEAD
    git reset -q --hard main

}

suite_commit_msg_robustness() {
    # ------------------------------------------------------- the trailer itself

    base_repo c2
    git rm -q internal/plan/plan_test.go
    expect_commit "a 7-character reason is too short" fail "drop plan tests" "Test-Change: abcdefg"
    git rm -q internal/plan/plan_test.go
    expect_commit "an 8-character reason is enough" ok "drop plan tests" "Test-Change: abcdefgh"
    git rm -q internal/plan/plan_test.go
    expect_commit "spaces do not count towards the length of the reason" fail "drop plan tests" "Test-Change: a b c d e f"
    git rm -q internal/plan/plan_test.go
    expect_commit "the trailer name is not case-sensitive" ok "drop plan tests" "test-change: moved to another package"
    git rm -q internal/plan/plan_test.go
    expect_commit "a commented-out trailer is not a trailer" fail "drop plan tests" "# Test-Change: moved to another package"
    git rm -q internal/plan/plan_test.go
    expect_commit "the trailer may be anywhere in the message" ok "drop plan tests" \
        "$(printf 'Test-Change: moved to another package\n\nSigned-off-by: A <a@example.invalid>')"
    for prefix in 'squash!' 'amend!'; do
        git rm -q internal/plan/plan_test.go
        expect_commit "a $prefix commit is folded later and not judged" ok "$prefix base: tests, golden and BUILD file"
    done
    git rm -q internal/plan/plan_test.go
    expect_commit "a subject that merely starts with fixup is judged" fail "fixup of the plan tests"

    # ------------------------------------------------- the check fails closed

    base_repo f1
    expect_status "a range with an unknown revision fails closed" 2 "$hooks/lib/test-change.sh" range no-such-ref..HEAD
    expect_message "the failure names the range" "no-such-ref"
    expect_status "an unknown commit fails closed" 2 "$hooks/lib/test-change.sh" commit 0123456789abcdef0123456789abcdef01234567
    expect_status "an unknown mode is a usage error" 2 "$hooks/lib/test-change.sh" frobnicate HEAD
    git rm -q internal/plan/plan_test.go
    if CUCINA_AWK=false try_commit "drop plan tests" "Test-Change: obsolete; the unit moved to internal/scaling"; then
        fail "a classifier that fails refuses the commit, trailer or not"
    else
        pass "a classifier that fails refuses the commit, trailer or not"
    fi
    expect_message "the refusal says the change could not be classified" "could not classify"
    git reset -q --hard HEAD

    # R-TEST-5: a failed BUILD blob read must not become an empty file and pass
    # even with a valid trailer. Exercise the public hook with a failing git I/O.
    export CUCINA_REAL_GIT
    CUCINA_REAL_GIT="$(command -v git)"
    mkdir -p "$work/failing-git"
    cat >"$work/failing-git/git" <<'EOF'
#!/bin/sh
if [ "$1" = show ]; then exit 43; fi
exec "$CUCINA_REAL_GIT" "$@"
EOF
    chmod +x "$work/failing-git/git"
    build_file manual-test | put BUILD.bazel
    git add -A
    printf 'change BUILD\n\nTest-Change: this fixture deliberately changes a target\n' >"$work/message.txt"
    # Invoke commit-msg directly: git commit prepends its own exec path to PATH.
    if PATH="$work/failing-git:$PATH" "$hooks/commit-msg" "$work/message.txt" >"$out" 2>&1; then
        fail "a failed BUILD blob read refuses the commit, trailer or not"
    else
        pass "a failed BUILD blob read refuses the commit, trailer or not"
    fi
    expect_message "a blob-read failure explains that classification failed" "could not classify"
    git reset -q --hard HEAD
}

suite_commit_msg_environment() {
    # ------------------------------ user configuration does not change what is seen

    for setting in diff.noprefix=true diff.mnemonicPrefix=true color.diff=always color.ui=always diff.external=true diff.renames=false; do
        base_repo cfg
        git config "${setting%%=*}" "${setting#*=}"
        git mv internal/plan/plan_test.go internal/plan/planner_test.go
        expect_commit "a renamed test file is still not a deletion with $setting" ok "rename plan tests"
        printf 'package e2e\n\nimport "testing"\n\nfunc TestNeedsAWS(t *testing.T) {\n\tt.Skip("environment has no aws capability")\n}\n' | put test/e2e/scenario_test.go
        expect_commit "the e2e skip exemption holds with $setting" ok "add scenario"
        plan_test_go skip | put internal/plan/plan_test.go
        expect_commit "a skip is still refused with $setting" fail "silence a test"
        printf 'x: 1\n' | put sim/scenarios/a.yaml
        git add -A
        git -c core.hooksPath=/dev/null commit -q -m "fixture: a scenario"
        printf 'x: 2\n' | put sim/scenarios/a.yaml
        expect_commit "a scenario rewrite is still refused with $setting" fail "tune the scenario"
        git rm -q internal/plan/plan_test.go
        expect_commit "a deleted test is still refused with $setting" fail "drop the plan tests"
    done

    # A textconv driver would make git compare converted text: the check reads the raw diff.
    base_repo tc
    printf '*.golden diff=hide\n' | put .gitattributes
    git config diff.hide.textconv 'echo same #'
    printf 'one\ntwo\n' | put internal/render/testdata/tc.golden
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: a golden behind a textconv driver"
    printf 'one\n2\n' | put internal/render/testdata/tc.golden
    expect_commit "a golden rewrite is refused even behind a textconv driver" fail "regenerate golden"

    # diff.relative would hide everything outside the directory the check runs in.
    base_repo rel
    git config diff.relative true
    git rm -q cli/cucinactl/tests/login.rs
    git -c core.hooksPath=/dev/null commit -q -m "drop a test without the hook"
    cd internal/plan
    expect_status "diff.relative does not hide a deletion outside the current directory" 1 "$hooks/lib/test-change.sh" commit HEAD
    cd "$work/rel"

    # ------------------------------------------- a long list of renames is no problem

    base_repo big
    long="$(printf 'a%.0s' $(seq 1 100))"
    mkdir -p "old/$long"
    i=0
    while [ "$i" -lt 700 ]; do
        i=$((i + 1))
        printf 'package a\n' >"old/$long/${long}-${i}_test.go"
    done
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: many test files"
    git mv old new
    expect_commit "renaming hundreds of test files is not a deletion" ok "move the tests"
    git mv old new
    git rm -q internal/plan/plan_test.go
    expect_commit "a deletion among hundreds of renames is still found" fail "move the tests"
    expect_message "the refusal names the deleted file" "plan_test.go"

    # --------------------------------------------------- history check (CI mode)

    base_repo h1
    base_sha="$(git rev-parse HEAD)"
    git -c core.hooksPath=/dev/null commit -q --allow-empty -m "unrelated change"
    git rm -q internal/plan/plan_test.go
    git -c core.hooksPath=/dev/null commit -q -m "drop plan tests (hook bypassed)"
    git -c core.hooksPath=/dev/null commit -q --allow-empty -m "later change"
    if "$hooks/lib/test-change.sh" range "$base_sha..HEAD" >"$out" 2>&1; then
        fail "range check catches a bypassed commit"
    else
        pass "range check catches a bypassed commit"
    fi
    expect_message "range check names the offending commit" "drop plan tests (hook bypassed)"
    if "$hooks/lib/test-change.sh" commit HEAD >"$out" 2>&1; then
        pass "commit check passes a commit without findings"
    else
        fail "commit check passes a commit without findings"
    fi

    base_repo h2
    base_sha="$(git rev-parse HEAD)"
    git rm -q internal/plan/plan_test.go
    git -c core.hooksPath=/dev/null commit -q -m "drop plan tests" -m "Test-Change: obsolete; the unit moved to internal/scaling"
    if "$hooks/lib/test-change.sh" range "$base_sha..HEAD" >"$out" 2>&1; then
        pass "range check accepts an acknowledged deletion"
    else
        fail "range check accepts an acknowledged deletion"
    fi

    new_repo first
    printf 'a\n' | put api/testdata/a.golden
    printf 'package x\n' | put x_test.go
    if try_commit "root"; then
        pass "the first commit of a repository needs no trailer"
    else
        fail "the first commit of a repository needs no trailer"
    fi

}

# One case per pattern of the classifiers (classify-diff.awk, classify-build.awk):
# a mutant that deletes any single pattern fails one of these cases
# (tools/ci/mutate-hooks.sh runs every such mutant).
suite_rules() {
    suite_rules_sources
    suite_rules_build
    suite_rules_cases
}

# Independently runnable groups keep each Bazel target within the integration budget.
suite_rules_sources() {
    base_repo rsources
    local n f call

    # ---- Deleting a test source: one case per pattern of is_test_source.
    for f in rs/a_test.rs sh/a_test.sh py/a_test.py cc/a_test.cc cpp/a_test.cpp c/a_test.c ts/a_test.ts js/a_test.js \
        tools/test-lint.sh tools/notices/test.sh infra/a.tftest.hcl svc/tests/data.txt svc/test/data.txt; do
        rule "deleting $f is refused" fail "$f" 'one' - test-deleted
    done
    rule "deleting an ordinary source file needs no trailer" ok pkg/lib.go 'package a' -

    # ---- Goldens and scenario data: one case per pattern of is_golden.
    for f in g1/testdata/a.txt g2/a.golden g3/a.golden.json g4/goldens/a.txt g5/golden/a.txt \
        g6/__snapshots__/a.txt g7/a.snap sim/scenarios/a.yaml; do
        rule "rewriting a line of $f is refused" fail "$f" $'one\ntwo\nthree' $'one\n2\nthree' golden-changed
        rule "appending to $f is fine" ok "$f" $'one\ntwo' $'one\ntwo\nthree'
    done
    for f in g1/testdata/b.txt sim/scenarios/b.yaml; do
        rule "deleting $f is refused" fail "$f" 'one' - golden-deleted
    done
    rule "rewriting an ordinary data file is fine" ok data/a.txt $'one\ntwo' $'one\n2'

    printf '\000\001\002\003' | put g8/a.snap
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: a binary golden"
    printf '\000\001\002\004' | put g8/a.snap
    expect_staged "replacing a binary golden is refused" fail "regenerate the snapshot"
    expect_message "the refusal says binary" "binary file replaced"
    printf '\000\001\002\003\005' | put g8/new.snap
    expect_staged "adding a binary golden is fine" ok "add a snapshot"

    printf 'a\nb\nc\nd\n' | put g9/old.golden
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: a golden to move"
    git mv g9/old.golden g9/new.golden
    printf 'a\nb\nc\nX\n' | put g9/new.golden
    expect_staged "renaming a golden and editing it on the way is refused" fail "move and edit the golden"
    expect_message "the refusal says golden-deleted" "golden-deleted"

    # ---- Skip markers in Go tests.
    n=0
    for call in 't.Skip("later")' 't.Skipf("later %d", 1)' 't.SkipNow()' 'b.Skip("later")'; do
        n=$((n + 1))
        rule "adding $call to a test is refused" fail "sk$n/a_test.go" "$(go_test 'x()')" "$(go_test "$call" 'x()')" skip-added
    done
    rule "a skip inside a test added by the same change is fine" ok sk10/a_test.go "$(go_test 'x()')" \
        "$(go_test 'x()')"$'\n\nfunc TestB(t *testing.T) {\n\tif testing.Short() {\n\t\tt.Skip("slow")\n\t}\n\tx()\n}'
    rule "a skip in a new test file is fine" ok sk11/a_test.go - "$(go_test 'if testing.Short() {' $'\tt.Skip("slow")' '}' 'x()')"
    rule "a test rewritten to skip is refused" fail sk12/a_test.go $'package a\n\nfunc TestA(t *testing.T) { x() }' \
        $'package a\n\nfunc TestA(t *testing.T) { t.Skip("later"); x() }' skip-added
    rule "moving a skip within the file is fine" ok sk13/a_test.go "$(go_test 't.Skip("later")' 'x()' 'y()')" "$(go_test 'x()' 't.Skip("later")' 'y()')"
    rule "a skip in test/e2e is fine" ok test/e2e/sk14_test.go "$(go_test 'x()')" "$(go_test 't.Skip("no aws")' 'x()')"
    rule "Skip( in a file that is not a test is not a skip" ok sk15/a.go 'package a' $'package a\n\nfunc f() { r.Skip(3) }'
    rule "a skip in a helper of a new test file is fine" ok sk19/h_test.go - $'package a\n\nfunc skipUnlessSlow(t *testing.T) {\n\tt.Skip("slow")\n}'
    rule "a commented-out skip made real is refused" fail sk16/a_test.go "$(go_test $'// t.Skip("later")' 'x()')" "$(go_test 't.Skip("later")' 'x()')" skip-added
    printf '%s\n' "$(go_test 'x()' 'y()')" | put sk17/a_test.go
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: a test to rename"
    git mv sk17/a_test.go sk17/b_test.go
    printf '%s\n' "$(go_test 't.Skip("later")' 'x()' 'y()')" | put sk17/b_test.go
    expect_staged "a skip smuggled in through a rename is refused" fail "rename and silence"
    expect_message "the refusal says skip-added" "skip-added"
    printf '%s\n' "$(go_test 't.Skip("later")' 'x()' 'y()')" | put sk18/a_test.go
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: a skipped test to rename"
    git mv sk18/a_test.go sk18/b_test.go
    printf '%s\n' "$(go_test 't.Skip("later")' 'x()' 'y()' 'z()')" | put sk18/b_test.go
    expect_staged "renaming a test that was already skipped is fine" ok "rename a skipped test"

    # ---- Skip markers in Rust tests.
    rule "adding #[ignore] to a test is refused" fail rs1/a.rs $'#[test]\nfn a() {}' $'#[test]\n#[ignore]\nfn a() {}' skip-added
    rule "adding #[ignore = reason] is refused" fail rs2/a.rs $'#[test]\nfn a() {}' $'#[test]\n#[ignore = "slow"]\nfn a() {}' skip-added
    rule "an ignored test added by the same change is fine" ok rs3/a.rs $'#[test]\nfn a() {}' $'#[test]\nfn a() {}\n\n#[test]\n#[ignore]\nfn b() {}'
    rule "a comment mentioning #[ignore] is fine" ok rs4/a.rs $'#[test]\nfn a() {}' $'#[test]\n// #[ignore]\nfn a() {}'

}

suite_rules_build() {
    base_repo rbuild
    local n attr kind
    # ---- Hiding a test in a BUILD file: one case per marker and per kind of test rule.
    n=0
    for attr in 'tags = ["manual"],' 'tags = ["quarantine"],' 'target_compatible_with = ["@platforms//:incompatible"],' \
        'flaky = True,' 'flaky = 1,'; do
        n=$((n + 1))
        rule "adding $attr to a test target is refused" fail "bz$n/BUILD.bazel" "$(bt cucina_go_test t)" "$(bt cucina_go_test t "$attr")" skip-added
    done
    for kind in cucina_scenario test_suite cucina_sh_test; do
        n=$((n + 1))
        rule "hiding a $kind is refused" fail "bz$n/BUILD.bazel" "$(bt "$kind" t)" "$(bt "$kind" t 'tags = ["manual"],')" skip-added
    done
    for kind in go_library cc_binary; do
        n=$((n + 1))
        rule "tagging a $kind manual is fine" ok "bz$n/BUILD.bazel" "$(bt "$kind" t)" "$(bt "$kind" t 'tags = ["manual"],')"
    done
    rule "a new test target with a platform restriction is fine" ok bz30/BUILD.bazel "$(bt cucina_go_test t)" \
        "$(bt cucina_go_test t)"$'\n'"$(bt cucina_go_test u 'target_compatible_with = select({"@platforms//os:windows": ["@platforms//:incompatible"], "//conditions:default": []}),')"
    rule "a new BUILD file may restrict its tests" ok bz31/BUILD.bazel - "$(bt cucina_go_test t 'tags = ["manual"],')"
    rule "moving a marker inside its rule is fine" ok bz32/BUILD.bazel \
        $'cucina_go_test(\n    name = "t",\n    tags = ["manual"],\n    tier = "unit",\n)' \
        $'cucina_go_test(\n    name = "t",\n    tier = "unit",\n    tags = ["manual"],\n)'
    rule "reformatting a rule that keeps its markers is fine" ok bz33/BUILD.bazel \
        'cucina_go_test(name = "t", tags = ["manual"], tier = "unit")' \
        $'cucina_go_test(\n    name = "t",\n    tags = ["manual"],\n    tier = "unit",\n)'
    rule "a rule rewritten with an added marker is refused" fail bz34/BUILD.bazel "$(bt cucina_go_test t)" \
        $'cucina_go_test(\n    name = "t",\n    srcs = ["t.go"],\n    tags = ["manual"],\n    tier = "unit",\n)' skip-added
    rule "a marker in a comment is not a marker" ok bz35/BUILD.bazel "$(bt cucina_go_test t)" "$(bt cucina_go_test t '# tags = ["manual"],')"
    rule "a comment at column 0 inside a rule is not a marker" ok bz37/BUILD.bazel "$(bt cucina_go_test t)" \
        $'cucina_go_test(\n    name = "t",\n    srcs = ["t.go"],\n# tags = ["manual"],\n    tier = "unit",\n)'
    rule "hiding the tests of a list comprehension is refused" fail bz36/BUILD.bazel \
        $'[\n    cucina_go_test(\n        name = "t_%s" % s,\n        srcs = ["t.go"],\n        tier = "unit",\n    )\n    for s in ["a", "b"]\n]' \
        $'[\n    cucina_go_test(\n        name = "t_%s" % s,\n        srcs = ["t.go"],\n        tags = ["quarantine"],\n        tier = "unit",\n    )\n    for s in ["a", "b"]\n]' skip-added

    # R-TEST-5 regression: a compact declaration must not treat the following
    # attributes as part of its name and silently classify a changed target as new.
    rule "hiding a one-line BUILD target is refused" fail bz38/BUILD.bazel \
        'cucina_go_test(name = "t", tier = "unit")' \
        'cucina_go_test(name = "t", tags = ["manual"], tier = "unit")' skip-added
    rule "changing the tier of a one-line BUILD target is refused" fail bz39/BUILD.bazel \
        'cucina_go_test(name = "t", tier = "unit")' \
        'cucina_go_test(name = "t", tier = "simulation")' tier-changed

    # R-TEST-5 regression: two packages may both name their test "t". Changes to
    # the second one must not hide a tier change in the first.
    printf '%s\n' "$(bt cucina_go_test t)" | put same1/BUILD.bazel
    printf '%s\n' "$(bt cucina_go_test t)" | put same2/BUILD.bazel
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: same target name in two packages"
    printf '%s\n' "$(bt cucina_go_test t | sed 's/"unit"/"simulation"/')" | put same1/BUILD.bazel
    printf '\n# comment only\n' >>same2/BUILD.bazel
    expect_staged "same-named targets do not hide a tier change" fail
    expect_message "the refusal identifies the changed tier" "tier-changed"

    # ---- Moving a test to another tier, dropping a test target.
    rule "moving a test to the simulation tier is refused" fail bz40/BUILD.bazel "$(bt cucina_go_test t)" \
        "$(bt cucina_go_test t | sed 's/"unit"/"simulation"/')" tier-changed
    rule "removing a test target is refused" fail bz41/BUILD.bazel "$(bt cucina_go_test t)"$'\n'"$(bt cucina_go_test u)" "$(bt cucina_go_test t)" test-rule-removed
    rule "replacing a test target by another is fine" ok bz42/BUILD.bazel "$(bt cucina_go_test t)" "$(bt cucina_go_test u)"
    rule "removing a target that is not a test is fine" ok bz43/BUILD.bazel "$(bt cucina_go_test t)"$'\n'"$(bt go_library lib)" "$(bt cucina_go_test t)"
    rule "deleting a BUILD file that declares tests is refused" fail bz44/BUILD.bazel "$(bt cucina_go_test t)" - test-rule-removed
    printf '%s\n' "$(bt cucina_go_test t)" | put mv1/BUILD.bazel
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: a test target to move"
    git rm -q mv1/BUILD.bazel
    printf '%s\n' "$(bt cucina_go_test t)" | put mv2/BUILD.bazel
    expect_staged "moving a test target to another BUILD file is fine" ok "move the target"
    printf '%s\n' "$(bt cucina_go_test t)" | put mv3/BUILD.bazel
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: a BUILD file to move"
    mkdir -p mv4
    git mv mv3/BUILD.bazel mv4/BUILD.bazel
    printf '%s\n' "$(bt cucina_go_test t 'tags = ["manual"],')" | put mv4/BUILD.bazel
    expect_staged "moving a BUILD file and hiding its test on the way is refused" fail "move and hide"
    expect_message "the refusal says skip-added" "skip-added"

}

suite_rules_cases() {
    base_repo rcases
    # ---- Removing test cases: one case per marker that is counted.
    rule "removing a Go Test function is refused" fail m1/a_test.go $'package a\n\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}' \
        $'package a\n\nfunc TestA(t *testing.T) {}' tests-removed
    rule "removing a Go Fuzz function is refused" fail m2/a_test.go $'package a\n\nfunc FuzzA(f *testing.F) {}\nfunc FuzzB(f *testing.F) {}' \
        $'package a\n\nfunc FuzzA(f *testing.F) {}' tests-removed
    rule "removing a t.Run subtest is refused" fail m3/a_test.go "$(go_test 't.Run("a", func(t *testing.T) {})' 't.Run("b", func(t *testing.T) {})')" \
        "$(go_test 't.Run("a", func(t *testing.T) {})')" tests-removed
    rule "removing a rapid.Check is refused" fail m4/a_test.go "$(go_test 'rapid.Check(t, f)' 'rapid.Check(t, g)')" "$(go_test 'rapid.Check(t, f)')" tests-removed
    rule "removing a rapid.MakeCheck is refused" fail m5/a_test.go "$(go_test 'a := rapid.MakeCheck(f)' 'b := rapid.MakeCheck(g)')" \
        "$(go_test 'a := rapid.MakeCheck(f)')" tests-removed
    rule "removing a Rust #[test] is refused" fail m6/a.rs $'#[test]\nfn a() {}\n#[test]\nfn b() {}' $'#[test]\nfn a() {}' tests-removed
    rule "removing a Rust #[tokio::test] is refused" fail m7/a.rs $'#[tokio::test]\nasync fn a() {}\n#[tokio::test]\nasync fn b() {}' \
        $'#[tokio::test]\nasync fn a() {}' tests-removed
    rule "removing a Rust #[rstest::rstest] is refused" fail m11/a.rs $'#[rstest::rstest]\nfn a() {}\n#[rstest::rstest]\nfn b() {}' $'#[rstest::rstest]\nfn a() {}' tests-removed
    rule "removing a Rust #[rstest] is refused" fail m8/a.rs $'#[rstest]\nfn a() {}\n#[rstest]\nfn b() {}' $'#[rstest]\nfn a() {}' tests-removed
    rule "removing a Rust proptest! block is refused" fail m9/a.rs $'proptest! {\n}\nproptest! {\n}' $'proptest! {\n}' tests-removed
    rule "replacing a test case by another is fine" ok m10/a_test.go $'package a\n\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}' \
        $'package a\n\nfunc TestA(t *testing.T) {}\nfunc TestC(t *testing.T) {}'

    # ---- Shrinking test sources.
    rule "shrinking a file that is not a test is fine" ok sh1/big.go "$(seq 1 30 | sed 's/^/x/')" "$(seq 1 5 | sed 's/^/x/')"
    rule "shrinking a test file by more than the limit is refused" fail sh2/big_test.go "$(seq 1 30 | sed 's/^/x/')" "$(seq 1 5 | sed 's/^/x/')" tests-shrunk
    rule "shrinking a test file within the limit is fine" ok sh3/big_test.go "$(seq 1 30 | sed 's/^/x/')" "$(seq 1 25 | sed 's/^/x/')"
    rule "rewriting a test file without shrinking it is fine" ok sh4/big_test.go "$(seq 1 30 | sed 's/^/x/')" "$(seq 1 30 | sed 's/^/y/')"
    rule "deleting a test file whose name has spaces is refused" fail "sp ace/with space_test.go" 'package a' - test-deleted
    rule "deleting a test file with a non-ASCII name is refused" fail "uni/tëst_test.go" 'package a' - test-deleted

    # ---- A manual checklist.
    rule "deleting a manual checklist is refused" fail docs/testing/manual/MT-042.md '# MT-042' - manual-check-deleted
    rule "deleting another document needs no trailer" ok docs/testing/manual/notes.md '# notes' -
    printf '# MT-043\n' | put docs/testing/manual/MT-043.md
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "fixture: a checklist to rename"
    git mv docs/testing/manual/MT-043.md docs/testing/manual/MT-143.md
    expect_staged "renaming a manual checklist unchanged is fine" ok "rename the checklist"

    # ---- A history check over a commit that was made without the hook.
    base_repo r2
    base_sha="$(git rev-parse HEAD)"
    git -c core.hooksPath=/dev/null commit -q --allow-empty -m "root of the test"
    printf 'x\n' | put svc/tests/data.txt
    git add -A
    git -c core.hooksPath=/dev/null commit -q -m "add data"
    git rm -q svc/tests/data.txt
    git -c core.hooksPath=/dev/null commit -q -m "drop data"
    expect_status "the range check catches a deletion under tests/" 1 "$hooks/lib/test-change.sh" range "$base_sha..HEAD"
    expect_status "a commit that adds only is fine in a range check" 0 "$hooks/lib/test-change.sh" commit HEAD~1
    expect_status "the root commit of a repository is judged by what it adds" 0 "$hooks/lib/test-change.sh" commit "$(git rev-list --max-parents=0 HEAD)"
}

suite_pre_push() {
    suite_pre_push_transport
    suite_pre_push_hygiene
}

pre_push_repo() {
    # One repository and one bare "remote"; every scenario is a branch.
    make_remote "$1"
    on_branch() { # on_branch <name>: a fresh branch from main
        git switch -q main
        git switch -q -c "$1"
    }
    commit_line() { # commit_line <message> <line>
        printf '%s\n' "$2" >>internal/plan/plan.go
        # This suite exercises pre-push; commit-msg is tested separately.
        git add -A && git -c core.hooksPath=/dev/null commit -q -m "$1"
    }
}

suite_pre_push_transport() {
    pre_push_repo pp
    on_branch ok
    commit_line "ordinary change" "// ordinary change"
    if git push origin ok >"$out" 2>&1; then
        pass "a clean push of a new branch goes through"
    else
        fail "a clean push of a new branch goes through"
    fi

    # An existing remote branch: the outgoing range is remote..local.
    commit_line "oops" "const credential = \"$leaky\""
    before="$(remote_head pp ok)"
    if git push origin ok >"$out" 2>&1; then
        fail "a push containing a secret is refused"
    else
        pass "a push containing a secret is refused"
    fi
    expect_message "the refusal says gitleaks" "gitleaks"
    expect_message "the refusal names the file that holds the secret" "plan.go"
    if [ "$(remote_head pp ok)" = "$before" ]; then
        pass "the remote is unchanged after a refused push"
    else
        fail "the remote is unchanged after a refused push"
    fi
    if grep -q "$fixture_tail" "$out"; then
        fail "the secret is redacted in the hook output"
    else
        pass "the secret is redacted in the hook output"
    fi

    # A new branch is scanned from where it forks off the remote history. Fixing
    # the tip is not enough while the secret sits in an outgoing commit;
    # dropping that commit is.
    on_branch leaky
    commit_line "secret on a new branch" "const credential = \"$leaky\""
    commit_line "remove it again" "// removed"
    if git push origin leaky >"$out" 2>&1; then
        fail "a new branch whose history contains a secret is refused"
    else
        pass "a new branch whose history contains a secret is refused"
    fi
    git reset -q --hard main
    commit_line "clean" "// clean"
    if git push origin leaky >"$out" 2>&1; then
        pass "the same branch without the secret commit is pushed"
    else
        fail "the same branch without the secret commit is pushed"
    fi

    on_branch acct
    commit_line "add role" "// role: arn:aws:iam::${account_digits}:role/ci"
    if git push origin acct >"$out" 2>&1; then
        fail "a push containing an AWS account ID is refused"
    else
        pass "a push containing an AWS account ID is refused"
    fi
    expect_message "the refusal says hygiene" "hygiene"
    git reset -q --hard main
    commit_line "documentation example account" "// role: arn:aws:iam::123456789012:role/ci"
    if git push origin acct >"$out" 2>&1; then
        pass "the documentation example account ID is allowed"
    else
        fail "the documentation example account ID is allowed"
    fi

    if git push origin --delete ok >"$out" 2>&1; then
        pass "deleting a remote branch is not blocked"
    else
        fail "deleting a remote branch is not blocked"
    fi

}

suite_pre_push_hygiene() {
    pre_push_repo ppids
    # Every shape of identifier is refused on its own; a documentation placeholder is not,
    # and a placeholder on the same line does not shield a real identifier. The
    # identifiers are assembled at run time: this file must not trip the scan it tests.
    idn=0
    expect_hygiene() { # expect_hygiene <name> <ok|fail> <line>
        local name="$1" expected="$2" line="$3" got=ok
        idn=$((idn + 1))
        on_branch "id$idn"
        commit_line "identifier case $idn" "// $line"
        # Shape table for the public history scanner. The transport suite above
        # proves pre-push invokes it; no need to rerun gitleaks for every ARN.
        "$hooks/lib/hygiene.sh" main..HEAD >"$out" 2>&1 || got=fail
        if [ "$got" = "$expected" ]; then pass "$name"; else fail "$name (scan returned $got, expected $expected)"; fi
        git switch -q main
    }
    ec2_dns() { printf 'ec2-%s.us-west-1.compute.amazonaws.%s' "$1" com; }
    ecr_host() { printf '%s.dkr.ecr.us-west-1.amazonaws.%s' "$1" com; }
    sso_url() { printf 'd-1234567890.awsapps.%s/start' com; }
    expect_hygiene "an EC2 public DNS name is refused" fail "host $(ec2_dns 54-183-12-7)"
    expect_hygiene "an EC2 name in the 203.0.113.0/24 documentation range is allowed" ok "host $(ec2_dns 203-0-113-7)"
    expect_hygiene "an EC2 name in the 198.51.100.0/24 documentation range is allowed" ok "host $(ec2_dns 198-51-100-4)"
    expect_hygiene "an EC2 name in the 192.0.2.0/24 documentation range is allowed" ok "host $(ec2_dns 192-0-2-9)"
    expect_hygiene "a real EC2 name beside a documentation one is refused" fail "hosts $(ec2_dns 203-0-113-7) $(ec2_dns 54-183-12-7)"
    expect_hygiene "an ECR host with a real account ID is refused" fail "image $(ecr_host "$account_digits")/cucina"
    expect_hygiene "an ECR host with the documentation account is allowed" ok "image $(ecr_host 123456789012)/cucina"
    expect_hygiene "an IAM Identity Center start URL is refused" fail "login $(sso_url)"
    expect_hygiene "an ECR FIPS host with a real account ID is refused" fail "image $(printf '%s.dkr.ecr-fips.us-west-1.amazonaws.%s' "$account_digits" com)/cucina"
    expect_hygiene "a GovCloud ARN with a real account ID is refused" fail "role: arn:aws-us-gov:iam::${account_digits}:role/ci"
    for acct in 123456789012 111122223333 444455556666 777788889999 000000000000 999999999999 210987654321; do
        expect_hygiene "the documentation account $acct is allowed in an ARN" ok "role: arn:aws:iam::$acct:role/ci"
    done
    expect_hygiene "a real account beside a documentation one on the same line is refused" fail \
        "roles: arn:aws:iam::123456789012:role/a arn:aws:iam::${account_digits}:role/b"

    on_branch msgid
    printf '%s\n' "// harmless" >>internal/plan/plan.go
    git add -A && git commit -q -m "deploy role arn:aws:iam::${account_digits}:role/ci"
    if git push origin msgid >"$out" 2>&1; then
        fail "an identifier in a commit message is refused"
    else
        pass "an identifier in a commit message is refused"
    fi
    git switch -q main

    # A push to a remote that has no tracking refs here (a fork, a URL) is judged on the new
    # commits only: history that is already public is not the push's problem.
    on_branch hist
    commit_line "old hit" "// role: arn:aws:iam::${account_digits}:role/old"
    git push --no-verify origin hist >"$out" 2>&1
    commit_line "clean after" "// clean after"
    git init -q --bare "$work/pp-fork.git"
    git remote add fork "$work/pp-fork.git"
    if git push fork hist >"$out" 2>&1; then
        pass "a push to a remote without tracking refs scans only the new commits"
    else
        fail "a push to a remote without tracking refs scans only the new commits"
    fi
    git switch -q main

    expect_status "the hygiene scan fails closed on an unknown revision" 2 "$hooks/lib/hygiene.sh" no-such-ref..HEAD

    on_branch nogitleaks
    commit_line "change" "// change"
    if CUCINA_GITLEAKS="$work/does-not-exist" git push origin nogitleaks >"$out" 2>&1; then
        fail "pushing without gitleaks installed is refused"
    else
        pass "pushing without gitleaks installed is refused"
    fi
    expect_message "the refusal explains how to install gitleaks" "mise install gitleaks"
}

case "$suite" in
all) suite_commit_msg; suite_rules; suite_pre_push ;;
commit-msg) suite_commit_msg ;;
commit-msg-policy) suite_commit_msg_policy ;;
commit-msg-robustness) suite_commit_msg_robustness ;;
commit-msg-environment) suite_commit_msg_environment ;;
rules) suite_rules ;;
rules-sources) suite_rules_sources ;;
rules-build) suite_rules_build ;;
rules-cases) suite_rules_cases ;;
pre-push) suite_pre_push ;;
pre-push-transport) suite_pre_push_transport ;;
pre-push-hygiene) suite_pre_push_hygiene ;;
*) echo "usage: test-githooks.sh [commit-msg[-policy|-robustness|-environment] | rules[-sources|-build|-cases] | pre-push[-transport|-hygiene]]" >&2; exit 2 ;;
esac

echo
echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
