#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# kubelet image credential provider for Amazon ECR, installed on the e2e k3s node
# (kubelet CredentialProviderRequest on stdin -> CredentialProviderResponse on stdout).
# It signs ecr:GetAuthorizationToken with the node's instance-profile credentials (IMDSv2) and
# lets the kubelet pull cucina/* images without a registries.yaml token that expires after 12 h.
#
# Needs curl >= 7.75 (--aws-sigv4) and jq. Credentials come from AWS_ACCESS_KEY_ID /
# AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN when set (tests), otherwise from IMDSv2.
# CUCINA_ECR_CURL overrides the curl binary (tests inject a stub).

set -eu
curl_bin=${CUCINA_ECR_CURL:-curl}

image=$(jq -r '.image // empty')
registry=${image%%/*}
case "$registry" in
  [0-9]*.dkr.ecr.*.amazonaws.com) ;;
  *)
    echo "ecr-credential-provider: not an ECR registry: $registry" >&2
    exit 1
    ;;
esac
region=${registry#*.dkr.ecr.}
region=${region%%.*}

if [ -z "${AWS_ACCESS_KEY_ID:-}" ]; then
  imds=http://169.254.169.254/latest
  token=$("$curl_bin" -fsS -X PUT "$imds/api/token" -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
  role=$("$curl_bin" -fsS -H "X-aws-ec2-metadata-token: $token" "$imds/meta-data/iam/security-credentials/")
  creds=$("$curl_bin" -fsS -H "X-aws-ec2-metadata-token: $token" "$imds/meta-data/iam/security-credentials/$role")
  AWS_ACCESS_KEY_ID=$(printf '%s' "$creds" | jq -r '.AccessKeyId')
  AWS_SECRET_ACCESS_KEY=$(printf '%s' "$creds" | jq -r '.SecretAccessKey')
  AWS_SESSION_TOKEN=$(printf '%s' "$creds" | jq -r '.Token')
fi

set -- -fsS --max-time 20 --aws-sigv4 "aws:amz:$region:ecr" --user "$AWS_ACCESS_KEY_ID:$AWS_SECRET_ACCESS_KEY" \
  -H 'Content-Type: application/x-amz-json-1.1' \
  -H 'X-Amz-Target: AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken' -d '{}'
if [ -n "${AWS_SESSION_TOKEN:-}" ]; then
  set -- "$@" -H "x-amz-security-token: $AWS_SESSION_TOKEN"
fi
response=$("$curl_bin" "$@" "https://api.ecr.$region.amazonaws.com/")

# authorizationToken is base64("AWS:<password>"), valid for 12 hours.
printf '%s' "$response" | jq --arg registry "$registry" '
  (.authorizationData[0].authorizationToken | @base64d) as $up
  | {
      kind: "CredentialProviderResponse",
      apiVersion: "credentialprovider.kubelet.k8s.io/v1",
      cacheKeyType: "Registry",
      cacheDuration: "6h",
      auth: { ($registry): { username: ($up | split(":")[0]), password: ($up | sub("^[^:]*:"; "")) } }
    }'
