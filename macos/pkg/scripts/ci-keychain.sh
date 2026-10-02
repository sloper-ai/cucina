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
# create prints "KEYCHAIN=<path>" and "IDENTITY_SHA1=<hex>" for the following signing step.
set -eu

. "$(dirname -- "$0")/lib.sh"
[ $# -ge 1 ] || cucina_die "usage: ci-keychain.sh create|delete [options]"
cmd=$1
shift
keychain=${RUNNER_TEMP:-${TMPDIR:-/tmp}}/cucina-signing.keychain-db
p12='' p12_env='' pass_file='' pass_env=''
while [ $# -gt 0 ]; do
	case $1 in
	--p12) p12=$2 && shift 2 ;;
	--p12-base64-env) p12_env=$2 && shift 2 ;;
	--pass-file) pass_file=$2 && shift 2 ;;
	--pass-env) pass_env=$2 && shift 2 ;;
	--keychain) keychain=$2 && shift 2 ;;
	*) cucina_die "unknown argument: $1" ;;
	esac
done
cucina_need security openssl

case $cmd in
create)
	[ ! -e "$keychain" ] || cucina_die "$keychain exists (delete it first)"
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/cucina-ci.XXXXXX")
	chmod 0700 "$tmp"
	trap 'rm -rf "$tmp"' EXIT INT TERM
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
	# `security` appends "-db" to names ending in .keychain; follow whatever file it created.
	if [ ! -e "$keychain" ] && [ -e "$keychain-db" ]; then keychain=$keychain-db; fi
	[ -e "$keychain" ] || cucina_die "keychain $keychain was not created"
	security set-keychain-settings "$keychain" # no auto-lock timeout
	security unlock-keychain -p "$kc_pass" "$keychain"
	security import "$p12" -k "$keychain" -f pkcs12 -P "$(cat "$tmp/pass")" -x \
		-T /usr/bin/codesign -T /usr/bin/productbuild -T /usr/bin/productsign -T /usr/bin/pkgbuild >/dev/null
	security set-key-partition-list -S apple-tool:,apple: -s -k "$kc_pass" "$keychain" >/dev/null
	# productbuild looks identities up through the search list: prepend the throwaway keychain.
	current=$(security list-keychains -d user | tr -d '"' | tr -s ' \n' ' ')
	# shellcheck disable=SC2086 # one keychain path per word
	security list-keychains -d user -s "$keychain" $current
	kc_pass=''
	sha1=$(security find-identity -p codesigning "$keychain" | awk '/^ *1\)/ { print $2; exit }')
	[ -n "$sha1" ] || cucina_die "no code-signing identity in the imported PKCS#12"
	printf 'KEYCHAIN=%s\nIDENTITY_SHA1=%s\n' "$keychain" "$sha1"
	;;
delete)
	remaining=$(security list-keychains -d user | tr -d '"' | tr -s ' \n' ' ' | tr ' ' '\n' | grep -vxF "$keychain" | tr '\n' ' ')
	# shellcheck disable=SC2086
	security list-keychains -d user -s $remaining
	[ ! -e "$keychain" ] || security delete-keychain "$keychain"
	cucina_info "deleted $keychain"
	;;
*) cucina_die "unknown command: $cmd" ;;
esac
