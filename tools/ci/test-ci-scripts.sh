#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Tests for the policy checks in tools/ci/: check-quarantine.sh (flake policy),
# check-manual-tests.sh (manual checklist structure), check-alert-runbooks.sh (alert
# table of the operations README) and adr-index.sh (ADR index).
# Each case builds a tiny tree in a temporary directory; nothing outside it is read.
#
# Usage: tools/ci/test-ci-scripts.sh      (exit 0 = all cases passed)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/cucina-ci-scripts.XXXXXX")"
work="$(cd "$work" && pwd)"
trap 'rm -rf "$work"' EXIT

passed=0
failed=0
out="$work/out.txt"

pass() {
    passed=$((passed + 1))
    echo "ok - $1"
}

fail() {
    failed=$((failed + 1))
    echo "not ok - $1"
    sed 's/^/    | /' "$out"
}

# expect <name> <ok|fail> <command...>: run the command, compare its outcome.
expect() {
    local name="$1" want="$2" got=ok
    shift 2
    "$@" >"$out" 2>&1 || got=fail
    if [ "$got" = "$want" ]; then pass "$name"; else fail "$name (wanted $want, got $got)"; fi
}

mentions() { # mentions <name> <text>: the last output contains <text>
    if grep -q -- "$2" "$out"; then pass "$1"; else fail "$1 (output lacks: $2)"; fi
}

# ------------------------------------------------------------ check-quarantine

q="$here/check-quarantine.sh"
today="2026-10-02"

build() { # build <dir>: write stdin to <dir>/BUILD.bazel
    mkdir -p "$1"
    cat >"$1/BUILD.bazel"
}

tree="$work/q1"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    srcs = ["a_test.go"],
    tags = ["requires-network"],
    tier = "unit",
)
EOF
expect "no quarantined tests passes" ok "$q" --root "$tree" --today "$today"
mentions "the summary counts zero" "0 quarantined"

tree="$work/q2"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    tags = [
        "quarantine",
        "quarantine-until-2026-10-16",
        "quarantine-issue-123",
        "quarantine-owner-someone",
    ],
    tier = "unit",
)
EOF
expect "a complete quarantine within 14 days passes" ok "$q" --root "$tree" --today "$today"
mentions "the quarantined target is listed" "//:a_test"
expect "the same quarantine fails once expired" fail "$q" --root "$tree" --today "2026-10-17"
mentions "the refusal says expired" "expired"
expect "a quarantine that runs out today still passes" ok "$q" --root "$tree" --today "2026-10-16"

tree="$work/q3"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    tags = ["quarantine", "quarantine-until-2026-12-31", "quarantine-issue-1", "quarantine-owner-x"],
    tier = "unit",
)
EOF
expect "an expiry more than 14 days away fails" fail "$q" --root "$tree" --today "$today"
mentions "the refusal says how far" "more than 14 days"

tree="$work/q4"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    tags = ["quarantine", "quarantine-until-2026-10-10"],
    tier = "unit",
)
EOF
expect "a quarantine without issue and owner fails" fail "$q" --root "$tree" --today "$today"
mentions "the refusal asks for the issue" "quarantine-issue"
mentions "the refusal asks for the owner" "quarantine-owner"

tree="$work/q5"
mkdir -p "$tree"
: >"$tree/BUILD.bazel"
for i in 1 2 3 4 5 6; do
    cat >>"$tree/BUILD.bazel" <<EOF
cucina_go_test(
    name = "t${i}_test",
    tags = ["quarantine", "quarantine-until-2026-10-10", "quarantine-issue-${i}", "quarantine-owner-x"],
    tier = "unit",
)
EOF
done
expect "six quarantined tests exceed the limit" fail "$q" --root "$tree" --today "$today"
mentions "the refusal names the limit" "limit is 5"
expect "five quarantined tests are within the limit" ok "$q" --root "$tree" --today "$today" --max 6

tree="$work/q6"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    flaky = True,
    tier = "unit",
)
EOF
expect "flaky = True fails" fail "$q" --root "$tree" --today "$today"
mentions "the refusal says flaky" "flaky"

