#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Renders the Cucina configuration-profile templates (macos/profiles/templates/*.mobileconfig) into ready-to-upload
# .mobileconfig files: Apple Business "Custom configuration" (< 1 MB, one or more payloads) or any MDM (R-MAC-10).
#
# Template syntax (templates stay valid property lists, so `plutil -lint` passes on them):
#   ${CUCINA_NAME}                          required value (XML-escaped), only inside <string> or <data>
#   <!-- @if CUCINA_NAME: <xml…> -->        one-line optional element, emitted only when CUCINA_NAME is set
#   <!-- @if CUCINA_NAME --> … <!-- @endif -->  optional block (no nesting)
# Whole-line comments are dropped from the output.
#
# usage: render.sh [--config FILE] [--out DIR] [--only 01,02,…] [options]
#   --config FILE                  KEY=VALUE lines with CUCINA_* settings (see example.env); no shell expansion
#   --out DIR                      output directory (default: ~/.config/cucina/profiles, created 0700). Rendered
#                                  profiles contain the site enrollment token (02) and maybe a password (05):
#                                  refused inside a git work tree unless --allow-repo-output (tests only)
#   --signer-cert FILE             package-signing certificate (PEM or DER); give twice during a rotation
#   --ca-cert FILE                 Cucina CA (PEM or DER): hostd CACertificate and, unless --no-trust-ca, a root
#                                  payload in 01-cucina-trust
#   --token-file FILE              site enrollment token (else CUCINA_SITE_ENROLLMENT_TOKEN)
#   --autologin-password-file FILE renders 05-cucina-autologin (alternative path; off by default, ADR 0751)
#   --only LIST                    comma-separated template numbers to render (default: all applicable)
# Settings (env, --config or --set KEY=VALUE): CUCINA_CONTROLLER_URL (required), CUCINA_CONTROLLER_SERVER_NAME,
#   CUCINA_CA_PIN_SHA256 (comma list, alternative/addition to --ca-cert), CUCINA_IDENTITY_LABEL, CUCINA_SITE,
#   CUCINA_LABELS (k=v,k2=v2), CUCINA_VM_SLOTS (1-2), CUCINA_VM_CPU_COUNT, CUCINA_VM_MEMORY_GIB,
#   CUCINA_L2_SIZE_GIB (10-4096), CUCINA_VM_MAX_AGE_HOURS (1-2160), CUCINA_LOG_LEVEL, CUCINA_RUN_AS_USER,
#   CUCINA_TART_PATH (only for the DDM background-tasks variant),
#   CUCINA_CREATE_USER / CUCINA_MANAGE_AUTOLOGIN / CUCINA_RESTART_AFTER_FIRST_INSTALL (true|false),
#   CUCINA_LOCAL_NETWORK_CIDRS (comma list), CUCINA_AUTOLOGIN_USER (default cucina),
#   CUCINA_FIREWALL_STEALTH (default true), CUCINA_ORGANIZATION (default "Cucina").
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
TEMPLATES=$HERE/templates

die() {
	printf 'render.sh: error: %s\n' "$*" >&2
	exit 1
}

out=${CUCINA_PROFILES_OUT:-$HOME/.config/cucina/profiles}
only='' allow_repo=0 signer1='' signer2='' ca_cert='' trust_ca=1 token_file='' pw_file=''

# set_var NAME VALUE — records a setting given on the command line or in a config file.
set_var() {
	case $1 in
	CUCINA_[A-Z0-9_]*) ;;
	*) die "unknown setting $1 (settings start with CUCINA_)" ;;
	esac
	case $1 in *[!A-Z0-9_]*) die "invalid setting name $1" ;; esac
	eval "$1=\$2"
	export "${1?}"
}

load_config() {
	[ -r "$1" ] || die "config not readable: $1"
	while IFS= read -r line || [ -n "$line" ]; do
		case $line in '' | '#'*) continue ;; esac
		case $line in *=*) ;; *) die "invalid line in $1: $line" ;; esac
		set_var "${line%%=*}" "${line#*=}"
	done <"$1"
}

while [ $# -gt 0 ]; do
	case $1 in
	--config) load_config "$2" && shift 2 ;;
	--set) set_var "${2%%=*}" "${2#*=}" && shift 2 ;;
	--out) out=$2 && shift 2 ;;
	--only) only=$2 && shift 2 ;;
	--allow-repo-output) allow_repo=1 && shift ;;
	--signer-cert)
		if [ -z "$signer1" ]; then signer1=$2; elif [ -z "$signer2" ]; then signer2=$2; else die "at most two --signer-cert"; fi
		shift 2
		;;
	--ca-cert) ca_cert=$2 && shift 2 ;;
	--no-trust-ca) trust_ca=0 && shift ;;
	--token-file) token_file=$2 && shift 2 ;;
	--autologin-password-file) pw_file=$2 && shift 2 ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' && exit 0 ;;
	*) die "unknown argument: $1 (try --help)" ;;
	esac
