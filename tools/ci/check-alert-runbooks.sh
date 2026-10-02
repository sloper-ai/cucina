#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# check-alert-runbooks.sh: every alert the chart ships appears in the alert table
# of docs/operations/README.md (with its runbook, or the statement that it has
# none), and the table names no alert that does not exist.
#
#   * the alerts are the `- alert: <Name>` entries of the chart's rule files;
#   * the table is every `Cucina<Name>` in backticks in the operations README.
#
# Usage: tools/ci/check-alert-runbooks.sh [--rules FILE]... [--readme FILE]
#        (defaults: charts/cucina/files/rules/*.yaml and docs/operations/README.md)
# Exit:  0 = in sync, 1 = drift, 2 = usage error.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$here/../.."
rules=()
readme="$root/docs/operations/README.md"

while [ "$#" -gt 0 ]; do
    case "$1" in
    --rules) rules+=("${2:?--rules needs a file}"); shift 2 ;;
    --readme) readme="${2:?--readme needs a file}"; shift 2 ;;
    -h | --help)
        sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "check-alert-runbooks: unknown argument: $1" >&2
        exit 2
        ;;
    esac
done
if [ "${#rules[@]}" -eq 0 ]; then
    for f in "$root"/charts/cucina/files/rules/*.yaml; do rules+=("$f"); done
fi
for f in "${rules[@]}" "$readme"; do
    [ -f "$f" ] || {
        echo "check-alert-runbooks: no such file: $f" >&2
        exit 2
    }
done

alerts="$(grep -h -E '^[[:space:]]*-[[:space:]]alert:[[:space:]]' "${rules[@]}" |
    sed -E 's/^[[:space:]]*-[[:space:]]alert:[[:space:]]*//; s/[[:space:]]*#.*$//; s/[[:space:]]+$//; s/^"(.*)"$/\1/' | sort -u || true)"
tick="$(printf '\140')"
documented="$(grep -o -E "${tick}Cucina[A-Za-z0-9]+${tick}" "$readme" | tr -d "$tick" | sort -u || true)"

status=0
if [ -z "$alerts" ]; then
    echo "check-alert-runbooks: no alerts found in the rule files; is the pattern still right?" >&2
    exit 2
fi
while IFS= read -r a; do
    [ -n "$a" ] || continue
    if ! printf '%s\n' "$documented" | grep -qx "$a"; then
        echo "check-alert-runbooks: the alert $a is not in the table of $(basename "$readme"): add its runbook (or say it has none)" >&2
        status=1
    fi
done <<EOF
$alerts
EOF
while IFS= read -r d; do
    [ -n "$d" ] || continue
    if ! printf '%s\n' "$alerts" | grep -qx "$d"; then
        echo "check-alert-runbooks: $d is in the table of $(basename "$readme") but no rule file defines it" >&2
        status=1
    fi
done <<EOF
$documented
EOF

count="$(printf '%s\n' "$alerts" | grep -c . || true)"
echo "check-alert-runbooks: $count alert(s) checked"
exit "$status"
