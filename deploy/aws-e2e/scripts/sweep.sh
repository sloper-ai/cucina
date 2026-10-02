#!/bin/sh
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Tag sweep of the e2e environment (PROMPT.md §12): inventory every resource carrying
# cucina:env=e2e AND cucina:run=$CUCINA_RUN_ID, plus resources tagged CreatedBy=EC2 Fast Launch,
# and exit non-zero when anything is left.
#
#   sweep.sh [--report]        inventory only (default); exit 0 clean, 1 leftovers, 2 API error
#   sweep.sh --delete-amis     first deregister the run's AMIs and delete their snapshots, after
#                              DISABLING EC2 FAST LAUNCH on each AMI and waiting until it is
#                              disabled; then report
#   sweep.sh --selftest        query-only demonstration of the tag filters (creates and deletes
#                              nothing): env-only vs env+run vs a bogus run id vs a bogus env
#   sweep.sh --run-id ID       sweep another run id (default $CUCINA_RUN_ID)
#
# Covered: instances, volumes, snapshots, AMIs, ENIs, EIPs, security groups, subnets, route
# tables, internet and egress-only gateways, VPC endpoints, NAT gateways, VPCs, launch templates,
# Fast Launch images/snapshots/templates/instances, IAM roles/policies/instance profiles, ECR
# repositories, SSM parameters, Secrets Manager secrets - and, through the Resource Groups
# Tagging API (us-west-1 and us-east-1 for IAM), anything else that carries the tags (log
# groups, S3 buckets, ...). Only AMIs (and their snapshots) are ever deleted here, and only
# when they carry all three campaign tags; everything else is removed by `tofu destroy` / the controller.

set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib.sh
. "$here/lib.sh"

mode=report
selftest=0
run_id_arg=""
while [ $# -gt 0 ]; do
  case "$1" in
    --report) mode=report ;;
    --delete-amis) mode=delete-amis ;;
    --selftest) selftest=1 ;;
    --run-id)
      run_id_arg=${2:?--run-id needs a value}
      shift
      ;;
    -h | --help)
      e2e_usage "$0"
      exit 0
      ;;
    *) e2e_die "unknown argument: $1" ;;
  esac
  shift
done

e2e_require aws jq awk
e2e_setup
e2e_session_check
RUN=${run_id_arg:-$E2E_RUN_ID}
case "$RUN" in
  "" | *[!a-z0-9-]*) e2e_die "run id '$RUN' must be lowercase alphanumerics and dashes" ;;
esac
FASTLAUNCH_TAG_VALUE="EC2 Fast Launch"

