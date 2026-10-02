#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Inspects a built Cucina host package without installing it (R-MAC-8/-9 validation; T14 pre-flight):
# signature status, distribution options, payload layout and ownership, no AppleDouble entries, LaunchDaemon plist,
# tart.app untouched (Cirrus Labs signature, CDHash, vm.networking entitlement), Cucina binaries signed with the
# expected certificate + hardened runtime, LC_UUID present (Local Network privacy, TN3179).
#
# usage: check-pkg.sh PKG [--version X.Y.Z] [--signed] [--cert-sha1 HEX] [--uninstaller]
#   --signed          require a product signature and signed Mach-O binaries
#   --cert-sha1 HEX   require that signer certificate (SHA-1 of the DER, as in anchor = H"...")
#   --uninstaller     PKG is the payload-free uninstaller
set -eu

. "$(dirname -- "$0")/lib.sh"
PKG_DIR=$(cucina_pkg_dir)
cucina_load_pins "$PKG_DIR/pins.env"

[ $# -ge 1 ] || cucina_die "usage: check-pkg.sh PKG [--version X.Y.Z] [--signed] [--cert-sha1 HEX] [--uninstaller]"
pkg=$1
shift
want_version='' signed=0 cert_sha1='' uninstaller=0
while [ $# -gt 0 ]; do
	case $1 in
	--version) want_version=$2 && shift 2 ;;
	--signed) signed=1 && shift ;;
	--cert-sha1) cert_sha1=$(printf '%s' "$2" | tr 'a-f' 'A-F' | tr -d ': ') && shift 2 ;;
	--uninstaller) uninstaller=1 && shift ;;
	*) cucina_die "unknown argument: $1" ;;
	esac
done
[ -f "$pkg" ] || cucina_die "no such package: $pkg"

fails=0
ok() { printf 'ok    %s\n' "$*"; }
bad() {
	printf 'FAIL  %s\n' "$*"
	fails=$((fails + 1))
}
check() { # DESCRIPTION COMMAND...
	ck_d=$1
	shift
	if "$@" >/dev/null 2>&1; then ok "$ck_d"; else bad "$ck_d"; fi
}
expect_eq() { # DESCRIPTION GOT WANT
	if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi
}
has_line() { # TEXT LINE
	printf '%s\n' "$1" | grep -qxF "$2"
}

work=$(mktemp -d "${TMPDIR:-/tmp}/cucina-check.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM

# --- signature ------------------------------------------------------------------------------------------------
sig=$(pkgutil --check-signature "$pkg" 2>&1 || true)
status=$(printf '%s\n' "$sig" | sed -n 's/^ *Status: //p' | head -1)
printf 'info  signature status: %s\n' "${status:-unknown}"
if [ "$signed" = 1 ]; then
	case $status in *signed*) ok "product archive is signed" ;; *) bad "product archive is not signed" ;; esac
	if [ -n "$cert_sha1" ]; then
		# The product signature's leaf certificate is the first X509Certificate in the xar table of contents.
		leaf_sha1=$(xar --dump-toc=- -f "$pkg" 2>/dev/null |
			awk '/<X509Certificate>/ { on = 1 } on { print } /<\/X509Certificate>/ { exit }' |
			sed -e 's/<[^>]*>//g' | tr -d ' \n\r' | base64 -D 2>/dev/null | shasum -a 1 | cut -d ' ' -f 1 | tr 'a-f' 'A-F')
		expect_eq "product signer certificate" "$leaf_sha1" "$cert_sha1"
	fi
fi

# --- structure ------------------------------------------------------------------------------------------------
pkgutil --expand-full "$pkg" "$work/x" >/dev/null
dist=$work/x/Distribution
check "Distribution present" test -f "$dist"
if [ "$uninstaller" = 1 ]; then
	comp=$work/x/cucina-host-uninstall-component.pkg
	check "uninstaller postinstall present" test -x "$comp/Scripts/postinstall"
	check "uninstaller embeds cucina-host-uninstall" test -x "$comp/Scripts/cucina-host-uninstall"
	check "uninstaller has no payload" test ! -d "$comp/Payload"
	[ "$fails" = 0 ] || cucina_die "$fails check(s) failed"
	printf 'all checks passed: %s\n' "$pkg"
	exit 0
fi
comp=$work/x/cucina-host-component.pkg
P=$comp/Payload
check "distribution: arm64 only" grep -q 'hostArchitectures="arm64"' "$dist"
check "distribution: startup volume only" grep -q 'rootVolumeOnly="true"' "$dist"
check "distribution: macOS >= $CUCINA_MIN_MACOS" grep -q "<os-version min=\"$CUCINA_MIN_MACOS\"/>" "$dist"
check "distribution: product $CUCINA_PKG_ID" grep -q "<product id=\"$CUCINA_PKG_ID\"" "$dist"
if [ -n "$want_version" ]; then
	check "distribution: product version $want_version" grep -q "<product id=\"$CUCINA_PKG_ID\" version=\"$want_version\"" "$dist"
	expect_eq "payload VERSION" "$(cat "$P/usr/local/cucina/VERSION")" "$want_version"
fi
check "component identifier $CUCINA_PKG_ID" grep -q "identifier=\"$CUCINA_PKG_ID\"" "$comp/PackageInfo"
check "component not relocatable" grep -q 'relocatable="false"' "$comp/PackageInfo"
check "preinstall executable" test -x "$comp/Scripts/preinstall"
check "postinstall executable" test -x "$comp/Scripts/postinstall"
if find "$P" -name '._*' | grep -q .; then bad "payload contains AppleDouble ._ entries"; else ok "no AppleDouble entries"; fi

