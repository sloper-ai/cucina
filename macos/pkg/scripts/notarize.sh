#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Developer ID path only (R-MAC-9, OFF by default; called by sign.sh --developer-id): submits a Developer ID
# Installer-signed package to Apple's notary service with an App Store Connect API key, staples the ticket and
# verifies the result. Needed for Microsoft Intune line-of-business apps, recommended by Fleet, and for manual
# installs outside MDM. Not needed for MDM installs with the private certificate (MDM-installed files are not
# quarantined, so Gatekeeper never assesses them).
#
# usage: notarize.sh PKG --key AuthKey_XXXX.p8 --key-id KEYID --issuer ISSUER-UUID [--timeout 1h]
set -eu

. "$(dirname -- "$0")/lib.sh"
[ $# -ge 1 ] || cucina_die "usage: notarize.sh PKG --key PATH --key-id ID --issuer UUID"
pkg=$1
shift
key='' key_id='' issuer='' timeout=1h
while [ $# -gt 0 ]; do
	case $1 in
	--key) key=$2 && shift 2 ;;
	--key-id) key_id=$2 && shift 2 ;;
	--issuer) issuer=$2 && shift 2 ;;
	--timeout) timeout=$2 && shift 2 ;;
	*) cucina_die "unknown argument: $1" ;;
	esac
done
[ -f "$pkg" ] || cucina_die "no such package: $pkg"
[ -f "$key" ] || cucina_die "API key file not found: $key (keep it outside the repository)"
if [ -z "$key_id" ] || [ -z "$issuer" ]; then cucina_die "--key-id and --issuer are required"; fi
cucina_need xcrun pkgutil spctl

pkgutil --check-signature "$pkg" | grep -q 'Developer ID Installer' ||
	cucina_die "$pkg is not signed with a Developer ID Installer identity"

cucina_info "submitting $pkg to the notary service (waits up to $timeout)"
xcrun notarytool submit "$pkg" --key "$key" --key-id "$key_id" --issuer "$issuer" --wait --timeout "$timeout"
xcrun stapler staple "$pkg"
xcrun stapler validate "$pkg"
pkgutil --check-signature "$pkg"
# Gatekeeper's install assessment must accept it as notarized Developer ID.
spctl --assess --verbose=2 --type install "$pkg"
