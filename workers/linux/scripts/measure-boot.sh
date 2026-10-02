#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Measures cold boot of a Cucina Linux worker AMI on real instances (R-POOL-4, NFR-P1), then terminates them:
#   launch -> kernel start, launch -> `systemctl is-system-running` (startup finished), launch -> SSM online,
#   launch -> bb-worker.service active (with --with-config: a throwaway bb_worker/bb_runner configuration is put
#   in place by cloud-init so the units really start: FUSE mount, runner socket, run_commands_as), plus
#   `systemd-analyze` and the top of `systemd-analyze blame`.
#
#   measure-boot.sh --family linux-ubuntu-x86_64 [--ami AMI] [--type m7i.large] [--count 3]
#                   [--subnet public|private] [--init-rate MiBps] [--with-config] [--out FILE]
# Prints one JSON object per launch (also appended to FILE) and a p50 summary.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
source "$here/ec2lib.sh"

family="" ami="" type="" count=3 subnet_kind=public init_rate=0 with_config=0
out=${CUCINA_DEV_STORAGE:?}/logs/images/boot-measurements.jsonl
while [[ $# -gt 0 ]]; do
  case "$1" in
    --family) family=$2; shift 2 ;;
    --ami) ami=$2; shift 2 ;;
    --type) type=$2; shift 2 ;;
    --count) count=$2; shift 2 ;;
    --subnet) subnet_kind=$2; shift 2 ;;
    --init-rate) init_rate=$2; shift 2 ;;
    --with-config) with_config=1; shift ;;
    --out) out=$2; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ -n "$family" ]] || { echo "--family is required" >&2; exit 2; }
if [[ -z "$ami" ]]; then
  ami=$(aws ec2 describe-images --region "$EC2LIB_REGION" --owners self \
    --filters "Name=tag:cucina:image-family,Values=$family" "Name=state,Values=available" \
    --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)
fi
[[ "$ami" == ami-* ]] || { echo "no AMI for $family" >&2; exit 1; }
if [[ -z "$type" ]]; then
  case "$family" in *arm64) type=m7g.large ;; *) type=m7i.large ;; esac
fi
case "$subnet_kind" in
  public) subnet=$(ec2lib_out public_subnet_id); EC2LIB_PUBLIC_IP=true ;;
  private) subnet=$(ec2lib_out private_subnet_id); EC2LIB_PUBLIC_IP=false ;;
  *) echo "--subnet public|private" >&2; exit 2 ;;
esac
export EC2LIB_PUBLIC_IP
sg=$(ec2lib_out sg_workers)
profile=$(ec2lib_out worker_instance_profile_name)
root_dev=$(aws ec2 describe-images --region "$EC2LIB_REGION" --image-ids "$ami" --query 'Images[0].RootDeviceName' --output text)
root_size=$(aws ec2 describe-images --region "$EC2LIB_REGION" --image-ids "$ami" --query 'Images[0].BlockDeviceMappings[0].Ebs.VolumeSize' --output text)
mkdir -p "$(dirname "$out")"
if ((with_config)) && [[ "$(aws ec2 describe-images --region "$EC2LIB_REGION" --image-ids "$ami" \
  --query "Images[0].Tags[?Key=='cucina:worker-agent'].Value | [0]" --output text)" == yes ]]; then
  # The agent treats any non-Cucina user data as a failed bootstrap and powers the instance off.
  echo "--with-config needs an image without cucina-worker-agent ($ami has it)" >&2
  exit 2
fi

userdata=""
if ((with_config)); then
  userdata=$(mktemp)
  cat >"$userdata" <<'UD'
