#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# test-change.sh: enforce the Test-Change commit-trailer rule (TESTING.md).
#
# A commit that deletes, weakens or skips tests, or rewrites goldens, must say
# why in its message with a line "Test-Change: <reason>". Tests verify
# correctness; they do not define the solution, so changing them is allowed but
# never silent.
#
#   test-change.sh staged <msgfile>   check the commit being created (commit-msg hook)
#   test-change.sh commit <rev>       check one existing commit (against its first parent)
#   test-change.sh range  <range>     check every non-merge commit in <range>
#                                     (CI and review: e.g. origin/main..HEAD)
#
# Exit status: 0 = fine, 1 = a finding without a Test-Change reason, 2 = the
# check itself failed or was misused (an unknown revision, git or awk failing):
# a check that cannot run never passes.
# Environment: CUCINA_TEST_SHRINK_LINES (default 10) = net lines that may be removed
# from test sources before a change counts as weakening.
#
# Written for bash 3.2 (the macOS system shell): no mapfile, no associative arrays.
# Inside `cmd || status=...` errexit is off, so every step that can fail is checked
# explicitly instead of relying on `set -e`.
set -euo pipefail

lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
shrink="${CUCINA_TEST_SHRINK_LINES:-10}"
awk_bin="${CUCINA_AWK:-awk}"
min_reason_chars=8
tmp=""

usage() {
    cat >&2 <<'EOF'
usage: test-change.sh staged <msgfile>
       test-change.sh commit <rev>
       test-change.sh range  <rev-range>
EOF
    exit 2
}

die() {
    echo "test-change.sh: $*" >&2
    exit 2
}

# blob <tree-ish|--cached> <path>: the file as it is on that side of the change
# (--cached = the index); nothing when it does not exist there.
blob() {
    local side="$1" file="$2" spec
    if [ "$side" = "--cached" ]; then spec=":$file"; else spec="$side:$file"; fi
    git cat-file -e "$spec" 2>/dev/null || return 0
    git show "$spec"
}

# build_sections <old> <new>: the OLD and NEW content of every changed BUILD
# file, in the format classify-build.awk reads.
build_sections() {
    local old="$1" new="$2" file
    : >"$tmp/builds"
    while IFS= read -r -d '' file; do
        case "$file" in
        BUILD | BUILD.bazel | */BUILD | */BUILD.bazel) ;;
        *) continue ;;
        esac
        {
            printf '\037OLD\t%s\n' "$file"
            blob "$old" "$file" || return 2
            printf '\n\037NEW\t%s\n' "$file"
            blob "$new" "$file" || return 2
            printf '\n'
        } >>"$tmp/builds" || return 2
    done <"$tmp/names"
}

# findings <old> <new>: print "<code>\t<message>" lines for the change from the
# tree-ish <old> to <new> (a tree-ish, or --cached for the index); non-zero when
# the change could not be classified.
# Files that git detects as renamed are not deletions: their paths go to the
# classifier, which then reads the diff without rename detection. The diff is
# written to files, not passed as arguments, so its size does not matter; the
# path prefixes, quoting, relative paths, colour, external diff and textconv
# settings are pinned so that user configuration cannot change what the
# classifiers see. Go, Rust and
# data files are judged from the diff; BUILD files from their old and new content.
findings() {
    local old="$1" new="$2" args
    local common=(-c core.quotepath=false -c diff.relative=false)
    if [ "$new" = "--cached" ]; then args=(--cached "$old"); else args=("$old" "$new"); fi
    git "${common[@]}" diff -M --diff-filter=R --name-status --no-color --no-ext-diff "${args[@]}" >"$tmp/renames" || return 2
    git "${common[@]}" diff -U0 --no-color --no-ext-diff --no-textconv --no-renames \
        --src-prefix=a/ --dst-prefix=b/ "${args[@]}" >"$tmp/diff" || return 2
    git "${common[@]}" diff --name-only -z --no-color --no-ext-diff --no-renames "${args[@]}" >"$tmp/names" || return 2
    "$awk_bin" -v shrink="$shrink" -v renames_file="$tmp/renames" -f "$lib_dir/classify-diff.awk" "$tmp/diff" || return 2
    build_sections "$old" "$new" || return 2
    "$awk_bin" -f "$lib_dir/classify-build.awk" "$tmp/builds" || return 2
}

# reason_from_message: read a commit message on stdin; print the Test-Change
# reason(s), one per line. Any line that starts with "Test-Change: <text>" counts,
# wherever it is (squash merges concatenate bodies, so the trailer is not always
# last); a commented-out line starts with the comment character and never matches.
reason_from_message() {
    sed -n 's/^[Tt][Ee][Ss][Tt]-[Cc][Hh][Aa][Nn][Gg][Ee]:[[:space:]]*//p'
}

