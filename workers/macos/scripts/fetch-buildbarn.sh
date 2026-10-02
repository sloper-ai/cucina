#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Fetch the pinned Buildbarn darwin/arm64 binaries for the macOS worker image and verify each one twice: against the
# release's own `sha256` asset and against the pin in ../versions.json (R-0.5: binaries by version + SHA-256).
#   fetch-buildbarn.sh <out-dir>
# A local copy whose hash matches (e.g. $CUCINA_DEV_STORAGE/bb-release/<release>/, BB_RELEASE_CACHE) is reused.
set -euo pipefail

die() { printf 'fetch-buildbarn: %s\n' "$*" >&2; exit 1; }
[ $# -eq 1 ] || die "usage: fetch-buildbarn.sh <out-dir>"
out="$1"
here="$(cd "$(dirname "$0")/.." && pwd)"
versions="$here/versions.json"
command -v jq >/dev/null || die "jq is required"

release="$(jq -r .buildbarn.bbRemoteExecution "$versions")"
url="$(jq -r .buildbarn.releaseUrl "$versions")"
cache="${BB_RELEASE_CACHE:-${CUCINA_DEV_STORAGE:-/nonexistent}/bb-release/$release}"
mkdir -p "$out"

sha() { shasum -a 256 "$1" | awk '{print $1}'; }

curl -fsSL --retry 3 --retry-delay 2 -o "$out/sha256.tmp" "$url/sha256"
mv "$out/sha256.tmp" "$out/sha256"

for key in bb_worker bb_runner; do
  name="$(jq -r ".buildbarn.assets.$key.name" "$versions")"
  pin="$(jq -r ".buildbarn.assets.$key.sha256" "$versions")"
  published="$(awk -v n="assets/$name" '$2 == n {print $1}' "$out/sha256")"
  [ -n "$published" ] || die "$name is not listed in the $release sha256 asset"
  [ "$published" = "$pin" ] || die "$name: release sha256 asset says $published, versions.json pins $pin"
  if [ -f "$out/$name" ] && [ "$(sha "$out/$name")" = "$pin" ]; then
    :
  elif [ -f "$cache/$name" ] && [ "$(sha "$cache/$name")" = "$pin" ]; then
    cp "$cache/$name" "$out/$name.tmp"
    mv "$out/$name.tmp" "$out/$name"
  else
    curl -fsSL --retry 3 --retry-delay 2 -o "$out/$name.tmp" "$url/$name"
    got="$(sha "$out/$name.tmp")"
    [ "$got" = "$pin" ] || { rm -f "$out/$name.tmp"; die "$name: downloaded sha256 $got, want $pin"; }
    mv "$out/$name.tmp" "$out/$name"
  fi
  chmod 0755 "$out/$name"
  printf '%s  %s (%s, verified against the release sha256 asset and versions.json)\n' "$pin" "$name" "$release"
done