for f in "/Library/LaunchDaemons/$CUCINA_LABEL.plist" /private/etc/newsyslog.d/ai.sloper.cucina.conf \
	/usr/local/cucina/bin/cucina-hostd /usr/local/cucina/bin/bb_storage /usr/local/cucina/bin/tart \
	/usr/local/cucina/bin/cucina-host-setup /usr/local/cucina/bin/cucina-host-uninstall \
	/usr/local/cucina/share/doc/LICENSE.md /usr/local/cucina/share/doc/THIRD_PARTY_NOTICES.md \
	/usr/local/cucina/share/doc/tart/LICENSE /usr/local/cucina/tart.app/Contents/MacOS/tart; do
	check "payload has $f" test -e "$P$f"
done
# Ownership/modes as installed come from the BOM.
bom=$(lsbom -p MUGf "$comp/Bom")
plist_entry=$(printf '%s\n' "$bom" | awk -F '\t' -v p="./Library/LaunchDaemons/$CUCINA_LABEL.plist" '$4 == p { sub(/ +$/, "", $1); print $1 ":" $2 ":" $3 }')
expect_eq "LaunchDaemon plist mode and owner" "$plist_entry" "-rw-r--r--:root:wheel"
if printf '%s\n' "$bom" | awk -F '\t' '$2 != "root" || $3 != "wheel" { bad = 1 } END { exit bad }'; then
	ok "every payload entry is root:wheel"
else
	bad "payload entries not owned by root:wheel"
fi

plist=$P/Library/LaunchDaemons/$CUCINA_LABEL.plist
check "LaunchDaemon plist lints" plutil -lint "$plist"
expect_eq "Label" "$(plutil -extract Label raw -o - "$plist" 2>/dev/null)" "$CUCINA_LABEL"
expect_eq "ProgramArguments[0]" "$(plutil -extract ProgramArguments.0 raw -o - "$plist" 2>/dev/null)" /usr/local/cucina/bin/cucina-hostd
expect_eq "ProgramArguments[1]" "$(plutil -extract ProgramArguments.1 raw -o - "$plist" 2>/dev/null)" run
expect_eq "KeepAlive" "$(plutil -extract KeepAlive raw -o - "$plist" 2>/dev/null)" true
expect_eq "RunAtLoad" "$(plutil -extract RunAtLoad raw -o - "$plist" 2>/dev/null)" true
expect_eq "runs as root (no UserName)" "$(plutil -extract UserName raw -o - "$plist" 2>/dev/null || echo root)" root

# --- tart.app exactly as released ---------------------------------------------------------------------------
app=$P/usr/local/cucina/tart.app
check "tart.app signature intact (deep, strict)" codesign --verify --deep --strict "$app"
tinfo=$(codesign -dvvv "$app" 2>&1 || true)
if has_line "$tinfo" "TeamIdentifier=$TART_TEAM_ID"; then ok "tart.app TeamIdentifier $TART_TEAM_ID"; else bad "tart.app TeamIdentifier"; fi
if has_line "$tinfo" "CDHash=$TART_CDHASH"; then ok "tart.app CDHash $TART_CDHASH (as released)"; else bad "tart.app CDHash changed"; fi
if codesign -d --entitlements - --xml "$app" 2>/dev/null | grep -q com.apple.vm.networking; then
	ok "tart.app keeps com.apple.vm.networking"
else
	bad "tart.app lost com.apple.vm.networking"
fi
expect_eq "tart.app version" "$(plutil -extract CFBundleShortVersionString raw -o - "$app/Contents/Info.plist")" "$TART_VERSION"

# --- Cucina's binaries ----------------------------------------------------------------------------------------
for bin in cucina-hostd bb_storage; do
	f=$P/usr/local/cucina/bin/$bin
	case $(lipo -archs "$f" 2>/dev/null) in *arm64*) ok "$bin is arm64" ;; *) bad "$bin is not arm64" ;; esac
	if otool -l "$f" | grep -q LC_UUID; then ok "$bin has LC_UUID"; else bad "$bin lacks LC_UUID (Local Network privacy)"; fi
	[ "$signed" = 1 ] || continue
	binfo=$(codesign -dvvv "$f" 2>&1 || true)
	check "$bin signature verifies" codesign --verify --strict "$f"
	if has_line "$binfo" "Signature=adhoc"; then bad "$bin is only ad-hoc signed"; fi
	if printf '%s\n' "$binfo" | grep -q '^CodeDirectory .*flags=.*runtime'; then ok "$bin hardened runtime"; else bad "$bin lacks hardened runtime"; fi
	case $bin in cucina-hostd) ident=ai.sloper.cucina.hostd ;; *) ident=ai.sloper.cucina.bb_storage ;; esac
	if has_line "$binfo" "Identifier=$ident"; then ok "$bin identifier $ident"; else bad "$bin identifier (want $ident)"; fi
	if [ -n "$cert_sha1" ]; then
		(cd "$work" && codesign -d --extract-certificates="$bin.cert" "$f" >/dev/null 2>&1) || true
		got=$(shasum -a 1 "$work/$bin.cert0" 2>/dev/null | cut -d ' ' -f 1 | tr 'a-f' 'A-F')
		expect_eq "$bin signer certificate" "$got" "$cert_sha1"
	fi
done
if [ "$(cucina_sha256 "$P/usr/local/cucina/bin/bb_storage")" = "$BB_STORAGE_SHA256" ]; then
	printf 'info  bb_storage bytes are the pinned upstream release\n'
else
	printf 'info  bb_storage re-signed (pinned upstream SHA-256 is verified before signing)\n'
fi

[ "$fails" = 0 ] || cucina_die "$fails check(s) failed"
printf 'all checks passed: %s\n' "$pkg"
