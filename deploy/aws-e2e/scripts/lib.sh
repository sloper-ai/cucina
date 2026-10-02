# SPDX-License-Identifier: FSL-1.1-ALv2
# shellcheck shell=sh
#
# Shared helpers for deploy/aws-e2e/scripts/*.sh (POSIX sh). Source it, do not execute it.
# The caller sets `here` to the scripts directory before sourcing:
#   here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
#   . "$here/lib.sh"
#
# Environment (all optional except the run tags, normally exported by the lead's env.sh):
#   CUCINA_SECRETS_DIR  default ~/.config/cucina   (0700; OpenTofu state, outputs, kubeconfig)
#   CUCINA_RUN_ID       value of the cucina:run tag
#   CUCINA_EXPIRES      value of the cucina:expires tag
#   AWS_PROFILE         must be "default" (§12) ; AWS_REGION must be us-west-1

umask 077

E2E_REGION="us-west-1"
# shellcheck disable=SC2154  # `here` is set by the sourcing script
E2E_ROOT=$(CDPATH='' cd -- "$here/.." && pwd)
E2E_REPO_ROOT=$(CDPATH='' cd -- "$here/../../.." && pwd)
E2E_SECRETS_DIR="${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}"
E2E_STATE_DIR="$E2E_SECRETS_DIR/aws-e2e"
E2E_TF_DATA_ROOT="${TF_DATA_DIR:-$E2E_SECRETS_DIR/tf-data}"

e2e_log() { printf '%s\n' "$*" >&2; }

# e2e_usage <script>: print the script's leading comment block (after the SPDX line).
e2e_usage() {
  awk 'NR >= 3 && /^#/ { sub(/^# ?/, ""); print; next } NR >= 3 { exit }' "$1"
}
e2e_die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

e2e_require() {
  for e2e_cmd in "$@"; do
    command -v "$e2e_cmd" >/dev/null 2>&1 || e2e_die "required command not found: $e2e_cmd"
  done
}

# e2e_setup: pin profile/region (§12), export the tag values, prepare directories.
# `cd` into the repo so the mise shims (tofu, tflint, ...) resolve through the repo's mise.toml
# whatever the caller's working directory is.
e2e_setup() {
  cd "$E2E_REPO_ROOT" || e2e_die "cannot cd to $E2E_REPO_ROOT"

  if [ -n "${AWS_PROFILE:-}" ] && [ "$AWS_PROFILE" != "default" ]; then
    e2e_die "AWS_PROFILE is '$AWS_PROFILE'; the campaign uses the 'default' profile only (PROMPT.md §12)"
  fi
  AWS_PROFILE=default
  for e2e_r in "${AWS_REGION:-$E2E_REGION}" "${AWS_DEFAULT_REGION:-$E2E_REGION}"; do
    [ "$e2e_r" = "$E2E_REGION" ] || e2e_die "region '$e2e_r' is not allowed; the campaign is restricted to $E2E_REGION"
  done
  AWS_REGION=$E2E_REGION
  AWS_DEFAULT_REGION=$E2E_REGION
  AWS_PAGER=""
  # IAM and tagging calls are throttled aggressively when listed item by item (sweep.sh).
  AWS_RETRY_MODE=standard
  AWS_MAX_ATTEMPTS=10
  export AWS_PROFILE AWS_REGION AWS_DEFAULT_REGION AWS_PAGER AWS_RETRY_MODE AWS_MAX_ATTEMPTS

  [ -n "${CUCINA_RUN_ID:-}" ] || e2e_die "CUCINA_RUN_ID is not set (source .work/env.sh)"
  [ -n "${CUCINA_EXPIRES:-}" ] || e2e_die "CUCINA_EXPIRES is not set (source .work/env.sh)"
  E2E_RUN_ID=$CUCINA_RUN_ID
  E2E_EXPIRES=$CUCINA_EXPIRES
  TF_VAR_run_id=$E2E_RUN_ID
  TF_VAR_expires=$E2E_EXPIRES
  TF_VAR_state_dir=$E2E_STATE_DIR
  export TF_VAR_run_id TF_VAR_expires TF_VAR_state_dir

  mkdir -p "$E2E_SECRETS_DIR" "$E2E_STATE_DIR" "$E2E_TF_DATA_ROOT"
  chmod 700 "$E2E_SECRETS_DIR" "$E2E_STATE_DIR" 2>/dev/null || true
}

# e2e_session_check: the SSO session must be valid. Never run `aws sso login` ourselves: the
# browser step belongs to the user (the lead asks for it).
e2e_session_check() {
  if ! aws sts get-caller-identity --query Account --output text >/dev/null 2>&1; then
    e2e_log "AWS session is not valid for profile '$AWS_PROFILE'."
    e2e_log "Stop here and ask the user to run: aws sso login --profile default"
    exit 2
  fi
}

# e2e_tofu <layer> <tofu args...>: run tofu against base|env with per-layer data dir.
e2e_tofu() {
  e2e_layer=$1
  shift
  TF_DATA_DIR="$E2E_TF_DATA_ROOT/$e2e_layer" tofu -chdir="$E2E_ROOT/$e2e_layer" "$@"
}

# e2e_state_exists <layer>: true when the layer's state file exists and is non-empty.
e2e_state_exists() {
  [ -s "$E2E_STATE_DIR/$1/terraform.tfstate" ]
}

# e2e_write_outputs <layer>: flatten `tofu output -json` into <layer>-outputs.json (0600).
e2e_write_outputs() {
  e2e_layer=$1
  e2e_tmp="$E2E_STATE_DIR/.$e2e_layer-outputs.json.tmp"
  e2e_tofu "$e2e_layer" output -json | jq 'map_values(.value)' >"$e2e_tmp" || e2e_die "could not read $e2e_layer outputs"
  chmod 600 "$e2e_tmp"
  mv "$e2e_tmp" "$E2E_STATE_DIR/$e2e_layer-outputs.json"
}

# e2e_output <layer> <key>: raw value from the saved outputs file.
e2e_output() {
  jq -r --arg k "$2" '.[$k] // empty' "$E2E_STATE_DIR/$1-outputs.json"
}

# e2e_admin_cidrs_json: JSON array of admin CIDRs. CUCINA_ADMIN_CIDRS (comma separated) wins;
# otherwise this machine's public IPv4 /32 (never committed, only passed to tofu at apply).
e2e_admin_cidrs_json() {
  if [ -n "${CUCINA_ADMIN_CIDRS:-}" ]; then
    printf '%s' "$CUCINA_ADMIN_CIDRS" | jq -Rc 'split(",") | map(gsub("^\\s+|\\s+$"; ""))'
    return
  fi
  e2e_ip=$(curl -fsS --max-time 10 https://checkip.amazonaws.com | tr -d '[:space:]') || e2e_die "could not determine the public IP (set CUCINA_ADMIN_CIDRS)"
  printf '%s' "$e2e_ip" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$' || e2e_die "checkip returned something that is not an IPv4 address"
  printf '["%s/32"]' "$e2e_ip"
}

# Tag filters shared by every inventory/destructive EC2 call (§12: filter by tag, always).
# usage: e2e_ec2_tag_filters [run-id]   (empty run id = environment tag only)
e2e_ec2_tag_filters() {
  printf 'Name=tag:cucina:env,Values=e2e'
  if [ -n "${1:-}" ]; then
    printf ' Name=tag:cucina:run,Values=%s' "$1"
  fi
}
