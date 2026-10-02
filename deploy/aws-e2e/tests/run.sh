#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Offline tests of deploy/aws-e2e (R-TEST-6 "Infrastructure"; tier `static` plus plan-time
# `tofu test`). Nothing here talks to AWS and no credentials are needed:
#
#   1. tofu fmt -check, tofu validate, tflint (bundled ruleset) for both layers
#   2. tofu test with mock_provider + command = plan  (base/tests, env/tests)
#   3. shellcheck on every script
#   4. structural policy checks that HCL assertions cannot express: no NAT gateway, exactly one
#      EIP, no 0.0.0.0/0 or ::/0 ingress, tags on every resource, hygiene (no ids/IPs/state),
#      SPDX headers, identical provider locks
#
#   tests/run.sh                 run everything
#   E2E_TESTS_SKIP="tflint shellcheck"   skip tools that are not installed locally
#
# The real apply/destroy of the campaign is the integration test (no Terratest).

set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH='' cd -- "$here/.." && pwd)
cd "$root"

skip=" ${E2E_TESTS_SKIP:-} "
failures=0
step() { printf '== %s\n' "$*"; }
pass() { printf '   ok    %s\n' "$*"; }
fail() {
  printf '   FAIL  %s\n' "$*"
  failures=$((failures + 1))
}
skipped() { case "$skip" in *" $1 "*) return 0 ;; *) return 1 ;; esac }

# Never let a stray credential or profile reach AWS from the tests.
unset AWS_PROFILE AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
AWS_EC2_METADATA_DISABLED=true
AWS_REGION=us-west-1
export AWS_EC2_METADATA_DISABLED AWS_REGION

work=$(mktemp -d "${TMPDIR:-/tmp}/cucina-e2e-tests.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
TF_PLUGIN_CACHE_DIR=${TF_PLUGIN_CACHE_DIR:-$work/plugin-cache}
mkdir -p "$TF_PLUGIN_CACHE_DIR"
TF_VAR_state_dir=$work/state
TF_IN_AUTOMATION=1
export TF_PLUGIN_CACHE_DIR TF_VAR_state_dir TF_IN_AUTOMATION

command -v tofu >/dev/null 2>&1 || {
  echo "tofu not found" >&2
  exit 1
}

# --- 1. fmt / validate / tflint ---------------------------------------------------------------
step "tofu fmt -check"
if tofu fmt -check -recursive -diff . >"$work/fmt.txt" 2>&1; then pass "formatted"; else
  head -n 40 "$work/fmt.txt"
  fail "tofu fmt -check (run: tofu fmt -recursive deploy/aws-e2e)"
fi

for layer in base env; do
  step "$layer: init, validate, tflint, tofu test"
  export TF_DATA_DIR="$work/tfdata-$layer"
  if ! tofu -chdir="$layer" init -backend=false -input=false -no-color >"$work/init-$layer.txt" 2>&1; then
    tail -n 20 "$work/init-$layer.txt"
    fail "$layer: tofu init"
    continue
  fi
  if tofu -chdir="$layer" validate -no-color >"$work/validate-$layer.txt" 2>&1; then pass "validate"; else
    cat "$work/validate-$layer.txt"
    fail "$layer: tofu validate"
  fi
  if skipped tflint; then
    echo "   skip  tflint (E2E_TESTS_SKIP)"
  elif command -v tflint >/dev/null 2>&1; then
    if (cd "$layer" && tflint --config "$root/.tflint.hcl" >"$work/tflint-$layer.txt" 2>&1); then pass "tflint"; else
      cat "$work/tflint-$layer.txt"
      fail "$layer: tflint"
    fi
  else
    fail "$layer: tflint not found (install it or set E2E_TESTS_SKIP=tflint)"
  fi
  if tofu -chdir="$layer" test -no-color >"$work/test-$layer.txt" 2>&1; then
    pass "tofu test: $(tail -n 1 "$work/test-$layer.txt")"
  else
    cat "$work/test-$layer.txt"
    fail "$layer: tofu test"
  fi
done

# --- 2b. the kubelet ECR credential provider (offline, curl stubbed) ---------------------------
step "ecr credential provider: kubelet protocol (stubbed curl)"
if ! command -v jq >/dev/null 2>&1; then
  fail "jq not found (needed by the credential provider and its test)"
else
  stub="$work/stub"
  mkdir -p "$stub"
  cat >"$stub/curl" <<'STUB'
#!/bin/sh
# records its arguments and answers like ECR's GetAuthorizationToken
printf '%s\n' "$*" >"${STUB_ARGS:?}"
for last; do :; done
case "$last" in
  https://api.ecr.us-west-1.amazonaws.com/)
    printf '{"authorizationData":[{"authorizationToken":"%s"}]}' "$(printf 'AWS:pass:with:colons' | base64 | tr -d '\n')"
    ;;
  *) echo "unexpected URL: $last" >&2; exit 22 ;;