done
command -v openssl >/dev/null 2>&1 || die "openssl is required"

xml_escape() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'; }

# cert_b64 FILE — DER base64 (one line); cert_cn FILE — subject common name.
cert_der() {
	if grep -q 'BEGIN CERTIFICATE' "$1" 2>/dev/null; then openssl x509 -in "$1" -outform DER; else openssl x509 -inform DER -in "$1" -outform DER; fi
}
cert_b64() { cert_der "$1" | base64 | tr -d '\n'; }
cert_cn() {
	cert_der "$1" | openssl x509 -inform DER -noout -subject -nameopt RFC2253 | sed -n 's/^subject= *//p' |
		tr ',' '\n' | sed -n 's/^CN=//p' | head -1 | tr -cd 'A-Za-z0-9 ._()-'
}

# --- certificates and secrets from files ------------------------------------------------------------------------
if [ -n "$signer1" ]; then
	[ -f "$signer1" ] || die "no such file: $signer1"
	set_var CUCINA_SIGNER_CERT_B64 "$(cert_b64 "$signer1")"
	set_var CUCINA_SIGNER_CERT_NAME "$(cert_cn "$signer1")"
fi
if [ -n "$signer2" ]; then
	[ -f "$signer2" ] || die "no such file: $signer2"
	set_var CUCINA_SIGNER2_CERT_B64 "$(cert_b64 "$signer2")"
	set_var CUCINA_SIGNER2_CERT_NAME "$(cert_cn "$signer2")"
fi
if [ -n "$ca_cert" ]; then
	[ -f "$ca_cert" ] || die "no such file: $ca_cert"
	set_var CUCINA_CA_CERT_B64 "$(cert_b64 "$ca_cert")"
	if [ "$trust_ca" = 1 ]; then
		set_var CUCINA_TRUST_CA_CERT_B64 "$CUCINA_CA_CERT_B64"
		set_var CUCINA_TRUST_CA_CERT_NAME "$(cert_cn "$ca_cert")"
	fi
fi
if [ -n "$token_file" ]; then
	[ -f "$token_file" ] || die "no such file: $token_file"
	set_var CUCINA_SITE_ENROLLMENT_TOKEN "$(tr -d '\r\n' <"$token_file")"
fi
if [ -n "$pw_file" ]; then
	[ -f "$pw_file" ] || die "no such file: $pw_file"
	set_var CUCINA_AUTOLOGIN_PASSWORD "$(tr -d '\r\n' <"$pw_file")"
fi

# --- defaults and validation ------------------------------------------------------------------------------------
: "${CUCINA_ORGANIZATION:=Cucina}" "${CUCINA_AUTOLOGIN_USER:=cucina}" "${CUCINA_FIREWALL_STEALTH:=true}"
export CUCINA_ORGANIZATION CUCINA_AUTOLOGIN_USER CUCINA_FIREWALL_STEALTH

