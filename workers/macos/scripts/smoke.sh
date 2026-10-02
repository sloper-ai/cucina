#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Smoke test of a Cucina macOS worker VM, run INSIDE the guest as root:
#   tart exec <vm> /usr/bin/sudo -n -- /usr/local/cucina/libexec/cucina-smoke [--wait-session <seconds>] [--configured] [--json]
# Installed in the image as /usr/local/cucina/libexec/cucina-smoke (hostd may use it as a health check).
# Checks (R-MAC-3/4/7, R-VER-3): Xcode/SDK/clang at the fixed paths, exactly one Xcode, Buildbarn binaries runnable,
# case-sensitive data volume, Spotlight off, build user auto-logged-in, Tart Guest Agent RPC as root, the Buildbarn
# launchd jobs installed but not loaded (hostd bootstraps them after pushing config), no remote password logins.
# --configured: for a VM hostd has already configured; expects both Buildbarn jobs running instead of not loaded.
# Exit 0 = all checks passed. Output: one line per check, then a JSON summary (last line).
set -uo pipefail

wait_session=0
json_only=false
configured=false
while [ $# -gt 0 ]; do
  case "$1" in
    --wait-session) wait_session="$2"; shift 2 ;;
    --json) json_only=true; shift ;;
    --configured) configured=true; shift ;;
    *) echo "usage: cucina-smoke [--wait-session SECONDS] [--configured] [--json]" >&2; exit 2 ;;
  esac
done

IMAGE_JSON=/usr/local/cucina/image.json
passed=0
failed=0
warnings=0
failures=""

say() { $json_only || printf '%s\n' "$*"; }
ok() { passed=$((passed + 1)); say "ok   $1"; }
fail() { failed=$((failed + 1)); failures="$failures${failures:+; }$1: $2"; say "FAIL $1: $2"; }
warn() { warnings=$((warnings + 1)); say "warn $1: $2"; }
check() { # name, command...
  local name="$1"; shift
  local out
  if out="$("$@" 2>&1)"; then ok "$name"; else fail "$name" "$(printf '%s' "$out" | tail -n 1)"; fi
}
jget() { plutil -extract "$1" raw -o - "$IMAGE_JSON" 2>/dev/null; }

[ "$(id -u)" = "0" ] || { echo "cucina-smoke must run as root (tart exec <vm> sudo -n -- ...)" >&2; exit 2; }

# --- Image label.
if [ -f "$IMAGE_JSON" ] && [ "$(jget schema)" = "1" ]; then ok "image.json schema 1"; else fail "image.json" "missing or schema != 1"; fi
image_version="$(jget imageVersion)"
if [ -n "$image_version" ] && [ "$(cat /etc/cucina/image-version 2>/dev/null)" = "$image_version" ]; then
  ok "image version $image_version"
else
  fail "image version" "/etc/cucina/image-version does not match image.json"
fi
build_user="$(jget buildUser)"
build_uid="$(jget buildUid)"
xcode_build="$(jget xcode.build)"
developer_dir="$(jget xcode.developerDir)"
sdk_path="$(jget xcode.sdkPath)"
volume_mount="$(jget dataVolume.mountPoint)"

# --- Xcode / SDK / clang at fixed paths (R-MAC-7, R-XPLAT-8).
xv="$(xcodebuild -version 2>&1)"
if [ "$(printf '%s\n' "$xv" | awk '/^Build version/ {print $3}')" = "$xcode_build" ]; then
  ok "xcodebuild -version: $(printf '%s' "$xv" | tr '\n' ' ')"
else
  fail "xcodebuild -version" "$(printf '%s' "$xv" | tr '\n' ' ') (want build $xcode_build)"
fi
xcode_count="$(find /Applications -maxdepth 1 -name 'Xcode*.app' | wc -l | tr -d ' ')"
if [ "$xcode_count" = "1" ] && [ -d /Applications/Xcode.app ] && [ ! -L /Applications/Xcode.app ]; then
  ok "exactly one Xcode (/Applications/Xcode.app)"
else
  fail "exactly one Xcode" "found $xcode_count Xcode bundles"
