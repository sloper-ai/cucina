#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Emits the MDM manifest and companions for one published package version (R-MAC-8). The same ManifestURL document
# serves both delivery mechanisms:
#   * the MDM command InstallEnterpriseApplication (inline "Manifest" or "ManifestURL"), any managed Mac;
#   * the declaration com.apple.configuration.package ("ManifestURL"), macOS 26+, supervised (DDM; it takes
#     precedence over the command for the same package).
# Schema: apple/device-management other/manifesturl.yaml — items[].assets[] {kind: software-package, url, sha256},
# items[].metadata {bundle-identifier, bundle-version, kind: software, title, subtitle}.
#
# usage: make-manifest.sh --pkg FILE --version X.Y.Z --url HTTPS_URL [--manifest-url HTTPS_URL] [--out-dir DIR]
#   --url           where the .pkg will be served (unauthenticated HTTPS, one URL per version, immutable)
#   --manifest-url  where the manifest .plist will be served (needed for the DDM declaration / ManifestURL)
# Writes <base>.plist (manifest), <base>.pkg.sha256, <base>.json (values for Apple Business's package form),
# <base>.install-enterprise-application.plist (example MDM command) and, with --manifest-url,
# <base>.ddm-package.json (example declaration). <base> = cucina-host-<MAJOR>-<MINOR>-<PATCH>.
set -eu

. "$(dirname -- "$0")/lib.sh"

pkg='' version='' url='' manifest_url='' out_dir=''
while [ $# -gt 0 ]; do
	case $1 in
	--pkg) pkg=$2 && shift 2 ;;
	--version) version=$2 && shift 2 ;;
	--url) url=$2 && shift 2 ;;
	--manifest-url) manifest_url=$2 && shift 2 ;;
	--out-dir) out_dir=$2 && shift 2 ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' && exit 0 ;;
	*) cucina_die "unknown argument: $1 (try --help)" ;;
	esac
done
[ -f "$pkg" ] || cucina_die "--pkg FILE is required"
cucina_validate_version "$version"
cucina_need plutil pkgutil shasum uuidgen
for u in "$url" ${manifest_url:+"$manifest_url"}; do
	case $u in
	https://*) ;;
	*) cucina_die "URLs must be HTTPS (got '$u')" ;;
	esac
	case $u in *[\"\\\ \<\>]*) cucina_die "URL contains characters that need escaping: $u" ;; esac
done
[ -n "$out_dir" ] || out_dir=$(dirname -- "$pkg")
mkdir -p "$out_dir"

# The package must carry the version we publish it as (Installer and MDM compare it).
tmp=$(mktemp -d "${TMPDIR:-/tmp}/cucina-manifest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM
pkgutil --expand "$pkg" "$tmp/x" >/dev/null
grep -q "<product id=\"$CUCINA_PKG_ID\" version=\"$version\"" "$tmp/x/Distribution" ||
	cucina_die "$pkg is not $CUCINA_PKG_ID version $version"
case $(pkgutil --check-signature "$pkg" 2>&1 | sed -n 's/^ *Status: //p') in
*signed*) ;;
*) cucina_warn "$pkg is unsigned: MDM installs require a signature verifiable by the device (R-MAC-9)" ;;
esac

base=$(cucina_asset_base "$version")
sha=$(cucina_sha256 "$pkg")
size=$(wc -c <"$pkg" | tr -d ' ')
m=$out_dir/$base.plist

# write_manifest_items PLIST KEYPATH — the items array at KEYPATH.
write_manifest_items() {
	plutil -insert "$2" -array "$1"
	plutil -insert "$2.0" -dictionary "$1"
	plutil -insert "$2.0.assets" -array "$1"
	plutil -insert "$2.0.assets.0" -dictionary "$1"
	plutil -insert "$2.0.assets.0.kind" -string software-package "$1"
	plutil -insert "$2.0.assets.0.url" -string "$url" "$1"
	plutil -insert "$2.0.assets.0.sha256" -string "$sha" "$1"
	plutil -insert "$2.0.metadata" -dictionary "$1"
	plutil -insert "$2.0.metadata.bundle-identifier" -string "$CUCINA_PKG_ID" "$1"
	plutil -insert "$2.0.metadata.bundle-version" -string "$version" "$1"
	plutil -insert "$2.0.metadata.kind" -string software "$1"
	plutil -insert "$2.0.metadata.title" -string "$CUCINA_PKG_TITLE" "$1"
	plutil -insert "$2.0.metadata.subtitle" -string "$CUCINA_PKG_VENDOR" "$1"
}

rm -f "$m"
plutil -create xml1 "$m"
write_manifest_items "$m" items
plutil -lint "$m" >/dev/null

printf '%s  %s.pkg\n' "$sha" "$base" >"$out_dir/$base.pkg.sha256"

# Example MDM command (inline manifest: works without hosting the manifest; ManifestURL variant documented).
c=$out_dir/$base.install-enterprise-application.plist
rm -f "$c"
plutil -create xml1 "$c"
plutil -insert Command -dictionary "$c"
plutil -insert Command.RequestType -string InstallEnterpriseApplication "$c"
plutil -insert Command.Manifest -dictionary "$c"
write_manifest_items "$c" Command.Manifest.items
plutil -insert CommandUUID -string "$(uuidgen)" "$c"
plutil -lint "$c" >/dev/null

# Values for Apple Business > Devices > macOS Packages, and for automation.
cat >"$out_dir/$base.json" <<EOF
{
  "name": "Cucina host agent $version",
  "url": "$url",
  "sha256": "$sha",
  "size": $size,
  "bundle_id": "$CUCINA_PKG_ID",
  "version": "$version",
  "title": "$CUCINA_PKG_TITLE",
  "manifest": "$base.plist",
  "manifest_url": "${manifest_url}",
  "login_item_label": "$CUCINA_LABEL"
}
EOF
plutil -convert xml1 -o /dev/null "$out_dir/$base.json"

if [ -n "$manifest_url" ]; then
	cat >"$out_dir/$base.ddm-package.json" <<EOF
{
  "Type": "com.apple.configuration.package",
  "Identifier": "ai.sloper.cucina.ddm.package.cucina-host",
  "ServerToken": "$version-$(printf '%s' "$sha" | cut -c1-12)",
  "Payload": {
    "ManifestURL": "$manifest_url",
    "InstallBehavior": { "Install": "Required" },
    "UninstallBehavior": { "Remove": true }
  }
}
EOF
	plutil -convert xml1 -o /dev/null "$out_dir/$base.ddm-package.json"
fi

cat <<EOF
manifest:      $m
pkg SHA-256:   $sha  ($size bytes)
pkg URL:       $url
bundle ID:     $CUCINA_PKG_ID   version $version
manifest URL:  ${manifest_url:-<not given: pass --manifest-url for the DDM declaration>}
EOF