work=$(mktemp -d "${TMPDIR:-/tmp}/cucina-sweep.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
inv="$work/inventory.tsv" # type <TAB> id <TAB> detail
: >"$inv"
api_errors=0

note_api_error() {
  api_errors=$((api_errors + 1))
  e2e_log "  (api error while querying $1; the result below may be incomplete)"
}

# --- generic query helpers ---------------------------------------------------------------

# aws_json <outfile> <aws args...>: run an AWS CLI call, keep stdout as JSON, show errors.
aws_json() {
  aj_out=$1
  shift
  if ! aws "$@" --output json >"$aj_out" 2>"$aj_out.err"; then
    sed 's/^/    aws: /' "$aj_out.err" >&2
    return 1
  fi
}

# add_rows <type>: read "id<TAB>detail" rows on stdin and append "type<TAB>id<TAB>detail".
add_rows() {
  awk -F '\t' -v t="$1" 'NF { print t "\t" $0 }' >>"$inv"
}

# ec2q <filters> <type> <jq program> <ec2 subcommand> [extra args]: tag-filtered EC2 inventory.
# <filters> is a space separated list of Name=...,Values=... words (word splitting intended).
ec2q() {
  eq_filters=$1
  eq_type=$2
  eq_jq=$3
  eq_sub=$4
  shift 4
  # shellcheck disable=SC2086
  if aws_json "$work/r.json" ec2 "$eq_sub" "$@" --filters $eq_filters; then
    jq -r "$eq_jq" "$work/r.json" | add_rows "$eq_type"
  else
    note_api_error "ec2 $eq_sub"
  fi
}

# has_tags_jq: jq definitions. `ours($run)` is true for a {Key,Value}/{key,value} tag list that
# carries cucina:env=e2e and (when $run is not empty) cucina:run=$run. Used for services whose
# list calls cannot filter by tag. The jq source is meant to stay unexpanded.
# shellcheck disable=SC2016
has_tags_jq='
  def tagmap: (. // []) | map({ ((.Key // .key)): (.Value // .value) }) | add // {};
  def tagmap_of($t): $t | tagmap;
  def ours($run): (tagmap) as $m | ($m["cucina:env"] == "e2e") and ($run == "" or $m["cucina:run"] == $run);
'

# --- inventories ---------------------------------------------------------------------------

# collect <run-id or "">: fill $inv for the given run id (empty = every e2e run).
collect() {
  c_run=$1
  c_f=$(e2e_ec2_tag_filters "$c_run")
  : >"$inv"

  ec2q "$c_f" instance '.Reservations[].Instances[] | select(.State.Name != "terminated") | [.InstanceId, .State.Name, .InstanceType, ((.Tags // []) | map(select(.Key == "cucina:e2e-role" or .Key == "cucina:pool") | .Value) | join("/"))] | @tsv' describe-instances
  ec2q "$c_f" volume '.Volumes[] | [.VolumeId, .State, "\(.Size)GiB"] | @tsv' describe-volumes
  ec2q "$c_f" snapshot '.Snapshots[] | [.SnapshotId, .State, "\(.VolumeSize)GiB"] | @tsv' describe-snapshots --owner-ids self
  ec2q "$c_f" ami '.Images[] | [.ImageId, .State, .Name, (.Platform // "linux")] | @tsv' describe-images --owners self
  ec2q "$c_f" network-interface '.NetworkInterfaces[] | [.NetworkInterfaceId, .Status, (.Description // "")] | @tsv' describe-network-interfaces
  ec2q "$c_f" elastic-ip '.Addresses[] | [(.AllocationId // .PublicIp), (.AssociationId // "unassociated")] | @tsv' describe-addresses
  ec2q "$c_f" security-group '.SecurityGroups[] | [.GroupId, .GroupName] | @tsv' describe-security-groups
  ec2q "$c_f" subnet '.Subnets[] | [.SubnetId, .CidrBlock] | @tsv' describe-subnets
  ec2q "$c_f" route-table '.RouteTables[] | [.RouteTableId, ((.Tags // []) | map(select(.Key == "Name") | .Value) | join(""))] | @tsv' describe-route-tables
  ec2q "$c_f" internet-gateway '.InternetGateways[] | [.InternetGatewayId, ""] | @tsv' describe-internet-gateways
  ec2q "$c_f" egress-only-igw '.EgressOnlyInternetGateways[] | [.EgressOnlyInternetGatewayId, ""] | @tsv' describe-egress-only-internet-gateways
  ec2q "$c_f" vpc-endpoint '.VpcEndpoints[] | select(.State != "deleted") | [.VpcEndpointId, .State, .ServiceName] | @tsv' describe-vpc-endpoints
  ec2q "$c_f" vpc '.Vpcs[] | [.VpcId, .CidrBlock] | @tsv' describe-vpcs
  ec2q "$c_f" launch-template '.LaunchTemplates[] | [.LaunchTemplateId, .LaunchTemplateName] | @tsv' describe-launch-templates
  ec2q "$c_f" key-pair '.KeyPairs[] | [.KeyPairId, .KeyName] | @tsv' describe-key-pairs

  # NAT gateways take `--filter` (singular) and must never exist in this topology.
  # shellcheck disable=SC2086
  if aws_json "$work/r.json" ec2 describe-nat-gateways --filter $c_f; then
    jq -r '.NatGateways[] | select(.State != "deleted") | [.NatGatewayId, .State, "FORBIDDEN: no NAT gateway in this topology"] | @tsv' "$work/r.json" | add_rows nat-gateway
  else
    note_api_error "ec2 describe-nat-gateways"
  fi

  collect_fast_launch "$c_run"
  collect_iam "$c_run"
  collect_ecr "$c_run"
  collect_ssm_secrets "$c_run"
  collect_tagging_api "$c_run"
}

# Fast Launch: AMIs with a Fast Launch configuration, and the snapshots / launch templates /
# instances EC2 creates for it (tag CreatedBy=EC2 Fast Launch). Resources are attributed to
# this run when they carry our tags or mention one of the run's AMI IDs; the rest are listed as
# "unattributed" and still count as leftovers (conservative).
collect_fast_launch() {
  cf_run=$1
  cf_f="$(e2e_ec2_tag_filters "$cf_run") Name=tag:cucina:expires,Values=$E2E_EXPIRES"
  : >"$work/ours-amis"
  : >"$work/ours-lts"
  # shellcheck disable=SC2086
  if aws_json "$work/r.json" ec2 describe-images --owners self --filters $cf_f; then
    jq -r '.Images[].ImageId' "$work/r.json" >"$work/ours-amis"
  fi
  # shellcheck disable=SC2086
  if aws_json "$work/r.json" ec2 describe-launch-templates --filters $cf_f; then
    jq -r '.LaunchTemplates[].LaunchTemplateId' "$work/r.json" >"$work/ours-lts"
  fi

  if aws_json "$work/r.json" ec2 describe-fast-launch-images; then
    jq -r --rawfile mine "$work/ours-amis" '
      ($mine | split("\n") | map(select(. != ""))) as $amis
      | .FastLaunchImages[] | select(.State != "disabled")
      | [.ImageId, .State, (if (.ImageId as $i | $amis | index($i)) then "ours" else "unattributed" end)] | @tsv' "$work/r.json" | add_rows fastlaunch-image
  else
    note_api_error "ec2 describe-fast-launch-images"
  fi

  if aws_json "$work/r.json" ec2 describe-snapshots --owner-ids self --filters "Name=tag:CreatedBy,Values=$FASTLAUNCH_TAG_VALUE"; then
    # Read-only attribution requires BOTH independently tagged parents and exact AWS child lineage. Missing
    # child campaign tags remain visible; conflicting tags are not authority to relabel another run's resources.
    jq -r --arg run "$cf_run" --arg expires "$E2E_EXPIRES" --rawfile mine "$work/ours-amis" --rawfile lts "$work/ours-lts" "$has_tags_jq"'
      ($mine | split("\n") | map(select(. != ""))) as $amis
      | ($lts | split("\n") | map(select(. != ""))) as $templates
      | .Snapshots[] | .Description as $description | tagmap_of(.Tags) as $tags
      | ($tags.CreatedByLaunchTemplateId) as $lt
      | ($tags.CreatedBy == "EC2 Fast Launch" and $lt != null and ($templates | index($lt)) != null
         and ($amis | any(. as $a | $description == ("This is Fast Launch snapshot for image " + $a)))
         and (($tags | has("cucina:env") | not) or $tags["cucina:env"] == "e2e")
         and (($tags | has("cucina:run") | not) or $run == "" or $tags["cucina:run"] == $run)
         and (($tags | has("cucina:expires") | not) or $tags["cucina:expires"] == $expires)) as $owned
      | [.SnapshotId, .State, (if $owned then
          (if ($tags | has("cucina:env") and has("cucina:run") and has("cucina:expires")) then "ours" else "ours-missing-tags" end)
          else "unattributed" end)] | @tsv' "$work/r.json" | add_rows fastlaunch-snapshot
  else
    note_api_error "ec2 describe-snapshots (Fast Launch)"
  fi

  if aws_json "$work/r.json" ec2 describe-launch-templates --filters "Name=tag:CreatedBy,Values=$FASTLAUNCH_TAG_VALUE"; then
    jq -r --arg run "$cf_run" --rawfile mine "$work/ours-amis" "$has_tags_jq"'
      ($mine | split("\n") | map(select(. != ""))) as $amis
      | .LaunchTemplates[]
      | (.LaunchTemplateName + (.Tags | tojson)) as $hay
      | [.LaunchTemplateId, .LaunchTemplateName, (if ((.Tags | ours($run)) or ($amis | any(. as $a | $hay | contains($a)))) then "ours" else "unattributed" end)] | @tsv' "$work/r.json" | add_rows fastlaunch-template
  else
    note_api_error "ec2 describe-launch-templates (Fast Launch)"
  fi

  if aws_json "$work/r.json" ec2 describe-instances --filters "Name=tag:CreatedBy,Values=$FASTLAUNCH_TAG_VALUE"; then
    jq -r '.Reservations[].Instances[] | select(.State.Name != "terminated") | [.InstanceId, .State.Name, "fast-launch prep instance"] | @tsv' "$work/r.json" | add_rows fastlaunch-instance
  else
    note_api_error "ec2 describe-instances (Fast Launch)"
  fi

  if aws_json "$work/r.json" ec2 describe-volumes --filters "Name=tag:CreatedBy,Values=$FASTLAUNCH_TAG_VALUE"; then
    jq -r '.Volumes[] | [.VolumeId, .State, "\(.Size)GiB"] | @tsv' "$work/r.json" | add_rows fastlaunch-volume
  else
    note_api_error "ec2 describe-volumes (Fast Launch)"
  fi
}

# IAM has no tag filters on its list calls: list, then check the tags of each candidate.
collect_iam() {
  ci_run=$1
  if aws_json "$work/r.json" iam get-account-authorization-details --filter Role; then
    jq -r --arg run "$ci_run" "$has_tags_jq"'.RoleDetailList[] | select(.Tags | ours($run)) | [.RoleName, .Arn] | @tsv' "$work/r.json" | add_rows iam-role
  else
    note_api_error "iam get-account-authorization-details"
  fi
  if aws_json "$work/r.json" iam list-policies --scope Local; then
    jq -r '.Policies[] | [.PolicyName, .Arn] | @tsv' "$work/r.json" >"$work/policies.tsv"
    while IFS="$(printf '\t')" read -r name arn; do
      [ -n "$arn" ] || continue
      if aws_json "$work/t.json" iam list-policy-tags --policy-arn "$arn"; then
        jq -r --arg run "$ci_run" --arg name "$name" --arg arn "$arn" "$has_tags_jq"'select(.Tags | ours($run)) | [$name, $arn] | @tsv' "$work/t.json" | add_rows iam-policy
      else
        note_api_error "iam list-policy-tags $name"
      fi
    done <"$work/policies.tsv"
  else
    note_api_error "iam list-policies"
  fi
  if aws_json "$work/r.json" iam list-instance-profiles; then
    jq -r '.InstanceProfiles[] | [.InstanceProfileName, .Arn] | @tsv' "$work/r.json" >"$work/profiles.tsv"
    while IFS="$(printf '\t')" read -r name arn; do
      [ -n "$name" ] || continue
      if aws_json "$work/t.json" iam list-instance-profile-tags --instance-profile-name "$name"; then
        jq -r --arg run "$ci_run" --arg name "$name" --arg arn "$arn" "$has_tags_jq"'select(.Tags | ours($run)) | [$name, $arn] | @tsv' "$work/t.json" | add_rows iam-instance-profile
      else
        note_api_error "iam list-instance-profile-tags $name"
      fi
    done <"$work/profiles.tsv"
  else
    note_api_error "iam list-instance-profiles"
  fi
}

collect_ecr() {
  ce_run=$1
  if aws_json "$work/r.json" ecr describe-repositories; then
    jq -r '.repositories[] | [.repositoryName, .repositoryArn] | @tsv' "$work/r.json" >"$work/repos.tsv"
    while IFS="$(printf '\t')" read -r name arn; do
      [ -n "$arn" ] || continue
      if aws_json "$work/t.json" ecr list-tags-for-resource --resource-arn "$arn"; then
        jq -r --arg run "$ce_run" --arg name "$name" --arg arn "$arn" "$has_tags_jq"'select(.tags | ours($run)) | [$name, $arn] | @tsv' "$work/t.json" | add_rows ecr-repository
      else
        note_api_error "ecr list-tags-for-resource $name"
      fi
    done <"$work/repos.tsv"
  else
    note_api_error "ecr describe-repositories"
  fi
}

collect_ssm_secrets() {
  cs_run=$1
  cs_pf="Key=tag:cucina:env,Option=Equals,Values=e2e"
  if [ -n "$cs_run" ]; then
    cs_pf="$cs_pf Key=tag:cucina:run,Option=Equals,Values=$cs_run"
  fi
  # shellcheck disable=SC2086
  if aws_json "$work/r.json" ssm describe-parameters --parameter-filters $cs_pf; then
    jq -r '.Parameters[] | [.Name, .Type] | @tsv' "$work/r.json" | add_rows ssm-parameter
  else
    note_api_error "ssm describe-parameters"
  fi
  if aws_json "$work/r.json" secretsmanager list-secrets --filters Key=tag-key,Values=cucina:env; then
    jq -r --arg run "$cs_run" "$has_tags_jq"'.SecretList[] | select(.Tags | ours($run)) | [.Name, .ARN] | @tsv' "$work/r.json" | add_rows secret
  else
    note_api_error "secretsmanager list-secrets"
  fi
}

# Catch-all: the Resource Groups Tagging API sees every taggable resource (log groups, S3
# buckets, and anything unexpected). IAM resources are indexed in us-east-1. Types already
# inventoried above are skipped so nothing is counted twice.
collect_tagging_api() {
  ct_run=$1
  ct_filters="Key=cucina:env,Values=e2e"
  if [ -n "$ct_run" ]; then
    ct_filters="$ct_filters Key=cucina:run,Values=$ct_run"
  fi
  for ct_region in "$E2E_REGION" us-east-1; do
    # shellcheck disable=SC2086
    if aws_json "$work/r.json" resourcegroupstaggingapi get-resources --region "$ct_region" --tag-filters $ct_filters; then
      jq -r '
        .ResourceTagMappingList[].ResourceARN
        | capture("^arn:[^:]*:(?<svc>[^:]*):[^:]*:[^:]*:(?<rest>.*)$")
        | (.rest | split("/")[0] | split(":")[0]) as $type
        | select(
            ((.svc == "ec2") and ($type | IN("instance","volume","snapshot","image","network-interface","elastic-ip","security-group","security-group-rule","subnet","route-table","internet-gateway","egress-only-internet-gateway","vpc-endpoint","vpc","natgateway","launch-template","key-pair")) | not)
            and ((.svc == "iam") and ($type | IN("role","policy","instance-profile")) | not)
            and ((.svc == "ecr") and ($type == "repository") | not)
            and ((.svc == "ssm") and ($type == "parameter") | not)
            and ((.svc == "secretsmanager") and ($type == "secret") | not)
          )
        | [(.svc + ":" + $type), .rest] | @tsv' "$work/r.json" | add_rows tagged-other
    else
      note_api_error "resourcegroupstaggingapi ($ct_region)"
    fi
  done
}

# --- report ------------------------------------------------------------------------------------

report() {
  total=$(grep -c . "$inv" || true)
  printf 'sweep run=%s region=%s\n' "$RUN" "$E2E_REGION"
  if [ "$total" -eq 0 ]; then
    echo "  no resources found for these tags"
  else
    cut -f1 "$inv" | sort | uniq -c | awk '{ printf "  %-22s %s\n", $2, $1 }'
    echo "leftovers:"
    sort -k1,1 -k2,2 "$inv" | awk -F '\t' '{ n[$1]++; if (n[$1] <= 15) printf "  %-22s %s  %s %s\n", $1, $2, $3, $4 } END { for (t in n) if (n[t] > 15) printf "  %-22s ... and %d more\n", t, n[t] - 15 }'
  fi
  if [ "$api_errors" -gt 0 ]; then
    printf 'RESULT: INCOMPLETE (%s API error(s)); treat as not clean\n' "$api_errors"
    return 2
  fi
  if [ "$total" -gt 0 ]; then
    printf 'RESULT: %s leftover resource(s)\n' "$total"
    return 1
  fi
  echo "RESULT: clean"
  return 0
}

# --- AMI deletion with Fast Launch disabled first ------------------------------------------------

# fast_launch_state <ami>: prints the Fast Launch state, or "none" when the AMI has no Fast
# Launch configuration (the API stops listing an image once disabling has completed).
fast_launch_state() {
  if ! aws_json "$work/fl.json" ec2 describe-fast-launch-images; then
    echo "error"
    return 0
  fi
  jq -r --arg ami "$1" '[.FastLaunchImages[] | select(.ImageId == $ami) | .State] | (.[0] // "none")' "$work/fl.json"
}

wait_fast_launch_disabled() {
  wf_ami=$1
  wf_deadline=$(($(date +%s) + ${FASTLAUNCH_WAIT_SECONDS:-2700}))
  wf_last=""
  while :; do
    wf_state=$(fast_launch_state "$wf_ami")
    case "$wf_state" in
      none | disabled)
        # Configuration disappearance is not proof that replacement-child deletion has completed.
        # Query by AWS lineage independently of campaign tags, since those may never have been reconciled.
        aws_json "$work/fl-children.json" ec2 describe-snapshots --owner-ids self \
          --filters "Name=tag:CreatedBy,Values=$FASTLAUNCH_TAG_VALUE" \
            "Name=description,Values=This is Fast Launch snapshot for image $wf_ami" || return 1
        if [ "$(jq --arg ami "$wf_ami" '[.Snapshots[] | select(.Description == ("This is Fast Launch snapshot for image " + $ami))] | length' "$work/fl-children.json")" -eq 0 ]; then return 0; fi
        e2e_log "  $wf_ami: disabled but Fast Launch children still exist; waiting, not deregistering"
        ;;
      error) ;;
      enabled | enabled-failed | enabling-failed | disabling-failed)
        e2e_log "  $wf_ami: Fast Launch is '$wf_state'; disabling"
        if ! aws ec2 disable-fast-launch --image-id "$wf_ami" >/dev/null 2>"$work/dfl.err"; then
          sed 's/^/    aws: /' "$work/dfl.err" >&2
        fi
        ;;
      *) # enabling or disabling: wait for the transition to settle
        if [ "$wf_state" != "$wf_last" ]; then
          e2e_log "  $wf_ami: Fast Launch is '$wf_state'; waiting"
          wf_last=$wf_state
        fi
        ;;
    esac
    [ "$(date +%s)" -lt "$wf_deadline" ] || {
      e2e_log "  $wf_ami: Fast Launch did not report disabled in time (state: $wf_state)"
      return 1
    }
    sleep 20 || return 1
  done
}