fi
if [ ! -e /Library/Developer/CommandLineTools ]; then ok "no Command Line Tools"; else fail "no Command Line Tools" "/Library/Developer/CommandLineTools exists"; fi
if [ "$(xcode-select -p)" = "$developer_dir" ]; then ok "xcode-select -p = $developer_dir"; else fail "xcode-select" "$(xcode-select -p)"; fi
selected_sdk="$(xcrun --sdk macosx --show-sdk-path 2>/dev/null)"
if [ -d "$sdk_path/usr/include" ] && [ -d "$selected_sdk" ] &&
  [ "$(cd "$selected_sdk" && pwd -P)" = "$(cd "$sdk_path" && pwd -P)" ]; then
  ok "SDK $sdk_path ($(xcrun --sdk macosx --show-sdk-version))"
else
  fail "SDK path" "$(xcrun --sdk macosx --show-sdk-path 2>&1)"
fi
check "xcodebuild first launch done" xcodebuild -checkFirstLaunchStatus
if cv="$(/usr/bin/clang --version 2>&1)"; then ok "clang: $(printf '%s' "$cv" | head -n 1)"; else fail "clang --version" "$cv"; fi
probe_dir="$(mktemp -d)"
printf '#include <stdio.h>\nint main(void){puts("cucina");return 0;}\n' >"$probe_dir/p.c"
if /usr/bin/clang -o "$probe_dir/p" "$probe_dir/p.c" 2>"$probe_dir/err" && [ "$("$probe_dir/p")" = "cucina" ]; then
  ok "clang compiles and links against the SDK"
else
  fail "clang compile" "$(tail -n 1 "$probe_dir/err")"
fi
rm -rf "$probe_dir"

# --- Buildbarn binaries (usage output only).
for bin in bb_worker bb_runner; do
  path="/usr/local/cucina/bin/$bin"
  out="$("$path" 2>&1)"
  case "$out" in
    *"Usage: $bin "*) ok "$bin runnable ($(stat -f '%Su:%Sg %Lp' "$path"))" ;;
    *) fail "$bin" "${out:-missing}" ;;
  esac
done
if [ "$(jget workerAgent)" = "present" ]; then
  check "cucina-worker-agent runnable" /usr/local/cucina/bin/cucina-worker-agent version
else
  fail "cucina-worker-agent" "required binary is absent; no in-VM render call site"
fi

# --- Case-sensitive data volume.
vinfo="$(diskutil info -plist "$volume_mount" 2>/dev/null)"
personality="$(printf '%s' "$vinfo" | plutil -extract FilesystemUserVisibleName raw -o - - 2>/dev/null)"
case "$personality" in
  *[Cc]ase-sensitive*) ok "data volume $volume_mount: $personality" ;;
  *) fail "data volume" "$volume_mount not mounted or not case-sensitive (${personality:-none})" ;;
esac
if [ "$(printf '%s' "$vinfo" | plutil -extract GlobalPermissionsEnabled raw -o - - 2>/dev/null)" = "true" ]; then
  ok "data volume owners enabled"
else
  fail "data volume owners" "ownership is ignored on $volume_mount"
fi
worker_user="$(jget workerUser)"
worker_user="${worker_user:-$build_user}"
for d in "$volume_mount/build:$worker_user" "$volume_mount/tmp:$build_user" "$volume_mount/cache:$worker_user" "$(jget paths.state):$worker_user" "$volume_mount:root"; do
  dir="${d%%:*}" owner="${d#*:}"
  if [ "$(stat -f '%Su' "$dir" 2>/dev/null)" = "$owner" ]; then ok "$dir owned by $owner"; else fail "$dir" "missing or not owned by $owner"; fi
done
wplist_user="$(/usr/libexec/PlistBuddy -c 'Print :UserName' /usr/local/cucina/launchd/ai.sloper.cucina.bb-worker.plist 2>/dev/null || echo root)"
if [ "$wplist_user" = "$worker_user" ]; then ok "bb_worker runs as $worker_user (image.json workerUser)"; else fail "worker user" "plist UserName $wplist_user != workerUser $worker_user"; fi
cprobe="$volume_mount/tmp/.smoke-case-$$"
if mkdir "$cprobe" && touch "$cprobe/x" "$cprobe/X" && [ "$(find "$cprobe" -type f | wc -l | tr -d ' ')" = "2" ]; then
  ok "data volume is case-sensitive (x != X)"
else
  fail "case probe" "x and X collide on $volume_mount"
fi
rm -rf "$cprobe"
free_gib=$(($(df -k "$volume_mount" | awk 'NR == 2 {print $4}') / 1048576))
if [ "$free_gib" -ge 50 ]; then ok "free space ${free_gib} GiB (L1 default 40 GiB + build dirs)"; else fail "free space" "${free_gib} GiB < 50 GiB"; fi