tree="$work/q7"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    tags = ["exclusive"],
    tier = "unit",
)
EOF
expect "the exclusive tag fails" fail "$q" --root "$tree" --today "$today"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    tags = ["exclusive-if-local"],
    tier = "unit",
)
EOF
expect "exclusive-if-local passes" ok "$q" --root "$tree" --today "$today"

tree="$work/q8"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    # tags = ["quarantine"],
    tier = "unit",
)

cucina_go_test(
    name = "b_test",
    tags = ["quarantine-until-2026-10-10"],
    tier = "unit",
)
EOF
expect "tags in comments are ignored; orphan quarantine-* tags fail" fail "$q" --root "$tree" --today "$today"
mentions "the orphan is named" "b_test"

tree="$work/q9"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    tier = "unit",
)
EOF
mkdir -p "$tree/third_party/x"
cat >"$tree/third_party/x/BUILD.bazel" <<'EOF'
some_test(
    name = "vendored_test",
    flaky = True,
)
EOF
expect "vendored BUILD files are not scanned" ok "$q" --root "$tree" --today "$today"

# A checkout that sits below a directory called bazel-*, third_party or .work
# (a Bazel output tree, a worktree) is still scanned: exclusions apply to what
# is inside the root, not to the path above it.
tree="$work/bazel-out/.work/third_party/q10"
build "$tree" <<'EOF'
cucina_go_test(
    name = "a_test",
    flaky = True,
    tier = "unit",
)
EOF
expect "a checkout below bazel-*, .work or third_party directories is still scanned" fail "$q" --root "$tree" --today "$today"
mentions "the flaky target in it is found" "flaky"

# ------------------------------------------------------- check-manual-tests

m="$here/check-manual-tests.sh"

