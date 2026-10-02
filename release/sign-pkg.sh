#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# CI signing of the macOS host package (R-MAC-9 "CI secret store in production", ADR 0151):
# signs the exact Bazel-built inputs of the release (release/build.sh macos → pkg-inputs/) with
# an identity already imported by macos/pkg/scripts/ci-keychain.sh, checks the result and writes
# the MDM manifests for the GitHub Release URL plus the signer's public certificate.
#
# usage: release/sign-pkg.sh --inputs DIR --out DIR --identity SHA1 --keychain PATH
set -euo pipefail
# shellcheck source=release/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

inputs='' out='' identity='' keychain=''
while [[ $# -gt 0 ]]; do
	case $1 in
	--inputs) inputs=$2 && shift 2 ;;
	--out) out=$2 && shift 2 ;;
	--identity) identity=$2 && shift 2 ;;
	--keychain) keychain=$2 && shift 2 ;;
	*) die "unknown argument $1" ;;
	esac
done
[[ -d $inputs && -n $out && -n $identity && -n $keychain ]] ||
	die "usage: sign-pkg.sh --inputs DIR --out DIR --identity SHA1 --keychain PATH"
[[ $(host_os) == macos ]] || die "signing needs macOS (codesign, productbuild)"
# VERSION, CORE, RELEASE_URL, ... of the build that produced the inputs.
# shellcheck disable=SC1091
. "$inputs/buildinfo.env"
pkgs="$release_root/macos/pkg/scripts"
base="cucina-host-${CORE//./-}"
mkdir -p "$out"

"$pkgs/sign.sh" --identity "$identity" --keychain "$keychain" -- \
	--hostd "$inputs/cucina-hostd" --bb-storage "$inputs/bb_storage" --tart-tarball "$inputs/tart.tar.gz" \
	--license "$inputs/LICENSE.md" --notices "$inputs/THIRD_PARTY_NOTICES.md" \
	--version "$CORE" --out "$out/$base.pkg"
"$pkgs/check-pkg.sh" "$out/$base.pkg" --version "$CORE" --signed --cert-sha1 "$identity"
"$pkgs/make-manifest.sh" --pkg "$out/$base.pkg" --version "$CORE" --url "$RELEASE_URL/$base.pkg" \
	--manifest-url "$RELEASE_URL/$base.plist" --out-dir "$out" >/dev/null

# The signer's public certificate, for the MDM trust profile (as `make pkg-publish` attaches it).
cert="$(mktemp)"
trap 'rm -f "$cert"' EXIT
security find-certificate -a -Z -p "$keychain" | awk -v want="$identity" '
	/^SHA-1 hash:/ { take = (toupper($3) == toupper(want)) }
	take && /-----BEGIN CERTIFICATE-----/ { on = 1 }
	take && on { print }
	take && /-----END CERTIFICATE-----/ { exit }' >"$cert"
[[ -s $cert ]] || die "certificate $identity not found in $keychain"
short="$(openssl x509 -in "$cert" -outform DER | shasum -a 1 | cut -c1-8)"
cp "$cert" "$out/cucina-host-signer-$short.pem"
info "signed $base.pkg ($VERSION) with $identity"
