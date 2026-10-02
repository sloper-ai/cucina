#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Case-sensitive APFS data volume for build directories and the persistent L1 (R-MAC-4, R-CACHE-2/-3).
# Native build directories on the default case-insensitive APFS hit dyld/case bugs (bb-remote-execution #123, #134).
# The volume lives in the boot APFS container, so it shares (and grows with) the VM disk: `tart set --disk-size` plus
# the guest agent's --resize-disk claim new space at boot. No quota: L1 (default 40 GiB, rendered by hostd) and build
# directories draw from the container's free space. Layout per docs/dev/hostd.md §1.1. Runs as root.
set -euo pipefail

: "${BUILD_USER:?BUILD_USER is required}"
WORKER_USER="${WORKER_USER:-$BUILD_USER}"
: "${DATA_VOLUME:?DATA_VOLUME is required}"
MOUNT="/Volumes/$DATA_VOLUME"

log() { printf '[40-data-volume] %s\n' "$*"; }
die() { printf '[40-data-volume] ERROR: %s\n' "$*" >&2; exit 1; }

container="$(diskutil info -plist / | plutil -extract APFSContainerReference raw -o - -)"
[ -n "$container" ] || die "cannot find the boot APFS container"
if [ -d "$MOUNT" ] && diskutil info "$MOUNT" >/dev/null 2>&1; then
  die "$MOUNT already exists in the base image"
fi
log "adding case-sensitive APFS volume '$DATA_VOLUME' to $container"
diskutil apfs addVolume "$container" APFSX "$DATA_VOLUME" >/dev/null
diskutil info "$MOUNT" >/dev/null || die "volume did not mount at $MOUNT"
# Non-boot APFS volumes ignore ownership by default; bb_worker/bb_runner rely on real owners and modes.
diskutil enableOwnership "$MOUNT" >/dev/null

info="$(diskutil info -plist "$MOUNT")"
personality="$(printf '%s' "$info" | plutil -extract FilesystemUserVisibleName raw -o - -)"
case "$personality" in *[Cc]ase-sensitive*) ;; *) die "volume personality is '$personality', want Case-sensitive APFS" ;; esac

# No Spotlight, no FSEvents journal: build trees churn millions of files.
mdutil -i off "$MOUNT" >/dev/null 2>&1 || true
touch "$MOUNT/.metadata_never_index"
install -d -o root -g wheel -m 0700 "$MOUNT/.fseventsd"
touch "$MOUNT/.fseventsd/no_log"

chown "$BUILD_USER":staff "$MOUNT"
chmod 0755 "$MOUNT"
# build/ tmp/ (build user) and cache/: hostd contract (docs/dev/hostd.md §1.1). state/ (0700): suggested StateRoot (L1
# blocks + persistent state, file pool, NFSv4 socket) so the persistent L1 never sits under a directory that
# bb_worker wipes at startup (the native build directory's cache).
for dir in build tmp; do
  install -d -o "$BUILD_USER" -g staff -m 0755 "$MOUNT/$dir"
done
# cache/ (native input cache, wiped by bb_worker at start) and state/ belong to the user that runs bb_worker; with
# workerUser=root actions cannot reach the L1, file pool or worker key (ADR 0350).
if [ "$WORKER_USER" = root ]; then
  install -d -o root -g wheel -m 0700 "$MOUNT/cache" "$MOUNT/state"
else
  install -d -o "$WORKER_USER" -g staff -m 0755 "$MOUNT/cache"
  install -d -o "$WORKER_USER" -g staff -m 0700 "$MOUNT/state"
fi

# Functional check: two names differing only in case are two files.
probe="$MOUNT/tmp/.case-probe-$$"
mkdir "$probe"
touch "$probe/a" "$probe/A"
[ "$(find "$probe" -type f | wc -l | tr -d ' ')" = "2" ] || die "volume is not case-sensitive"
rm -rf "$probe"

uuid="$(printf '%s' "$info" | plutil -extract VolumeUUID raw -o - -)"
log "volume $DATA_VOLUME ($uuid, $personality) mounted at $MOUNT, owners enabled, Spotlight off"