#cloud-config
# Throwaway Buildbarn configuration for boot measurements only: the scheduler/storage address is unreachable,
# so bb_worker starts, mounts its FUSE build directory and keeps retrying.
write_files:
  - path: /etc/cucina/bb/runner.json
    permissions: "0644"
    content: |
      {"buildDirectoryPath": "/var/lib/cucina/build",
       "grpcServers": [{"listenPaths": ["/run/cucina/runner.sock"], "authenticationPolicy": {"allow": {}}}],
       "runCommandsAs": {"userId": 2000, "groupId": 2000}}
  - path: /etc/cucina/bb/worker.json
    permissions: "0644"
    content: |
      {"blobstore": {"contentAddressableStorage": {"grpc": {"client": {"address": "127.0.0.1:9"}}},
                     "actionCache": {"grpc": {"client": {"address": "127.0.0.1:9"}}}},
       "maximumMessageSizeBytes": 16777216,
       "scheduler": {"address": "127.0.0.1:9"},
       "buildDirectories": [{
         "virtual": {"mount": {"mountPath": "/var/lib/cucina/build", "fuse": {"directoryEntryValidity": "300s", "inodeAttributeValidity": "300s", "allowOther": true, "mountMethod": "DIRECT"}},
                     "maximumExecutionTimeoutCompensation": "3600s", "maximumWritableFileUploadDelay": "60s"},
         "runners": [{"endpoint": {"address": "unix:///run/cucina/runner.sock"}, "concurrency": 1,
                      "maximumFilePoolFileCount": 10000, "maximumFilePoolSizeBytes": 1073741824,
                      "platform": {}, "workerId": {"node": "boot-measurement"}}]}],
       "filePool": {"blockDevice": {"file": {"path": "/var/lib/cucina/filepool/filepool", "sizeBytes": 1073741824}}},
       "inputDownloadConcurrency": 10, "outputUploadConcurrency": 11,
       "directoryCache": {"maximumCount": 1000, "maximumSizeBytes": 1048576, "cacheReplacementPolicy": "LEAST_RECENTLY_USED"}}
UD
fi

probe=$(mktemp)
cat >"$probe" <<'SH'
state=$(timeout 180 systemctl is-system-running --wait 2>/dev/null || true)
ts() { systemctl show "$1" -p ActiveEnterTimestamp --timestamp=unix --value 2>/dev/null | tr -d '@'; }
boot=$(echo "$(date +%s.%N) $(cut -d' ' -f1 /proc/uptime)" | awk '{printf "%.3f", $1 - $2}')
echo "STATE=$state"
echo "T_BOOT=$boot"
echo "T_FINISH=$(systemctl show -p FinishTimestamp --timestamp=unix --value | tr -d '@')"
echo "T_SSM=$(ts amazon-ssm-agent.service)"
echo "T_BBRUNNER=$(ts bb-runner.service)"
echo "T_BBWORKER=$(ts bb-worker.service)"
echo "T_CLOUDFINAL=$(ts cloud-final.service)"
echo "FUSE=$(grep -c ' /var/lib/cucina/build fuse' /proc/mounts || true)"
echo "ANALYZE=$(systemd-analyze 2>/dev/null | head -n 1)"
echo "KERNEL=$(uname -r)"
echo "FAILED=$(systemctl --failed --no-legend --plain | awk '{print $1}' | tr '\n' ' ')"
echo "CRITICAL:"
systemd-analyze critical-chain --no-pager multi-user.target 2>/dev/null | grep -E '@|\+' | sed 's/^[^a-zA-Z0-9]*//' | head -n 14 | tr '\n' ';'
echo
echo "DIAG=$(ls -la /boot/initr* 2>/dev/null | awk '{print $5, $9}' | tr '\n' ' ') | $(grep -h -E '^(hostonly|compress)' /etc/dracut.conf.d/*.conf /usr/lib/dracut/dracut.conf.d/*.conf 2>/dev/null | tr '\n' ' ')"
echo "BBLOG=$(journalctl -b -u bb-worker.service -u bb-runner.service --no-pager -o cat 2>/dev/null | tail -n 4 | cut -c 1-200 | tr '\n' '|')"
echo "BLAME:"
systemd-analyze blame --no-pager 2>/dev/null | grep -v -E '\.device$' | head -n 12
SH

