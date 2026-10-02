#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# T14 observations, run as root INSIDE the throwaway test VM by scripts/t14-vm.sh (or by hand). Prints one
# PASS/FAIL/INFO line per observation and exits non-zero if any FAIL. Never prints secrets.
# usage: t14-guest-checks.sh after-install VERSION | after-reboot | no-login | mark | after-upgrade VERSION |
#        after-uninstall
set -u
# Safety guard: no observations that mutate users/keychains may run on a physical Mac or an unmarked VM.
[ "$(/usr/bin/id -u)" = 0 ] || exit 2
case $(/usr/sbin/sysctl -n hw.model) in VirtualMac*) ;; *) printf 'FAIL  physical host refused\n'; exit 2 ;; esac
[ "$(/usr/bin/stat -f '%u:%Lp' /var/db/cucina-t14-throwaway 2>/dev/null)" = 0:600 ] || exit 2

LABEL=ai.sloper.cucina.hostd
PKG_ID=ai.sloper.cucina.host
TEAM=9M2P8L4D89
fails=0
pass() { printf 'PASS  %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	fails=$((fails + 1))
}
info() { printf 'INFO  %s\n' "$*"; }
expect() { # DESCRIPTION GOT WANT
	if [ "$2" = "$3" ]; then pass "$1 ($2)"; else fail "$1: got '$2', want '$3'"; fi
}
daemon_state() { launchctl print "system/$LABEL" 2>/dev/null | awk -F' = ' '/^\tstate = / { print $2; exit }'; }
console() { stat -f %Su /dev/console 2>/dev/null; }
# keychain_probe: can the cucina user write to its login keychain without UI (what Virtualization.framework needs)?
keychain_probe() {
	uid=$(id -u cucina 2>/dev/null) || return 1
	launchctl asuser "$uid" sudo -H -u cucina /usr/bin/perl -e 'alarm 15; exec @ARGV or die "exec failed"' -- /usr/bin/security add-generic-password -U -a t14 -s cucina-t14-probe -w probe \
		>/dev/null 2>&1 || return 1
	launchctl asuser "$uid" sudo -H -u cucina /usr/bin/perl -e 'alarm 15; exec @ARGV or die "exec failed"' -- /usr/bin/security delete-generic-password -a t14 -s cucina-t14-probe >/dev/null 2>&1
	return 0
}

phase=${1:-}
case $phase in
after-install)
	v=${2:?version}
	expect "receipt version" "$(pkgutil --pkg-info "$PKG_ID" 2>/dev/null | awk '/^version:/ { print $2 }')" "$v"
	expect "installed VERSION" "$(cat /usr/local/cucina/VERSION 2>/dev/null)" "$v"
	expect "LaunchDaemon plist mode/owner" "$(stat -f '%Sp %Su:%Sg' "/Library/LaunchDaemons/$LABEL.plist" 2>/dev/null)" "-rw-r--r-- root:wheel"
	expect "daemon state" "$(daemon_state)" running
	expect "daemon program" "$(launchctl print "system/$LABEL" 2>/dev/null | awk -F' = ' '/^\tprogram = / { print $2; exit }')" /usr/local/cucina/bin/cucina-hostd
	if sfltool dumpbtm 2>/dev/null | grep -q "$LABEL"; then pass "BTM lists $LABEL"; else fail "BTM does not list $LABEL"; fi
	info "BTM entry: $(sfltool dumpbtm 2>/dev/null | grep -A12 "Identifier:.*$LABEL" | grep -E 'Disposition|Type' | tr -s ' ' | tr '\n' ';')"
	if dscl . -read /Users/cucina UniqueID >/dev/null 2>&1; then pass "user cucina exists"; else fail "user cucina missing"; fi
	if dseditgroup -o checkmember -m cucina admin >/dev/null 2>&1; then fail "cucina is an admin"; else pass "cucina is a standard user"; fi
	expect "auto-login user" "$(defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null)" cucina
	expect "/etc/kcpassword mode" "$(stat -f '%Sp %Su' /etc/kcpassword 2>/dev/null)" "-rw------- root"
	expect "FileVault" "$(fdesetup status | head -1)" "FileVault is Off."
	expect "pmset sleep" "$(pmset -g | awk '$1 == "sleep" { print $2; exit }')" 0
	info "pmset womp/autorestart (VMs may not support them): $(pmset -g | awk '$1 == "womp" || $1 == "autorestart" { printf "%s=%s ", $1, $2 }')"
	info "restart after freeze: $(systemsetup -getrestartfreeze 2>&1 | tail -1)"
	expect "vmnet DHCP lease" "$(plutil -extract bootpd.DHCPLeaseTimeSecs raw -o - /Library/Preferences/SystemConfiguration/com.apple.InternetSharing.default.plist 2>/dev/null)" 600
	expect "state dir" "$(stat -f '%Sp %Su' /var/db/cucina 2>/dev/null)" "drwx------ root"
	expect "log dir" "$(stat -f '%Sp %Su:%Sg' /Library/Logs/Cucina 2>/dev/null)" "drwxr-x--- root:admin"
	if codesign --verify --deep --strict /usr/local/cucina/tart.app 2>/dev/null &&
		codesign -dvv /usr/local/cucina/tart.app 2>&1 | grep -qx "TeamIdentifier=$TEAM"; then
		pass "tart.app signature intact (Cirrus Labs)"
	else fail "tart.app signature"; fi
	expect "tart --version" "$(/usr/local/cucina/bin/tart --version 2>/dev/null)" 2.40.1
	if codesign --verify --strict /usr/local/cucina/bin/cucina-hostd 2>/dev/null; then pass "hostd signature verifies"; else fail "hostd signature"; fi
	info "postinstall warnings: $(grep -c WARNING /Library/Logs/Cucina/install.log 2>/dev/null)"
	;;
