#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# mutate-hooks.sh: prove that every rule of the git hooks is load-bearing
# (TESTING.md, "Mutation testing"; ADR 0025).
#
# For each mutant - one pattern, alternative or condition of the files in
# .githooks deleted or disabled - it copies the hooks to a temporary directory,
# applies the mutation and runs tools/ci/test-githooks.sh against the copy. A
# mutant that still passes every case is a rule without a test: add a case to
# tools/ci/test-githooks.sh (the "rules" suite has one case per pattern).
# The control mutant (a comment edit) must survive: if it is caught, the harness
# itself is broken.
#
# Usage: tools/ci/mutate-hooks.sh [--list] [--jobs N] [--only TEXT]
#   --list     print the mutants and exit
#   --jobs N   run N mutants in parallel (default 4)
#   --only T   run only the mutants whose description contains T
# Exit: 0 = every mutant was caught, 1 = a mutant survived, 2 = usage error or a
# stale mutant (its text is no longer in the file: update the list below).
# Takes a few minutes; run it when you change the hooks, and nightly.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
hooks="${CUCINA_HOOKS_DIR:-$(cd "$here/../../.githooks" && pwd)}"
jobs=4
only=""
mode=run
one=""

while [ "$#" -gt 0 ]; do
    case "$1" in
    --list) mode=list; shift ;;
    --jobs) jobs="${2:?--jobs needs a number}"; shift 2 ;;
    --only) only="${2:?--only needs text}"; shift 2 ;;
    --one) mode=one; one="${2:?--one needs an index}"; shift 2 ;;
    -h | --help)
        sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "mutate-hooks: unknown argument: $1" >&2
        exit 2
        ;;
    esac
done

m_file=()
m_suites=()
m_from=()
m_to=()
m_control=()

# mutant <file> <suites> <from> <to> [control]: replace the text <from> (it must
# occur exactly once) by <to>; run the suites in order until one fails.
mutant() {
    m_file+=("$1")
    m_suites+=("$2")
    m_from+=("$3")
    m_to+=("$4")
    m_control+=("${5:-}")
}