esac
STUB
  chmod +x "$stub/curl"
  registry="123456789012.dkr.ecr.us-west-1.amazonaws.com"
  out=$(printf '{"image":"%s/cucina/controller:dev"}' "$registry" |
    STUB_ARGS="$work/curl-args" CUCINA_ECR_CURL="$stub/curl" AWS_ACCESS_KEY_ID=AKIDEXAMPLE AWS_SECRET_ACCESS_KEY=example AWS_SESSION_TOKEN=exampletoken \
      sh env/files/ecr-credential-provider.sh 2>"$work/ecr-provider.err") || out=""
  if printf '%s' "$out" | jq -e --arg r "$registry" '
      .kind == "CredentialProviderResponse" and .apiVersion == "credentialprovider.kubelet.k8s.io/v1"
      and .cacheKeyType == "Registry" and .auth[$r].username == "AWS" and .auth[$r].password == "pass:with:colons"' >/dev/null 2>&1 &&
    grep -q -- '--aws-sigv4 aws:amz:us-west-1:ecr' "$work/curl-args" && grep -q 'GetAuthorizationToken' "$work/curl-args" && grep -q 'x-amz-security-token: exampletoken' "$work/curl-args"; then
    pass "returns the registry credentials in the kubelet CredentialProviderResponse format, signed for ecr in the image's region"
  else
    cat "$work/ecr-provider.err" 2>/dev/null || true
    fail "ecr credential provider output or request is wrong"
  fi
  if printf '{"image":"docker.io/library/busybox:latest"}' | STUB_ARGS="$work/curl-args2" CUCINA_ECR_CURL="$stub/curl" AWS_ACCESS_KEY_ID=x AWS_SECRET_ACCESS_KEY=y sh env/files/ecr-credential-provider.sh >/dev/null 2>&1; then
    fail "the credential provider must refuse non-ECR registries"
  else
    pass "refuses non-ECR registries"
  fi
fi

# --- 3. shellcheck ----------------------------------------------------------------------------
step "shellcheck"
if skipped shellcheck; then
  echo "   skip  shellcheck (E2E_TESTS_SKIP)"