# --- Spotlight off.
for vol in / "$volume_mount"; do
  if mdutil -s "$vol" 2>&1 | grep -qi 'disabled'; then ok "Spotlight off on $vol"; else fail "Spotlight $vol" "$(mdutil -s "$vol" 2>&1 | tail -n 1)"; fi
done

# --- Build user and its auto-login GUI session.
if id "$build_user" >/dev/null 2>&1 && [ "$(id -u "$build_user")" = "$build_uid" ]; then ok "build user $build_user ($build_uid)"; else fail "build user" "$build_user/$build_uid missing"; fi
if dseditgroup -o checkmember -m "$build_user" admin >/dev/null 2>&1; then fail "build user unprivileged" "$build_user is an administrator"; else ok "build user is not an administrator"; fi
# Guards R-SEC-5: actions must not gain root via sudo or replace root-owned service/config/log paths.
if sudo -n -u "$build_user" -- sudo -n /usr/bin/true >/dev/null 2>&1; then
  fail "build user sudo" "$build_user can run a root command without authentication"
else
  ok "build user cannot sudo to root"
fi
for protected in /var/log/cucina /etc/cucina/bb /usr/local/cucina/bin /usr/local/cucina/launchd; do
  if [ "$(stat -f %u "$protected" 2>/dev/null)" = 0 ] &&
    ! sudo -n -u "$build_user" -- /bin/test -w "$protected"; then
    ok "root-owned, action-unwritable $protected"
  else
    fail "protected directory" "$protected is not root-owned or is writable by $build_user"
  fi
done
if [ "$(stat -f %Su /var/log/cucina/bb_worker.log)" = "$worker_user" ] &&
  [ "$(stat -f %Su /var/log/cucina/bb_runner.log)" = "$build_user" ]; then
  ok "log files owned by their respective service users"
else
  fail "log owners" "worker/runner log ownership does not match the manifest"
fi
if [ "$worker_user" = root ]; then
  if ! sudo -n -u "$build_user" -- /bin/test -w /var/log/cucina/bb_worker.log &&
    ! sudo -n -u "$build_user" -- /bin/test -x /etc/cucina/pki &&
    ! sudo -n -u "$build_user" -- /bin/test -x "$(jget paths.state)"; then
    ok "actions cannot write worker log or traverse worker PKI/state"
  else
    fail "worker isolation" "builder has access to root worker log, PKI or state"
  fi
fi
if [ "$(defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser 2>/dev/null)" = "$build_user" ]; then ok "auto-login user $build_user"; else fail "auto-login" "not $build_user"; fi
deadline=$(($(date +%s) + wait_session))
while :; do
  console="$(stat -f '%Su' /dev/console)"
  if [ "$console" = "$build_user" ] && launchctl print "gui/$build_uid" >/dev/null 2>&1; then break; fi
  [ "$(date +%s)" -ge "$deadline" ] && break
  sleep 1
done
if [ "$console" = "$build_user" ]; then ok "GUI session of $build_user is up (gui/$build_uid)"; else fail "GUI session" "console owner is $console"; fi
# Guards a usable noninteractive GUI session, not the lifetime of Apple's Setup Assistant process.
# Add/delete only our unique, throwaway generic-password item; nothing authenticates with this value.
keychain="/Users/$build_user/Library/Keychains/login.keychain-db"
service="ai.sloper.cucina.image-smoke.$(openssl rand -hex 8)"
probe_secret="$(openssl rand -hex 16)"
user_security() {
  /usr/bin/perl -e 'alarm shift @ARGV; exec @ARGV or die $!' 15 \
    /bin/launchctl asuser "$build_uid" /usr/bin/sudo -H -u "$build_user" -- /usr/bin/security "$@"
}
if user_security add-generic-password -a image-smoke -s "$service" -w "$probe_secret" "$keychain" >/dev/null 2>&1; then
  if user_security delete-generic-password -a image-smoke -s "$service" "$keychain" >/dev/null 2>&1; then
    ok "GUI login keychain supports noninteractive add/delete"
  else
    fail "login keychain" "could not delete the smoke probe item"
  fi
else
  user_security delete-generic-password -a image-smoke -s "$service" "$keychain" >/dev/null 2>&1 || true
  fail "login keychain" "noninteractive write failed or timed out (keychain may be locked)"
