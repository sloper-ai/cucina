#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Verify that this shell can drive the e2e environment (§12): the right profile, region and
# a live SSO session, the pinned tools, and that the vCPU quotas are visible. Read-only.
#
#   env-check.sh
#
# Exit codes: 0 all required checks passed, 1 a required check failed, 2 the AWS session
# is not valid (the user must run `aws sso login`).

set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

failures=0
ok() { printf '  ok    %s\n' "$*"; }
warn() { printf '  warn  %s\n' "$*"; }
bad() {
  printf '  FAIL  %s\n' "$*"
  failures=$((failures + 1))
}

e2e_setup
printf 'profile=%s region=%s run=%s expires=%s\n' "$AWS_PROFILE" "$AWS_REGION" "$E2E_RUN_ID" "$E2E_EXPIRES"

echo "tools"
for t in tofu aws jq curl; do
  if command -v "$t" >/dev/null 2>&1; then ok "$t found"; else bad "$t not found"; fi
done
if command -v tofu >/dev/null 2>&1; then
  v=$(tofu version -json 2>/dev/null | jq -r '.terraform_version' 2>/dev/null || true)
  if [ "$v" = "1.13.1" ]; then ok "tofu $v"; else bad "tofu is '$v', expected 1.13.1"; fi
fi
for t in tflint shellcheck session-manager-plugin; do
  if command -v "$t" >/dev/null 2>&1 && "$t" --version >/dev/null 2>&1; then ok "$t usable"; else warn "$t not usable (optional)"; fi
done

echo "session"
if aws sts get-caller-identity --query Account --output text >/dev/null 2>&1; then
  ok "caller identity resolves (SSO session is valid)"
else
  bad "AWS session is not valid: ask the user to run 'aws sso login --profile default'"
  printf '%s\n' "$failures failure(s)"
  exit 2
fi
configured_region=$(aws configure get region 2>/dev/null || true)
if [ -z "$configured_region" ] || [ "$configured_region" = "$E2E_REGION" ]; then
  ok "profile region ${configured_region:-unset} (calls are pinned to $E2E_REGION)"
else
  warn "profile region is $configured_region; this tooling still pins $E2E_REGION"
fi

echo "account state"
vpcs=$(aws ec2 describe-vpcs --filters Name=is-default,Values=true --query 'length(Vpcs)' --output text 2>/dev/null || echo "?")
if [ "$vpcs" = "0" ]; then ok "no default VPC (expected; the base layer creates a dedicated VPC)"; else warn "default VPCs in $E2E_REGION: $vpcs"; fi

echo "quotas (read-only, informational)"
quota() {
  q_code=$1
  q_label=$2
  q_val=$(aws service-quotas get-service-quota --service-code ec2 --quota-code "$q_code" --query 'Quota.Value' --output text 2>/dev/null || true)
  if [ -n "$q_val" ] && [ "$q_val" != "None" ]; then ok "$q_label = $q_val"; else warn "$q_label not visible"; fi
}
quota L-1216C47A "Running On-Demand Standard instances (vCPUs)"
quota L-34B43A08 "All Standard Spot Instance Requests (vCPUs)"

echo "layers"
for l in base env; do
  if e2e_state_exists "$l"; then
    ok "$l: state present"
  else
    warn "$l: no state yet"
  fi
  if [ -s "$E2E_STATE_DIR/$l-outputs.json" ]; then
    mode=$(stat -f '%Lp' "$E2E_STATE_DIR/$l-outputs.json" 2>/dev/null || stat -c '%a' "$E2E_STATE_DIR/$l-outputs.json" 2>/dev/null || echo "?")
    if [ "$mode" = "600" ]; then ok "$l-outputs.json present (0600)"; else bad "$l-outputs.json has mode $mode, expected 600"; fi
  fi
done

if [ "$failures" -gt 0 ]; then
  printf '%s\n' "$failures failure(s)"
  exit 1
fi
echo "all required checks passed"
