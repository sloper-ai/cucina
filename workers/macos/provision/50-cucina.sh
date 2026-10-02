#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Cucina payload of the worker image (docs/dev/hostd.md §1.1, §1.3):
#   /usr/local/cucina/bin/{bb_worker,bb_runner,cucina-worker-agent}   root:wheel 0755 (SHA-256 re-verified here)
#   /usr/local/cucina/launchd/ai.sloper.cucina.bb-{worker,runner}.plist (bootstrapped by hostd, never at boot)
#   /usr/local/cucina/libexec/{cucina-smoke,cucina-render}            smoke test / in-VM render call site
#   /usr/local/cucina/image.json + /etc/cucina/image-version           version label (pool generation)
#   /etc/cucina/{bb,pki}, /var/log/cucina, /etc/newsyslog.d/cucina.conf
# Nothing secret is installed: hostd pushes keys, certificates and rendered configs at every VM start.
# Runs as root.
set -euo pipefail

: "${CUCINA_STAGE:?}" "${BUILD_USER:?}" "${BUILD_UID:?}" "${WORKER_USER:?}" "${DATA_VOLUME:?}" "${IMAGE_VERSION:?}" "${CUCINA_VERSION:?}"
: "${XCODE_VERSION:?}" "${BB_RELEASE:?}" "${BB_WORKER_SHA256:?}" "${BB_RUNNER_SHA256:?}" "${BASE_IMAGE:?}"
WORKER_AGENT_SHA256="${WORKER_AGENT_SHA256:-}"

log() { printf '[50-cucina] %s\n' "$*"; }
die() { printf '[50-cucina] ERROR: %s\n' "$*" >&2; exit 1; }

stage="$CUCINA_STAGE"
prefix=/usr/local/cucina

check_sha() { # file expected
  local got
  got="$(shasum -a 256 "$1" | awk '{print $1}')"
  [ "$got" = "$2" ] || die "$1: sha256 $got, want $2"
}

install -d -o root -g wheel -m 0755 "$prefix" "$prefix/bin" "$prefix/launchd" "$prefix/libexec"

check_sha "$stage/bin/bb_worker.darwin_arm64" "$BB_WORKER_SHA256"
check_sha "$stage/bin/bb_runner.darwin_arm64" "$BB_RUNNER_SHA256"
install -o root -g wheel -m 0755 "$stage/bin/bb_worker.darwin_arm64" "$prefix/bin/bb_worker"
install -o root -g wheel -m 0755 "$stage/bin/bb_runner.darwin_arm64" "$prefix/bin/bb_runner"
agent_state="absent"
if [ -f "$stage/bin/cucina-worker-agent" ]; then
  [ -z "$WORKER_AGENT_SHA256" ] || check_sha "$stage/bin/cucina-worker-agent" "$WORKER_AGENT_SHA256"
  install -o root -g wheel -m 0755 "$stage/bin/cucina-worker-agent" "$prefix/bin/cucina-worker-agent"
  agent_state="present"
else
  log "warning: cucina-worker-agent not provided; the in-VM render call site is unavailable in this image"
