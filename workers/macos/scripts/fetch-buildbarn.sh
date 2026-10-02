#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# R-MAC-7/R-VER-3: download immutable darwin/arm64 binaries, checking both the release sha256 asset and our pins.
#   fetch-buildbarn.sh <payload-dir>
#   fetch-buildbarn.sh <release-cache-root> --smoke-servers
# The second form stages host-only bb_scheduler/bb_storage for the tiny remote build; never into the worker image.
set -euo pipefail

die() { printf 'fetch-buildbarn: %s\n' "$*" >&2; exit 1; }
[ $# -ge 1 ] && [ $# -le 2 ] || die 'usage: fetch-buildbarn.sh <dir> [--smoke-servers]'
root="$1"
mode="${2:-}"
case "$mode" in ''|--smoke-servers) ;; *) die "unknown mode $mode" ;; esac
here="$(cd "$(dirname "$0")/.." && pwd)"
versions="$here/versions.json"
command -v jq >/dev/null || die 'jq is required'
sha() { shasum -a 256 "$1" | awk '{print $1}'; }

manifest() { # destination, release URL
  mkdir -p "$1"
  if [ ! -s "$1/sha256" ]; then
    local tmp
    tmp="$(mktemp "$1/.sha256.XXXXXX")"
    if ! curl -fsSL --retry 3 --connect-timeout 20 --max-time 120 -o "$tmp" "$2/sha256"; then
      rm -f "$tmp"; die 'could not fetch the release checksum asset'
    fi
    mv "$tmp" "$1/sha256"
  fi
}

fetch() { # destination, cache, URL, asset, expected SHA-256
  local out="$1" cache="$2" url="$3" name="$4" pin="$5" published tmp
  manifest "$out" "$url"
  published="$(awk -v n="assets/$name" '$2 == n {print $1}' "$out/sha256")"
  [ -n "$published" ] && [ "$published" = "$pin" ] || die "$name: release checksum differs from versions.json"
  if [ -f "$out/$name" ] && [ "$(sha "$out/$name")" = "$pin" ]; then
    :
  else
    tmp="$(mktemp "$out/.$name.XXXXXX")"
    if [ -f "$cache/$name" ] && [ "$(sha "$cache/$name")" = "$pin" ]; then
      cp "$cache/$name" "$tmp"
    elif ! curl -fsSL --retry 3 --connect-timeout 20 --max-time 600 -o "$tmp" "$url/$name"; then
      rm -f "$tmp"; die "$name: download failed"
    fi
    if [ "$(sha "$tmp")" != "$pin" ]; then rm -f "$tmp"; die "$name: downloaded checksum mismatch"; fi
    mv "$tmp" "$out/$name"
  fi
  chmod 0755 "$out/$name"
  printf '%s  %s (release manifest + repository pin verified)\n' "$pin" "$name"
}

if [ "$mode" = --smoke-servers ]; then
  for key in bb_scheduler bb_storage; do
    release="$(jq -r ".smokeServers.$key.release" "$versions")"
    url="$(jq -r ".smokeServers.$key.releaseUrl" "$versions")"
    name="$(jq -r ".smokeServers.$key.name" "$versions")"
    pin="$(jq -r ".smokeServers.$key.sha256" "$versions")"
    fetch "$root/$release" "${CUCINA_DEV_STORAGE:-/nonexistent}/bb-release/$release" "$url" "$name" "$pin"
  done
else
  release="$(jq -r .buildbarn.bbRemoteExecution "$versions")"
  url="$(jq -r .buildbarn.releaseUrl "$versions")"
  cache="${BB_RELEASE_CACHE:-${CUCINA_DEV_STORAGE:-/nonexistent}/bb-release/$release}"
  for key in bb_worker bb_runner; do
    name="$(jq -r ".buildbarn.assets.$key.name" "$versions")"
    pin="$(jq -r ".buildbarn.assets.$key.sha256" "$versions")"
    fetch "$root" "$cache" "$url" "$name" "$pin"
  done
fi
