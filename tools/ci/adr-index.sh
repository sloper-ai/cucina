#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# adr-index.sh: keep the index in docs/adr/README.md in step with the ADR files.
#
# Many people add ADRs in parallel, so the index is generated, not edited: it is
# the table between the markers
#     <!-- BEGIN ADR INDEX -->  and  <!-- END ADR INDEX -->
# built from every docs/adr/NNNN-*.md (title = first heading, status = the
# "Status" line, area = the number range, see docs/adr/README.md).
#
# Usage: tools/ci/adr-index.sh [--write | --check]    (default --write)
# Exit:  0 = up to date (or written), 1 = --check found a stale index, 2 = error.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
adr_dir="${CUCINA_ADR_DIR:-$here/../../docs/adr}"
mode="--write"
if [ "$#" -ge 1 ]; then mode="$1"; fi
case "$mode" in
--write | --check) ;;
*)
    echo "usage: adr-index.sh [--write | --check]" >&2
    exit 2
    ;;
esac

readme="$adr_dir/README.md"
[ -f "$readme" ] || {
    echo "adr-index: $readme not found" >&2
    exit 2
}
if ! grep -q '^<!-- BEGIN ADR INDEX -->$' "$readme" || ! grep -q '^<!-- END ADR INDEX -->$' "$readme"; then
    echo "adr-index: $readme lacks the BEGIN/END ADR INDEX markers" >&2
    exit 2
fi

area() { # area <number>
    local n=$((10#$1))
    if [ "$n" -lt 100 ]; then
        echo "architecture"
        return
    fi
    case $((n / 100)) in
    1) echo "build (bazel)" ;;
    2) echo "aws (infra)" ;;
    3) echo "images" ;;
    4) echo "chart and buildbarn" ;;
    5) echo "controller and scaling" ;;
    6) echo "auth, STS, PKI" ;;
    7) echo "macOS, hostd, pkg" ;;
    8) echo "cli" ;;
    9) echo "cross-platform" ;;
    10) echo "testing and e2e" ;;
    *) echo "other" ;;
    esac
}

table="$(
    echo "| ADR | Title | Status | Area |"
    echo "| --- | --- | --- | --- |"
    for f in "$adr_dir"/[0-9][0-9][0-9][0-9]-*.md; do
        [ -f "$f" ] || continue
        base="$(basename "$f")"
        num="${base%%-*}"
        title="$(sed -n 's/^#[[:space:]]*//p' "$f" | head -n 1)"
        # "0001 — Title", "ADR 0001: Title", "0001. Title" -> "Title"
        title="$(printf '%s' "$title" | sed -E 's/^(ADR[[:space:]]*)?[0-9]+[[:space:]]*[—:.-]*[[:space:]]*//')"
        status="$(grep -i -m 1 -E '^[*_[:space:]-]*status[*_]*[[:space:]]*:' "$f" |
            sed -E 's/^[^:]*:[*_[:space:]]*//; s/[[:space:]]*\(.*$//; s/[*_]+$//' || true)"
        [ -n "$status" ] || status="?"
        printf '| [%s](%s) | %s | %s | %s |\n' "$num" "$base" "$title" "$status" "$(area "$num")"
    done
)"

tmp="$(mktemp)"
table_file="$(mktemp)"
trap 'rm -f "$tmp" "$table_file"' EXIT
printf '%s\n' "$table" >"$table_file"
awk -v table_file="$table_file" '
    /^<!-- BEGIN ADR INDEX -->$/ {
        print
        while ((getline row < table_file) > 0) print row
        close(table_file)
        skipping = 1
        next
    }
    /^<!-- END ADR INDEX -->$/ { skipping = 0 }
    !skipping { print }
' "$readme" >"$tmp"

if cmp -s "$tmp" "$readme"; then
    [ "$mode" = "--check" ] && echo "adr-index: index is up to date"
    exit 0
fi
if [ "$mode" = "--check" ]; then
    echo "adr-index: docs/adr/README.md is stale; run tools/ci/adr-index.sh" >&2
    exit 1
fi
cat "$tmp" >"$readme"
echo "adr-index: updated $readme"
