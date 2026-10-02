#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Prints the next pool generation number for an image family: 1 + the highest `cucina:generation` tag on
# this account's AMIs of that family (R-POOL-8, R-OPS-2). Used by the Linux and Windows Makefiles.
#
#   next-generation.sh <family> [region]
set -euo pipefail

family=${1:?usage: next-generation.sh <family> [region]}
region=${2:-${AWS_REGION:-us-west-1}}

# shellcheck disable=SC2016 # backticks are JMESPath literals, not command substitution
current=$(aws ec2 describe-images --region "$region" --owners self \
  --filters "Name=tag:cucina:image-family,Values=${family}" \
  --query 'Images[].Tags[?Key==`cucina:generation`].Value[]' --output text |
  tr -s '\t ' '\n' | grep -E '^[0-9]+$' | sort -n | tail -n 1 || true)
echo $((${current:-0} + 1))
