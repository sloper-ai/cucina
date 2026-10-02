#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# T14 MDM simulation, run as root INSIDE a throwaway macOS VM (never on a real host or the dev Mac). It reproduces
# the effect of the profiles MDM delivers before the package (macos/profiles, push order in
# docs/macos/mac-mini-setup.md §4):
#   01-cucina-trust     -> the package-signing certificate becomes a trusted root in the System keychain, so
#                          `installer -pkg ... -target /` works WITHOUT -allowUntrusted;
#   02-cucina-hostd-preferences -> /Library/Managed Preferences/ai.sloper.cucina.hostd.plist (+ the pkg's install
#                          settings domain ai.sloper.cucina.host);
#   05 auto-login       -> default "postinstall": nothing to do, the package creates the cucina user with an
#                          on-device random password and enables auto-login (ADR 0751); "preset": create the user
#                          and auto-login now, like an MDM that creates the account (loginwindow AutologinUsername).
# Login items (com.apple.servicemanagement) and loginwindow Autologin* keys are MDM-only payloads and cannot be
# installed by hand; BTM still lists the daemon (enabled, not managed).
#
# usage: sudo simulate-mdm.sh --signer-cert FILE --controller-url URL (--ca-cert FILE | --ca-pin HEX)
#                             [--token-file FILE] [--site NAME] [--vm-slots N] [--log-level L]
#                             [--autologin postinstall|preset] [--allow-restart] [--no-local-copy]
#        sudo simulate-mdm.sh --undo --signer-cert FILE
#   --token-file FILE  file with the site enrollment token (default: $CUCINA_SITE_TOKEN); never printed or put in argv
#   --allow-restart    let the package restart the VM after its first install (default: RestartAfterFirstInstall=false,
#                      the scenario reboots explicitly)
#   --no-local-copy    do not also write /Library/Preferences/ai.sloper.cucina.hostd.plist (fallback copy, 0600, used
#                      by hostd if macOS discards hand-made managed preferences without a profile)
set -eu
umask 022

SYSTEM_KEYCHAIN=/Library/Keychains/System.keychain
MP_DIR="/Library/Managed Preferences"
HOSTD_DOMAIN=ai.sloper.cucina.hostd
SETUP_DOMAIN=ai.sloper.cucina.host
MARKER=/var/db/cucina-simulated-mdm

die() {
	echo "simulate-mdm: error: $*" >&2
	exit 1
}
log() { echo "simulate-mdm: $*"; }

signer='' ca_cert='' ca_pin='' controller_url='' token_file='' site='' vm_slots='' log_level=''
autologin=postinstall allow_restart=0 local_copy=1 undo=0
while [ $# -gt 0 ]; do
	case $1 in
	--signer-cert) signer=$2 && shift 2 ;;
	--ca-cert) ca_cert=$2 && shift 2 ;;
	--ca-pin) ca_pin=$2 && shift 2 ;;
	--controller-url) controller_url=$2 && shift 2 ;;
	--token-file) token_file=$2 && shift 2 ;;
	--site) site=$2 && shift 2 ;;
	--vm-slots) vm_slots=$2 && shift 2 ;;
	--log-level) log_level=$2 && shift 2 ;;
	--autologin) autologin=$2 && shift 2 ;;
	--allow-restart) allow_restart=1 && shift ;;
	--no-local-copy) local_copy=0 && shift ;;
	--undo) undo=1 && shift ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d' && exit 0 ;;
	*) die "unknown argument: $1" ;;
	esac
done
[ "$(/usr/bin/id -u)" = 0 ] || die "run as root inside the test VM"
case $(/usr/sbin/sysctl -n hw.model) in VirtualMac*) ;; *) die "refusing to change trust or preferences outside a macOS VM" ;; esac
[ "$(/usr/bin/stat -f '%u:%Lp' /var/db/cucina-t14-throwaway 2>/dev/null)" = 0:600 ] ||
	die "missing root-owned T14 throwaway sentinel (/var/db/cucina-t14-throwaway, mode 0600)"
[ -f "$signer" ] || die "--signer-cert FILE is required"

# DER SHA-1 of the signer (works for PEM and DER input).
if grep -q 'BEGIN CERTIFICATE' "$signer" 2>/dev/null; then
	signer_sha1=$(openssl x509 -in "$signer" -outform DER | shasum -a 1 | cut -d ' ' -f 1 | tr 'a-f' 'A-F')
else
	signer_sha1=$(shasum -a 1 "$signer" | cut -d ' ' -f 1 | tr 'a-f' 'A-F')
fi

if [ "$undo" = 1 ]; then
	security remove-trusted-cert -d "$signer" 2>/dev/null || true
	security delete-certificate -Z "$signer_sha1" "$SYSTEM_KEYCHAIN" 2>/dev/null || true
	rm -f "$MP_DIR/$HOSTD_DOMAIN.plist" "$MP_DIR/$SETUP_DOMAIN.plist" "/Library/Preferences/$HOSTD_DOMAIN.plist" "/Library/Preferences/$SETUP_DOMAIN.plist"
	rm -f "$MARKER"
	log "removed simulated trust and preferences (the cucina user, if any, is left in place)"
	exit 0
fi

[ -n "$controller_url" ] || die "--controller-url is required"
case $controller_url in https://*) ;; *) die "--controller-url must be https://" ;; esac
[ -n "$ca_cert" ] || [ -n "$ca_pin" ] || die "pass --ca-cert FILE or --ca-pin HEX"
[ -z "$ca_cert" ] || [ -f "$ca_cert" ] || die "no such file: $ca_cert"
case $autologin in postinstall | preset) ;; *) die "--autologin must be postinstall or preset" ;; esac
codec=$(dirname -- "$0")/../payload/cucina-kcpassword
[ "$autologin" != preset ] || [ -x "$codec" ] || die "preset mode requires the complete kit (missing kcpassword codec)"

