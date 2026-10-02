#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Production/CI signing keychain (R-MAC-9: "CI secret store in production"). Imports the private signing identity
# from a PKCS#12 (secret store -> file or base64 env var) into a THROWAWAY keychain with a random password, unlocks
# it without auto-lock, grants Apple's signing tools prompt-free access (set-key-partition-list) and adds it to the
# user's keychain search list, so scripts/sign.sh --keychain <path> runs unattended. `delete` removes the keychain
# and restores the search list. For ephemeral CI runners and throwaway VMs only; never on a workstation.
#
# usage: ci-keychain.sh create (--p12 FILE | --p12-base64-env VAR) (--pass-file FILE | --pass-env VAR) [--keychain PATH]
#        ci-keychain.sh delete [--keychain PATH]
# Optional --installer-p12 FILE / --installer-p12-base64-env VAR plus --installer-pass-file FILE / --installer-pass-env VAR
# imports a separate private installer identity (ADR 0752). This helper does NOT change certificate trust.
# create prints KEYCHAIN, APPLICATION_IDENTITY_SHA1, INSTALLER_IDENTITY_SHA1 and IDENTITY_SHA1 (application alias).
set -eu

. "$(dirname -- "$0")/lib.sh"
[ $# -ge 1 ] || cucina_die "usage: ci-keychain.sh create|delete [options]"
cmd=$1
shift
keychain=${RUNNER_TEMP:-${TMPDIR:-/tmp}}/cucina-signing.keychain-db
p12='' p12_env='' pass_file='' pass_env=''
installer_p12='' installer_p12_env='' installer_pass_file='' installer_pass_env=''
while [ $# -gt 0 ]; do
	case $1 in
	--p12) p12=$2 && shift 2 ;;
	--p12-base64-env) p12_env=$2 && shift 2 ;;
	--pass-file) pass_file=$2 && shift 2 ;;
	--pass-env) pass_env=$2 && shift 2 ;;
	--installer-p12) installer_p12=$2 && shift 2 ;;
	--installer-p12-base64-env) installer_p12_env=$2 && shift 2 ;;
	--installer-pass-file) installer_pass_file=$2 && shift 2 ;;
	--installer-pass-env) installer_pass_env=$2 && shift 2 ;;
	--keychain) keychain=$2 && shift 2 ;;
	*) cucina_die "unknown argument: $1" ;;
	esac
