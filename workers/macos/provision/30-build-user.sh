#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# The build user (R-MAC-4, R-SEC-5): a dedicated *standard* account (no sudo, not in `admin`) that is logged in
# automatically to a GUI (Aqua) session. bb_runner runs as a LaunchAgent in that session and bb_worker as a
# LaunchDaemon with UserName=<build user> (docs/dev/hostd.md §1.3), so every action runs unprivileged.
# Its password is random, generated here and never printed; it exists only because loginwindow's auto-login needs one
# (/etc/kcpassword). Runs as root.
set -euo pipefail

: "${BUILD_USER:?BUILD_USER is required}"
: "${BUILD_UID:?BUILD_UID is required}"

log() { printf '[30-build-user] %s\n' "$*"; }
die() { printf '[30-build-user] ERROR: %s\n' "$*" >&2; exit 1; }

home="/Users/$BUILD_USER"
if id "$BUILD_USER" >/dev/null 2>&1; then
  die "user $BUILD_USER already exists in the base image"
fi
if dscl . -list /Users UniqueID | awk -v uid="$BUILD_UID" '$2 == uid {found=1} END {exit !found}'; then
  die "UID $BUILD_UID is taken in the base image"
fi

pw="$(openssl rand -hex 24)"
sysadminctl -addUser "$BUILD_USER" -fullName "Cucina build" -UID "$BUILD_UID" -shell /bin/zsh -home "$home" \
  -password "$pw" 2>&1 | grep -v -i password || true
id "$BUILD_USER" >/dev/null 2>&1 || die "sysadminctl did not create $BUILD_USER"
createhomedir -c -u "$BUILD_USER" >/dev/null
if dseditgroup -o checkmember -m "$BUILD_USER" admin >/dev/null 2>&1; then
  die "$BUILD_USER must not be an administrator"
fi

# Developer tools without authorization prompts (debugger, xctrace, simulators) for a non-admin user.
DevToolsSecurity -enable >/dev/null 2>&1 || true
dseditgroup -o edit -a "$BUILD_USER" -t user _developer

# Auto-login replaces the Cirrus `admin` auto-login.
sysadminctl -autologin set -userName "$BUILD_USER" -password "$pw" 2>&1 | grep -v -i password || true
unset pw
[ "$(defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null)" = "$BUILD_USER" ] ||
  die "auto-login was not configured for $BUILD_USER"
[ -f /etc/kcpassword ] || die "/etc/kcpassword missing after enabling auto-login"

# Skip the per-user Setup Assistant at the first (automatic) login.
prefs="$home/Library/Preferences"
install -d -o "$BUILD_USER" -g staff -m 0700 "$home/Library" "$prefs"
os_version="$(sw_vers -productVersion)"
os_build="$(sw_vers -buildVersion)"
sa="$prefs/com.apple.SetupAssistant"
for key in DidSeeAccessibility DidSeeActivationLock DidSeeAppearanceSetup DidSeeApplePaySetup DidSeeAvatarSetup \
  DidSeeCloudSetup DidSeeIntelligence DidSeeLockdownMode DidSeePrivacy DidSeeScreenTime DidSeeSiriSetup \
  DidSeeSyncSetup DidSeeSyncSetup2 DidSeeTermsOfAddress DidSeeTouchIDSetup DidSeeTrueTonePrivacy DidSeeWalletSetup \
  SkipFirstLoginOptimization; do
  defaults write "$sa" "$key" -bool true
done
defaults write "$sa" GestureMovieSeen -string none
defaults write "$sa" LastSeenCloudProductVersion -string "$os_version"
defaults write "$sa" LastSeenBuddyBuildVersion -string "$os_build"
defaults write "$sa" LastPreLoginTasksPerformedVersion -string "$os_version"
defaults write "$sa" LastPreLoginTasksPerformedBuild -string "$os_build"
chown -R "$BUILD_USER":staff "$home/Library"
chmod 0600 "$sa.plist"

log "created standard user $BUILD_USER (uid $BUILD_UID) with auto-login; first login happens at the next reboot"
