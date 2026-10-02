#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Guest system settings for a headless build worker: Spotlight off, no Software Update activity, no sleep, no Screen
# Sharing, quiet Bonjour, and the Tart Guest Agent's `tart exec` RPC moved into the root LaunchDaemon so it answers
# before the GUI login and runs commands as root (docs/dev/hostd.md §1.1 option B; docs/adr/0350-*).
# Runs as root.
set -euo pipefail

log() { printf '[20-system] %s\n' "$*"; }
die() { printf '[20-system] ERROR: %s\n' "$*" >&2; exit 1; }

# --- Spotlight (R-MAC-3): no indexing on any volume (the data volume repeats this in 40-data-volume.sh).
mdutil -a -i off >/dev/null

# --- Software Update: images are rebuilt instead (R-VER-3); no background checks or downloads in a worker.
softwareupdate --schedule off >/dev/null 2>&1 || true
for key in AutomaticCheckEnabled AutomaticDownload AutomaticallyInstallMacOSUpdates ConfigDataInstall CriticalUpdateInstall; do
  defaults write /Library/Preferences/com.apple.SoftwareUpdate "$key" -bool false
done
defaults write /Library/Preferences/com.apple.commerce AutoUpdate -bool false

# --- Power: never sleep; hostd decides when the VM stops (tart stop).
pmset -a sleep 0 displaysleep 0 disksleep 0 >/dev/null 2>&1 || true
defaults write /Library/Preferences/com.apple.screensaver loginWindowIdleTime 0
defaults write /Library/Preferences/com.apple.screensaver idleTime 0

# --- Screen Sharing (enabled in the Cirrus base, with the default credentials): management is `tart exec` only.
launchctl bootout system/com.apple.screensharing >/dev/null 2>&1 || true
launchctl disable system/com.apple.screensharing || log "warning: could not disable com.apple.screensharing"

# --- Networking on Tart's shared (NAT) vmnet: DHCP from the host, nothing to advertise on Bonjour.
defaults write /Library/Preferences/com.apple.mDNSResponder.plist NoMulticastAdvertisements -bool true
# No crash-reporter dialogs in a session nobody looks at.
defaults write /Library/Preferences/com.apple.CrashReporter DialogType -string none

# --- Tart Guest Agent: RPC (tart exec, tart ip --resolver=agent) in the root daemon, nothing in the GUI session.
daemon_plist=/Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist
agent_plist=/Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist
[ -f "$daemon_plist" ] || die "$daemon_plist missing: the base image must ship the Tart Guest Agent"
agent_bin="$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$daemon_plist")"
[ -x "$agent_bin" ] || die "guest agent binary $agent_bin is not executable"
/usr/libexec/PlistBuddy -c 'Delete :ProgramArguments' "$daemon_plist"
/usr/libexec/PlistBuddy \
  -c 'Add :ProgramArguments array' \
  -c "Add :ProgramArguments:0 string $agent_bin" \
  -c 'Add :ProgramArguments:1 string --run-daemon' \
  -c 'Add :ProgramArguments:2 string --run-rpc' \
  "$daemon_plist"
plutil -lint "$daemon_plist" >/dev/null
chown root:wheel "$daemon_plist"
chmod 0644 "$daemon_plist"
if [ -f "$agent_plist" ]; then
  log "removing the per-user guest agent (RPC moved to the daemon; clipboard sharing is not needed headless)"
  rm -f "$agent_plist"
fi
# The running per-user agent keeps serving until the reboot scheduled by the Packer template; the new daemon
# definition takes effect then.
log "guest agent: $agent_bin -> $(readlink "$agent_bin" 2>/dev/null || echo "$agent_bin") --run-daemon --run-rpc"

for item in /Library/LaunchDaemons/*.plist /Library/LaunchAgents/*.plist; do
  [ -e "$item" ] || continue
  log "third-party launchd item present: $item"
done
