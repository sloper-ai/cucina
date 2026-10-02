#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# The build user (R-MAC-4, R-SEC-5): a dedicated *standard* account (no sudo, not in `admin`) that is logged in
# automatically to a GUI (Aqua) session. bb_runner runs as a LaunchAgent in that session; bb_worker is a separate
# LaunchDaemon (docs/dev/hostd.md §1.3). Every action runs as this unprivileged user.
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

# Precreate the login keychain with the login password while the new user is logged out (ADR 0755).
# Otherwise first-login Setup Assistant may leave the account logged in with no usable unlocked keychain.
keychain="$home/Library/Keychains/login.keychain-db"
install -d -o "$BUILD_USER" -g staff -m 0700 "$home/Library/Keychains"
sudo -H -u "$BUILD_USER" /usr/bin/security create-keychain -p "$pw" "$keychain" >/dev/null
sudo -H -u "$BUILD_USER" /usr/bin/security default-keychain -d user -s "$keychain" >/dev/null
sudo -H -u "$BUILD_USER" /usr/bin/security set-keychain-settings "$keychain" >/dev/null

# Auto-login replaces the Cirrus `admin` auto-login.
# Golden Gate's SACSetAutoLoginPassword silently fails (error 22, exit 0) in SSH/system bootstrap context.
# Use the Cirrus admin's already-running GUI context; this is a supported sysadminctl call, not a file rewrite.
admin_uid="$(id -u "${BUILD_ADMIN:-admin}")"
deadline=$((SECONDS + 90))
until launchctl print "gui/$admin_uid" >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || die "base administrator GUI session is unavailable"
  sleep 1
done
login_output="$(launchctl asuser "$admin_uid" /usr/sbin/sysadminctl -autologin set -userName "$BUILD_USER" -password "$pw" 2>&1)"
printf '%s\n' "${login_output//$pw/[redacted]}"
dscl . -authonly "$BUILD_USER" "$pw" >/dev/null 2>&1 || die "generated build-user password does not authenticate"
# Compare just the encoded password + NUL (Apple may choose different padding); never print the credential.
# Reuse the pkg's tested codec, rather than maintaining another obfuscation implementation (ADR 0755).
encoder="${CUCINA_STAGE:?}/files/cucina-kcpassword"
length=$((${#pw} + 1))
cmp -s <(printf %s "$pw" | /usr/bin/perl "$encoder" | head -c "$length") <(head -c "$length" /etc/kcpassword) ||
  die "automatic login did not persist the new user's credential"
unset pw login_output
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
