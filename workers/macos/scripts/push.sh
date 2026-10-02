#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Publish a locally built worker image to the PRIVATE GHCR package (R-OPS-7): Xcode's licence forbids public
# redistribution, so the package `ghcr.io/sloper-ai/cucina-worker-macos` must stay private.
#   CUCINA_PUBLISH=1 TART_REGISTRY_USERNAME=<user> TART_REGISTRY_PASSWORD=<token with write:packages> \
#     push.sh cucina-worker-macos:27.0-0.1.0 [extra-tag ...]
# * Credentials come from the environment only (Tart reads TART_REGISTRY_*); they are never written to disk or the
#   keychain. Hosts pull with a separate read-only package credential handed out by the controller (hostd contract).
# * Labels on the OCI config carry the version metadata that maps the image to a pool generation.
# * Re-pushing a new version reuses unchanged disk chunks already in the repository (Tart's chunked disk layers), and
#   hosts that hold the previous version download only changed chunks (R-DATA-5).
# Nothing is published during the acceptance campaign: the guard below refuses to run without CUCINA_PUBLISH=1.
set -euo pipefail

die() { printf 'push: %s\n' "$*" >&2; exit 1; }
[ $# -ge 1 ] || die "usage: push.sh <local-image> [extra-tag ...]"
[ "${CUCINA_PUBLISH:-}" = "1" ] || die "refusing to publish without CUCINA_PUBLISH=1 (R-OPS-7: releases are the user's call)"
[ -n "${TART_REGISTRY_USERNAME:-}" ] && [ -n "${TART_REGISTRY_PASSWORD:-}" ] || die "set TART_REGISTRY_USERNAME/TART_REGISTRY_PASSWORD"

local_image="$1"
shift
repository="${CUCINA_IMAGE_REPOSITORY:-ghcr.io/sloper-ai/cucina-worker-macos}"
here="$(cd "$(dirname "$0")/.." && pwd)"
versions="$here/versions.json"
tag="${local_image##*:}"
xcode="${tag%%-*}"
cucina_version="${tag#*-}"
xcode_build="$(jq -r --arg x "$xcode" '.xcode[$x].build' "$versions")"
release="$(jq -r --arg x "$xcode" '.xcode[$x].macosRelease' "$versions")"
bb="$(jq -r .buildbarn.bbRemoteExecution "$versions")"
tart get "$local_image" >/dev/null || die "no local image $local_image"

targets=("$repository:$tag")
for extra in "$@"; do targets+=("$repository:$extra"); done

tart push "$local_image" "${targets[@]}" \
  --label "org.opencontainers.image.source=https://github.com/sloper-ai/cucina" \
  --label "org.opencontainers.image.version=$tag" \
  --label "org.opencontainers.image.licenses=LicenseRef-Proprietary-Xcode AND FSL-1.1-ALv2" \
  --label "ai.sloper.cucina.image-version=$tag" \
  --label "ai.sloper.cucina.cucina-version=$cucina_version" \
  --label "ai.sloper.cucina.macos-release=$release" \
  --label "ai.sloper.cucina.xcode-version=$xcode" \
  --label "ai.sloper.cucina.xcode-build=$xcode_build" \
  --label "ai.sloper.cucina.buildbarn=$bb"
printf 'pushed %s\n' "${targets[@]}"