v() { eval "printf '%s' \"\${$1:-}\""; }
check_int() { # NAME MIN MAX
	ci=$(v "$1")
	[ -n "$ci" ] || return 0
	case $ci in *[!0-9]*) die "$1 must be an integer (got '$ci')" ;; esac
	if [ "$ci" -lt "$2" ] || [ "$ci" -gt "$3" ]; then die "$1 must be within $2..$3 (got $ci)"; fi
}
check_bool() {
	case $(v "$1") in '' | true | false) ;; *) die "$1 must be true or false" ;; esac
}
check_int CUCINA_VM_SLOTS 1 2
check_int CUCINA_VM_CPU_COUNT 0 1024
check_int CUCINA_VM_MEMORY_GIB 0 4096
check_int CUCINA_L2_SIZE_GIB 10 4096
check_int CUCINA_VM_MAX_AGE_HOURS 1 2160
for b in CUCINA_CREATE_USER CUCINA_MANAGE_AUTOLOGIN CUCINA_RESTART_AFTER_FIRST_INSTALL CUCINA_FIREWALL_STEALTH; do check_bool "$b"; done
case $(v CUCINA_LOG_LEVEL) in '' | debug | info | warn | error) ;; *) die "CUCINA_LOG_LEVEL must be debug|info|warn|error" ;; esac
case $(v CUCINA_CONTROLLER_URL) in '' | https://*) ;; *) die "CUCINA_CONTROLLER_URL must start with https://" ;; esac
case $(v CUCINA_RUN_AS_USER)$(v CUCINA_AUTOLOGIN_USER) in *[!a-z0-9_.-]*) die "user names must be lowercase short names" ;; esac
case $(v CUCINA_TART_PATH) in '' | /*) ;; *) die "CUCINA_TART_PATH must be an absolute path" ;; esac
for b64 in CUCINA_SIGNER_CERT_B64 CUCINA_SIGNER2_CERT_B64 CUCINA_CA_CERT_B64; do
	case $(v "$b64") in *[!A-Za-z0-9+/=]*) die "$b64 is not base64" ;; esac
done

# Generated XML fragments (values are validated, then escaped).
if [ -n "$(v CUCINA_CA_PIN_SHA256)" ]; then
	pins=$(v CUCINA_CA_PIN_SHA256 | tr ',' ' ')
	n=0 frag=''
	for p in $pins; do
		printf '%s' "$p" | grep -Eq '^[0-9a-f]{64}$' || die "CUCINA_CA_PIN_SHA256 entries must be 64 lowercase hex digits"
		frag="$frag<string>$p</string>"
		n=$((n + 1))
	done
	[ "$n" = 1 ] || frag="<array>$frag</array>"
	set_var CUCINA_CA_PIN_XML "$frag"
fi
if [ -n "$(v CUCINA_LABELS)" ]; then
	frag='<dict>'
	old_ifs=$IFS
	IFS=,
	for kv in $(v CUCINA_LABELS); do
		case $kv in *=*) ;; *) die "CUCINA_LABELS entries must be key=value" ;; esac
		frag="$frag<key>$(xml_escape "${kv%%=*}")</key><string>$(xml_escape "${kv#*=}")</string>"
	done
	IFS=$old_ifs
	set_var CUCINA_LABELS_XML "$frag</dict>"
fi
if [ -n "$(v CUCINA_LOCAL_NETWORK_CIDRS)" ]; then
	frag='<array>'
	for c in $(v CUCINA_LOCAL_NETWORK_CIDRS | tr ',' ' '); do
		case $c in */*) ;; *) die "CUCINA_LOCAL_NETWORK_CIDRS entries must be CIDRs" ;; esac
		case $c in *[!0-9a-fA-F:./]*) die "invalid CIDR: $c" ;; esac
		frag="$frag<string>$c</string>"
	done
	set_var CUCINA_LOCAL_NETWORK_CIDRS_XML "$frag</array>"
fi
if [ -n "$(v CUCINA_SITE_ENROLLMENT_TOKEN)$(v CUCINA_CONTROLLER_URL)" ] &&
	[ -z "$(v CUCINA_CA_CERT_B64)" ] && [ -z "$(v CUCINA_CA_PIN_XML)" ]; then
	die "hostd needs --ca-cert FILE or CUCINA_CA_PIN_SHA256 to verify the controller"
fi

# Profile 02 is site-specific (token, site, labels): with CUCINA_SITE its identifiers get a site suffix, so one MDM
# can hold one 02 profile per site. Keep the raw site name before escaping.
raw_site=$(v CUCINA_SITE)
site_sfx=$(printf '%s' "$raw_site" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-' | sed -e 's/-*$//' -e 's/^-*//')
uuid_from() { # SEED — deterministic UUID-shaped identifier (stable across re-renders)
	uf_h=$(printf '%s' "$1" | shasum -a 256 | cut -c1-32 | tr 'a-f' 'A-F')
	printf '%s-%s-%s-%s-%s' "$(printf '%s' "$uf_h" | cut -c1-8)" "$(printf '%s' "$uf_h" | cut -c9-12)" \
		"$(printf '%s' "$uf_h" | cut -c13-16)" "$(printf '%s' "$uf_h" | cut -c17-20)" "$(printf '%s' "$uf_h" | cut -c21-32)"
}

# XML-escape every plain string setting in place (B64/XML fragments/booleans are validated above).
for name in $(env | sed -n 's/^\(CUCINA_[A-Z0-9_]*\)=.*/\1/p'); do
	case $name in *_B64 | *_XML) continue ;; esac
	set_var "$name" "$(xml_escape "$(v "$name")")"
done

# --- output directory -------------------------------------------------------------------------------------------
existing=$out
while [ ! -d "$existing" ]; do existing=$(dirname -- "$existing"); done
if [ "$allow_repo" = 0 ] && git -C "$existing" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	die "refusing to write rendered profiles (secrets) inside a git work tree: $out"
fi
mkdir -p "$out"
chmod 0700 "$out"
out=$(CDPATH='' cd -- "$out" && pwd -P)

