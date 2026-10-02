#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Verifies a published package end to end the way a Mac's MDM agent consumes it (R-MAC-8, T14): fetch the
# manifest over HTTPS, check its structure, download the package from the manifest's URL (following redirects,
# e.g. GitHub release assets -> release-assets.githubusercontent.com), and compare the SHA-256.
# Prints hosts and paths only, never query strings (pre-signed URLs carry temporary credentials).
#
# usage: verify-manifest.sh MANIFEST_URL|MANIFEST_FILE [--expect-version X.Y.Z] [--expect-sha256 HEX] [--keep FILE]
set -eu

. "$(dirname -- "$0")/lib.sh"
[ $# -ge 1 ] || cucina_die "usage: verify-manifest.sh MANIFEST_URL|FILE [--expect-version V] [--expect-sha256 H] [--keep FILE]"
src=$1
shift
want_version='' want_sha='' keep=''
while [ $# -gt 0 ]; do
	case $1 in
	--expect-version) want_version=$2 && shift 2 ;;
	--expect-sha256) want_sha=$2 && shift 2 ;;
	--keep) keep=$2 && shift 2 ;;
	*) cucina_die "unknown argument: $1" ;;
	esac
done
cucina_need curl plutil shasum

redact() { printf '%s' "$1" | sed 's/?.*/?<redacted>/'; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/cucina-verify.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM

case $src in
https://*)
	curl --fail --silent --show-error --location --max-redirs 5 --proto '=https' --proto-redir '=https' \
		-o "$tmp/manifest.plist" "$src"
	printf 'manifest: fetched %s\n' "$(redact "$src")"
	;;
http://*) cucina_die "manifest URL must be HTTPS" ;;
*) cp "$src" "$tmp/manifest.plist" ;;
esac
m=$tmp/manifest.plist
plutil -lint "$m" >/dev/null || cucina_die "manifest is not a valid property list"

get() { plutil -extract "$1" raw -o - "$m" 2>/dev/null || true; }
kind=$(get items.0.assets.0.kind)
url=$(get items.0.assets.0.url)
sha=$(get items.0.assets.0.sha256)
bid=$(get items.0.metadata.bundle-identifier)
bver=$(get items.0.metadata.bundle-version)
mkind=$(get items.0.metadata.kind)
title=$(get items.0.metadata.title)
[ "$kind" = software-package ] || cucina_die "items[0].assets[0].kind is '$kind', want software-package"
case $url in https://*) ;; *) cucina_die "items[0].assets[0].url must be HTTPS" ;; esac
printf '%s' "$sha" | grep -Eq '^[0-9a-f]{64}$' || cucina_die "items[0].assets[0].sha256 is not a SHA-256"
[ "$bid" = "$CUCINA_PKG_ID" ] || cucina_die "bundle-identifier is '$bid', want $CUCINA_PKG_ID"
[ "$mkind" = software ] || cucina_die "metadata.kind is '$mkind', want software"
[ -n "$title" ] || cucina_die "metadata.title missing"
[ -z "$want_version" ] || [ "$bver" = "$want_version" ] || cucina_die "bundle-version is '$bver', want $want_version"
[ -z "$want_sha" ] || [ "$sha" = "$want_sha" ] || cucina_die "manifest sha256 $sha != expected $want_sha"
printf 'manifest: ok (bundle %s %s, "%s")\n' "$bid" "$bver" "$title"

stats=$(curl --fail --silent --show-error --location --max-redirs 5 --proto '=https' --proto-redir '=https' \
	-o "$tmp/pkg" -w '%{http_code} %{num_redirects} %{size_download} %{content_type}' "$url")
final_host=$(curl --silent --location --max-redirs 5 --proto '=https' --range 0-0 -o /dev/null -w '%{url_effective}' "$url" |
	sed -E 's#^https://([^/?]+).*#\1#')
got=$(cucina_sha256 "$tmp/pkg")
[ "$got" = "$sha" ] || cucina_die "downloaded package SHA-256 $got != manifest $sha"
read -r code redirects size ctype <<EOF
$stats
EOF
printf 'package:  ok (HTTP %s, %s redirect(s), served by %s, %s bytes, %s) %s\n' \
	"$code" "$redirects" "$final_host" "$size" "$ctype" "$(redact "$url")"
case $(pkgutil --check-signature "$tmp/pkg" 2>&1 | sed -n 's/^ *Status: //p') in
*signed*) printf 'package:  signed (%s)\n' "$(pkgutil --check-signature "$tmp/pkg" | sed -n 's/^ *1\. //p' | head -1)" ;;
*) cucina_warn "the published package is unsigned" ;;
esac
[ -z "$keep" ] || cp "$tmp/pkg" "$keep"
printf 'verified: SHA-256 %s\n' "$sha"