# --- 01-cucina-trust: trusted root in the System keychain (what a com.apple.security.root payload does) ----------
# Root on the throwaway macOS 27 VM can add administrator trust directly. Never rewrite authorizationdb:
# macOS rejects that policy edit, and weakening a global right is unnecessary even in this simulation.
security add-trusted-cert -d -r trustRoot -k "$SYSTEM_KEYCHAIN" "$signer"
log "trusted package signer $signer_sha1 in the System keychain"

# --- 02-cucina-hostd-preferences ------------------------------------------------------------------------------
xml_escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }
token=''
if [ -n "$token_file" ]; then
	[ -f "$token_file" ] || die "no such file: $token_file"
	token=$(tr -d '\r\n' <"$token_file")
elif [ -n "${CUCINA_SITE_TOKEN:-}" ]; then
	token=$CUCINA_SITE_TOKEN
fi
[ -n "$token" ] || die "no site enrollment token (--token-file FILE or CUCINA_SITE_TOKEN)"

prefs=$(mktemp)
{
	cat <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
EOF
	printf '\t<key>ControllerURL</key>\n\t<string>%s</string>\n' "$(printf '%s' "$controller_url" | xml_escape)"
	if [ -n "$ca_cert" ]; then
		printf '\t<key>CACertificate</key>\n\t<string>%s</string>\n' "$(xml_escape <"$ca_cert")"
	else
		printf '\t<key>CAPinSHA256</key>\n\t<string>%s</string>\n' "$(printf '%s' "$ca_pin" | xml_escape)"
	fi
	printf '\t<key>SiteEnrollmentToken</key>\n\t<string>%s</string>\n' "$(printf '%s' "$token" | xml_escape)"
	[ -z "$site" ] || printf '\t<key>Site</key>\n\t<string>%s</string>\n' "$(printf '%s' "$site" | xml_escape)"
	[ -z "$vm_slots" ] || printf '\t<key>VMSlots</key>\n\t<integer>%d</integer>\n' "$vm_slots"
	[ -z "$log_level" ] || printf '\t<key>LogLevel</key>\n\t<string>%s</string>\n' "$log_level"
	printf '</dict>\n</plist>\n'
} >"$prefs"
token=''
plutil -lint "$prefs" >/dev/null || die "generated managed preferences do not lint"
mkdir -p "$MP_DIR"
install -o root -g wheel -m 0644 "$prefs" "$MP_DIR/$HOSTD_DOMAIN.plist"
if [ "$local_copy" = 1 ]; then
	install -o root -g wheel -m 0600 "$prefs" "/Library/Preferences/$HOSTD_DOMAIN.plist"
fi
rm -f "$prefs"
log "wrote $MP_DIR/$HOSTD_DOMAIN.plist (world-readable, like MDM managed preferences; token not shown)"

setup=$(mktemp)
plutil -create xml1 "$setup"
if [ "$allow_restart" = 1 ]; then
	plutil -insert RestartAfterFirstInstall -bool YES "$setup"
else
	plutil -insert RestartAfterFirstInstall -bool NO "$setup"
fi
install -o root -g wheel -m 0644 "$setup" "$MP_DIR/$SETUP_DOMAIN.plist"
if [ "$local_copy" = 1 ]; then
	# Hand-written managed preferences may be purged at reboot; retain the no-auto-restart setting too.
	install -o root -g wheel -m 0600 "$setup" "/Library/Preferences/$SETUP_DOMAIN.plist"
fi
rm -f "$setup"
log "wrote $MP_DIR/$SETUP_DOMAIN.plist (RestartAfterFirstInstall=$([ "$allow_restart" = 1 ] && echo true || echo false))"

# --- auto-login -------------------------------------------------------------------------------------------------
if [ "$autologin" = preset ]; then
	if dscl . -read /Users/cucina UniqueID >/dev/null 2>&1; then
		log "user cucina already exists; leaving it"
	else
		pw=$(openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | cut -c1-32)
		sysadminctl -addUser cucina -fullName "Cucina VM runner" -shell /bin/zsh -home /Users/cucina -password "$pw" >/dev/null 2>&1
		createhomedir -c -u cucina >/dev/null 2>&1 || true
		dscl . -authonly cucina "$pw" >/dev/null 2>&1 || die "created account password does not authenticate"
		install -d -o cucina -g staff -m 0700 /Users/cucina/Library/Keychains
		sudo -H -u cucina security create-keychain -p "$pw" /Users/cucina/Library/Keychains/login.keychain-db
		sudo -H -u cucina security default-keychain -d user -s /Users/cucina/Library/Keychains/login.keychain-db
		sudo -H -u cucina security set-keychain-settings /Users/cucina/Library/Keychains/login.keychain-db
		encoded=$(mktemp /private/etc/kcpassword.cucina.XXXXXX)
		printf '%s' "$pw" | "$codec" >"$encoded"
		chown root:wheel "$encoded" && chmod 0600 "$encoded" && mv -f "$encoded" /etc/kcpassword
		defaults write /Library/Preferences/com.apple.loginwindow autoLoginUser -string cucina
		pw=''
		log "created cucina (standard user) with auto-login, as an MDM-created account would be"
	fi
else
	log "auto-login: left to the package's postinstall (default path)"
fi

printf 'trust=%s\nautologin=%s\n' "$signer_sha1" "$autologin" >"$MARKER"
log "FileVault: $(fdesetup status | head -1)"
log "next: installer -pkg <cucina-host-X-Y-Z.pkg> -target /   (no -allowUntrusted)"
