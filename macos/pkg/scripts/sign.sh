#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Local-only signing step (needs keychain access, so it never runs in a Bazel action; `bazel run` or make only).
# Builds the signed host package from the same inputs as build-pkg.sh, then verifies it (R-MAC-9).
#
# Default: separate PRIVATE application and installer certificates (scripts/make-signing-cert.sh, ADR 0752).
# codesign --timestamp --options runtime signs hostd/bb_storage; productbuild signs the archive. Never re-sign Tart.
# Optional, OFF by default: --developer-id (Developer ID Application for binaries, Developer ID Installer for the
# package, then notarization + stapling via scripts/notarize.sh). It is used only when explicitly requested.
#
# usage: sign.sh [signing options] -- <build-pkg.sh input options (--hostd, --version, --out, ...)>
#   --identity ID            private signing identity (name or SHA-1); default $CUCINA_SIGN_IDENTITY
#   --keychain PATH          keychain holding the identity (default: search list)
#   --no-timestamp           skip secure timestamps (offline test builds only)
#   --developer-id           use the Developer ID path instead (requires the options below or their env vars)
#   --app-identity ID        Developer ID Application identity      ($CUCINA_DEVID_APP)
#   --installer-identity ID  private installer ($CUCINA_INSTALLER_IDENTITY), or Developer ID Installer when opted in
#   --notary-key PATH        App Store Connect API key (.p8)         ($CUCINA_NOTARY_KEY)
#   --notary-key-id ID       API key ID                              ($CUCINA_NOTARY_KEY_ID)
#   --notary-issuer UUID     API issuer ID                           ($CUCINA_NOTARY_ISSUER)
set -eu

. "$(dirname -- "$0")/lib.sh"
SCRIPTS=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)

identity=${CUCINA_SIGN_IDENTITY:-}
keychain=''
timestamp=1
devid=0
case ${CUCINA_SIGNING:-private} in
private) ;;
developer-id) devid=1 ;;
*) cucina_die "CUCINA_SIGNING must be 'private' or 'developer-id'" ;;
esac
app_id=${CUCINA_DEVID_APP:-}
inst_id=${CUCINA_INSTALLER_IDENTITY:-${CUCINA_DEVID_INSTALLER:-}}
notary_key=${CUCINA_NOTARY_KEY:-}
notary_key_id=${CUCINA_NOTARY_KEY_ID:-}
notary_issuer=${CUCINA_NOTARY_ISSUER:-}
while [ $# -gt 0 ]; do
	case $1 in
	--identity) identity=$2 && shift 2 ;;
	--keychain) keychain=$2 && shift 2 ;;
	--no-timestamp) timestamp=0 && shift ;;
	--developer-id) devid=1 && shift ;;
	--app-identity) app_id=$2 && shift 2 ;;
	--installer-identity) inst_id=$2 && shift 2 ;;
	--notary-key) notary_key=$2 && shift 2 ;;
	--notary-key-id) notary_key_id=$2 && shift 2 ;;
	--notary-issuer) notary_issuer=$2 && shift 2 ;;
	--) shift && break ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' && exit 0 ;;
	*) cucina_die "unknown argument: $1 (build-pkg.sh options go after --)" ;;
	esac
done
cucina_need security codesign productbuild

out=''
prev=''
for a in "$@"; do
	[ "$prev" != --out ] || out=$a
	prev=$a
done
[ -n "$out" ] || cucina_die "pass --out FILE.pkg after --"

# cert_sha1 NAME_OR_SHA1 — SHA-1 of the identity's certificate (unambiguous codesign identity).
cert_sha1() {
	case $1 in
	[0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f][0-9A-Fa-f]*)
		if [ "${#1}" -eq 40 ]; then
			printf '%s' "$1" | tr 'a-f' 'A-F'
			return 0
		fi
		;;
	esac
	if [ -n "$keychain" ]; then
		cs_out=$(security find-certificate -a -c "$1" -Z "$keychain" 2>/dev/null || true)
	else
		cs_out=$(security find-certificate -a -c "$1" -Z 2>/dev/null || true)
	fi
	cs_n=$(printf '%s\n' "$cs_out" | grep -c '^SHA-1 hash:' || true)
	[ "$cs_n" = 1 ] || cucina_die "identity '$1' matches $cs_n certificates; pass its SHA-1 or a unique name"
	printf '%s\n' "$cs_out" | awk '/^SHA-1 hash:/ { print $3 }'
}

