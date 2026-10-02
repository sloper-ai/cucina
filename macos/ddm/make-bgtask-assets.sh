#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Builds the assets of the DDM background-tasks variant (R-MAC-10 SHOULD, macOS 15+, supervised) from a SIGNED host
# package: the executable zip (bin/, tart.app as released, share/, VERSION — exactly the package payload under
# /usr/local/cucina), the two launchd plists, and the com.apple.asset.data declarations with sizes and SHA-256
# hashes (required by services.background-tasks). Upload the files to --base-url (unauthenticated HTTPS, one URL per
# version) and give the declarations to an MDM that accepts custom declarations.
#
# usage: make-bgtask-assets.sh --pkg SIGNED.pkg --version X.Y.Z --base-url https://… [--out-dir DIR]
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
die() {
	echo "make-bgtask-assets: error: $*" >&2
	exit 1
}
pkg='' version='' base_url='' out=''
while [ $# -gt 0 ]; do
	case $1 in
	--pkg) pkg=$2 && shift 2 ;;
	--version) version=$2 && shift 2 ;;
	--base-url) base_url=${2%/} && shift 2 ;;
	--out-dir) out=$2 && shift 2 ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' && exit 0 ;;
	*) die "unknown argument: $1" ;;
	esac
done
[ -f "$pkg" ] || die "--pkg SIGNED.pkg is required"
case $version in '' | *[!0-9.]*) die "--version X.Y.Z is required" ;; esac
case $base_url in https://*) ;; *) die "--base-url must be https://" ;; esac
case $(pkgutil --check-signature "$pkg" 2>&1 | sed -n 's/^ *Status: //p') in *signed*) ;; *) die "the package must be signed" ;; esac
[ -n "$out" ] || out=$(dirname -- "$pkg")
mkdir -p "$out"
dashed=$(printf '%s' "$version" | tr '.' '-')

tmp=$(mktemp -d "${TMPDIR:-/tmp}/cucina-bgtask.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM
pkgutil --expand-full "$pkg" "$tmp/x" >/dev/null
src=$tmp/x/cucina-host-component.pkg/Payload/usr/local/cucina
[ -x "$src/bin/cucina-hostd" ] || die "unexpected package layout"
codesign --verify --deep --strict "$src/tart.app" || die "tart.app signature broken in the package"

zip=cucina-hostd-bgtask-$dashed.zip
# ditto keeps modes and the app bundle as is; --norsrc/--noextattr keep provenance metadata out of the archive.
(cd "$src" && ditto -c -k --norsrc --noextattr --noacl . "$out/$zip")
cp "$HERE/background-tasks/ai.sloper.cucina.hostd.plist" "$out/ai.sloper.cucina.hostd.plist"
cp "$HERE/background-tasks/ai.sloper.cucina.host-setup.plist" "$out/ai.sloper.cucina.host-setup.plist"

asset() { # ID FILE CONTENT_TYPE
	a_size=$(wc -c <"$out/$2" | tr -d ' ')
	a_sha=$(shasum -a 256 "$out/$2" | cut -d ' ' -f 1)
	cat >"$out/asset.$1.json" <<EOF
{
  "Type": "com.apple.asset.data",
  "Identifier": "ai.sloper.cucina.ddm.asset.$1",
  "ServerToken": "$version-$(printf '%s' "$a_sha" | cut -c1-12)",
  "Payload": {
    "Reference": {
      "DataURL": "$base_url/$2",
      "ContentType": "$3",
      "Size": $a_size,
      "Hash-SHA-256": "$a_sha"
    },
    "Authentication": { "Type": "None" }
  }
}
EOF
	plutil -convert xml1 -o /dev/null "$out/asset.$1.json"
	printf '%-28s %s  %s bytes\n' "$1" "$a_sha" "$a_size"
}
asset hostd-files "$zip" application/zip
asset hostd-launchd ai.sloper.cucina.hostd.plist application/xml
asset host-setup-launchd ai.sloper.cucina.host-setup.plist application/xml
sed "s/\"ServerToken\": \"[^\"]*\"/\"ServerToken\": \"cucina-bgtask-$version\"/" \
	"$HERE/services.background-tasks.cucina-hostd.json" >"$out/services.background-tasks.cucina-hostd.json"
echo "assets in $out (upload $zip and the two plists to $base_url/)"
