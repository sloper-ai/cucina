#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Guards R-MAC-10 (tier: integration, macOS): every profile template renders into a .mobileconfig that lints, has no
# placeholder left, is < 1 MB (Apple Business "Custom configuration"), carries values exactly (XML escaping), keeps
# the hostd key schema (docs/dev/hostd.md §3), and render.sh fails fast on missing/invalid input and refuses to write
# secrets into a git work tree. Fixtures are generated at test time; no network.
set -eu

# Bazel runs the test as <runfiles>/_main/macos/profiles/render_test; locate the package through TEST_SRCDIR there.
if [ -n "${TEST_SRCDIR:-}" ]; then HERE=$TEST_SRCDIR/${TEST_WORKSPACE:-_main}/macos/profiles; else
	HERE=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
fi
RENDER=$HERE/render.sh
T=$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/render-test.XXXXXX")
trap 'rm -rf "$T"' EXIT INT TERM
fails=0
ok() { printf 'ok    %s\n' "$*"; }
bad() {
	printf 'FAIL  %s\n' "$*"
	fails=$((fails + 1))
}
expect_eq() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi; }
get() { plutil -extract "$2" raw -o - "$1" 2>/dev/null || true; }
render() { env -i HOME="$T/home" PATH=/usr/bin:/bin:/usr/sbin:/sbin "$RENDER" "$@"; }

mkdir -p "$T/home"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj '/CN=Test Signer A' -keyout "$T/a.key" -out "$T/signer-a.pem" 2>/dev/null
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj '/CN=Test Signer B' -keyout "$T/b.key" -out "$T/signer-b.pem" 2>/dev/null
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj '/CN=Test Cucina CA' -keyout "$T/c.key" -out "$T/ca.pem" 2>/dev/null
token='tok&<en>"quote'"'"'apos-0123456789'
printf '%s\n' "$token" >"$T/token"
printf 'pw-\n' >"$T/pw"

# 1. Full render: every template, rotation (two signers), CA, optional settings, alternative auto-login.
O=$T/out1
if render --signer-cert "$T/signer-a.pem" --signer-cert "$T/signer-b.pem" --ca-cert "$T/ca.pem" --token-file "$T/token" \
	--autologin-password-file "$T/pw" --set CUCINA_CONTROLLER_URL=https://cucina.example.com:8443 \
	--set CUCINA_VM_SLOTS=1 --set CUCINA_LABELS='rack=a,role=x&y' --set CUCINA_RESTART_AFTER_FIRST_INSTALL=false \
	--set CUCINA_SITE=Site-A \
	--set CUCINA_LOCAL_NETWORK_CIDRS=192.168.64.0/24 --set CUCINA_CA_PIN_SHA256="$(printf 'a%.0s' $(seq 1 64)),$(printf 'b%.0s' $(seq 1 64))" \
	--out "$O" >/dev/null; then ok "renders all templates"; else bad "full render failed"; fi
