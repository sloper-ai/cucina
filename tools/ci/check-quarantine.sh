#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# check-quarantine.sh: enforce the flake policy on the BUILD files (TESTING.md,
# "Flaky tests and quarantine"):
#
#   * no test sets `flaky = True` (gating runs use --flaky_test_attempts=1);
#   * no target uses the `exclusive` tag (it disables remote execution; use
#     `exclusive-if-local`);
#   * a quarantined test carries the tags
#         "quarantine"                     the marker (excluded from gating runs)
#         "quarantine-until-YYYY-MM-DD"    expiry, at most 14 days away
#         "quarantine-issue-<number>"      the tracking issue
#         "quarantine-owner-<handle>"      who fixes it
#   * an expired quarantine fails CI: fix the test or delete it;
#   * at most 5 tests are quarantined at once.
#
# The tags must be literals in the BUILD file (that is how this script reads
# them); the tier macros pass them through unchanged.
#
# Usage: tools/ci/check-quarantine.sh [--root DIR] [--today YYYY-MM-DD] [--max N] [--max-days N]
# Exit:  0 = policy holds, 1 = violations, 2 = usage error.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="."
today=""
max_quarantined=5
max_days=14

while [ "$#" -gt 0 ]; do
    case "$1" in
    --root) root="${2:?--root needs a directory}"; shift 2 ;;
    --today) today="${2:?--today needs YYYY-MM-DD}"; shift 2 ;;
    --max) max_quarantined="${2:?--max needs a number}"; shift 2 ;;
    --max-days) max_days="${2:?--max-days needs a number}"; shift 2 ;;
    -h | --help)
        sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "check-quarantine: unknown argument: $1" >&2
        exit 2
        ;;
    esac
done

[ -d "$root" ] || {
    echo "check-quarantine: not a directory: $root" >&2
    exit 2
}
root="$(cd "$root" && pwd)"
[ -n "$today" ] || today="$(date -u +%Y-%m-%d)"
case "$today" in
[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) ;;
*)
    echo "check-quarantine: --today must be YYYY-MM-DD, got: $today" >&2
    exit 2
    ;;
esac

# Search relative to the root: the exclusions must apply to what is inside the
# checkout, not to the directories above it (a checkout under ".work/" or a
# "bazel-*" output tree would otherwise find no BUILD file at all).
cd "$root"
files="$(find . \( -name BUILD -o -name BUILD.bazel \) -type f \
    -not -path '*/.git/*' -not -path '*/bazel-*' -not -path '*/third_party/*' \
    -not -path '*/node_modules/*' -not -path '*/.work/*' | sort)"

if [ -z "$files" ]; then
    echo "check-quarantine: no BUILD files under $root; nothing to check"
    exit 0
fi

report="$(printf '%s\n' "$files" | tr '\n' '\0' |
    xargs -0 awk -f "$here/check-quarantine.awk" -v root=. -v today="$today" -v max_days="$max_days")"

errors=0
count=0
details=""
while IFS="$(printf '\t')" read -r kind a b; do
    case "$kind" in
    Q)
        count=$((count + 1))
        details="${details}  ${a}  until ${b:-?}"$'\n'
        ;;
    E)
        printf 'check-quarantine: %s: %s\n' "$a" "$b" >&2
        errors=$((errors + 1))
        ;;
    esac
done <<EOF
$report
EOF

if [ "$count" -gt "$max_quarantined" ]; then
    echo "check-quarantine: $count tests are quarantined, the limit is $max_quarantined: fix some before quarantining more" >&2
    errors=$((errors + 1))
fi

echo "check-quarantine: $count quarantined test target(s), limit $max_quarantined (today $today)"
[ -z "$details" ] || printf '%s' "$details"

[ "$errors" -eq 0 ]
