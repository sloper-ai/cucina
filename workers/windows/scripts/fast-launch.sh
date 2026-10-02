#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# EC2 Fast Launch for Windows worker AMIs (R-POOL-2, user-approved standing cost).
#
#   fast-launch.sh enable  --ami AMI [--count N] [--max-parallel M] [--launch-template-id LT] [--wait]
#   fast-launch.sh disable --ami AMI [--no-wait]      # default: wait until describe-fast-launch-images says disabled
#   fast-launch.sh status  --ami AMI
#   fast-launch.sh tag     --ami AMI                  # campaign tags on this AMI's Fast Launch snapshots (best effort)
#
# enable: TargetResourceCount = the Windows pool's max (default 4) pre-provisioned snapshots, prepared by small,
# cheap prep instances from a launch template (private subnet, IMDSv2). The template comes from --launch-template-id,
# else the e2e environment output `fast_launch_template_id`, else a tagged template is created here.
# The first enable in an account creates the service-linked role AWSServiceRoleForEC2FastLaunch (left in place).
# disable must run (and finish) before an AMI is deregistered, on image rollout and at teardown; otherwise the
# snapshots and prep resources (tagged CreatedBy=EC2 Fast Launch) linger.
set -euo pipefail

REGION=${AWS_REGION:-us-west-1}
OUTPUTS=${OUTPUTS:-${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}/aws-e2e/base-outputs.json}
PREP_TYPE=${FAST_LAUNCH_PREP_TYPE:-m7i.large}
RUN_ID=${CUCINA_RUN_ID:?source .work/env.sh (CUCINA_RUN_ID)}
EXPIRES=${CUCINA_EXPIRES:?source .work/env.sh (CUCINA_EXPIRES)}
ENV_TAG=${CUCINA_ENV:-e2e}

cmd=${1:-}
shift || true
ami="" count=4 max_parallel=6 lt="" wait=0
[[ "$cmd" == disable ]] && wait=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ami) ami=$2; shift 2 ;;
    --count) count=$2; shift 2 ;;
    --max-parallel) max_parallel=$2; shift 2 ;;
    --launch-template-id) lt=$2; shift 2 ;;
    --wait) wait=1; shift ;;
    --no-wait) wait=0; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$ami" ]] || { echo "usage: fast-launch.sh enable|disable|status|tag --ami AMI [...]" >&2; exit 2; }

ec2() { aws ec2 --region "$REGION" "$@"; }
log() { printf '%s fast-launch: %s\n' "$(date -u +%H:%M:%SZ)" "$*" >&2; }
# API failures must not be mistaken for "disabled": teardown must fail closed.
state() { ec2 describe-fast-launch-images --image-ids "$ami" --query 'FastLaunchImages[0].State' --output text; }
tagspec() { echo "{Key=cucina:env,Value=$ENV_TAG},{Key=cucina:run,Value=$RUN_ID},{Key=cucina:expires,Value=$EXPIRES}"; }

# Standalone invocations have the same ownership boundary as prune-amis.sh. An arbitrary AMI ID is not authority
# to disable a pool or retag its snapshots; require all three campaign tags on our own AMI before any mutation.
case "$cmd" in
  enable|disable|tag)
    owned=$(ec2 describe-images --owners self --image-ids "$ami" \
      --filters "Name=tag:cucina:env,Values=$ENV_TAG" "Name=tag:cucina:run,Values=$RUN_ID" "Name=tag:cucina:expires,Values=$EXPIRES" \
      --query 'Images[0].ImageId' --output text)
    [[ "$owned" == "$ami" ]] || { log "refusing $cmd: AMI does not carry this campaign's three tags"; exit 1; }
    ;;
esac