expect_eq "rendered file count" "$(find "$O" -name '*.mobileconfig' | wc -l | tr -d ' ')" 7
for f in "$O"/*.mobileconfig; do
	plutil -lint "$f" >/dev/null || bad "lint $(basename "$f")"
	if grep -q 'CUCINA_\|@if' "$f"; then bad "placeholder left in $(basename "$f")"; fi
	[ "$(wc -c <"$f")" -lt 1048576 ] || bad "$(basename "$f") >= 1 MB"
	expect_eq "$(basename "$f") mode" "$(stat -f %Lp "$f")" 600
done
P=$O/02-cucina-hostd-preferences.mobileconfig
# Preference domains contain dots, which plutil key paths cannot address: read the profile with Foundation (JXA),
# converting data to base64 and keeping booleans/numbers typed.
JX_CONV='ObjC.import("Foundation");
function conv(x) {
  if (x.isKindOfClass($.NSDictionary)) { var r = {}, ks = x.allKeys; for (var i = 0; i < ks.count; i++) { var k = ks.objectAtIndex(i); r[k.js] = conv(x.objectForKey(k)); } return r; }
  if (x.isKindOfClass($.NSArray)) { var a = []; for (var j = 0; j < x.count; j++) a.push(conv(x.objectAtIndex(j))); return a; }
  if (x.isKindOfClass($.NSData)) return x.base64EncodedStringWithOptions(0).js;
  if (x.isKindOfClass($.NSNumber)) return x.className.js.indexOf("Boolean") >= 0 ? !!x.boolValue : x.doubleValue;
  return ObjC.unwrap(x);
}'
jx() { osascript -l JavaScript -e "$JX_CONV
function run(a) { var o = conv(\$.NSDictionary.dictionaryWithContentsOfFile(a[0])).PayloadContent[0].PayloadContent; var v = ($1); return (typeof v === 'object') ? JSON.stringify(v) : String(v); }" "$P"; }
HS='o["ai.sloper.cucina.hostd"].Forced[0].mcx_preference_settings'
SS='o["ai.sloper.cucina.host"].Forced[0].mcx_preference_settings'
expect_eq "hostd keys rendered" "$(jx "Object.keys($HS).sort().join(' ')")" \
	"CACertificate CAPinSHA256 ControllerURL Labels Site SiteEnrollmentToken VMSlots"
expect_eq "site-specific profile identifier (one 02 profile per site)" \
	"$(get "$P" PayloadIdentifier)" ai.sloper.cucina.profile.hostd-preferences.site-a
# Every key any template can emit must exist in hostd's strict schema (unknown keys make hostd exit 2).
allowed=" ControllerURL ControllerServerName CACertificate CAPinSHA256 SiteEnrollmentToken IdentityLabel Site Labels VMSlots VMCPUCount VMMemoryGiB L2SizeGiB LogLevel TartPath RunAsUser VMMaxAgeHours MetricsListen CreateUser ManageAutoLogin RestartAfterFirstInstall LocalNetworkAllowedEthernetAddresses Forced mcx_preference_settings PayloadContent PayloadDescription PayloadDisplayName PayloadIdentifier PayloadOrganization PayloadScope PayloadType PayloadUUID PayloadVersion "
sed -n 's/.*<!-- @if CUCINA_[A-Z0-9_]*: <key>\([A-Za-z0-9]*\)<\/key>.*/\1/p; s/^[[:space:]]*<key>\([A-Za-z]*\)<\/key>$/\1/p' \
	"$HERE/templates/02-cucina-hostd-preferences.mobileconfig" | sort -u >"$T/keys"
while read -r k; do
	case $allowed in
	*" $k "*) ;;
	*) bad "template key $k is not in hostd's schema (docs/dev/hostd.md §3) or the install settings" ;;
	esac
done <"$T/keys"
expect_eq "template keys checked" "$([ -s "$T/keys" ] && echo yes)" yes
expect_eq "token survives XML escaping" "$(jx "$HS.SiteEnrollmentToken")" "$token"
expect_eq "VMSlots is an integer" "$(jx "typeof $HS.VMSlots + ':' + $HS.VMSlots")" "number:1"
expect_eq "Labels dictionary" "$(jx "$HS.Labels")" '{"rack":"a","role":"x&y"}'
expect_eq "CA pins become an array" "$(jx "$HS.CAPinSHA256.length")" 2
expect_eq "install setting is a boolean" "$(jx "$SS.RestartAfterFirstInstall")" false
expect_eq "Local Network CIDRs" "$(jx "$SS.LocalNetworkAllowedEthernetAddresses")" '["192.168.64.0/24"]'
expect_eq "CACertificate is the CA's DER" "$(jx "$HS.CACertificate")" "$(openssl x509 -in "$T/ca.pem" -outform DER | base64 | tr -d '\n')"
expect_eq "trust profile payloads (signer, signer2, CA)" "$(plutil -p "$O/01-cucina-trust.mobileconfig" | grep -c 'com.apple.security.root')" 3
expect_eq "login item rule" "$(get "$O/03-cucina-login-items.mobileconfig" PayloadContent.0.Rules.0.RuleValue)" ai.sloper.cucina.hostd
expect_eq "FileVault prevention" "$(get "$O/06-cucina-filevault-off.mobileconfig" PayloadContent.0.dontAllowFDEEnable)" true
expect_eq "stealth mode default" "$(get "$O/07-cucina-firewall.mobileconfig" PayloadContent.0.EnableStealthMode)" true
expect_eq "never block all incoming" "$(get "$O/07-cucina-firewall.mobileconfig" PayloadContent.0.BlockAllIncoming)" false

