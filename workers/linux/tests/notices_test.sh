#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# Guards: R-ARTIFACT — the image legal payload is complete, byte-preserved, and mandatory.
set -euo pipefail
if [[ -n "${TEST_SRCDIR:-}" ]]; then
  root="$TEST_SRCDIR/$TEST_WORKSPACE/workers/linux"
else
  root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fi
work=$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/cucina-notices.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/source/licenses"
printf 'Fixture project license\n' >"$work/source/LICENSE.md"
printf 'Fixture third-party notices\n' >"$work/source/THIRD_PARTY_NOTICES.md"
printf 'Fixture vendor license\n' >"$work/source/licenses/vendor.txt"

bash "$root/scripts/install-notices.sh" "$work/source" "$work/image"
for name in LICENSE.md THIRD_PARTY_NOTICES.md licenses/vendor.txt; do
  cmp "$work/source/$name" "$work/image/$name"
done
# Every required input is a table row, including the redistributed-license directory's contents.
for name in LICENSE.md THIRD_PARTY_NOTICES.md licenses/vendor.txt; do
  mv "$work/source/$name" "$work/withheld"
  if bash "$root/scripts/install-notices.sh" "$work/source" "$work/rejected" >/dev/null 2>&1; then
    printf 'accepted incomplete legal payload: %s\n' "$name" >&2
    exit 1
  fi
  test ! -e "$work/rejected"
  mv "$work/withheld" "$work/source/$name"
done
