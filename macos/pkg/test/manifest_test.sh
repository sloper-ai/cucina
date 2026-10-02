#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Guards R-MAC-8 (tier: integration, macOS): make-manifest.sh emits the ManifestURL document consumed by BOTH the
# InstallEnterpriseApplication command and the com.apple.configuration.package declaration
# (apple/device-management other/manifesturl.yaml: items[].assets[] kind software-package + https url + sha256,
# items[].metadata bundle-identifier/bundle-version/kind software/title), with the package's real SHA-256, and it
# refuses non-HTTPS URLs and a version that does not match the package. Fixture: a payload-free product archive
# built with the real distribution template. No network.
set -eu
PATH=$PATH:/usr/bin:/bin:/usr/sbin:/sbin

# Bazel runs the test as <runfiles>/_main/macos/pkg/manifest_test; locate the package through TEST_SRCDIR there.
if [ -n "${TEST_SRCDIR:-}" ]; then PKG_DIR=$TEST_SRCDIR/${TEST_WORKSPACE:-_main}/macos/pkg; else
	PKG_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
fi
T=$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/manifest-test.XXXXXX")
trap 'rm -rf "$T"' EXIT INT TERM
fails=0
ok() { printf 'ok    %s\n' "$*"; }
bad() {
	printf 'FAIL  %s\n' "$*"
	fails=$((fails + 1))
}
expect_eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi; }
get() { plutil -extract "$2" raw -o - "$1" 2>/dev/null || true; }

# Fixture product archive: ai.sloper.cucina.host 1.2.3 through the real distribution template.
mkdir -p "$T/components"
pkgbuild --quiet --nopayload --identifier ai.sloper.cucina.host --version 1.2.3 "$T/components/cucina-host-component.pkg"
sed -e 's|@PKG_ID@|ai.sloper.cucina.host|g' -e 's|@TITLE@|Cucina host agent|g' -e 's|@VERSION@|1.2.3|g' \
	-e 's|@MIN_MACOS@|26.0|g' -e 's|@COMPONENT@|cucina-host-component.pkg|g' \
	"$PKG_DIR/resources/distribution.xml.in" >"$T/distribution.xml"
productbuild --quiet --distribution "$T/distribution.xml" --package-path "$T/components" "$T/fixture.pkg"
sha=$(shasum -a 256 "$T/fixture.pkg" | cut -d ' ' -f 1)

url=https://downloads.example.com/cucina/1.2.3/cucina-host-1-2-3.pkg
murl=https://downloads.example.com/cucina/1.2.3/cucina-host-1-2-3.plist
"$PKG_DIR/scripts/make-manifest.sh" --pkg "$T/fixture.pkg" --version 1.2.3 --url "$url" --manifest-url "$murl" \
	--out-dir "$T/out" >/dev/null 2>&1 || bad "make-manifest.sh failed"
m=$T/out/cucina-host-1-2-3.plist
check_items() { # PLIST PREFIX
	expect_eq "$3 asset kind" "$(get "$1" "$2.0.assets.0.kind")" software-package
	expect_eq "$3 asset url" "$(get "$1" "$2.0.assets.0.url")" "$url"
	expect_eq "$3 asset sha256" "$(get "$1" "$2.0.assets.0.sha256")" "$sha"
	expect_eq "$3 bundle-identifier" "$(get "$1" "$2.0.metadata.bundle-identifier")" ai.sloper.cucina.host
	expect_eq "$3 bundle-version" "$(get "$1" "$2.0.metadata.bundle-version")" 1.2.3
	expect_eq "$3 metadata kind" "$(get "$1" "$2.0.metadata.kind")" software
	expect_eq "$3 title" "$(get "$1" "$2.0.metadata.title")" "Cucina host agent"
}
check_items "$m" items manifest
c=$T/out/cucina-host-1-2-3.install-enterprise-application.plist
expect_eq "command RequestType" "$(get "$c" Command.RequestType)" InstallEnterpriseApplication
check_items "$c" Command.Manifest.items command
d=$T/out/cucina-host-1-2-3.ddm-package.json
expect_eq "declaration type" "$(get "$d" Type)" com.apple.configuration.package
expect_eq "declaration ManifestURL" "$(get "$d" Payload.ManifestURL)" "$murl"
expect_eq "declaration installs as required" "$(get "$d" Payload.InstallBehavior.Install)" Required
expect_eq "declaration removes files on removal (macOS 27)" "$(get "$d" Payload.UninstallBehavior.Remove)" true
expect_eq "sha256 file" "$(cat "$T/out/cucina-host-1-2-3.pkg.sha256")" "$sha  cucina-host-1-2-3.pkg"
expect_eq "Apple Business values: bundle_id" "$(get "$T/out/cucina-host-1-2-3.json" bundle_id)" ai.sloper.cucina.host

# Fail fast (table).
while IFS='|' read -r name version u; do
	[ -n "$name" ] || continue
	if "$PKG_DIR/scripts/make-manifest.sh" --pkg "$T/fixture.pkg" --version "$version" --url "$u" --out-dir "$T/fail" >/dev/null 2>&1; then
		bad "accepted: $name"
	else
		ok "rejected: $name"
	fi
done <<EOF
http-url|1.2.3|http://downloads.example.com/x.pkg
version-mismatch|1.2.4|$url
prerelease-version|1.2.3-rc.1|$url
EOF

[ "$fails" = 0 ] || {
	echo "manifest_test: $fails failure(s)" >&2
	exit 1
}
echo "manifest_test: ok"