# mt_file <dir> <number> [<last_run> [<sign_off> [<step line>]]]
mt_file() {
    local dir="$1" n="$2" last_run="${3:-never}" sign_off="${4:-pending}" step="${5:-1. [human] Do the thing.}"
    mkdir -p "$dir"
    cat >"$dir/MT-$n.md" <<EOF
---
# SPDX-License-Identifier: FSL-1.1-ALv2
id: MT-$n
title: Example check $n
risks: [R-EXAMPLE]
trigger: before every release
owner: unassigned
last_run: $last_run
sign_off: $sign_off
---

# MT-$n

## Preconditions

- Something.

## Steps

$step
2. [agent] Run \`true\`.

## Expected results

- It works.

## Required evidence

- A transcript.
EOF
}

mt_set() { # mt_set <dir>: MT-001 .. MT-008 and a README linking them
    local dir="$1" n
    rm -rf "$dir"
    for n in 001 002 003 004 005 006 007 008; do mt_file "$dir" "$n"; done
    : >"$dir/README.md"
    for n in 001 002 003 004 005 006 007 008; do echo "- [MT-$n](MT-$n.md)" >>"$dir/README.md"; done
}

tree="$work/m1"
mt_set "$tree"
expect "a complete manual test set passes" ok "$m" --dir "$tree"

rm "$tree/MT-008.md"
expect "a missing MT-008 fails" fail "$m" --dir "$tree"
mentions "the refusal names the missing file" "MT-008.md is missing"

mt_set "$tree"
mt_file "$tree" 003 never pending "1. Do the thing without a marker."
expect "a step without [agent]/[human] fails" fail "$m" --dir "$tree"
mentions "the refusal quotes the step" "lacks an \[agent\] or \[human\] marker"

mt_set "$tree"
mt_file "$tree" 004 2026-10-02 pending
expect "a run without sign-off fails" fail "$m" --dir "$tree"
mentions "the refusal asks for a human" "a human must sign off"

mt_set "$tree"
mt_file "$tree" 004 2026-10-02 "Randolf Jung 2026-10-02"
expect "a signed-off run passes" ok "$m" --dir "$tree"

mt_set "$tree"
sed -i.bak 's/^id: MT-005$/id: MT-050/' "$tree/MT-005.md" && rm "$tree/MT-005.md.bak"
expect "an id that does not match the file name fails" fail "$m" --dir "$tree"

mt_set "$tree"
grep -v 'MT-006' "$tree/README.md" >"$tree/README.new" && mv "$tree/README.new" "$tree/README.md"
expect "a README that does not link every check fails" fail "$m" --dir "$tree"
mentions "the refusal names the unlinked check" "does not link MT-006.md"

mt_set "$tree"
"$m" --dir "$tree" --list-agent-steps >"$out" 2>&1 || true
mentions "--list-agent-steps lists the pre-runnable steps" "MT-001: 2. \[agent\]"

# A directory whose path has spaces is read like any other.
tree="$work/manual tests with spaces"
mt_set "$tree"
expect "a manual test directory with spaces passes" ok "$m" --dir "$tree"
mt_file "$tree" 003 never pending "1. Do the thing without a marker."
expect "and is still checked there" fail "$m" --dir "$tree"
mentions "the refusal names the file in the spaced directory" "MT-003.md"
"$m" --dir "$tree" --list-agent-steps >"$out" 2>&1 || true
mentions "--list-agent-steps works with spaces in the path" "MT-001: 2. \[agent\]"

# ------------------------------------------------------ check-alert-runbooks

ar="$here/check-alert-runbooks.sh"
tree="$work/alerts"
mkdir -p "$tree"
cat >"$tree/rules.yaml" <<'EOF'
groups:
  - name: example
    rules:
      - alert: CucinaOne
        expr: up == 0
      - alert: "CucinaTwo"   # a quoted name with a comment
        expr: up == 1
      - record: cucina:not_an_alert
        expr: up
EOF
cat >"$tree/README.md" <<'EOF'
| `CucinaOne` | one |
| `CucinaTwo` | two |
EOF
expect "every alert in the table passes" ok "$ar" --rules "$tree/rules.yaml" --readme "$tree/README.md"
mentions "the summary counts the alerts" "2 alert(s)"
cat >"$tree/README.md" <<'EOF'
| `CucinaOne` | one |
EOF
expect "an alert missing from the table fails" fail "$ar" --rules "$tree/rules.yaml" --readme "$tree/README.md"
mentions "the refusal names the alert" "CucinaTwo"
cat >"$tree/README.md" <<'EOF'
| `CucinaOne` | one |
| `CucinaTwo` | two |
| `CucinaGone` | none |
EOF
expect "a table row without an alert fails" fail "$ar" --rules "$tree/rules.yaml" --readme "$tree/README.md"
mentions "the refusal names the stale row" "CucinaGone"
expect "a missing rule file is an error" fail "$ar" --rules "$tree/none.yaml" --readme "$tree/README.md"

# ----------------------------------------------------------------- adr-index

a="$here/adr-index.sh"
tree="$work/adr"
mkdir -p "$tree"
cat >"$tree/README.md" <<'EOF'
# ADRs

<!-- BEGIN ADR INDEX -->
stale
<!-- END ADR INDEX -->

Footer text.
EOF
printf '# 0001 — Buildbarn pins\n\n* Status: accepted (2026-10-02)\n' >"$tree/0001-buildbarn-pins.md"
printf '# ADR 0500: Scaling policy\n\nStatus: Proposed\n' >"$tree/0500-scaling.md"
export CUCINA_ADR_DIR="$tree"
expect "--check reports a stale index" fail "$a" --check
expect "--write regenerates the index" ok "$a" --write
if grep -q '^| \[0001\](0001-buildbarn-pins.md) | Buildbarn pins | accepted | architecture |$' "$tree/README.md"; then
    pass "the index shows the title without its number, the status and the area"
else
    fail "the index shows the title without its number, the status and the area"
fi
if grep -q '^| \[0500\](0500-scaling.md) | Scaling policy | Proposed | controller and scaling |$' "$tree/README.md"; then
    pass "the index maps number ranges to areas and parses alternative headings"
else
    fail "the index maps number ranges to areas and parses alternative headings"
fi
if grep -q 'Footer text.' "$tree/README.md"; then pass "text outside the markers is preserved"; else fail "text outside the markers is preserved"; fi
expect "--check passes after --write" ok "$a" --check
printf '# 0002 — New decision\n\n* Status: accepted\n' >"$tree/0002-new.md"
expect "--check notices a new ADR" fail "$a" --check
unset CUCINA_ADR_DIR

echo
echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