after-reboot)
	expect "console user after restart (auto-login)" "$(console)" cucina
	expect "daemon state" "$(daemon_state)" running
	if keychain_probe; then pass "cucina login keychain unlocked (no UI needed)"; else fail "cucina login keychain locked"; fi
	expect "FileVault" "$(fdesetup status | head -1)" "FileVault is Off."
	;;
no-login)
	expect "console user (nobody logged in)" "$(console)" root
	expect "daemon running without any login" "$(daemon_state)" running
	;;
mark)
	# Sidecar markers never replace hostd's state or keys. Snapshot the real key WITHOUT modifying it.
	umask 077
	mkdir -p /var/root/.config/cucina/t14 /var/db/cucina/hostd /var/db/cucina/l2 /Users/cucina/.tart/vms/t14-vm
	echo t14 >/var/db/cucina/hostd/t14-marker
	echo t14 >/var/db/cucina/l2/t14-marker
	echo t14 >/Users/cucina/.tart/vms/t14-vm/t14-marker
	chown cucina /Users/cucina/.tart /Users/cucina/.tart/vms /Users/cucina/.tart/vms/t14-vm /Users/cucina/.tart/vms/t14-vm/t14-marker
	if security find-generic-password -s ai.sloper.cucina.hostd -a host-identity-key /Library/Keychains/System.keychain >/dev/null 2>&1; then
		security find-generic-password -w -s ai.sloper.cucina.hostd -a host-identity-key /Library/Keychains/System.keychain 2>/dev/null |
			shasum -a 256 >/var/root/.config/cucina/t14/identity.sha256
		pass "sidecar markers and real identity fingerprint saved (key untouched)"
	else fail "hostd has not created its identity key; cannot prove upgrade preservation"; fi
	;;
after-upgrade)
	v=${2:?version}
	expect "receipt version" "$(pkgutil --pkg-info "$PKG_ID" 2>/dev/null | awk '/^version:/ { print $2 }')" "$v"
	expect "installed VERSION" "$(cat /usr/local/cucina/VERSION 2>/dev/null)" "$v"
	expect "daemon state" "$(daemon_state)" running
	expect "hostd state preserved" "$(cat /var/db/cucina/hostd/t14-marker 2>/dev/null)" t14
	expect "VM preserved" "$(cat /Users/cucina/.tart/vms/t14-vm/t14-marker 2>/dev/null)" t14
	expect "L2 cache preserved" "$(cat /var/db/cucina/l2/t14-marker 2>/dev/null)" t14
	if security find-generic-password -s ai.sloper.cucina.hostd -a host-identity-key /Library/Keychains/System.keychain >/dev/null 2>&1 &&
		security find-generic-password -w -s ai.sloper.cucina.hostd -a host-identity-key /Library/Keychains/System.keychain 2>/dev/null |
			shasum -a 256 | cmp -s /var/root/.config/cucina/t14/identity.sha256 -; then
		pass "real identity key unchanged"
	else fail "identity key lost or changed"; fi
	expect "auto-login user" "$(defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null)" cucina
	;;
after-uninstall)
	if pkgutil --pkg-info "$PKG_ID" >/dev/null 2>&1; then fail "receipt still present"; else pass "receipt forgotten"; fi
	if launchctl print "system/$LABEL" >/dev/null 2>&1; then fail "daemon still loaded"; else pass "daemon unloaded"; fi
	for p in "/Library/LaunchDaemons/$LABEL.plist" /usr/local/cucina /etc/newsyslog.d/ai.sloper.cucina.conf /var/db/cucina /Library/Logs/Cucina; do
		if [ -e "$p" ]; then fail "$p still present"; else pass "$p removed"; fi
	done
	for account in host-identity-key host-l2-ca-key; do
		if security find-generic-password -s ai.sloper.cucina.hostd -a "$account" /Library/Keychains/System.keychain >/dev/null 2>&1; then
			fail "$account still in the System keychain"
		else pass "$account removed"; fi
	done
	if dscl . -read /Users/cucina UniqueID >/dev/null 2>&1; then pass "user cucina kept (no --purge)"; else fail "user cucina unexpectedly removed"; fi
	if [ -e /Users/cucina/.tart/vms/t14-vm/t14-marker ]; then pass "VMs kept (no --purge)"; else fail "VM marker unexpectedly removed"; fi
	;;
*)
	echo "usage: t14-guest-checks.sh after-install V|after-reboot|no-login|mark|after-upgrade V|after-uninstall" >&2
	exit 2
	;;
esac
[ "$fails" = 0 ]