delete_amis() {
  # Destructive paths require all three protective tags; a different expiry is not this campaign's authority.
  da_f="$(e2e_ec2_tag_filters "$RUN") Name=tag:cucina:expires,Values=$E2E_EXPIRES"
  # shellcheck disable=SC2086
  aws_json "$work/amis.json" ec2 describe-images --owners self --filters $da_f || return 1
  jq -r '.Images[].ImageId' "$work/amis.json" >"$work/ami-ids"
  if ! grep -q . "$work/ami-ids"; then
    e2e_log "no AMIs carry the tags of run $RUN"
    return 0
  fi
  da_fail=0
  while read -r ami; do
    [ -n "$ami" ] || continue
    e2e_log "AMI $ami"
    if ! wait_fast_launch_disabled "$ami"; then
      da_fail=1
      continue
    fi
    jq -r --arg ami "$ami" '.Images[] | select(.ImageId == $ami) | .BlockDeviceMappings[]? | .Ebs.SnapshotId // empty' "$work/amis.json" >"$work/snaps-$ami"
    # All three tags are guaranteed by the filter above, and child cleanup completed: deregister.
    if aws ec2 deregister-image --image-id "$ami" >/dev/null 2>"$work/dereg.err"; then
      e2e_log "  deregistered"
    else
      sed 's/^/    aws: /' "$work/dereg.err" >&2
      da_fail=1
      continue
    fi
    while read -r snap; do
      [ -n "$snap" ] || continue
      # Delete a snapshot only if it carries our tags too.
      # shellcheck disable=SC2086
      if aws_json "$work/s.json" ec2 describe-snapshots --snapshot-ids "$snap" --filters $da_f && [ "$(jq '.Snapshots | length' "$work/s.json")" -eq 1 ]; then
        if aws ec2 delete-snapshot --snapshot-id "$snap" >/dev/null 2>"$work/delsnap.err"; then
          e2e_log "  deleted snapshot $snap"
        else
          sed 's/^/    aws: /' "$work/delsnap.err" >&2
          da_fail=1
        fi
      else
        e2e_log "  snapshot $snap does not carry all three campaign tags: left alone"
        da_fail=1
      fi
    done <"$work/snaps-$ami"
  done <"$work/ami-ids"
  return "$da_fail"
}

