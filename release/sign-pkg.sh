#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
# CI/local signing wrapper: consumes only Bazel-built inputs, then writes and verifies the
# package, MDM manifests and public certificates (R-MAC-9; private identities have two roles).
# Never run this during a build action. --trust-installer is for ephemeral hosted CI ONLY;
# workstation signing uses already-trusted identities and never changes system trust here.
#
# usage: release/sign-pkg.sh --inputs DIR --out DIR --identity APP_SHA1 \
#          --installer-identity INSTALLER_SHA1 --keychain PATH [--trust-installer]
set -euo pipefail
# shellcheck source=release/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

inputs='' out='' identity='' installer='' keychain='' trust=0
while [[ $# -gt 0 ]]; do
	case $1 in
	--inputs) inputs=$2 && shift 2 ;;
	--out) out=$2 && shift 2 ;;
	--identity) identity=$2 && shift 2 ;;
	--installer-identity) installer=$2 && shift 2 ;;
	--keychain) keychain=$2 && shift 2 ;;
	--trust-installer) trust=1 && shift ;;
	*) die "unknown argument $1" ;;
	esac
done
[[ -d $inputs && -n $out && -n $identity && -n $installer && -n $keychain ]] ||
	die "inputs, output, application and installer identities, and keychain are required"
[[ $(host_os) == macos ]] || die "signing needs macOS (codesign, productbuild)"
if [[ $trust == 1 ]]; then
	[[ ${GITHUB_ACTIONS:-} == true && ${CUCINA_SIGNING_RUNNER:-} == github-hosted ]] ||
		die "--trust-installer is restricted to an ephemeral GitHub-hosted runner"
fi
# VERSION, CORE, RELEASE_URL, ... of the build that produced the inputs.
# shellcheck disable=SC1091
. "$inputs/buildinfo.env"
pkgs="$release_root/macos/pkg/scripts"
base="cucina-host-${CORE//./-}"
mkdir -p "$out"

# Both PUBLIC certificates are required for the MDM trust profile. Consume the whole security
# output (no early-exit pipeline, which can SIGPIPE with two imported identities).
export_certificate() {
	local role=$1 fingerprint=$2
	local cert="$out/cucina-host-signer-$role.pem"
	/usr/bin/security find-certificate -a -Z -p "$keychain" | awk -v want="$fingerprint" '
		/^SHA-1 hash:/ { take = (toupper($3) == toupper(want)) }
		take && /-----BEGIN CERTIFICATE-----/ { on = 1 }
		take && on { print }
		take && /-----END CERTIFICATE-----/ { on = 0; take = 0 }' >"$cert"
	[[ -s $cert ]] || die "$role certificate $fingerprint not found in $keychain"
	local actual
	actual="$(openssl x509 -in "$cert" -noout -fingerprint -sha1 | cut -d= -f2 | tr -d ':')"
	[[ $actual == "$fingerprint" ]] || die "$role certificate fingerprint mismatch"
}
export_certificate application "$identity"
export_certificate installer "$installer"
installer_cert="$out/cucina-host-signer-installer.pem"
trusted=0
cleanup() {
	local result=$?
	trap - EXIT INT TERM
	if [[ $trusted == 1 ]] && ! sudo -n /usr/bin/security remove-trusted-cert -d "$installer_cert"; then
		result=1
	fi
	exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ $trust == 1 ]]; then
	sudo -n /usr/bin/security add-trusted-cert -d -r trustRoot -k "$keychain" "$installer_cert"
	trusted=1
fi

"$pkgs/sign.sh" --identity "$identity" --installer-identity "$installer" --keychain "$keychain" -- \
	--hostd "$inputs/cucina-hostd" --bb-storage "$inputs/bb_storage" --tart-tarball "$inputs/tart.tar.gz" \
	--license "$inputs/LICENSE.md" --notices "$inputs/THIRD_PARTY_NOTICES.md" \
	--version "$CORE" --out "$out/$base.pkg"
"$pkgs/check-pkg.sh" "$out/$base.pkg" --version "$CORE" --signed --cert-sha1 "$installer" --app-cert-sha1 "$identity"
"$pkgs/make-manifest.sh" --pkg "$out/$base.pkg" --version "$CORE" --url "$RELEASE_URL/$base.pkg" \
	--manifest-url "$RELEASE_URL/$base.plist" --out-dir "$out" >/dev/null
plutil -convert json -o "$out/$base.manifest.json" "$out/$base.plist"
info "signed $base.pkg ($VERSION); application=$identity installer=$installer"
