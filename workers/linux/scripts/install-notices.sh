#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# R-ARTIFACT: install the staged, mandatory legal payload without discarding vendor license texts.
# Usage: install-notices.sh SOURCE_DIRECTORY DESTINATION_DIRECTORY
set -euo pipefail
source_dir=${1:?source directory required}
destination=${2:?destination directory required}
inputs=("$source_dir/LICENSE.md" "$source_dir/THIRD_PARTY_NOTICES.md" "$source_dir/licenses/"*.txt)
# Validate the whole payload before modifying the destination. An unmatched glob also fails this check.
for file in "${inputs[@]}"; do
  [[ -f "$file" && -s "$file" ]] || { printf 'missing or empty legal payload: %s\n' "$file" >&2; exit 1; }
done
install -d -m 0755 "$destination" "$destination/licenses"
for file in "${inputs[@]}"; do
  case "$file" in
    "$source_dir/licenses/"*) target="$destination/licenses/${file##*/}" ;;
    *) target="$destination/${file##*/}" ;;
  esac
  install -m 0644 "$file" "$target"
  cmp "$file" "$target"
done
printf 'Installed %s legal documents in %s\n' "${#inputs[@]}" "$destination"