# --- self test: query only ---------------------------------------------------------------------

# type_counts <file>: "<type> <count>" lines for the current inventory.
type_counts() {
  cut -f1 "$inv" | sort | uniq -c | awk '{ print $2, $1 }' >"$1"
}

# sum_counts <file>: total of the tag-filtered categories. The fastlaunch-* rows are reported for every
# run (they are found by their CreatedBy tag, not by the run's tags), so they are not part of the filter checks.
sum_counts() {
  awk '$1 !~ /^fastlaunch-/ { s += $2 } END { print s + 0 }' "$1"
}

selftest_run() {
  set +e # every check records its own result
  fails=0
  expect() { # expect <description> <command...>: passes when the command succeeds
    st_desc=$1
    shift
    if "$@" >/dev/null 2>&1; then
      printf '  ok    %s\n' "$st_desc"
    else
      printf '  FAIL  %s\n' "$st_desc"
      fails=$((fails + 1))
    fi
  }

  echo "filter construction"
  expect "env-only filter carries only cucina:env=e2e" \
    test "$(e2e_ec2_tag_filters)" = "Name=tag:cucina:env,Values=e2e"
  expect "run filter adds cucina:run=<id>" \
    test "$(e2e_ec2_tag_filters r1)" = "Name=tag:cucina:env,Values=e2e Name=tag:cucina:run,Values=r1"

  echo "tag predicate (jq) against fixtures"
  expect "ours(): env+run match; other run, other env and empty tag lists rejected; env-only accepted when the run is empty" \
    jq -en "$has_tags_jq"'
      ([{Key:"cucina:env",Value:"e2e"},{Key:"cucina:run",Value:"r1"}] | ours("r1"))
      and ([{Key:"cucina:env",Value:"e2e"},{Key:"cucina:run",Value:"r2"}] | ours("r1") | not)
      and ([{Key:"cucina:env",Value:"prod"},{Key:"cucina:run",Value:"r1"}] | ours("r1") | not)
      and ([{key:"cucina:env",value:"e2e"}] | ours(""))
      and ([] | ours("") | not)'

  echo "live queries (read-only): env-only vs env+run vs a run id that does not exist"
  collect "$RUN"
  type_counts "$work/c-run"
  collect "" # queried second: a superset, so resources launched meanwhile cannot invert the comparison
  type_counts "$work/c-env"
  collect "selftest-no-such-run"
  type_counts "$work/c-bogus"
  printf '  %-22s %8s %8s %10s\n' type env-only env+run bogus-run
  cut -d' ' -f1 "$work/c-env" "$work/c-run" "$work/c-bogus" | sort -u >"$work/types"
  while read -r t; do
    e=$(awk -v t="$t" '$1 == t { print $2 }' "$work/c-env")
    r=$(awk -v t="$t" '$1 == t { print $2 }' "$work/c-run")
    b=$(awk -v t="$t" '$1 == t { print $2 }' "$work/c-bogus")
    printf '  %-22s %8s %8s %10s\n' "$t" "${e:-0}" "${r:-0}" "${b:-0}"
  done <"$work/types"
  expect "a run id that does not exist matches nothing (the run filter is applied, not ignored)" \
    test "$(sum_counts "$work/c-bogus")" -eq 0
  expect "env+run never returns more than env-only" \
    test "$(sum_counts "$work/c-run")" -le "$(sum_counts "$work/c-env")"

  # A tag VALUE that does not exist: the env filter must not degrade to a tag-key match.
  bogus_env=0
  for sub in describe-instances describe-volumes describe-security-groups describe-subnets describe-vpcs describe-route-tables; do
    if aws ec2 "$sub" --filters "Name=tag:cucina:env,Values=__selftest-no-such-env__" --output json >"$work/b.json" 2>/dev/null; then
      n=$(jq '[.. | objects | select(has("Tags"))] | length' "$work/b.json")
      bogus_env=$((bogus_env + n))
    else
      bogus_env=$((bogus_env + 1000))
    fi
  done
  expect "a cucina:env value that does not exist matches nothing (sampled EC2 types)" \
    test "$bogus_env" -eq 0
  expect "every query succeeded (no API errors)" \
    test "$api_errors" -eq 0

  if [ "$fails" -gt 0 ]; then
    printf 'selftest: %s check(s) failed\n' "$fails"
    return 1
  fi
  echo "selftest: all checks passed (nothing was created or deleted)"
}

# --- main --------------------------------------------------------------------------------------

if [ "$selftest" = 1 ]; then
  selftest_run
  exit $?
fi

if [ "$mode" = "delete-amis" ]; then
  delete_amis || e2e_log "AMI deletion incomplete; the report below shows what is left"
fi

collect "$RUN"
rc=0
report || rc=$?
exit "$rc"