trap 'rm -f "$probe" ${userdata:+"$userdata"}' EXIT
extra=()
if ((init_rate > 0)); then
  extra+=(--block-device-mappings "DeviceName=$root_dev,Ebs={VolumeSize=$root_size,VolumeType=gp3,DeleteOnTermination=true,VolumeInitializationRate=$init_rate}")
fi
for i in $(seq 1 "$count"); do
  t0=$(ec2lib_now)
  iid=$(ec2lib_launch "$ami" "$type" "$subnet" "$sg" "$profile" "cucina-boot-$family" "$userdata" ${extra[@]+"${extra[@]}"})
  trap 'ec2lib_terminate "$iid" || true; rm -f "$probe" ${userdata:+"$userdata"}' EXIT
  ec2lib_log "measure-boot: $family #$i $iid ($type, $subnet_kind, init-rate $init_rate)"
  if ! ssm=$(ec2lib_wait_ssm "$iid" "$t0" 600); then
    ec2lib_log "measure-boot: $iid never came online in SSM"
    ec2lib_terminate "$iid"
    continue
  fi
  res=$(ec2lib_ssm_run "$iid" AWS-RunShellScript "$probe" 300 || true)
  ec2lib_terminate "$iid"
  kv() { sed -n "s/^$1=//p" <<<"$res" | head -n 1; }
  rel() { local v; v=$(kv "$1"); [[ -n "$v" ]] && python3 -c "print(round(float('$v') - $t0, 1))" || echo null; }
  blame=$(sed -n '/^BLAME:/,$p' <<<"$res" | tail -n +2 | head -n 10 | sed 's/^ *//' | jq -R . | jq -sc .)
  critical=$(sed -n '/^CRITICAL:/{n;p;}' <<<"$res" | head -n 1)
  json=$(jq -nc --arg family "$family" --arg ami "$ami" --arg type "$type" --arg subnet "$subnet_kind" --argjson init "$init_rate" \
    --argjson cfg "$with_config" --arg iid "$iid" --argjson ssm "$ssm" --argjson kernel "$(rel T_BOOT)" \
    --argjson finish "$(rel T_FINISH)" --argjson ssm_unit "$(rel T_SSM)" --argjson bbworker "$(rel T_BBWORKER)" \
    --argjson cloudfinal "$(rel T_CLOUDFINAL)" --arg state "$(kv STATE)" --arg analyze "$(kv ANALYZE)" \
    --arg kver "$(kv KERNEL)" --arg failed "$(kv FAILED)" --arg fuse "$(kv FUSE)" --argjson blame "$blame" --arg critical "$critical" --arg diag "$(kv DIAG)" --arg bblog "$(kv BBLOG)" \
    --arg at "$(date -u +%FT%TZ)" '{at: $at, family: $family, ami: $ami, type: $type, subnet: $subnet, init_rate: $init,
      with_config: ($cfg == 1), instance: $iid, launch_to_kernel_s: $kernel, launch_to_running_s: $finish,
      launch_to_ssm_unit_s: $ssm_unit, launch_to_ssm_online_s: $ssm, launch_to_bb_worker_s: $bbworker,
      launch_to_cloud_final_s: $cloudfinal, state: $state, fuse_mounted: ($fuse == "1"), systemd_analyze: $analyze,
      kernel: $kver, failed_units: $failed, blame: $blame, critical_chain: $critical, diag: $diag, bb_log: $bblog}')
  echo "$json" | tee -a "$out"
done
jq -s --arg f "$family" '[.[] | select(.family == $f)] | if length == 0 then empty else
  {family: $f, n: length,
   p50_launch_to_running_s: (map(.launch_to_running_s) | sort | .[(length-1)/2|floor]),
   p50_launch_to_ssm_online_s: (map(.launch_to_ssm_online_s) | sort | .[(length-1)/2|floor]),
   p50_launch_to_kernel_s: (map(.launch_to_kernel_s) | sort | .[(length-1)/2|floor])} end' "$out"
