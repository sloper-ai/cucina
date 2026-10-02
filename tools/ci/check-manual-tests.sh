#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# check-manual-tests.sh: structural check of the manual test checklists
# (docs/testing/manual/MT-NNN.md; policy in TESTING.md, "Manual tests").
#
#   * MT-001 .. MT-008 exist (the minimum set) and every MT file has the
#     front-matter keys, the sections and the [agent]/[human] step markers;
#   * "last run: never" goes with "sign off: pending";
#   * docs/testing/manual/README.md links every MT file.
#
# With --list-agent-steps it prints the steps an agent may pre-run instead.
#
# Usage: tools/ci/check-manual-tests.sh [--dir DIR] [--list-agent-steps]
# Exit:  0 = fine, 1 = problems, 2 = usage error.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dir="$here/../../docs/testing/manual"
list=0
while [ "$#" -gt 0 ]; do
    case "$1" in
    --dir) dir="${2:?--dir needs a directory}"; shift 2 ;;
    --list-agent-steps) list=1; shift ;;
    -h | --help)
        sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "check-manual-tests: unknown argument: $1" >&2
        exit 2
        ;;
    esac
done
[ -d "$dir" ] || {
    echo "check-manual-tests: not a directory: $dir" >&2
    exit 2
}
dir="$(cd "$dir" && pwd)"

files="$(find "$dir" -maxdepth 1 -name 'MT-[0-9][0-9][0-9].md' -type f | sort)"
errors=0

if [ "$list" -eq 1 ]; then
    while IFS= read -r f; do
        [ -n "$f" ] || continue
        awk -v list=1 -f "$here/check-manual-tests.awk" "$f" | while IFS="$(printf '\t')" read -r kind name step; do
            [ "$kind" = S ] && printf '%s: %s\n' "$(basename "$name" .md)" "$step"
        done
    done <<EOF
$files
EOF
    exit 0
fi

for n in 001 002 003 004 005 006 007 008; do
    if [ ! -f "$dir/MT-$n.md" ]; then
        echo "check-manual-tests: MT-$n.md is missing" >&2
        errors=$((errors + 1))
    fi
done

while IFS= read -r f; do
    [ -n "$f" ] || continue
    problems="$(awk -v list=0 -f "$here/check-manual-tests.awk" "$f" | while IFS="$(printf '\t')" read -r kind name msg; do
        [ "$kind" = E ] && printf '%s: %s\n' "$(basename "$name")" "$msg"
    done)"
    if [ -n "$problems" ]; then
        printf '%s\n' "$problems" | sed 's/^/check-manual-tests: /' >&2
        errors=$((errors + 1))
    fi
    if [ -f "$dir/README.md" ] && ! grep -q "$(basename "$f")" "$dir/README.md"; then
        echo "check-manual-tests: README.md does not link $(basename "$f")" >&2
        errors=$((errors + 1))
    fi
done <<EOF
$files
EOF

count="$(printf '%s\n' "$files" | grep -c . || true)"
echo "check-manual-tests: $count manual test file(s) checked"
[ "$errors" -eq 0 ]
