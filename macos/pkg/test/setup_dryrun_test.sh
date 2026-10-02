#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
# Guards R-MAC-8/R-MAC-2 (tier: integration, macOS, read-only): `cucina-host-setup install --dry-run` plans every host
# preparation step the package's postinstall performs (user + auto-login, pmset, restart after freeze, vmnet DHCP lease,
# state/log directories, LaunchDaemon bootstrap) and never echoes the generated password (installer output lands in
# /var/log/install.log). Dry-run changes nothing; it only reads system state.
set -eu
PATH=$PATH:/usr/bin:/bin:/usr/sbin:/sbin

if [ -n "${TEST_SRCDIR:-}" ]; then PKG_DIR=$TEST_SRCDIR/${TEST_WORKSPACE:-_main}/macos/pkg; else
	PKG_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
fi
fails=0
ok() { printf 'ok    %s\n' "$*"; }
bad() {
	printf 'FAIL  %s\n' "$*"
	fails=$((fails + 1))
}

out=$(sh "$PKG_DIR/payload/cucina-host-setup" install --dry-run 2>&1) || bad "dry-run exited non-zero"
has() { if printf '%s\n' "$out" | grep -qF -- "$2"; then ok "$1"; else bad "$1 (missing: $2)"; fi; }

has "state directory" "dry-run: mkdir -p /var/db/cucina/hostd"
has "log directory" "dry-run: chmod 0750 /Library/Logs/Cucina"
has "no sleep" "dry-run: pmset -a sleep 0"
has "wake on LAN" "dry-run: pmset -a womp 1"
has "restart after power loss" "dry-run: pmset -a autorestart 1"
has "restart after freeze" "dry-run: systemsetup -setrestartfreeze on"
has "vmnet DHCP lease 600 s" "bootpd -dict-add DHCPLeaseTimeSecs -int 600"
has "LaunchDaemon bootstrap" "launchctl bootstrap system /Library/LaunchDaemons/ai.sloper.cucina.hostd.plist"
if dscl . -read /Users/cucina UniqueID >/dev/null 2>&1; then
	ok "user cucina exists here: creation path not exercised"
else
	has "creates the Tart user" "dry-run: sysadminctl -addUser ... (arguments redacted)"
	has "enables auto-login" "dry-run: sysadminctl -autologin ... (arguments redacted)"
fi
if printf '%s\n' "$out" | grep -q -- '-password\|-newPassword'; then bad "a password argument was echoed"; else ok "no password argument echoed"; fi
if printf '%s\n' "$out" | grep -Eq '[A-Za-z0-9]{32}'; then bad "a 32-character token appears in the output"; else ok "no password-like string in the output"; fi

[ "$fails" = 0 ] || {
	echo "setup_dryrun_test: $fails failure(s)" >&2
	exit 1
}
echo "setup_dryrun_test: ok"
