# SPDX-License-Identifier: FSL-1.1-ALv2
# shellcheck shell=bash
#
# Helpers for short-lived test instances launched from Cucina AMIs (boot measurements, toolchain probes).
# Sourced by workers/{linux,windows}/scripts/*.sh. Every instance carries the campaign tags plus
# cucina:role=image-test, terminates on OS shutdown, and is terminated by ec2lib_terminate (tag-checked).
#
# Requires: aws, jq; CUCINA_RUN_ID / CUCINA_EXPIRES (source .work/env.sh).

EC2LIB_REGION=${AWS_REGION:-us-west-1}
EC2LIB_OUTPUTS=${OUTPUTS:-${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}/aws-e2e/base-outputs.json}

ec2lib_out() { jq -r --arg k "$1" '.[$k] // empty' "$EC2LIB_OUTPUTS"; }
ec2lib_now() { python3 -c 'import time; print(f"{time.time():.3f}")'; }
ec2lib_log() { printf '%s %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }

# ec2lib_launch AMI TYPE SUBNET SG PROFILE NAME [USER_DATA_FILE] [EXTRA_RUN_INSTANCES_ARGS...]
# Prints the instance ID. Callers take the launch timestamp themselves right before calling (the function runs
# in a command substitution, so it cannot export variables):  t0=$(ec2lib_now); iid=$(ec2lib_launch ...)
ec2lib_launch() {
  local ami=$1 type=$2 subnet=$3 sg=$4 profile=$5 name=$6 userdata=${7:-}
  shift 6
  [[ $# -gt 0 ]] && shift
  local tags="{Key=Name,Value=$name},{Key=cucina:env,Value=${CUCINA_ENV:-e2e}},{Key=cucina:run,Value=${CUCINA_RUN_ID:?}},{Key=cucina:expires,Value=${CUCINA_EXPIRES:?}},{Key=cucina:role,Value=image-test}"
  local args=(--region "$EC2LIB_REGION" --image-id "$ami" --instance-type "$type" --count 1
    --network-interfaces "DeviceIndex=0,SubnetId=$subnet,Groups=$sg,AssociatePublicIpAddress=${EC2LIB_PUBLIC_IP:-true},DeleteOnTermination=true"
    --metadata-options "HttpTokens=required,HttpEndpoint=enabled,HttpPutResponseHopLimit=1,InstanceMetadataTags=enabled"
    --instance-initiated-shutdown-behavior terminate
    --tag-specifications "ResourceType=instance,Tags=[$tags]" "ResourceType=volume,Tags=[$tags]" "ResourceType=network-interface,Tags=[$tags]"
    --query 'Instances[0].InstanceId' --output text)
  [[ -n "$profile" ]] && args+=(--iam-instance-profile "Name=$profile")
  [[ -n "$userdata" ]] && args+=(--user-data "file://$userdata")
  args+=("$@")
  aws ec2 run-instances "${args[@]}"
}

# ec2lib_wait_ssm IID T0 [TIMEOUT_S] -> prints seconds since T0 (epoch) when SSM reports Online.
ec2lib_wait_ssm() {
  local iid=$1 t0=$2 timeout=${3:-900} start ping
  start=$(date +%s)
  while (($(date +%s) - start < timeout)); do
    ping=$(aws ssm describe-instance-information --region "$EC2LIB_REGION" \
      --filters "Key=InstanceIds,Values=$iid" --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null || true)
    if [[ "$ping" == Online ]]; then
      python3 -c "import time; print(f'{time.time() - $t0:.1f}')"
      return 0
    fi
    sleep 1
  done
  return 1
}

# ec2lib_ssm_run IID DOCUMENT SCRIPT_FILE [TIMEOUT_S] -> prints stdout; returns 0 if the command succeeded.
# DOCUMENT is AWS-RunShellScript or AWS-RunPowerShellScript.
ec2lib_ssm_run() {
  local iid=$1 doc=$2 script=$3 timeout=${4:-600} input cid st start
  input=$(jq -n --arg i "$iid" --arg d "$doc" --rawfile s "$script" --arg t "$timeout" \
    '{InstanceIds: [$i], DocumentName: $d, TimeoutSeconds: 600, Parameters: {commands: [$s], executionTimeout: [$t]}}')
  cid=$(aws ssm send-command --region "$EC2LIB_REGION" --cli-input-json "$input" --query 'Command.CommandId' --output text)
  start=$(date +%s)
  while :; do
    sleep 3
    st=$(aws ssm get-command-invocation --region "$EC2LIB_REGION" --command-id "$cid" --instance-id "$iid" \
      --query 'Status' --output text 2>/dev/null || echo Pending)
    case "$st" in
      Pending | InProgress | Delayed) ;;
      *) break ;;
    esac
    if (($(date +%s) - start > timeout + 120)); then break; fi
  done
  aws ssm get-command-invocation --region "$EC2LIB_REGION" --command-id "$cid" --instance-id "$iid" \
    --query 'StandardOutputContent' --output text
  local err
  err=$(aws ssm get-command-invocation --region "$EC2LIB_REGION" --command-id "$cid" --instance-id "$iid" \
    --query 'StandardErrorContent' --output text)
  [[ -n "$err" && "$err" != None ]] && printf 'stderr: %s\n' "$err" | tail -n 20 >&2
  [[ "$st" == Success ]]
}

# ec2lib_terminate IID... (only image-test instances carrying all three matching campaign tags)
ec2lib_terminate() {
  local iid ids=()
  for iid in "$@"; do
    [[ -z "$iid" || "$iid" == None ]] && continue
    if [[ "$(aws ec2 describe-instances --region "$EC2LIB_REGION" --instance-ids "$iid" \
      --filters "Name=tag:cucina:env,Values=${CUCINA_ENV:-e2e}" "Name=tag:cucina:run,Values=${CUCINA_RUN_ID:?}" \
        "Name=tag:cucina:expires,Values=${CUCINA_EXPIRES:?}" "Name=tag:cucina:role,Values=image-test" \
      --query 'Reservations[0].Instances[0].InstanceId' --output text 2>/dev/null)" == "$iid" ]]; then
      ids+=("$iid")
    fi
  done
  ((${#ids[@]})) || return 0
  aws ec2 terminate-instances --region "$EC2LIB_REGION" --instance-ids "${ids[@]}" --query 'TerminatingInstances[].InstanceId' --output text >&2
}
