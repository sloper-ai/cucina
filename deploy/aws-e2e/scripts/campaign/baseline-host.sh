#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# NFR-P2 needs the cold Abseil build timed locally on ONE instance of the worker type (ADR 1005). This launches a
# temporary, tagged baseline client of the worker type from the linux-client's launch parameters, prints its instance
# ID for the environment descriptor (clients["linux-baseline"]), and terminates it afterwards — tag-checked (§12).
#
#   baseline-host.sh up --type m7i.large       selected small worker type only (<= 2 vCPU / 8 GiB)
#   baseline-host.sh down                       terminate every baseline host of this run
# A small-worker baseline is diagnostic, not the original large/max-four NFR-P2 qualification.
#
# Requires: AWS session (profile default, us-west-1), CUCINA_RUN_ID, CUCINA_EXPIRES, ~/.config/cucina/aws-e2e/env-outputs.json.
set -eu
umask 077
REGION=us-west-1
: "${CUCINA_RUN_ID:?}" "${CUCINA_EXPIRES:?}"
OUT=${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}/aws-e2e
TAGS="Key=cucina:env,Value=e2e},{Key=cucina:run,Value=$CUCINA_RUN_ID},{Key=cucina:expires,Value=$CUCINA_EXPIRES},{Key=cucina:role,Value=baseline-client"

aws() { command aws --profile default --region "$REGION" "$@"; }

up() {
	type=${CUCINA_BASELINE_INSTANCE_TYPE:-}
	case $# in 0) : ;; 2) [ "$1" = --type ] || { echo 'expected --type TYPE' >&2; exit 2; }; type=$2 ;; *) echo 'expected --type TYPE' >&2; exit 2 ;; esac
	case $type in
	*.large) : ;;
	*) echo 'baseline requires an explicitly selected .large worker type; larger shapes are forbidden' >&2; exit 2 ;;
	esac
	shape=$(aws ec2 describe-instance-types --instance-types "$type" --query 'InstanceTypes[0]' --output json)
	echo "$shape" | jq -e '.VCpuInfo.DefaultVCpus>0 and .VCpuInfo.DefaultVCpus<=2 and .MemoryInfo.SizeInMiB>0 and .MemoryInfo.SizeInMiB<=8192' >/dev/null || { echo 'selected type exceeds 2 vCPU / 8 GiB; refusing launch' >&2; exit 2; }
	src=$(jq -r '.linux_client_instance_id.value // .linux_client_instance_id' "$OUT/env-outputs.json")
	[ -n "$src" ] && [ "$src" != null ] || { echo "no linux_client_instance_id in env outputs" >&2; exit 1; }
	# Same AMI, subnet, security groups, instance profile and user data as linux-client.
	desc=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$src" \
		--filters "Name=tag:cucina:env,Values=e2e" "Name=tag:cucina:run,Values=$CUCINA_RUN_ID" "Name=tag:cucina:expires,Values=$CUCINA_EXPIRES" --query 'Reservations[0].Instances[0]' --output json)
	ami=$(echo "$desc" | jq -r .ImageId)
	subnet=$(echo "$desc" | jq -r .SubnetId)
	sgs=$(echo "$desc" | jq -r '[.SecurityGroups[].GroupId] | join(" ")')
	profile=$(echo "$desc" | jq -r '.IamInstanceProfile.Arn')
	echo "$desc" | jq -e '.ImageId and .SubnetId and .IamInstanceProfile.Arn' >/dev/null || { echo 'source client not found in this tagged campaign' >&2; exit 2; }
	udfile=$(mktemp "$OUT/.baseline-user-data.XXXXXX")
	trap 'rm -f "$udfile"' EXIT INT TERM
	aws ec2 describe-instance-attribute --region "$REGION" --instance-id "$src" --attribute userData \
		--query 'UserData.Value' --output text | base64 -d >"$udfile"
	# shellcheck disable=SC2086 # sgs is a space-separated list of IDs
	id=$(aws ec2 run-instances --region "$REGION" --image-id "$ami" --instance-type "$type" --subnet-id "$subnet" \
		--security-group-ids $sgs --iam-instance-profile "Arn=$profile" --user-data "file://$udfile" \
		--metadata-options HttpTokens=required,HttpPutResponseHopLimit=1 \
		--instance-initiated-shutdown-behavior terminate \
		--block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=100,VolumeType=gp3,DeleteOnTermination=true}' \
		--tag-specifications "ResourceType=instance,Tags=[{$TAGS}]" "ResourceType=volume,Tags=[{$TAGS}]" \
		--query 'Instances[0].InstanceId' --output text)
	rm -f "$udfile"
	echo "$id" >"$OUT/baseline-host.id"
	echo "launched $type baseline host; add to the descriptor: \"linux-baseline\": {\"os\": \"linux\", \"instanceId\": \"<$OUT/baseline-host.id>\", \"user\": \"ubuntu\"}"
}

down() {
	ids=$(aws ec2 describe-instances --region "$REGION" \
		--filters "Name=tag:cucina:env,Values=e2e" "Name=tag:cucina:run,Values=$CUCINA_RUN_ID" "Name=tag:cucina:expires,Values=$CUCINA_EXPIRES" "Name=tag:cucina:role,Values=baseline-client" \
		"Name=instance-state-name,Values=pending,running,stopping,stopped" \
		--query 'Reservations[].Instances[].InstanceId' --output text)
	[ -z "$ids" ] && { echo "no baseline hosts"; return 0; }
	# shellcheck disable=SC2086 # ids is a whitespace-separated list
	aws ec2 terminate-instances --region "$REGION" --instance-ids $ids >/dev/null
	echo "terminated $(echo "$ids" | wc -w | tr -d ' ') baseline host(s)"
	rm -f "$OUT/baseline-host.id"
}

case ${1:-} in
up) shift && up "$@" ;;
down) down ;;
*) echo "usage: baseline-host.sh up --type SMALL-WORKER-TYPE | down" >&2 && exit 2 ;;
esac