render_one() { # TEMPLATE OUTPUT
	awk '
	function fail(msg) { printf "render.sh: error: %s: %s\n", FILENAME, msg > "/dev/stderr"; bad = 1; exit 1 }
	function subst(s,    res, name, val) {
		res = ""
		while (match(s, /\$\{CUCINA_[A-Z0-9_]+\}/)) {
			name = substr(s, RSTART + 2, RLENGTH - 3)
			val = ENVIRON[name]
			if (val == "") missing[name] = 1
			res = res substr(s, 1, RSTART - 1) val
			s = substr(s, RSTART + RLENGTH)
		}
		return res s
	}
	/^[ \t]*<!-- @if CUCINA_[A-Z0-9_]+ -->[ \t]*$/ {
		if (inblock) fail("nested @if block")
		inblock = 1
		n = $0; sub(/^[ \t]*<!-- @if /, "", n); sub(/ -->[ \t]*$/, "", n)
		keep = (ENVIRON[n] != "")
		next
	}
	/^[ \t]*<!-- @endif -->[ \t]*$/ { if (!inblock) fail("@endif without @if"); inblock = 0; next }
	inblock && !keep { next }
	/^[ \t]*<!-- @if CUCINA_[A-Z0-9_]+: .* -->[ \t]*$/ {
		ind = $0; sub(/<!--.*$/, "", ind)
		n = $0; sub(/^[ \t]*<!-- @if /, "", n); sub(/:.*$/, "", n)
		body = $0; sub(/^[ \t]*<!-- @if CUCINA_[A-Z0-9_]+: /, "", body); sub(/ -->[ \t]*$/, "", body)
		if (ENVIRON[n] == "") next
		print ind subst(body)
		next
	}
	/^[ \t]*<!--.*-->[ \t]*$/ { next }
	{ print subst($0) }
	END {
		if (bad) exit 1
		if (inblock) fail("unterminated @if block")
		for (m in missing) { printf "render.sh: error: %s needs %s\n", FILENAME, m > "/dev/stderr"; miss = 1 }
		if (miss) exit 2
	}' "$1" >"$2.tmp" || {
		rm -f "$2.tmp"
		return 1
	}
	mv -f "$2.tmp" "$2"
	chmod 0600 "$2"
	plutil -lint "$2" >/dev/null || die "rendered $2 does not lint"
	# shellcheck disable=SC2016 # literal ${ is the placeholder syntax
	if grep -q '\${CUCINA_\|@if\|@endif' "$2"; then die "unrendered placeholders left in $2"; fi
	size=$(wc -c <"$2" | tr -d ' ')
	[ "$size" -lt 1048576 ] || die "$2 is $size bytes; Apple Business custom configurations must be < 1 MB"
	printf '%-44s %7s bytes  sha256 %s\n' "$(basename "$2")" "$size" "$(shasum -a 256 "$2" | cut -c1-16)"
}

rendered=0 failed=0
for t in "$TEMPLATES"/[0-9][0-9]-*.mobileconfig; do
	f=$(basename "$t")
	num=${f%%-*}
	if [ -n "$only" ]; then
		case ",$only," in *",$num,"*) ;; *) continue ;; esac
	elif [ "$num" = 05 ] && [ -z "$(v CUCINA_AUTOLOGIN_PASSWORD)" ]; then
		continue # alternative auto-login path: only with --autologin-password-file
	fi
	if render_one "$t" "$out/$f"; then rendered=$((rendered + 1)); else
		failed=$((failed + 1))
		continue
	fi
	if [ "$num" = 02 ] && [ -n "$site_sfx" ]; then
		plutil -replace PayloadIdentifier -string "ai.sloper.cucina.profile.hostd-preferences.$site_sfx" "$out/$f"
		plutil -replace PayloadUUID -string "$(uuid_from "cucina-profile-02-$site_sfx")" "$out/$f"
		plutil -replace PayloadDisplayName -string "Cucina 02 host agent settings ($raw_site)" "$out/$f"
		plutil -replace PayloadContent.0.PayloadIdentifier -string "ai.sloper.cucina.profile.hostd-preferences.$site_sfx.settings" "$out/$f"
		plutil -replace PayloadContent.0.PayloadUUID -string "$(uuid_from "cucina-profile-02-settings-$site_sfx")" "$out/$f"
		printf '%-44s site-specific identifiers (.%s)\n' "$f" "$site_sfx"
	fi
done
[ "$failed" = 0 ] || die "$failed profile(s) failed to render"
printf 'rendered %d profile(s) into %s (mode 0600: 02 holds the site enrollment token)\n' "$rendered" "$out"