# drop_each <file> <suites> <separator> <open> <alternatives> <close>: one mutant
# per alternative of <open><alternatives><close>, with that alternative removed.
drop_each() {
    local file="$1" suites="$2" sep="$3" open="$4" alts="$5" close="$6"
    local parts i j rest
    IFS="$sep" read -r -a parts <<<"$alts"
    for ((i = 0; i < ${#parts[@]}; i++)); do
        rest=""
        for ((j = 0; j < ${#parts[@]}; j++)); do
            [ "$j" -eq "$i" ] && continue
            rest="${rest:+$rest$sep}${parts[$j]}"
        done
        mutant "$file" "$suites" "$open$alts$close" "$open$rest$close"
    done
}

# The patterns are regular expressions and shell text that must not expand: single quotes on purpose.
# shellcheck disable=SC2016
define_mutants() {
    local d=lib/classify-diff.awk b=lib/classify-build.awk t=lib/test-change.sh h=lib/hygiene.sh
    local cls="rules commit-msg" tc="commit-msg rules" pp="pre-push"
    local q="'"
    local never='/@never@/'

    # ---- classify-diff.awk: which files are test sources, goldens, checklists.
    mutant "$d" "$cls" 'return (p ~ /_test\.go$/ ||' 'return (p ~ '"$never"' ||'
    drop_each "$d" "$cls" '|' 'p ~ /_test\.(' 'rs|sh|py|cc|cpp|c|ts|js' ')$/'
    mutant "$d" "$cls" 'p ~ /(^|\/)test(-[^\/]*)?\.sh$/' 'p ~ /(^|\/)test-[^\/]*\.sh$/'
    mutant "$d" "$cls" 'p ~ /(^|\/)test(-[^\/]*)?\.sh$/' "p ~ $never"
    mutant "$d" "$cls" 'p ~ /\.tftest\.hcl$/' "p ~ $never"
    mutant "$d" "$cls" 'p ~ /(^|\/)tests?\//' "p ~ $never"
    mutant "$d" "$cls" 'p ~ /(^|\/)tests?\//' 'p ~ /(^|\/)tests\//'
    mutant "$d" "$cls" 'p ~ /(^|\/)tests?\//' 'p ~ /(^|\/)test\//'
    mutant "$d" "$cls" 'p ~ /(^|\/)testdata\//' "p ~ $never"
    mutant "$d" "$cls" 'p ~ /\.golden(\.|$)/' "p ~ $never"
    mutant "$d" "$cls" 'p ~ /\.golden(\.|$)/' 'p ~ /\.golden$/'
    mutant "$d" "$cls" 'p ~ /\.golden(\.|$)/' 'p ~ /\.golden\./'
    mutant "$d" "$cls" 'p ~ /(^|\/)goldens?\//' "p ~ $never"
    mutant "$d" "$cls" 'p ~ /(^|\/)goldens?\//' 'p ~ /(^|\/)goldens\//'
    mutant "$d" "$cls" 'p ~ /(^|\/)goldens?\//' 'p ~ /(^|\/)golden\//'
    mutant "$d" "$cls" 'p ~ /__snapshots__\//' "p ~ $never"
    mutant "$d" "$cls" 'p ~ /\.snap$/' "p ~ $never"
    mutant "$d" "$cls" 'p ~ /^sim\/scenarios\//' "p ~ $never"
    mutant "$d" "$cls" 'return (p ~ /^docs\/testing\/manual\/MT-[0-9]+\.md$/)' 'return (0)'
    mutant "$d" "$cls" 'return (t ~ /^\/\//)' 'return (0)'
    mutant "$d" "$cls" 'cur_go_test = (p ~ /_test\.go$/ && p !~ /^test\/e2e\//)' 'cur_go_test = (p ~ /_test\.go$/)'
    mutant "$d" "$cls" 'cur_marker_lang = (p ~ /_test\.go$/ || p ~ /\.rs$/)' 'cur_marker_lang = (p ~ /\.rs$/)'
    mutant "$d" "$cls" 'cur_marker_lang = (p ~ /_test\.go$/ || p ~ /\.rs$/)' 'cur_marker_lang = (p ~ /_test\.go$/)'
    mutant "$d" "$cls" 'cur_rust = (p ~ /\.rs$/)' 'cur_rust = (0)'

    # ---- classify-diff.awk: what counts as a finding.
    mutant "$d" "$cls" 'if (cur_manual && cur_deleted && !(path in pure_path)) {' 'if (cur_manual && cur_deleted) {'
    mutant "$d" "$cls" 'if (!(path in pure_path)) flag("golden-deleted", path)' 'flag("golden-deleted", path)'
    mutant "$d" "$cls" '} else if (cur_binary && !cur_new) {' '} else if (cur_binary) {'
    mutant "$d" "$cls" '} else if (cur_removed > 0) {' '} else if (0) {'
    mutant "$d" "$cls" 'tsrc_added += cur_added' 'tsrc_added += 0'
    mutant "$d" "$cls" 'for (i = gone + 1; i <= skip_count[f]; i++)' 'for (i = 1; i <= skip_count[f]; i++)'
    mutant "$d" "$cls" 'if (!(deleted_path[i] in moved_path)) {' 'if (1) {'
    mutant "$d" "$cls" 'if (net > shrink) {' 'if (0) {'
    mutant "$d" "$cls" 'if (mark_removed > mark_added) {' 'if (0) {'
    mutant "$d" "$cls" 'if (part[1] == "R100") pure_path[part[2]] = 1' 'pure_path[part[2]] = 1'
    mutant "$d" "$cls" 'moved_path[part[2]] = 1' 'moved_unused[part[2]] = 1'
    mutant "$d" "$cls" 'moved_from[part[3]] = part[2]' 'moved_from[part[3]] = ""'

    # ---- classify-diff.awk: test-case markers and skip markers.
    mutant "$d" "$cls" 'if (path ~ /_test\.go$/) {' 'if (0) {'
    mutant "$d" "$cls" '(Test|Fuzz)[A-Za-z0-9_]*' '(Test)[A-Za-z0-9_]*'
    mutant "$d" "$cls" '(Test|Fuzz)[A-Za-z0-9_]*' '(Fuzz)[A-Za-z0-9_]*'
    mutant "$d" "$cls" 'if (text ~ /(^|[^A-Za-z0-9_])t\.Run\(/) return 1' 'if (0) return 1'
    mutant "$d" "$cls" 'rapid\.(Check|MakeCheck)\(' 'rapid\.(Check)\('
    mutant "$d" "$cls" 'rapid\.(Check|MakeCheck)\(' 'rapid\.(MakeCheck)\('
    mutant "$d" "$cls" '#\[(tokio::)?(test|rstest)' '#\[(test|rstest)'
    mutant "$d" "$cls" '#\[(tokio::)?(test|rstest)' '#\[(tokio::)?(rstest)'
    mutant "$d" "$cls" '#\[(tokio::)?(test|rstest)' '#\[(tokio::)?(test)'
    mutant "$d" "$cls" 'if (text ~ /proptest!/) return 1' 'if (0) return 1'
    mutant "$d" "$cls" 't ~ /\.Skip(f|Now)?\(/' 't ~ /\.Skip\(/'
    mutant "$d" "$cls" 't ~ /\.Skip(f|Now)?\(/' 't ~ /\.Skip(f)?\(/'
    mutant "$d" "$cls" 't ~ /\.Skip(f|Now)?\(/' 't ~ /\.Skip(Now)?\(/'
    mutant "$d" "$cls" 'if (cur_go_test && t ~' 'if (0 && t ~'
    mutant "$d" "$cls" 't ~ /#\[ignore/' "t ~ $never"
    mutant "$d" "$cls" 'if (cur_rust && t ~' 'if (0 && t ~'
    mutant "$d" "$cls" 'if (!is_comment(t) && is_skip(t) && ((path' 'if (is_skip(t) && ((path'
    mutant "$d" "$cls" '((path in moved_to) || (!cur_new && hunk_new_cases <= hunk_removed_cases))' '(!cur_new && hunk_new_cases <= hunk_removed_cases)'
    mutant "$d" "$cls" '((path in moved_to) || (!cur_new && hunk_new_cases <= hunk_removed_cases))' '((path in moved_to) || (hunk_new_cases <= hunk_removed_cases))'
    mutant "$d" "$cls" '((path in moved_to) || (!cur_new && hunk_new_cases <= hunk_removed_cases))' '((path in moved_to) || (!cur_new))'
    mutant "$d" "$cls" 'if (!is_comment(t) && is_skip(t)) skip_removed++' 'if (is_skip(t)) skip_removed++'

    # ---- classify-build.awk: BUILD files.
    mutant "$b" "$cls" 'return (r ~ /_test$/ || r == "cucina_scenario" || r == "test_suite")' 'return (r == "cucina_scenario" || r == "test_suite")'
    mutant "$b" "$cls" 'return (r ~ /_test$/ || r == "cucina_scenario" || r == "test_suite")' 'return (r ~ /_test$/ || r == "test_suite")'
    mutant "$b" "$cls" 'return (r ~ /_test$/ || r == "cucina_scenario" || r == "test_suite")' 'return (r ~ /_test$/ || r == "cucina_scenario")'
    mutant "$b" "$cls" 'if (s ~ /"manual"/) n++' 'if (0) n++'
    mutant "$b" "$cls" 'if (s ~ /"quarantine"/) n++' 'if (0) n++'
    mutant "$b" "$cls" 'if (s ~ /@platforms\/\/:incompatible/) n++' 'if (0) n++'
    mutant "$b" "$cls" 'if (s ~ /flaky[ \t]*=[ \t]*(True|1)/) n++' 'if (0) n++'
    mutant "$b" "$cls" '(True|1)/) n++' '(True)/) n++'
    mutant "$b" "$cls" '(True|1)/) n++' '(1)/) n++'
    mutant "$b" "$cls" 'sub(/(^|[ \t])#.*$/, "", s)' 'sub(/[ \t]#.*$/, "", s)'
    mutant "$b" "$cls" 'sub(/(^|[ \t])#.*$/, "", s)' 'sub(/(^)#.*$/, "", s)'
    mutant "$b" "$cls" 'if (!is_test_rule(kind)) return' 'if (0) return'
    mutant "$b" "$cls" 'if (u_marks["new", k] > u_marks["old", k]) {' 'if (0) {'
    mutant "$b" "$cls" 'if (u_tier["old", k] != u_tier["new", k]) {' 'if (0) {'
    mutant "$b" "$cls" 'if (removed > added) {' 'if (0) {'

    # ---- test-change.sh: the trailer and what is skipped.
    mutant "$t" "$tc" 'min_reason_chars=8' 'min_reason_chars=7'
    mutant "$t" "$tc" 'min_reason_chars=8' 'min_reason_chars=9'
    mutant "$t" "$tc" "tr -d ${q}[:space:]${q}" 'cat'
    mutant "$t" "$tc" '[Tt][Ee][Ss][Tt]-[Cc][Hh][Aa][Nn][Gg][Ee]' 'Test-Change'
    mutant "$t" "$tc" '(fixup|squash|amend)!' '(squash|amend)!'
    mutant "$t" "$tc" '(fixup|squash|amend)!' '(fixup|amend)!'
    mutant "$t" "$tc" '(fixup|squash|amend)!' '(fixup|squash)!'
    mutant "$t" "$tc" 'git rev-parse -q --verify MERGE_HEAD' 'git rev-parse -q --verify NO_SUCH_HEAD'
    mutant "$t" "$tc" '*) return 0 ;; # a merge' '*) base="${rev}^" ;; # a merge'
    mutant "$t" "$tc" '2) base="${rev}^" ;;' '2) base="${rev}" ;;'
    mutant "$t" "$tc" 'base="$tree"' 'base="${rev}^"'
    mutant "$t" "$tc" 'git "${common[@]}" diff -M --diff-filter=R' 'git "${common[@]}" diff --diff-filter=R'
    mutant "$t" "$tc" '-U0 --no-color --no-ext-diff --no-textconv --no-renames' '-U0 --no-color --no-ext-diff --no-textconv'
    mutant "$t" "$tc" '-U0 --no-color --no-ext-diff --no-textconv --no-renames' '-U0 --no-ext-diff --no-textconv --no-renames'
    mutant "$t" "$tc" '-U0 --no-color --no-ext-diff --no-textconv --no-renames' '-U0 --no-color --no-textconv --no-renames'
    mutant "$t" "$tc" '-U0 --no-color --no-ext-diff --no-textconv --no-renames' '-U0 --no-color --no-ext-diff --no-renames'
    mutant "$t" "$tc" '--src-prefix=a/ --dst-prefix=b/' ''
    mutant "$t" "$tc" '-c core.quotepath=false -c diff.relative=false' '-c diff.relative=false'
    mutant "$t" "$tc" '-c core.quotepath=false -c diff.relative=false' '-c core.quotepath=false'
    mutant "$t" "$tc" 'diff --name-only -z --no-color --no-ext-diff --no-renames' 'diff --name-only -z --no-color --no-ext-diff'
    mutant "$t" "$tc" $'refusing it" >&2\n        return 2' $'refusing it" >&2\n        return 0'
    mutant "$t" "$tc" $'not a commit: $rev" >&2\n        return 2' $'not a commit: $rev" >&2\n        return 0'
    mutant "$t" "$tc" 'revs="$(git rev-list --reverse --no-merges "$arg")" || die' 'revs="$(git rev-list --reverse --no-merges "$arg")" || echo'

    # ---- hygiene.sh: identifiers, documentation placeholders, fail-closed.
    mutant "$h" "$pp" 'patterns="arn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:${account}:"' 'patterns="@never@"'
    mutant "$h" "$pp" 'arn:aws[a-z-]*:' 'arn:aws:'
    mutant "$h" "$pp" 'patterns="${patterns}|${account}\\.dkr\\.ecr(-fips)?\\.[a-z0-9-]+\\.amazonaws\\.com"' 'patterns="${patterns}"'
    mutant "$h" "$pp" '\\.dkr\\.ecr(-fips)?\\.' '\\.dkr\\.ecr\\.'
    mutant "$h" "$pp" 'patterns="${patterns}|ec2-[0-9]+-[0-9]+-[0-9]+-[0-9]+\\.[a-z0-9.-]*amazonaws\\.com"' 'patterns="${patterns}"'
    mutant "$h" "$pp" 'patterns="${patterns}|[a-z0-9-]+\\.awsapps\\.com/start"' 'patterns="${patterns}"'
    drop_each "$h" "$pp" ' ' "allowed_accounts=' " '123456789012 111122223333 444455556666 777788889999 000000000000 999999999999 210987654321' " '"
    mutant "$h" "$pp" '192.0.2.* | 198.51.100.* | 203.0.113.*)' '198.51.100.* | 203.0.113.*)'
    mutant "$h" "$pp" '192.0.2.* | 198.51.100.* | 203.0.113.*)' '192.0.2.* | 203.0.113.*)'
    mutant "$h" "$pp" '192.0.2.* | 198.51.100.* | 203.0.113.*)' '192.0.2.* | 198.51.100.*)'
    mutant "$h" "$pp" '    ec2-*)' '    ec2-@never@)'
    mutant "$h" "$pp" '!in_diff && NF { print c' '!in_diff && 0 { print c'
    mutant "$h" "$pp" $'(unknown revision?)" >&2\n    exit 2' $'(unknown revision?)" >&2\n    exit 0'

    # ---- pre-push.
    local p=pre-push
    mutant "$p" "$pp" '--no-banner --redact --verbose' '--no-banner --verbose'
    mutant "$p" "$pp" '--no-banner --redact --verbose' '--no-banner --redact'
    mutant "$p" "$pp" '"--not" "--remotes")' '"--not" "--remotes=${1:-origin}")'
    mutant "$p" "$pp" 'if [[ $local_sha =~ $zero ]]; then' 'if false; then'

    # ---- control: a comment edit changes no behaviour, so this mutant must survive.
    mutant "$t" "$tc" '# Written for bash 3.2' '# Written for bash 3.2 (control)' control
}

describe() { # describe <index>
    printf '%s: %s -> %s' "${m_file[$1]}" "${m_from[$1]}" "${m_to[$1]}"
}

# run_one <index>: print "killed", "SURVIVED" or "STALE" with the description; exit status 0/1/2.
run_one() {
    local i="$1" file suites from to dir content count rest suite log rc
    file="${m_file[$i]}"
    suites="${m_suites[$i]}"
    from="${m_from[$i]}"
    to="${m_to[$i]}"
    dir="$(mktemp -d "${TMPDIR:-/tmp}/cucina-mutant.XXXXXX")"
    cp -R "$hooks/." "$dir/"
    # Keep the trailing newlines: append a sentinel and strip it again.
    content="$(
        cat "$dir/$file"
        printf x
    )"
    content="${content%x}"
    rest="${content//"$from"/}"
    count=$(((${#content} - ${#rest}) / ${#from}))
    if [ "$count" -ne 1 ]; then
        echo "STALE     $(describe "$i") (found $count times, expected once)"
        rm -rf "$dir"
        return 2
    fi
    printf '%s' "${content/"$from"/"$to"}" >"$dir/$file"
    log="$dir/mutant.log"
    rc=1
    for suite in $suites; do
        if CUCINA_HOOKS_DIR="$dir" CUCINA_TEST_FAIL_FAST=1 "$here/test-githooks.sh" "$suite" >"$log" 2>&1; then
            rc=0
        else
            rc=1
            break
        fi
    done
    rm -rf "$dir"
    if [ "$rc" -eq 0 ]; then
        if [ -n "${m_control[$i]}" ]; then
            echo "survived  $(describe "$i") (the control mutant, as it must)"
            return 0
        fi
        echo "SURVIVED  $(describe "$i")"
        return 1
    fi
    if [ -n "${m_control[$i]}" ]; then
        echo "BROKEN    the control mutant was caught: $(describe "$i")"
        return 2
    fi
    echo "killed    $(describe "$i")"
}

define_mutants
n="${#m_file[@]}"

case "$mode" in
list)
    for ((i = 0; i < n; i++)); do echo "$i $(describe "$i")"; done
    exit 0
    ;;
one)
    run_one "$one"
    exit $?
    ;;
esac

indices=""
for ((i = 0; i < n; i++)); do
    if [ -z "$only" ] || [[ "$(describe "$i")" == *"$only"* ]]; then
        indices="$indices $i"
    fi
done

work="$(mktemp -d "${TMPDIR:-/tmp}/cucina-mutate.XXXXXX")"
trap 'rm -rf "$work"' EXIT
export CUCINA_HOOKS_DIR="$hooks"
# Each mutant is a separate process (--one), so they can run in parallel.
# shellcheck disable=SC2086 # the indices are a plain word list
printf '%s\n' $indices | xargs -P "$jobs" -I{} "$0" --one {} >"$work/results" 2>&1 || true

killed="$(grep -c '^killed' "$work/results" || true)"
survived="$(grep -c '^SURVIVED' "$work/results" || true)"
bad="$(grep -c -E '^(STALE|BROKEN)' "$work/results" || true)"
grep -E '^(SURVIVED|STALE|BROKEN)' "$work/results" | sort || true
total=0
for i in $indices; do total=$((total + 1)); done
echo "mutate-hooks: $killed mutant(s) caught, $survived survived, $bad stale or broken (of $total)"
if [ "$bad" -gt 0 ]; then exit 2; fi
[ "$survived" -eq 0 ]
