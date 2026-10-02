#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Records the AMI produced by the last Packer run into the (private, 0600) AMI registry used by the e2e
# environment, keeping the previous entry for rollback:
#
#   record-ami.sh <family> <packer-manifest.json> [amis.json]
#
# amis.json (default ~/.config/cucina/aws-e2e/amis.json):
#   { "<family>": { "ami_id", "name", "image_version", "generation", "region", "created", "source_ami",
#                   "previous": { ...same fields... } }, ... }
set -euo pipefail

family=${1:?usage: record-ami.sh <family> <manifest> [amis.json]}
manifest=${2:?usage: record-ami.sh <family> <manifest> [amis.json]}
amis=${3:-}
[[ -n "$amis" ]] || amis="${CUCINA_SECRETS_DIR:-$HOME/.config/cucina}/aws-e2e/amis.json"

run_uuid=$(jq -r '.last_run_uuid' "$manifest")
artifact=$(jq -r --arg u "$run_uuid" '[.builds[] | select(.packer_run_uuid == $u)] | last | .artifact_id' "$manifest")
if [[ -z "$artifact" || "$artifact" == "null" ]]; then
  echo "record-ami: no artifact for run $run_uuid in $manifest" >&2
  exit 1
fi
region=${artifact%%:*}
ami=${artifact#*:}

info=$(aws ec2 describe-images --region "$region" --image-ids "$ami" --output json \
  --query 'Images[0].{name:Name,created:CreationDate,tags:Tags}')
entry=$(jq -c --arg ami "$ami" --arg region "$region" '{
    ami_id: $ami,
    name: .name,
    region: $region,
    created: .created,
    image_version: ([.tags[]? | select(.Key == "cucina:image-version") | .Value] | first),
    generation: ([.tags[]? | select(.Key == "cucina:generation") | .Value] | first),
    source_ami: ([.tags[]? | select(.Key == "cucina:source-ami") | .Value] | first)
  }' <<<"$info")

umask 077
mkdir -p "$(dirname "$amis")"
[[ -s "$amis" ]] || echo '{}' >"$amis"
tmp=$(mktemp "${amis}.XXXXXX")
trap 'rm -f "$tmp"' EXIT
jq --arg f "$family" --argjson e "$entry" '
  .[$f] as $old
  | .[$f] = ($e + (if ($old != null and $old.ami_id != $e.ami_id) then {previous: ($old | del(.previous))}
                   elif $old != null then {previous: $old.previous} else {} end))
' "$amis" >"$tmp"
chmod 600 "$tmp"
mv "$tmp" "$amis"
echo "record-ami: $family -> $ami ($(jq -r '.image_version' <<<"$entry"), generation $(jq -r '.generation' <<<"$entry"))"
