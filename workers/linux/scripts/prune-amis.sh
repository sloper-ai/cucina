#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Keeps the newest KEEP AMIs of an image family (default 2: current + one previous for rollback, R-OPS-2) and
# deregisters the older ones together with their snapshots. Only images owned by this account that carry this
# run's campaign tags are considered. Windows worker AMIs get EC2 Fast Launch disabled first (and the script waits
# until it reports disabled), otherwise its pre-provisioned snapshots would linger.
#
#   prune-amis.sh <family> [--keep 2] [--dry-run]
set -euo pipefail

family=${1:?usage: prune-amis.sh <family> [--keep N] [--dry-run]}
shift
keep=2 dry=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep) keep=$2; shift 2 ;;
    --dry-run) dry=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
region=${AWS_REGION:-us-west-1}
run=${CUCINA_RUN_ID:?source .work/env.sh (CUCINA_RUN_ID)}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

campaign=("Name=tag:cucina:run,Values=$run" "Name=tag:cucina:env,Values=${CUCINA_ENV:-e2e}" "Name=tag:cucina:expires,Values=${CUCINA_EXPIRES:?}")
# Preserve AWS failures instead of treating a failed process substitution as an empty inventory.
images=$(aws ec2 describe-images --region "$region" --owners self \
  --filters "Name=tag:cucina:image-family,Values=$family" "${campaign[@]}" \
  --query 'sort_by(Images,&CreationDate)[].ImageId' --output text)
mapfile -t all < <(printf '%s\n' "$images" | tr '\t' '\n' | sed '/^$/d')
old=()
if ((${#all[@]} > keep)); then old=("${all[@]:0:${#all[@]}-keep}"); fi
if ((${#old[@]} == 0)); then
  echo "prune-amis: $family has <= $keep images; nothing to do"
  exit 0
fi
for ami in "${old[@]}"; do
  snaps=$(aws ec2 describe-images --region "$region" --owners self --image-ids "$ami" --filters "${campaign[@]}" \
    --query 'Images[0].BlockDeviceMappings[].Ebs.SnapshotId' --output text)
  echo "prune-amis: $family: deregister $ami (snapshots: $snaps)"
  ((dry)) && continue
  # Snapshot ownership is checked independently: an AMI's block mapping alone does not grant deletion authority.
  for s in $snaps; do
    [[ "$s" == snap-* ]] || { echo "prune-amis: invalid snapshot identity; refusing deletion" >&2; exit 1; }
    owned=$(aws ec2 describe-snapshots --region "$region" --owner-ids self --snapshot-ids "$s" --filters "${campaign[@]}" \
      --query 'Snapshots[0].SnapshotId' --output text)
    [[ "$owned" == "$s" ]] || { echo "prune-amis: snapshot lacks the matching campaign tags; refusing deletion" >&2; exit 1; }
  done
  if [[ "$family" == windows-* ]]; then "$here/../../windows/scripts/fast-launch.sh" disable --ami "$ami"; fi
  aws ec2 deregister-image --region "$region" --image-id "$ami" --query Return --output text >/dev/null
  for s in $snaps; do
    [[ "$s" == snap-* ]] && aws ec2 delete-snapshot --region "$region" --snapshot-id "$s"
  done
done
