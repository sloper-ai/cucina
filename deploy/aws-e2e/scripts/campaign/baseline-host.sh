#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# NFR-P2 needs the cold Abseil build timed locally on ONE instance of the worker type (ADR 1005). This launches a
# temporary, tagged baseline client of the worker type from the linux-client's launch parameters, prints its instance
# ID for the environment descriptor (clients["linux-baseline"]), and terminates it afterwards — tag-checked (§12).
#
#   baseline-host.sh up [--type c8i.8xlarge]    launch (≈ $1.87/h; terminate as soon as baseline-linux has run)
#   baseline-host.sh down                       terminate every baseline host of this run
#
# Requires: AWS session (profile default, us-west-1), CUCINA_RUN_ID, CUCINA_EXPIRES, ~/.config/cucina/aws-e2e/env-outputs.json.
set -eu
umask 077
REGION=us-west-1
: "${CUCINA_RUN_ID:?}" "${CUCINA_EXPIRES:?}"
OUT=${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}/aws-e2e
TAGS="Key=cucina:env,Value=e2e},{Key=cucina:run,Value=$CUCINA_RUN_ID},{Key=cucina:expires,Value=$CUCINA_EXPIRES},{Key=cucina:role,Value=baseline-client"

up() {
	type=c8i.8xlarge
	[ "${1:-}" = "--type" ] && type=$2
	src=$(jq -r '.linux_client_instance_id.value // .linux_client_instance_id' "$OUT/env-outputs.json")
	[ -n "$src" ] && [ "$src" != null ] || { echo "no linux_client_instance_id in env outputs" >&2; exit 1; }
	# Same AMI, subnet, security groups, instance profile and user data as linux-client.
	desc=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$src" \
		--filters "Name=tag:cucina:run,Values=$CUCINA_RUN_ID" --query 'Reservations[0].Instances[0]' --output json)
	ami=$(echo "$desc" | jq -r .ImageId)
	subnet=$(echo "$desc" | jq -r .SubnetId)
	sgs=$(echo "$desc" | jq -r '[.SecurityGroups[].GroupId] | join(" ")')
	profile=$(echo "$desc" | jq -r '.IamInstanceProfile.Arn')
	udfile=$(mktemp)
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
		--filters "Name=tag:cucina:run,Values=$CUCINA_RUN_ID" "Name=tag:cucina:role,Values=baseline-client" \
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
*) echo "usage: baseline-host.sh up [--type T] | down" >&2 && exit 2 ;;
esac
