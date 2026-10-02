#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Bring the temporary acceptance environment up (PROMPT.md §10.1).
#
#   up.sh [--plan-only] [--yes] base|env|all [-- <extra tofu plan args>]
#
# base: dual-stack single-AZ VPC, security groups, IAM, ECR   (costs ~$0 while idle)
# env : k3s node (the only EIP), linux-client, windows-client (needs the base layer applied)
# all : base, then env
#
# Writes ~/.config/cucina/aws-e2e/{base,env}-outputs.json and, after env, values-endpoints.json
# (the chart's `endpoints` block: Elastic IP, fixed private IP and the SANs; all 0600). OpenTofu state lives in
# ~/.config/cucina/aws-e2e/<layer>/terraform.tfstate, never in the repository.
#
# Inputs (environment):
#   CUCINA_ADMIN_CIDRS          comma-separated admin CIDRs (default: this machine's public IP /32)
#   CUCINA_WINDOWS_CLIENT_AMI   windows-base AMI id (env layer). Default: "windows-base".ami_id from
#                               $CUCINA_SECRETS_DIR/aws-e2e/amis.json (written by the image builds);
#                               none found = no Windows client yet
#   $CUCINA_SECRETS_DIR/aws-e2e/env.tfvars.json   optional extra variables for the env layer
#
# A plan that would destroy or replace anything is refused unless --yes is given.

set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

usage() {
  e2e_usage "$0"
  exit "${1:-0}"
}

plan_only=0
assume_yes=0
target=""
while [ $# -gt 0 ]; do
  case "$1" in
    --plan-only) plan_only=1 ;;
    --yes) assume_yes=1 ;;
    -h | --help) usage 0 ;;
    base | env | all)
      [ -z "$target" ] || usage 1
      target=$1
      ;;
    --)
      shift
      break
      ;;
    *) usage 1 ;;
  esac
  shift
done
[ -n "$target" ] || usage 1
# Remaining arguments (after --) are passed to `tofu plan` verbatim.

e2e_require tofu aws jq curl
e2e_setup
e2e_session_check

apply_layer() {
  layer=$1
  shift
  plan_file="$E2E_STATE_DIR/$layer.tfplan"
  mkdir -p "$E2E_TF_DATA_ROOT/$layer" "$E2E_STATE_DIR/$layer"

  e2e_log "== $layer: init"
  e2e_tofu "$layer" init -input=false -no-color >"$E2E_STATE_DIR/$layer.init.txt" 2>&1 || {
    tail -n 20 "$E2E_STATE_DIR/$layer.init.txt" >&2
    e2e_die "tofu init failed for $layer"
  }

  e2e_log "== $layer: plan"
  e2e_tofu "$layer" plan -input=false -no-color -out="$plan_file" "$@" >"$E2E_STATE_DIR/$layer.plan.txt" 2>&1 || {
    tail -n 30 "$E2E_STATE_DIR/$layer.plan.txt" >&2
    e2e_die "tofu plan failed for $layer (full log: $E2E_STATE_DIR/$layer.plan.txt)"
  }
  chmod 600 "$plan_file" # tofu writes plan files 0644 whatever the umask
  e2e_tofu "$layer" show -json -no-color "$plan_file" >"$E2E_STATE_DIR/$layer.plan.json"

  jq -r '[.resource_changes[]? | .change.actions | join("+")] | group_by(.) | map("\(.[0]) x\(length)") | join(", ") | if . == "" then "no changes" else . end' \
    "$E2E_STATE_DIR/$layer.plan.json" | sed "s/^/   plan: /" >&2

  destructive=$(jq -r '.resource_changes[]? | select(.change.actions | index("delete")) | .address' "$E2E_STATE_DIR/$layer.plan.json")
  if [ -n "$destructive" ] && [ "$assume_yes" != 1 ]; then
    e2e_log "The plan would destroy or replace:"
    printf '%s\n' "$destructive" | sed 's/^/   - /' >&2
    e2e_die "refusing to apply without --yes"
  fi

  if [ "$plan_only" = 1 ]; then
    e2e_log "== $layer: --plan-only, not applying (plan: $plan_file)"
    return 0
  fi

  e2e_log "== $layer: apply"
  e2e_tofu "$layer" apply -input=false -no-color "$plan_file" >"$E2E_STATE_DIR/$layer.apply.txt" 2>&1 || {
    tail -n 30 "$E2E_STATE_DIR/$layer.apply.txt" >&2
    e2e_die "tofu apply failed for $layer (full log: $E2E_STATE_DIR/$layer.apply.txt)"
  }
  grep -E '^(Apply complete|No changes)' "$E2E_STATE_DIR/$layer.apply.txt" | sed 's/^/   /' >&2 || true
  e2e_write_outputs "$layer"
  e2e_log "== $layer: outputs written to $E2E_STATE_DIR/$layer-outputs.json"
}

run_base() {
  admin_cidrs=$(e2e_admin_cidrs_json)
  # A VAR=value assignment would be visible in `ps`; the CIDR is not secret but stays out of argv.
  TF_VAR_admin_cidrs=$admin_cidrs
  export TF_VAR_admin_cidrs
  apply_layer base "$@"
}

run_env() {
  e2e_state_exists base || e2e_die "the base layer has no state yet: run '$0 base' first"
  windows_ami=${CUCINA_WINDOWS_CLIENT_AMI:-}
  # The image builds record their AMIs in amis.json (key windows-base); use it unless overridden.
  if [ -z "$windows_ami" ] && [ -s "$E2E_STATE_DIR/amis.json" ]; then
    windows_ami=$(jq -r '.["windows-base"].ami_id // empty' "$E2E_STATE_DIR/amis.json" 2>/dev/null || true)
    [ -z "$windows_ami" ] || e2e_log "   windows-client AMI taken from amis.json (windows-base)"
  fi
  if [ -n "$windows_ami" ]; then
    TF_VAR_windows_client_ami=$windows_ami
    export TF_VAR_windows_client_ami
  else
    e2e_log "   no windows-base AMI yet: the Windows client is skipped"
  fi
  if [ -s "$E2E_STATE_DIR/env.tfvars.json" ]; then
    apply_layer env -var-file="$E2E_STATE_DIR/env.tfvars.json" "$@"
  else
    apply_layer env "$@"
  fi
  if [ "$plan_only" != 1 ]; then
    # Helm accepts JSON values files: the endpoints block for the chart (public EIP, private IP, SANs).
    jq '{endpoints: .chart_endpoints}' "$E2E_STATE_DIR/env-outputs.json" >"$E2E_STATE_DIR/.values-endpoints.json" &&
      mv "$E2E_STATE_DIR/.values-endpoints.json" "$E2E_STATE_DIR/values-endpoints.json" &&
      e2e_log "== env: chart endpoint values written to $E2E_STATE_DIR/values-endpoints.json"
  fi
}

case "$target" in
  base) run_base "$@" ;;
  env) run_env "$@" ;;
  all)
    run_base "$@"
    if [ "$plan_only" = 1 ] && ! e2e_state_exists base; then
      e2e_log "== env: skipped (--plan-only and the base layer is not applied yet)"
    else
      run_env "$@"
    fi
    ;;
esac
