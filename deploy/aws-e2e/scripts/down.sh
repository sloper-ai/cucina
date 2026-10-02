#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Tear the temporary acceptance environment down (PROMPT.md §12 "Teardown always runs", T15).
#
#   down.sh [--force] [--delete-amis] [--keep-base] [--keep-files]
#
#   1. Refuses to proceed while tagged WORKER instances exist (cucina:env=e2e + cucina:run, not
#      cucina:protected): `helm uninstall` first, its finalizers terminate them. --force
#      terminates them here instead (tag-filtered) and waits for them to be gone.
#   2. tofu destroy: env layer, then base layer (--keep-base stops after env).
#   3. sweep.sh --report (or --delete-amis: disable Fast Launch, deregister the run's AMIs and
#      delete their snapshots first). The exit status is the sweep's: 0 only when nothing is left.
#
# On full success the generated local files (outputs, values, kubeconfig, plans) are removed from
# ~/.config/cucina/aws-e2e; --keep-files keeps them.

set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

force=0
delete_amis=0
keep_base=0
keep_files=0
while [ $# -gt 0 ]; do
  case "$1" in
    --force) force=1 ;;
    --delete-amis) delete_amis=1 ;;
    --keep-base) keep_base=1 ;;
    --keep-files) keep_files=1 ;;
    -h | --help)
      e2e_usage "$0"
      exit 0
      ;;
    *) e2e_die "unknown argument: $1" ;;
  esac
  shift
done

e2e_require tofu aws jq
e2e_setup
e2e_session_check

# --- 1. worker instances -----------------------------------------------------------------------
filters=$(e2e_ec2_tag_filters "$E2E_RUN_ID")
# shellcheck disable=SC2086
aws ec2 describe-instances --filters $filters --output json >"$E2E_STATE_DIR/.down-instances.json"
workers=$(jq -r '
  .Reservations[].Instances[]
  | select(.State.Name != "terminated")
  | select((.Tags // []) | any(.Key == "cucina:protected" and .Value == "true") | not)
  | .InstanceId' "$E2E_STATE_DIR/.down-instances.json")
rm -f "$E2E_STATE_DIR/.down-instances.json"

if [ -n "$workers" ]; then
  e2e_log "tagged worker instances exist:"
  printf '%s\n' "$workers" | sed 's/^/   /' >&2
  if [ "$force" != 1 ]; then
    e2e_die "refusing to tear down: run 'helm uninstall' (its finalizers terminate pool instances) or pass --force"
  fi
  e2e_log "--force: terminating them (tag-filtered selection above)"
  # shellcheck disable=SC2086
  aws ec2 terminate-instances --instance-ids $workers >/dev/null
  # shellcheck disable=SC2086
  aws ec2 wait instance-terminated --instance-ids $workers
fi

# --- 2. tofu destroy ---------------------------------------------------------------------------
destroy_layer() {
  layer=$1
  shift
  if ! e2e_state_exists "$layer"; then
    e2e_log "== $layer: no state, nothing to destroy"
    return 0
  fi
  mkdir -p "$E2E_TF_DATA_ROOT/$layer"
  e2e_tofu "$layer" init -input=false -no-color >/dev/null 2>&1 || e2e_die "tofu init failed for $layer"
  e2e_log "== $layer: destroy"
  e2e_tofu "$layer" destroy -input=false -no-color -auto-approve "$@" >"$E2E_STATE_DIR/$layer.destroy.txt" 2>&1 || {
    tail -n 30 "$E2E_STATE_DIR/$layer.destroy.txt" >&2
    e2e_die "tofu destroy failed for $layer (full log: $E2E_STATE_DIR/$layer.destroy.txt); fix and re-run"
  }
  grep -E '^Destroy complete' "$E2E_STATE_DIR/$layer.destroy.txt" | sed 's/^/   /' >&2 || true
}

env_args=""
if [ -s "$E2E_STATE_DIR/env.tfvars.json" ]; then
  env_args="-var-file=$E2E_STATE_DIR/env.tfvars.json"
fi
# shellcheck disable=SC2086
destroy_layer env $env_args
if [ "$keep_base" != 1 ]; then
  destroy_layer base
fi

# --- 3. sweep ----------------------------------------------------------------------------------
if [ "$delete_amis" = 1 ]; then
  sweep_mode=--delete-amis
else
  sweep_mode=--report
fi
rc=0
"$here/sweep.sh" "$sweep_mode" || rc=$?

if [ "$rc" -eq 0 ] && [ "$keep_base" != 1 ] && [ "$keep_files" != 1 ]; then
  rm -f "$E2E_STATE_DIR"/base-outputs.json "$E2E_STATE_DIR"/env-outputs.json "$E2E_STATE_DIR"/values-endpoints.json \
    "$E2E_STATE_DIR"/kubeconfig "$E2E_STATE_DIR"/*.tfplan "$E2E_STATE_DIR"/*.plan.json
  e2e_log "teardown complete; generated local files removed"
fi
exit "$rc"
