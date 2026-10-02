#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Fetch the k3s kubeconfig from the e2e node through SSM Run Command (no SSH, no inbound
# port) and write it to ~/.config/cucina/aws-e2e/kubeconfig (0600) with the server address
# rewritten to the node's Elastic IP.
#
#   kubeconfig.sh [--timeout SECONDS]      (default 900: waits for SSM + the k3s bootstrap)
#
# The kubeconfig carries cluster-admin credentials: it never leaves ~/.config/cucina/ and
# is never printed. The credentials die with the cluster at teardown. Note that SSM stores
# the output of Run Command invocations (visible to principals that may call
# ssm:GetCommandInvocation in this account) - acceptable for a temporary, single-user
# account; see docs/operations/aws-e2e.md.

set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

timeout=900
while [ $# -gt 0 ]; do
  case "$1" in
    --timeout)
      timeout=${2:?--timeout needs a value}
      shift
      ;;
    -h | --help)
      e2e_usage "$0"
      exit 0
      ;;
    *) e2e_die "unknown argument: $1" ;;
  esac
  shift
done

e2e_require aws jq sed
e2e_setup
e2e_session_check
[ -s "$E2E_STATE_DIR/env-outputs.json" ] || e2e_die "no env outputs: apply the env layer first (up.sh env)"

instance_id=$(e2e_output env k3s_instance_id)
eip=$(e2e_output env k3s_public_ip)
[ -n "$instance_id" ] && [ -n "$eip" ] || e2e_die "env outputs lack k3s_instance_id / k3s_public_ip"
out="$E2E_STATE_DIR/kubeconfig"
tmp="$E2E_STATE_DIR/.kubeconfig.tmp"
deadline=$(($(date +%s) + timeout))

e2e_log "waiting for the SSM agent on the k3s node to come online"
while :; do
  ping=$(aws ssm describe-instance-information --filters "Key=InstanceIds,Values=$instance_id" --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null || true)
  [ "$ping" = "Online" ] && break
  [ "$(date +%s)" -lt "$deadline" ] || e2e_die "SSM agent not online within ${timeout}s (instance $instance_id)"
  sleep 10
done

e2e_log "waiting for the k3s bootstrap and fetching the kubeconfig"
while :; do
  cmd_id=$(aws ssm send-command --instance-ids "$instance_id" --document-name AWS-RunShellScript \
    --comment "cucina e2e kubeconfig" --timeout-seconds 60 \
    --parameters 'commands=["test -f /var/lib/cucina/bootstrap.done && cat /etc/rancher/k3s/k3s.yaml"]' \
    --query 'Command.CommandId' --output text) || e2e_die "ssm send-command failed"
  # `wait` exits non-zero when the command failed (bootstrap not done yet); that is not fatal.
  aws ssm wait command-executed --command-id "$cmd_id" --instance-id "$instance_id" >/dev/null 2>&1 || true
  status=$(aws ssm get-command-invocation --command-id "$cmd_id" --instance-id "$instance_id" --query 'Status' --output text 2>/dev/null || echo Unknown)
  if [ "$status" = "Success" ]; then
    aws ssm get-command-invocation --command-id "$cmd_id" --instance-id "$instance_id" --query 'StandardOutputContent' --output text >"$tmp"
    break
  fi
  [ "$(date +%s)" -lt "$deadline" ] || e2e_die "k3s bootstrap not finished within ${timeout}s (last status: $status); inspect /var/log/cucina-bootstrap.log over SSM"
  sleep 15
done

grep -q 'server: https://127.0.0.1:6443' "$tmp" || {
  rm -f "$tmp"
  e2e_die "unexpected kubeconfig content (no loopback server entry)"
}
# Point at the Elastic IP (the API server certificate carries it as a SAN) and give the
# cluster/user/context a recognisable name instead of k3s' generic "default".
sed -e "s#server: https://127.0.0.1:6443#server: https://$eip:6443#" \
  -e 's/^  name: default$/  name: cucina-e2e/' \
  -e 's/^    cluster: default$/    cluster: cucina-e2e/' \
  -e 's/^    user: default$/    user: cucina-e2e/' \
  -e 's/^- name: default$/- name: cucina-e2e/' \
  -e 's/^current-context: default$/current-context: cucina-e2e/' \
  "$tmp" >"$out"
rm -f "$tmp"
chmod 600 "$out"
e2e_log "kubeconfig written to $out (context cucina-e2e)"

if command -v kubectl >/dev/null 2>&1; then
  if kubectl --kubeconfig "$out" --request-timeout=20s get nodes >/dev/null 2>&1; then
    e2e_log "kubectl can reach the API server from this machine"
  else
    e2e_log "warning: kubectl could not reach the API server (admin CIDR changed? re-run up.sh base)"
  fi
fi
