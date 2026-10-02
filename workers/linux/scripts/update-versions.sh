#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Records the installed-version inventory of a Linux image (/etc/cucina/image.json, downloaded by Packer) under
# `installed.<family>` in workers/linux/versions.json (R-VER-1).
#   update-versions.sh <family> <image.json>
set -euo pipefail

family=${1:?usage: update-versions.sh <family> <image.json>}
inventory=${2:?usage: update-versions.sh <family> <image.json>}
versions="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/versions.json"
tmp=$(mktemp "${versions}.XXXXXX")
trap 'rm -f "$tmp"' EXIT
jq --indent 2 --arg f "$family" --slurpfile i "$inventory" '.installed[$f] = $i[0]' "$versions" >"$tmp"
mv "$tmp" "$versions"
echo "update-versions: $family kernel $(jq -r '.kernel' "$inventory")"