fi
xattr -c "$prefix"/bin/* 2>/dev/null || true

for plist in ai.sloper.cucina.bb-worker.plist ai.sloper.cucina.bb-runner.plist; do
  sed "s/@BUILD_USER@/$BUILD_USER/g" "$stage/files/launchd/$plist" >"$prefix/launchd/$plist"
  chown root:wheel "$prefix/launchd/$plist"
  chmod 0644 "$prefix/launchd/$plist"
  plutil -lint "$prefix/launchd/$plist" >/dev/null
done
if [ "$WORKER_USER" = root ]; then # image.json workerUser=root: the daemon runs as root (NFSv4 mounts, key isolation)
  /usr/libexec/PlistBuddy -c 'Delete :UserName' -c 'Delete :GroupName' "$prefix/launchd/ai.sloper.cucina.bb-worker.plist"
elif [ "$WORKER_USER" != "$BUILD_USER" ]; then
  /usr/libexec/PlistBuddy -c "Set :UserName $WORKER_USER" "$prefix/launchd/ai.sloper.cucina.bb-worker.plist"
fi
install -o root -g wheel -m 0755 "$stage/files/cucina-smoke" "$prefix/libexec/cucina-smoke"
install -o root -g wheel -m 0755 "$stage/files/cucina-render" "$prefix/libexec/cucina-render"

# Config, PKI and log directories (hostd writes their contents at every start).
install -d -o root -g wheel -m 0755 /private/etc/cucina /private/etc/cucina/bb
if [ "$WORKER_USER" = root ]; then
  install -d -o root -g wheel -m 0700 /private/etc/cucina/pki
else
  install -d -o "$WORKER_USER" -g staff -m 0700 /private/etc/cucina/pki
fi
install -d -o "$BUILD_USER" -g staff -m 0755 /private/var/log/cucina
for f in bb_worker.log bb_runner.log; do
  install -o "$BUILD_USER" -g staff -m 0644 /dev/null "/private/var/log/cucina/$f"
done
sed "s/@BUILD_USER@/$BUILD_USER/g" "$stage/files/newsyslog-cucina.conf" >/private/etc/newsyslog.d/cucina.conf
chmod 0644 /private/etc/newsyslog.d/cucina.conf

# Version label and machine facts. hostd reads image.json (schema 1; unknown fields are ignored).
xfacts="$stage/facts/xcode.json"
xf() { plutil -extract "$1" raw -o - "$xfacts"; }
built="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
mount="/Volumes/$DATA_VOLUME"
cat >"$prefix/image.json" <<EOF
{
  "schema": 1,
  "name": "cucina-worker-macos",
  "imageVersion": "$IMAGE_VERSION",
  "cucinaVersion": "$CUCINA_VERSION",
  "built": "$built",
  "base": "$BASE_IMAGE",
  "macos": "$(sw_vers -productVersion)",
  "macosBuild": "$(sw_vers -buildVersion)",
  "xcode": {
    "version": "$XCODE_VERSION",
    "fullVersion": "$(xf version)",
    "build": "$(xf build)",
    "path": "$(xf path)",
    "developerDir": "$(xf developerDir)",
    "sdkPath": "$(xf sdkPath)",
    "sdkVersion": "$(xf sdkVersion)",
    "clang": "$(xf clang)",
    "xcodeVersionOverride": "$(xf xcodeVersionOverride)"
  },
  "buildbarn": "$BB_RELEASE",
  "workerAgent": "$agent_state",
  "buildUser": "$BUILD_USER",
  "buildUid": $BUILD_UID,
  "workerUser": "$WORKER_USER",
  "dataVolume": { "name": "$DATA_VOLUME", "mountPoint": "$mount", "format": "APFSX" },
  "paths": {
    "bin": "$prefix/bin",
    "launchd": "$prefix/launchd",
    "config": "/etc/cucina",
    "bbConfig": "/etc/cucina/bb",
    "pki": "/etc/cucina/pki",
    "logs": "/var/log/cucina",
    "run": "/var/run/cucina",
    "runnerSocket": "/var/run/cucina/runner.sock",
    "build": "$mount/build",
    "cache": "$mount/cache",
    "tmp": "$mount/tmp",
    "state": "$mount/state"
  },
  "launchd": {
    "worker": { "label": "ai.sloper.cucina.bb-worker", "domain": "system", "plist": "$prefix/launchd/ai.sloper.cucina.bb-worker.plist" },
    "runner": { "label": "ai.sloper.cucina.bb-runner", "domain": "gui/$BUILD_UID", "plist": "$prefix/launchd/ai.sloper.cucina.bb-runner.plist" }
  },
  "runners": [
    { "name": "xcode", "properties": { "OSFamily": "macos", "ISA": "arm-a64", "xcode-version": "$XCODE_VERSION" } },
    { "name": "generic", "properties": { "OSFamily": "macos", "ISA": "arm-a64" } }
  ]
}
EOF
plutil -convert xml1 -o /dev/null "$prefix/image.json" || die "image.json is not valid JSON"
chown root:wheel "$prefix/image.json"
chmod 0644 "$prefix/image.json"
printf '%s\n' "$IMAGE_VERSION" >/private/etc/cucina/image-version
chmod 0644 /private/etc/cucina/image-version

# Smoke-test the binaries (usage output only; nothing is configured yet).
for bin in bb_worker bb_runner; do
  out="$("$prefix/bin/$bin" 2>&1 || true)"
  case "$out" in *"Usage: $bin "*) ;; *) die "$bin does not run: $out" ;; esac
done
log "installed image $IMAGE_VERSION (Buildbarn $BB_RELEASE, worker agent $agent_state)"