done
cucina_need security openssl
case $keychain in /*) ;; *) cucina_die "--keychain must be an absolute path" ;; esac

# security quotes each path on its own line. Preserve spaces instead of splitting paths into shell words.
keychains() { security list-keychains -d user | sed -e 's/^ *"//' -e 's/" *$//'; }
remove_from_search_list() {
	list=$(keychains) || return 1
	set --
	while IFS= read -r path; do
		[ -z "$path" ] || [ "$path" = "$keychain" ] || set -- "$@" "$path"
	done <<EOF
$list
EOF
	security list-keychains -d user -s "$@"
}

case $cmd in
create)
	[ ! -e "$keychain" ] && [ ! -e "$keychain-db" ] || cucina_die "$keychain exists (delete it first)"
	umask 077
	tmp=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/cucina-ci.XXXXXX")
	chmod 0700 "$tmp"
	created=0 handed_off=0
	cleanup_create() {
		code=$?
		trap - EXIT INT TERM
		if [ "$created" = 1 ] && [ "$handed_off" = 0 ]; then
			remove_from_search_list >/dev/null 2>&1 || true
			security delete-keychain "$keychain" >/dev/null 2>&1 || cucina_warn "could not delete incomplete signing keychain $keychain"
		fi
		rm -rf "$tmp"
		exit "$code"
	}
	trap cleanup_create EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	if [ -n "$p12_env" ]; then
		case $p12_env in *[!A-Z0-9_]*) cucina_die "invalid variable name $p12_env" ;; esac
		eval "printf '%s' \"\${$p12_env:?}\"" | base64 -D >"$tmp/id.p12"
		p12=$tmp/id.p12
	fi
	[ -f "$p12" ] || cucina_die "no PKCS#12 (--p12 FILE or --p12-base64-env VAR)"
	if [ -n "$pass_env" ]; then
		case $pass_env in *[!A-Z0-9_]*) cucina_die "invalid variable name $pass_env" ;; esac
		eval "printf '%s' \"\${$pass_env:?}\"" >"$tmp/pass"
	elif [ -n "$pass_file" ]; then
		cp "$pass_file" "$tmp/pass"
	else
		cucina_die "no PKCS#12 passphrase (--pass-file FILE or --pass-env VAR)"
	fi
	kc_pass=$(openssl rand -hex 24)
	security create-keychain -p "$kc_pass" "$keychain"
	created=1
	# `security` appends "-db" to names ending in .keychain; follow whatever file it created.
	if [ ! -e "$keychain" ] && [ -e "$keychain-db" ]; then keychain=$keychain-db; fi
	[ -e "$keychain" ] || cucina_die "keychain $keychain was not created"
	security set-keychain-settings "$keychain" # no auto-lock timeout
	security unlock-keychain -p "$kc_pass" "$keychain"
	security import "$p12" -k "$keychain" -f pkcs12 -P "$(cat "$tmp/pass")" -x \
		-T /usr/bin/codesign -T /usr/bin/productbuild -T /usr/bin/productsign -T /usr/bin/pkgbuild >/dev/null
	if [ -n "$installer_p12$installer_p12_env" ]; then
		if [ -n "$installer_p12_env" ]; then
			case $installer_p12_env in *[!A-Z0-9_]*) cucina_die "invalid installer PKCS#12 variable name" ;; esac
			eval "printf '%s' \"\${$installer_p12_env:?}\"" | base64 -D >"$tmp/installer.p12"
			installer_p12=$tmp/installer.p12
		fi
		[ -f "$installer_p12" ] || cucina_die "installer PKCS#12 is missing"
		if [ -n "$installer_pass_env" ]; then
			case $installer_pass_env in *[!A-Z0-9_]*) cucina_die "invalid installer passphrase variable name" ;; esac
			eval "printf '%s' \"\${$installer_pass_env:?}\"" >"$tmp/installer.pass"
		elif [ -n "$installer_pass_file" ]; then
			cp "$installer_pass_file" "$tmp/installer.pass"
		else cucina_die "installer PKCS#12 needs --installer-pass-file or --installer-pass-env"; fi
		security import "$installer_p12" -k "$keychain" -f pkcs12 -P "$(cat "$tmp/installer.pass")" -x \
			-T /usr/bin/productbuild -T /usr/bin/productsign -T /usr/bin/pkgbuild >/dev/null
	fi
	security set-key-partition-list -S apple-tool:,apple: -s -k "$kc_pass" "$keychain" >/dev/null
	# productbuild looks identities up through the search list: prepend the throwaway keychain.
	current=$(keychains)
	set -- "$keychain"
	while IFS= read -r path; do
		[ -z "$path" ] || [ "$path" = "$keychain" ] || set -- "$@" "$path"
	done <<EOF
$current
EOF
	security list-keychains -d user -s "$@"
	kc_pass=''
	sha1=$(security find-identity -p codesigning "$keychain" | awk '/^ *1\)/ { print $2; exit }')
	[ -n "$sha1" ] || cucina_die "no code-signing identity in the imported PKCS#12"
	installer_sha1=$(security find-identity "$keychain" | awk -v app="$sha1" '
		/Matching identities/ { matching = 1; next }
		/Valid identities/ { matching = 0 }
		matching && $1 ~ /^[0-9]+\)$/ && $2 != app { print $2 }')
	[ "$(printf '%s\n' "$installer_sha1" | grep -c . || true)" -le 1 ] || cucina_die "PKCS#12 has ambiguous installer identities"
	[ -z "$installer_p12$installer_p12_env" ] || [ -n "$installer_sha1" ] || cucina_die "no installer identity after import"
	printf 'KEYCHAIN=%s\nIDENTITY_SHA1=%s\nAPPLICATION_IDENTITY_SHA1=%s\nINSTALLER_IDENTITY_SHA1=%s\n' \
		"$keychain" "$sha1" "$sha1" "$installer_sha1"
	handed_off=1
	;;
delete)
	remove_from_search_list
	[ ! -e "$keychain" ] || security delete-keychain "$keychain"
	cucina_info "deleted $keychain"
	;;
*) cucina_die "unknown command: $cmd" ;;
esac