# cert_cn NAME_OR_SHA1 — the certificate's common name (productbuild looks identities up by name).
cert_cn() {
	if [ "${#1}" -ne 40 ] || printf '%s' "$1" | grep -q '[^0-9A-Fa-f]'; then
		printf '%s' "$1"
		return 0
	fi
	cc_want=$(printf '%s' "$1" | tr 'a-f' 'A-F')
	# shellcheck disable=SC2086 # optional keychain argument
	cc_pem=$(security find-certificate -a -Z -p ${keychain:+"$keychain"} 2>/dev/null | awk -v w="$cc_want" '
		/^SHA-1 hash:/ { cur = $3; next }
		/BEGIN CERTIFICATE/ { inb = (cur == w) }
		inb { print }
		/END CERTIFICATE/ { inb = 0 }')
	[ -n "$cc_pem" ] || cucina_die "no certificate with SHA-1 $cc_want"
	printf '%s\n' "$cc_pem" | openssl x509 -noout -subject -nameopt RFC2253 | sed -n 's/^subject= *//p' |
		tr ',' '\n' | sed -n 's/^CN=//p' | head -1
}

if [ "$devid" = 0 ]; then
	[ -n "$identity" ] || cucina_die "no signing identity: pass --identity or set CUCINA_SIGN_IDENTITY" \
		"(create one with scripts/make-signing-cert.sh)"
	case $identity in
	*"Developer ID"*) cucina_die "'$identity' is a Developer ID identity; the Developer ID path needs --developer-id" ;;
	esac
	sha1=$(cert_sha1 "$identity")
	cn=$(cert_cn "$sha1")
	case $cn in *"Developer ID"*) cucina_die "Developer ID certificate requires --developer-id (including SHA-1 selection)" ;; esac
	[ -n "$cn" ] || cucina_die "signer certificate has no common name"
	[ -n "$inst_id" ] || cucina_die "private signing needs --installer-identity (or CUCINA_INSTALLER_IDENTITY), separate from the application identity; see ADR 0752"
	inst_sha1=$(cert_sha1 "$inst_id")
	inst_cn=$(cert_cn "$inst_sha1")
	case $inst_cn in *"Developer ID"*) cucina_die "Developer ID installer certificate requires --developer-id" ;; esac
	[ "$inst_sha1" != "$sha1" ] || cucina_die "application and installer certificates must be distinct on macOS"
	cucina_info "private signing: application $sha1; installer $inst_sha1 (tart.app untouched)"
	set -- "$@" --sign-identity "$sha1" --installer-identity "$inst_cn"
	[ -z "$keychain" ] || set -- "$@" --keychain "$keychain"
	[ "$timestamp" = 1 ] || set -- "$@" --no-timestamp
	"$SCRIPTS/build-pkg.sh" "$@"
	"$SCRIPTS/check-pkg.sh" "$out" --signed --app-cert-sha1 "$sha1" --cert-sha1 "$inst_sha1"
	cat <<EOF
signed: $out
  SHA-256: $(cucina_sha256 "$out")
  installer signer: $inst_sha1 (devices need this certificate in profile 01 BEFORE the package)
  application signer: $sha1
EOF
	exit 0
fi

# --- Developer ID path (opt-in) ----------------------------------------------------------------------------------
if [ -z "$app_id" ] || [ -z "$inst_id" ]; then
	cucina_die "--developer-id needs --app-identity and --installer-identity"
fi
case $app_id in *"Developer ID Application"*) ;; *) cucina_die "--app-identity must be a 'Developer ID Application' identity" ;; esac
case $inst_id in *"Developer ID Installer"*) ;; *) cucina_die "--installer-identity must be a 'Developer ID Installer' identity" ;; esac
[ "$timestamp" = 1 ] || cucina_die "Developer ID signatures require secure timestamps (notarization rejects them otherwise)"
if [ -z "$notary_key" ] || [ -z "$notary_key_id" ] || [ -z "$notary_issuer" ]; then
	cucina_die "--developer-id needs an App Store Connect API key: --notary-key/--notary-key-id/--notary-issuer"
fi
app_sha1=$(cert_sha1 "$app_id")
cucina_info "Developer ID path: binaries $app_sha1, package '$inst_id'"
set -- "$@" --sign-identity "$app_sha1" --installer-identity "$inst_id"
[ -z "$keychain" ] || set -- "$@" --keychain "$keychain"
"$SCRIPTS/build-pkg.sh" "$@"
"$SCRIPTS/check-pkg.sh" "$out" --signed
"$SCRIPTS/notarize.sh" "$out" --key "$notary_key" --key-id "$notary_key_id" --issuer "$notary_issuer"
printf 'signed + notarized: %s\n  SHA-256: %s\n' "$out" "$(cucina_sha256 "$out")"
