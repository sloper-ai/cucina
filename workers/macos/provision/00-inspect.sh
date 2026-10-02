#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Record what the base image ships (build log only; nothing is changed). Runs as root.
set -euo pipefail

log() { printf '[00-inspect] %s\n' "$*"; }

log "base image: ${BASE_IMAGE:-?}"
log "macOS $(sw_vers -productVersion) ($(sw_vers -buildVersion)), $(sysctl -n hw.ncpu) vCPUs, $(($(sysctl -n hw.memsize) / 1073741824)) GiB"
for app in /Applications/Xcode*.app; do
  [ -e "$app" ] || continue
  if [ -L "$app" ]; then
    log "Xcode bundle: $app -> $(readlink "$app")"
  else
    log "Xcode bundle: $app $(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist" 2>/dev/null) ($(/usr/libexec/PlistBuddy -c 'Print :ProductBuildVersion' "$app/Contents/version.plist" 2>/dev/null))"
  fi
done
log "xcode-select: $(xcode-select -p 2>/dev/null || echo none)"
[ -d /Library/Developer/CommandLineTools ] && log "Command Line Tools present"
log "simulator runtimes: $(find /Library/Developer/CoreSimulator/Volumes -maxdepth 1 -mindepth 1 2>/dev/null | wc -l | tr -d ' ')"
log "users: $(dscl . -list /Users UniqueID | awk '$2 >= 500 {printf "%s(%s) ", $1, $2}')"
log "auto-login: $(defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null || echo none)"
log "root volume: $(df -h / | awk 'NR == 2 {print $2 " size, " $4 " free"}')"
for item in /Library/LaunchDaemons/*.plist /Library/LaunchAgents/*.plist; do
  [ -e "$item" ] && log "launchd: $item"
done
log "application firewall: $(/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate 2>/dev/null | tr -d '\n')"
log "SIP: $(csrutil status 2>/dev/null | tr -d '\n')"
if [ -d /Library/Apple/usr/libexec/oah ]; then log "Rosetta is inherited from the base; provisioning removes it (no x86_64 runners)"; fi
