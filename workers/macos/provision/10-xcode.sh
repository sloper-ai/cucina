#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Exactly one Xcode, at the fixed path /Applications/Xcode.app (R-MAC-7, R-XPLAT-8).
#
# * Keeps the base image's Xcode when its build equals XCODE_BUILD, otherwise copies the operator's Xcode.app that
#   the Packer build shares read-only at XCODE_SRC (virtiofs, build time only; see docs/adr/0352-*).
# * Removes every other Xcode bundle and the Command Line Tools, so `xcrun`, `/usr/bin/clang` and the SDK path that
#   the cross toolchain pins (-isysroot) resolve to one toolchain only.
# * /Applications/Xcode.app is a real directory (not a symlink): clang and the linker realpath their own location,
#   and a symlinked bundle would leak the versioned path into .d files and action outputs.
# Runs as root (Packer execute_command uses sudo -n).
set -euo pipefail

: "${XCODE_VERSION:?XCODE_VERSION is required (e.g. 27.0)}"
: "${XCODE_BUILD:?XCODE_BUILD is required (e.g. 27A266a)}"
XCODE_SRC="${XCODE_SRC:-}"
FACTS_DIR="${CUCINA_STAGE:-/private/tmp/cucina-stage}/facts"
TARGET=/Applications/Xcode.app
DEVELOPER_DIR_FIXED="$TARGET/Contents/Developer"
SDK_FIXED="$DEVELOPER_DIR_FIXED/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk"

log() { printf '[10-xcode] %s\n' "$*"; }
die() { printf '[10-xcode] ERROR: %s\n' "$*" >&2; exit 1; }

plist_get() { /usr/libexec/PlistBuddy -c "Print :$2" "$1" 2>/dev/null || true; }
bundle_id() { plist_get "$1/Contents/Info.plist" CFBundleIdentifier; }
app_build() { plist_get "$1/Contents/version.plist" ProductBuildVersion; }
app_version() { plist_get "$1/Contents/Info.plist" CFBundleShortVersionString; }

# Every Xcode bundle in /Applications (any name: Xcode.app, Xcode_27.0.app, Xcode-beta.app, ...), including symlinks.
xcodes=()
for app in /Applications/*.app; do
  [ -e "$app" ] || [ -L "$app" ] || continue
  if [ -L "$app" ] || [ "$(bundle_id "$app")" = "com.apple.dt.Xcode" ]; then
    case "$(basename "$app")" in Xcode*) xcodes+=("$app") ;; esac
  fi
done
log "Xcode bundles in the base image: ${xcodes[*]:-none}"

keep=""
for app in ${xcodes[@]+"${xcodes[@]}"}; do
  [ -L "$app" ] && continue
  b="$(app_build "$app")"
  log "  $app: version $(app_version "$app") build ${b:-?}"
  if [ "$b" = "$XCODE_BUILD" ] && [ -z "$keep" ]; then keep="$app"; fi
done

copied=false
if [ -z "$keep" ]; then
  [ -n "$XCODE_SRC" ] || die "no Xcode with build $XCODE_BUILD in the base image and no XCODE_SRC shared (set XCODE_APP=/Applications/Xcode.app for make)"
  for _ in $(seq 1 60); do [ -d "$XCODE_SRC/Contents" ] && break; sleep 1; done # virtiofs automount
  [ -d "$XCODE_SRC/Contents" ] || die "XCODE_SRC $XCODE_SRC is not an Xcode bundle (is the --dir share mounted?)"
  src_build="$(app_build "$XCODE_SRC")"
  [ "$src_build" = "$XCODE_BUILD" ] || die "shared Xcode has build ${src_build:-?}, want $XCODE_BUILD"
  log "copying Xcode $XCODE_VERSION ($src_build) from the build share (about 10 GB)"
  rm -rf "$TARGET.cucina-tmp"
  started=$(date +%s)
  /usr/bin/ditto "$XCODE_SRC" "$TARGET.cucina-tmp"
  log "copy took $(( $(date +%s) - started )) s"
  chown -R root:wheel "$TARGET.cucina-tmp"
  /usr/bin/xattr -dr com.apple.quarantine "$TARGET.cucina-tmp" 2>/dev/null || true
  keep="$TARGET.cucina-tmp"
  copied=true
fi

# Remove every other Xcode (bundles and symlinks), then move the kept one to the fixed path.
for app in ${xcodes[@]+"${xcodes[@]}"}; do
  [ "$app" = "$keep" ] && continue
  log "removing $app"
  rm -rf "$app"
done
if [ "$keep" != "$TARGET" ]; then
  rm -rf "$TARGET"
  mv "$keep" "$TARGET"
fi

# One toolchain only: the Command Line Tools carry their own (differently versioned) SDK.
if [ -d /Library/Developer/CommandLineTools ]; then
  log "removing /Library/Developer/CommandLineTools"
  rm -rf /Library/Developer/CommandLineTools
  for pkg in $(pkgutil --pkgs | grep -E '^com\.apple\.pkg\.CLTools_' || true); do pkgutil --forget "$pkg" >/dev/null || true; done
fi

xcode-select -s "$DEVELOPER_DIR_FIXED"
xcodebuild -license accept
xcodebuild -runFirstLaunch

# Verify the result exactly.
ver_out="$(xcodebuild -version)"
got_build="$(printf '%s\n' "$ver_out" | awk '/^Build version/ {print $3}')"
got_version="$(printf '%s\n' "$ver_out" | awk '/^Xcode/ {print $2}')"
[ "$got_build" = "$XCODE_BUILD" ] || die "xcodebuild reports build $got_build, want $XCODE_BUILD"
case "$got_version" in "$XCODE_VERSION" | "$XCODE_VERSION".*) ;; *) die "xcodebuild reports $got_version, want $XCODE_VERSION" ;; esac
[ "$(xcode-select -p)" = "$DEVELOPER_DIR_FIXED" ] || die "xcode-select points at $(xcode-select -p)"
sdk="$(xcrun --sdk macosx --show-sdk-path)"
[ "$sdk" = "$SDK_FIXED" ] || die "SDK path is $sdk, want $SDK_FIXED"
sdk_version="$(xcrun --sdk macosx --show-sdk-version)"
clang_version="$(xcrun clang --version | head -n 1)"
remaining=$(find /Applications -maxdepth 1 -name 'Xcode*.app' | wc -l | tr -d ' ')
[ "$remaining" = "1" ] || die "expected exactly one Xcode in /Applications, found $remaining"

# Bazel's xcode_locator reports <major.minor.patch>.<build>; bb_runner's appleXcodeDeveloperDirectories is keyed by it.
IFS=. read -r v_major v_minor v_patch <<<"$got_version"
override_key="${v_major}.${v_minor:-0}.${v_patch:-0}.${got_build}"

mkdir -p "$FACTS_DIR"
cat >"$FACTS_DIR/xcode.json" <<EOF
{
  "version": "$got_version",
  "build": "$got_build",
  "path": "$TARGET",
  "developerDir": "$DEVELOPER_DIR_FIXED",
  "sdkPath": "$SDK_FIXED",
  "sdkVersion": "$sdk_version",
  "clang": "$clang_version",
  "xcodeVersionOverride": "$override_key",
  "copiedFromHost": $copied
}
EOF
log "Xcode $got_version ($got_build) at $TARGET, SDK $sdk_version, $clang_version"