else
  if command -v shellcheck >/dev/null 2>&1 && shellcheck --version >/dev/null 2>&1; then
    sc="shellcheck"
  elif command -v mise >/dev/null 2>&1 && mise x aqua:koalaman/shellcheck@0.11.0 -- shellcheck --version >/dev/null 2>&1; then
    sc="mise x aqua:koalaman/shellcheck@0.11.0 -- shellcheck"
  else
    sc=""
  fi
  if [ -z "$sc" ]; then
    fail "shellcheck not found (install it or set E2E_TESTS_SKIP=shellcheck)"
  else
    # shellcheck disable=SC2086  # $sc is a command line
    if $sc -s sh -x -P SCRIPTDIR scripts/*.sh tests/run.sh env/files/*.sh >"$work/shellcheck.txt" 2>&1; then pass "scripts are shellcheck-clean"; else
      head -n 40 "$work/shellcheck.txt"
      fail "shellcheck"
    fi
  fi
fi

# --- 4. structural policy checks ---------------------------------------------------------------
tf_files=$(find base env -maxdepth 1 -name '*.tf' | sort)

step "no NAT gateway (R-DATA-4, NFR-T9)"
# shellcheck disable=SC2086
if grep -nE 'aws_nat_gateway|nat_gateway_id' $tf_files; then fail "NAT gateway referenced"; else pass "no aws_nat_gateway resource, no route targets one"; fi

step "exactly one Elastic IP (the k3s node's)"
eips=$(cat base/*.tf env/*.tf | grep -cE '^resource "aws_eip" ' || true)
base_eips=$(cat base/*.tf | grep -cE '^resource "aws_eip" ' || true)
if [ "$eips" = 1 ] && [ "$base_eips" = 0 ]; then pass "one aws_eip, in env"; else fail "expected exactly one aws_eip (in env), found $eips (base: $base_eips)"; fi

step "no 0.0.0.0/0 or ::/0 ingress (§12)"
# shellcheck disable=SC2086
if awk '
  /^resource "(aws_vpc_security_group_ingress_rule|aws_security_group_rule|aws_security_group|aws_default_security_group|aws_network_acl_rule)"/ { inres = 1; next }
  inres && /0\.0\.0\.0\/0|::\/0/ { print FILENAME ": " $0; bad = 1 }
  inres && /^[[:space:]]+ingress[[:space:]]*(\{|=)/ { print FILENAME ": inline ingress (use aws_vpc_security_group_ingress_rule): " $0; bad = 1 }
  /^}/ { inres = 0 }
  END { exit bad }' $tf_files; then pass "no world-open ingress in any security-group resource"; else fail "world-open or inline ingress found"; fi

step "tags on every resource (§12)"
untaggable="aws_route aws_route_table_association aws_iam_role_policy aws_iam_role_policy_attachment aws_eip_association aws_ecr_lifecycle_policy"
untagged=0
for f in $tf_files; do
  awk -v f="$f" -v skip="$untaggable" '
    BEGIN { n = split(skip, a, " "); for (i = 1; i <= n; i++) ok[a[i]] = 1 }
    /^resource "/ { type = $2; name = $3; gsub(/"/, "", type); gsub(/"/, "", name); inres = 1; has = 0; next }
    inres && /^  (tags|volume_tags)[[:space:]]*=/ { has = 1 }
    inres && /^}/ { if (!has && !(type in ok)) { printf "   %s: resource %s.%s has no tags\n", f, type, name; bad = 1 } inres = 0 }
    END { exit bad }' "$f" || untagged=1
done
if [ "$untagged" = 0 ]; then pass "every taggable resource sets tags"; else fail "resources without tags"; fi

step "worker role: only AmazonSSMManagedInstanceCore is attached"
n=$(grep -B1 -A3 'resource "aws_iam_role_policy_attachment"' base/iam.tf | grep -c 'role *= aws_iam_role.worker.name' || true)
if [ "$n" = 1 ]; then pass "one managed policy attachment on the worker role"; else fail "expected exactly one policy attachment on aws_iam_role.worker, found $n"; fi

step "public-repo hygiene (§12): no state, tfvars, ids, account numbers or addresses"
hyg=0
if find . -name '*.tfstate' -o -name '*.tfstate.*' -o -name '*.tfvars' -o -name '*.tfplan' | grep -q .; then
  find . -name '*.tfstate' -o -name '*.tfstate.*' -o -name '*.tfvars' -o -name '*.tfplan'
  hyg=1
fi
scan_files="$(find . -type f \( -name '*.tf' -o -name '*.tftest.hcl' -o -name '*.sh' -o -name '*.tftpl' -o -name '*.hcl' \) -not -path '*/.terraform/*' | sort) ../../docs/operations/aws-e2e.md"
for d in ../../docs/adr/02*.md; do [ -f "$d" ] && scan_files="$scan_files $d"; done
for f in $scan_files; do
  [ -f "$f" ] || continue
  if grep -nE '(^|[^0-9a-f])[0-9]{12}([^0-9a-f]|$)' "$f" | grep -vE '123456789012' | grep -q .; then
    grep -nE '(^|[^0-9a-f])[0-9]{12}([^0-9a-f]|$)' "$f" | grep -vE '123456789012'
    echo "   ^ $f: 12-digit number (account id?)"
    hyg=1
  fi
  if grep -noE '\b(vpc|subnet|sg|rtb|igw|eigw|vpce|eni|i|vol|snap|ami|lt|eipalloc|key)-[0-9a-f]{8,17}\b' "$f" | grep -vE -- '-(0123456789abcdef[0-9a-f]?|0+)$' | grep -q .; then
    grep -noE '\b(vpc|subnet|sg|rtb|igw|eigw|vpce|eni|i|vol|snap|ami|lt|eipalloc|key)-[0-9a-f]{8,17}\b' "$f" | grep -vE -- '-(0123456789abcdef[0-9a-f]?|0+)$'
    echo "   ^ $f: AWS resource id"
    hyg=1
  fi
  if grep -noE '\b([0-9]{1,3}\.){3}[0-9]{1,3}\b' "$f" | grep -vE ':(0\.0\.0\.0|127\.0\.0\.1|169\.254\.169\.254|10\.(42|43|244|245)\.[0-9]+\.[0-9]+|192\.0\.2\.[0-9]+|198\.51\.100\.[0-9]+|203\.0\.113\.[0-9]+)$' | grep -q .; then
    grep -noE '\b([0-9]{1,3}\.){3}[0-9]{1,3}\b' "$f" | grep -vE ':(0\.0\.0\.0|127\.0\.0\.1|169\.254\.169\.254|10\.(42|43|244|245)\.[0-9]+\.[0-9]+|192\.0\.2\.[0-9]+|198\.51\.100\.[0-9]+|203\.0\.113\.[0-9]+)$'
    echo "   ^ $f: IPv4 address outside the documentation ranges"
    hyg=1
  fi
done
if [ "$hyg" = 0 ]; then pass "clean"; else fail "hygiene findings above"; fi

step "SPDX headers"
spdx=0
for f in $(find . -type f \( -name '*.tf' -o -name '*.tftest.hcl' -o -name '*.sh' -o -name '*.tftpl' -o -name '.tflint.hcl' \) -not -path '*/.terraform/*' | sort); do
  head -n 3 "$f" | grep -q 'SPDX-License-Identifier: FSL-1.1-ALv2' || {
    echo "   $f: missing SPDX line"
    spdx=1
  }
done
if [ "$spdx" = 0 ]; then pass "all Cucina-authored files carry the SPDX line"; else fail "missing SPDX headers"; fi

step "provider pins"
if cmp -s base/.terraform.lock.hcl env/.terraform.lock.hcl; then pass "both layers share one provider lock"; else fail "base and env .terraform.lock.hcl differ"; fi
if grep -q 'version *= *"= 6.67.0"' base/versions.tf env/versions.tf && grep -q 'version *= *"6.67.0"' base/.terraform.lock.hcl; then pass "hashicorp/aws pinned to 6.67.0"; else fail "hashicorp/aws must be pinned to 6.67.0 (versions.tf and lock file)"; fi
if grep -q 'required_version *= *">= 1.13.1, < 1.14.0"' base/versions.tf env/versions.tf; then pass "OpenTofu 1.13.x required"; else fail "required_version must be >= 1.13.1, < 1.14.0"; fi

echo
if [ "$failures" -gt 0 ]; then
  printf 'FAILED: %s check(s)\n' "$failures"
  exit 1
fi
echo "all checks passed"