# 2. Defaults: no auto-login password -> 05 is skipped; --no-trust-ca keeps the CA out of the trust profile.
O=$T/out2
render --signer-cert "$T/signer-a.pem" --ca-cert "$T/ca.pem" --no-trust-ca --token-file "$T/token" \
	--set CUCINA_CONTROLLER_URL=https://c.example --out "$O" >/dev/null
if [ -e "$O/05-cucina-autologin.mobileconfig" ]; then bad "05 rendered without a password"; else ok "05 is opt-in"; fi
expect_eq "trust profile without CA" "$(plutil -p "$O/01-cucina-trust.mobileconfig" | grep -c 'com.apple.security.root')" 1

# R-MAC-9: both application and installer identities can rotate with old+new trust in parallel.
if render --only 01 --signer-cert "$T/signer-a.pem" --signer-cert "$T/signer-b.pem" \
	--signer-cert "$T/signer-a.pem" --signer-cert "$T/signer-b.pem" --ca-cert "$T/ca.pem" --out "$T/rotation" >/dev/null 2>&1; then
	expect_eq "dual-role rotation plus CA" "$(plutil -p "$T/rotation/01-cucina-trust.mobileconfig" | grep -c 'com.apple.security.root')" 5
else bad "dual-role rotation was rejected"; fi

# 3. Fail fast (table): each row must exit non-zero.
while IFS='|' read -r name args; do
	[ -n "$name" ] || continue
	# shellcheck disable=SC2086 # args is a word list on purpose
	if render $args --out "$T/fail-$name" >/dev/null 2>&1; then bad "accepted: $name"; else ok "rejected: $name"; fi
done <<EOF
missing-token|--signer-cert $T/signer-a.pem --ca-cert $T/ca.pem --set CUCINA_CONTROLLER_URL=https://c.example
missing-ca|--signer-cert $T/signer-a.pem --token-file $T/token --set CUCINA_CONTROLLER_URL=https://c.example
http-url|--signer-cert $T/signer-a.pem --ca-cert $T/ca.pem --token-file $T/token --set CUCINA_CONTROLLER_URL=http://c.example
vm-slots-3|--signer-cert $T/signer-a.pem --ca-cert $T/ca.pem --token-file $T/token --set CUCINA_CONTROLLER_URL=https://c.example --set CUCINA_VM_SLOTS=3
bad-bool|--only 02 --ca-cert $T/ca.pem --token-file $T/token --set CUCINA_CONTROLLER_URL=https://c.example --set CUCINA_CREATE_USER=yes
bad-pin|--only 02 --token-file $T/token --set CUCINA_CONTROLLER_URL=https://c.example --set CUCINA_CA_PIN_SHA256=xyz
missing-signer|--only 01
EOF

# 4. Secrets never land in a git work tree.
git init -q "$T/repo"
if render --only 03 --out "$T/repo/profiles" >/dev/null 2>&1; then bad "wrote into a git work tree"; else ok "refuses git work tree"; fi
if [ -e "$T/repo/profiles" ]; then bad "created a directory inside the work tree"; fi

[ "$fails" = 0 ] || {
	echo "render_test: $fails failure(s)" >&2
	exit 1
}
echo "render_test: ok"