ensure_template() {
  if [[ -z "$lt" && -s "$OUTPUTS" ]]; then lt=$(jq -r '.fast_launch_template_id // empty' "$OUTPUTS"); fi
  if [[ -n "$lt" ]]; then return; fi
  local name="cucina-fastlaunch-prep-${RUN_ID}"
  lt=$(ec2 describe-launch-templates --filters "Name=launch-template-name,Values=$name" \
    --query 'LaunchTemplates[0].LaunchTemplateId' --output text 2>/dev/null || true)
  if [[ -n "$lt" && "$lt" != None ]]; then return; fi
  local subnet sg data
  subnet=$(jq -r '.private_subnet_id' "$OUTPUTS")
  sg=$(jq -r '.sg_workers' "$OUTPUTS")
  # Fast Launch rejects templates with user data, termination protection, spot, terminate-on-shutdown or ENI tags.
  data=$(jq -nc --arg t "$PREP_TYPE" --arg s "$subnet" --arg g "$sg" --arg r "$RUN_ID" --arg e "$EXPIRES" --arg env "$ENV_TAG" '{
      InstanceType: $t,
      NetworkInterfaces: [{DeviceIndex: 0, SubnetId: $s, Groups: [$g], AssociatePublicIpAddress: false, DeleteOnTermination: true}],
      MetadataOptions: {HttpTokens: "required", HttpEndpoint: "enabled", HttpPutResponseHopLimit: 1},
      TagSpecifications: [
        {ResourceType: "instance", Tags: [{Key: "cucina:env", Value: $env}, {Key: "cucina:run", Value: $r}, {Key: "cucina:expires", Value: $e}, {Key: "Name", Value: "cucina-fastlaunch-prep"}]},
        {ResourceType: "volume", Tags: [{Key: "cucina:env", Value: $env}, {Key: "cucina:run", Value: $r}, {Key: "cucina:expires", Value: $e}]}]}')
  lt=$(ec2 create-launch-template --launch-template-name "$name" --launch-template-data "$data" \
    --tag-specifications "ResourceType=launch-template,Tags=[$(tagspec)]" \
    --query 'LaunchTemplate.LaunchTemplateId' --output text)
  log "created launch template $lt ($PREP_TYPE, private subnet)"
}

tag_snapshots() {
  # Fast Launch names its snapshots after the AMI; only those are tagged (never anyone else's resources).
  local ids
  ids=$(ec2 describe-snapshots --owner-ids self --filters "Name=tag:CreatedBy,Values=EC2 Fast Launch" \
    --query "Snapshots[?contains(Description || '', '$ami') || contains(to_string(Tags), '$ami')].SnapshotId" --output text)
  if [[ -n "$ids" && "$ids" != None ]]; then
    # shellcheck disable=SC2086
    ec2 create-tags --resources $ids --tags "Key=cucina:env,Value=$ENV_TAG" "Key=cucina:run,Value=$RUN_ID" "Key=cucina:expires,Value=$EXPIRES"
    log "tagged snapshots: $ids"
  else
    log "no Fast Launch snapshots for $ami yet"
  fi
}

case "$cmd" in
  enable)
    ensure_template
    version=$(ec2 describe-launch-templates --launch-template-ids "$lt" --query 'LaunchTemplates[0].DefaultVersionNumber' --output text)
    (( max_parallel >= 6 )) || max_parallel=6
    log "enabling on $ami: $count snapshots, template $lt v$version, max parallel $max_parallel"
    ec2 enable-fast-launch --image-id "$ami" --resource-type snapshot \
      --snapshot-configuration "TargetResourceCount=$count" --max-parallel-launches "$max_parallel" \
      --launch-template "LaunchTemplateId=$lt,Version=$version" --query 'State' --output text >/dev/null
    if (( wait )); then
      for _ in $(seq 1 120); do
        s=$(state)
        case "$s" in
          enabled) log "state enabled"; break ;;
          *failed*) log "state $s: $(ec2 describe-fast-launch-images --image-ids "$ami" --query 'FastLaunchImages[0].StateTransitionReason' --output text)"; exit 1 ;;
        esac
        sleep 30
      done
      tag_snapshots || true
    fi
    state
    ;;
  disable)
    s=$(state)
    if [[ "$s" == none || "$s" == None || "$s" == disabled ]]; then log "not enabled on $ami ($s)"; exit 0; fi
    log "disabling on $ami (was $s)"
    ec2 disable-fast-launch --image-id "$ami" --query 'State' --output text >/dev/null
    if (( wait )); then
      for _ in $(seq 1 240); do
        s=$(state)
        if [[ "$s" == disabled || "$s" == none || "$s" == None ]]; then log "disabled ($s)"; exit 0; fi
        sleep 15
      done
      log "timed out waiting for disable (last state $s)"
      exit 1
    fi
    ;;
  status)
    ec2 describe-fast-launch-images --image-ids "$ami" --output json \
      --query 'FastLaunchImages[0].{state:State,reason:StateTransitionReason,count:SnapshotConfiguration.TargetResourceCount,maxParallel:MaxParallelLaunches,template:LaunchTemplate}'
    ;;
  tag)
    tag_snapshots
    ;;
  *)
    echo "usage: fast-launch.sh enable|disable|status|tag --ami AMI [...]" >&2
    exit 2
    ;;
esac