reason_is_valid() {
    local reasons="$1" compact
    compact="$(printf '%s' "$reasons" | tr -d '[:space:]')"
    [ "${#compact}" -ge "$min_reason_chars" ]
}

explain() {
    local label="$1" found="$2" reasons="$3"
    {
        if [ -n "$reasons" ]; then
            echo "$label: the Test-Change trailer needs a real reason (at least $min_reason_chars characters)."
        else
            echo "$label: this change deletes, weakens or skips tests (or rewrites goldens)"
            echo "and the commit message has no \"Test-Change:\" line."
        fi
        echo
        echo "Findings:"
        printf '%s\n' "$found" | while IFS="$(printf '\t')" read -r code message; do
            printf '  - %s: %s\n' "$code" "$message"
        done
        cat <<'EOF'

Tests verify correctness; they do not define the solution. If the test was wrong
or the behaviour is gone on purpose, say so in the commit message, in the last
paragraph:

    Test-Change: <why this is not a weaker test, one sentence>

(git commit --trailer "Test-Change: ..." adds it.) If you changed the test only
to get a failing build to pass, stop and fix the code instead. See TESTING.md,
section "The Test-Change trailer".
EOF
    } >&2
}

# judge <label> <message-text> <old> <new>: 0 = fine, 1 = violation, 2 = the check failed.
judge() {
    local label="$1" message="$2" old="$3" new="$4" found reasons
    found="$(findings "$old" "$new")" || {
        echo "test-change.sh: could not classify the change of $label (git or awk failed); refusing it" >&2
        return 2
    }
    [ -z "$found" ] && return 0
    reasons="$(printf '%s\n' "$message" | reason_from_message)"
    if [ -n "$reasons" ] && reason_is_valid "$reasons"; then
        return 0
    fi
    explain "$label" "$found" "$reasons"
    return 1
}

empty_tree() {
    git hash-object -t tree /dev/null
}

# check_commit <rev>: 0 fine, 1 violation, 2 failure.
check_commit() {
    local rev="$1" subject parents base message tree
    git rev-parse --verify --quiet "${rev}^{commit}" >/dev/null || {
        echo "test-change.sh: not a commit: $rev" >&2
        return 2
    }
    parents="$(git rev-list --parents -n 1 "$rev")" || return 2
    # The line is "<commit> <parent>...": the number of words says what kind of commit it is.
    # shellcheck disable=SC2086 # word splitting is the point: count the words
    set -- $parents
    case "$#" in
    1)
        # The root commit: everything in it is an addition.
        tree="$(empty_tree)" || return 2
        base="$tree"
        ;;
    2) base="${rev}^" ;;
    *) return 0 ;; # a merge: the commits it brings in were judged themselves
    esac
    subject="$(git log -1 --format='%h %s' "$rev")" || return 2
    message="$(git log -1 --format=%B "$rev")" || return 2
    judge "commit $subject" "$message" "$base" "$rev"
}

main() {
    [ "$#" -ge 2 ] || usage
    local mode="$1" arg="$2" status=0 rc revs rev tree
    tmp="$(mktemp -d "${TMPDIR:-/tmp}/cucina-test-change.XXXXXX")" || die "cannot create a temporary directory"
    trap 'rm -rf "$tmp"' EXIT
    case "$mode" in
    staged)
        [ -f "$arg" ] || die "message file not found: $arg"
        # Skip fixup!/squash! commits: they are folded into a commit that
        # carries its own message, and the range check sees the final history.
        if head -n 1 "$arg" | grep -Eq '^(fixup|squash|amend)! '; then
            exit 0
        fi
        # A merge commit has no change of its own: its parents' commits were checked
        # (and CI skips merges too).
        if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
            exit 0
        fi
        # No parent yet (first commit): everything is an addition.
        rc=0
        if git rev-parse --verify --quiet HEAD >/dev/null; then
            judge "commit-msg" "$(cat "$arg")" HEAD --cached || rc=$?
        else
            tree="$(empty_tree)" || die "cannot compute the empty tree"
            judge "commit-msg" "$(cat "$arg")" "$tree" --cached || rc=$?
        fi
        [ "$rc" -le "$status" ] || status=$rc
        ;;
    commit)
        check_commit "$arg" || status=$?
        ;;
    range)
        revs="$(git rev-list --reverse --no-merges "$arg")" || die "cannot list the commits of '$arg' (unknown revision?)"
        while IFS= read -r rev; do
            [ -n "$rev" ] || continue
            rc=0
            check_commit "$rev" || rc=$?
            [ "$rc" -le "$status" ] || status=$rc
        done <<EOF
$revs
EOF
        ;;
    *)
        usage
        ;;
    esac
    exit "$status"
}

main "$@"