fi
unset probe_secret
for notice in LICENSE.md THIRD_PARTY_NOTICES.md; do
  if [ -s "/usr/local/cucina/$notice" ]; then ok "bundled $notice"; else fail "license notice" "$notice is missing or empty"; fi
done

# --- Tart Guest Agent: this script runs through it; RPC must live in the root daemon.
gad="$(launchctl print system/org.cirruslabs.tart-guest-daemon 2>/dev/null)"
if printf '%s' "$gad" | grep -q 'state = running' && printf '%s' "$gad" | grep -q -- '--run-rpc'; then
  ok "Tart Guest Agent RPC in the root daemon"
else
  fail "guest agent" "org.cirruslabs.tart-guest-daemon not running with --run-rpc"
fi
if [ ! -e /Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist ]; then ok "no per-user guest agent"; else fail "guest agent" "per-user agent still installed"; fi

# --- Buildbarn launchd jobs: installed, valid, not loaded, not running (hostd bootstraps them).
for job in bb-worker bb-runner; do
  plist="/usr/local/cucina/launchd/ai.sloper.cucina.$job.plist"
  if plutil -lint "$plist" >/dev/null 2>&1; then ok "plist $plist"; else fail "plist $job" "missing or invalid"; fi
done
if $configured; then
  for job in system/ai.sloper.cucina.bb-worker "gui/$build_uid/ai.sloper.cucina.bb-runner"; do
    if launchctl print "$job" 2>/dev/null | grep -q 'state = running'; then ok "$job running"; else fail "$job" "not running"; fi
  done
else
  if launchctl print system/ai.sloper.cucina.bb-worker >/dev/null 2>&1; then fail "bb-worker not loaded" "loaded before hostd configured it"; else ok "ai.sloper.cucina.bb-worker not loaded at boot"; fi
  if launchctl print "gui/$build_uid/ai.sloper.cucina.bb-runner" >/dev/null 2>&1; then fail "bb-runner not loaded" "loaded before hostd configured it"; else ok "ai.sloper.cucina.bb-runner not loaded at boot"; fi
  if pgrep -x bb_worker >/dev/null || pgrep -x bb_runner >/dev/null; then fail "Buildbarn processes" "running before configuration"; else ok "no Buildbarn process running"; fi
  if [ -z "$(ls -A /etc/cucina/pki 2>/dev/null)" ]; then ok "no credentials in the image (/etc/cucina/pki empty)"; else fail "credentials" "/etc/cucina/pki is not empty"; fi
fi

# --- Hardening and background activity.
if launchctl print-disabled system 2>/dev/null | grep -q '"com.apple.screensharing" => disabled'; then ok "Screen Sharing disabled"; else fail "Screen Sharing" "not disabled"; fi
if sshd -T 2>/dev/null | grep -qi '^passwordauthentication no'; then ok "sshd: password authentication off"; else fail "sshd" "password authentication not disabled"; fi
# Golden Gate can remove legacy AutomaticCheckEnabled at reboot; the immutable-image policy is enforced by launchd.
if launchctl print-disabled system 2>/dev/null | grep -q '"com.apple.softwareupdated" => disabled' &&
  ! launchctl print system/com.apple.softwareupdated >/dev/null 2>&1; then
  ok "Software Update daemon disabled and unloaded"
else
  fail "Software Update" "updater is not disabled or remains loaded"
fi
rosetta_runtime=absent
rosetta_receipt=absent
[ ! -e /Library/Apple/usr/libexec/oah/runtime ] || rosetta_runtime=present
if pkgutil --pkg-info com.apple.pkg.RosettaUpdateAuto >/dev/null 2>&1; then rosetta_receipt=present; fi
if [ "$rosetta_runtime" = absent ] && [ "$rosetta_receipt" = absent ]; then
  ok "no Rosetta runtime or receipt"
else
  fail "Rosetta" "runtime=$rosetta_runtime, receipt=$rosetta_receipt"
fi
# hostd scrapes bb_worker's metrics port on the VM address: the application firewall must not block it.
if /usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate 2>/dev/null | grep -qi 'disabled'; then ok "application firewall off (vmnet NAT is host-only)"; else warn "application firewall" "enabled: hostd's metrics scrape needs bb_worker allowed"; fi

summary=$(printf '{"image":"%s","passed":%d,"failed":%d,"warnings":%d,"failures":"%s"}' "$image_version" "$passed" "$failed" "$warnings" "$(printf '%s' "$failures" | tr '"' "'")")
printf '%s\n' "$summary"
[ "$failed" -eq 0 ]
