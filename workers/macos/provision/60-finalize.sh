#!/bin/bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Close the build-time access path and leave a clean image (runs as root, after the reboot):
# * SSH: no password or keyboard-interactive logins (sshd_config.d drop-in sorting first). `tart exec` is the
#   management path; a host may still install a per-VM public key.
# * The Cirrus `admin` account's published password is replaced by a random value that is never printed, so neither
#   SSH nor Screen Sharing (disabled anyway) nor the login window accepts the default credentials. The build-time
#   window in which admin/admin works is documented in docs/operations/macos-images.md.
# * Staging files, shell histories and logs written during the build are removed.
set -euo pipefail

: "${CUCINA_STAGE:?}" "${BUILD_USER:?}"
log() { printf '[60-finalize] %s\n' "$*"; }

# Finish the new user's first automatic login before rotating the build-time admin credential.
# SSH/RPC is available earlier than loginwindow. Complete first-login work in the image bake, not a worker cold start.
uid="$(id -u "$BUILD_USER")"
deadline=$((SECONDS + 120))
until [ "$(stat -f %Su /dev/console)" = "$BUILD_USER" ] && launchctl print "gui/$uid" >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || { log "build-user GUI login did not complete" >&2; exit 1; }
  sleep 1
done

install -o root -g wheel -m 0644 "$CUCINA_STAGE/files/sshd-cucina.conf" /private/etc/ssh/sshd_config.d/000-cucina.conf
sshd -t
sshd -T 2>/dev/null | grep -qi '^passwordauthentication no' || { echo "sshd still accepts passwords" >&2; exit 1; }

new_pw="$(openssl rand -hex 24)"
if dscl . -passwd /Users/admin "${SSH_PASSWORD_OLD:-admin}" "$new_pw" 2>/dev/null; then
  log "admin password rotated to an unrecorded random value"
else
  sysadminctl -resetPasswordFor admin -newPassword "$new_pw" 2>&1 | grep -v -i password || true
  log "admin password reset to an unrecorded random value"
fi
unset new_pw
if dscl . -authonly admin "${SSH_PASSWORD_OLD:-admin}" >/dev/null 2>&1; then
  echo "admin still accepts the published default password" >&2
  exit 1
fi

rm -rf "$CUCINA_STAGE"
rm -f /Users/admin/.zsh_history /Users/admin/.bash_history /var/root/.zsh_history /var/root/.bash_history
rm -rf /Users/admin/.zsh_sessions /var/root/.zsh_sessions
: >/private/var/log/cucina/bb_worker.log
: >/private/var/log/cucina/bb_runner.log
# Fresh logs for the first worker boot; the build's install logs stay in /var/log/install.log as usual.
rm -f /private/var/log/tart-guest-daemon.log /private/tmp/tart-guest-agent.log /private/tmp/tart-guest-daemon.log
log "build-time access closed"
