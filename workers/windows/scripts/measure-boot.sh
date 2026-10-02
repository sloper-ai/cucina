#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Measures cold start of the Windows worker AMI on real instances (R-POOL-2, NFR-P1), then terminates them:
#   launch -> boot (LastBootUpTime), launch -> SSM online, launch -> cucina-boot finished, launch -> services running.
# With --with-config, user data drops a throwaway bb_worker/bb_runner configuration and starts \cucina\cucina-boot
# again so the shawl services really start (WinFSP mount B:, runner socket). Label runs with --label
# (e.g. slow-path / fast-launch) to compare launches with and without a pre-provisioned Fast Launch snapshot.
#
#   measure-boot.sh [--ami AMI] [--type c7a.2xlarge] [--count 2] [--subnet public|private] [--with-config]
#                   [--label NAME] [--out FILE]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
source "$here/../../linux/scripts/ec2lib.sh"

ami="" type=c7a.2xlarge count=2 subnet_kind=public with_config=0 label=default
out=${CUCINA_DEV_STORAGE:?}/logs/images/boot-measurements.jsonl
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ami) ami=$2; shift 2 ;;
    --type) type=$2; shift 2 ;;
    --count) count=$2; shift 2 ;;
    --subnet) subnet_kind=$2; shift 2 ;;
    --with-config) with_config=1; shift ;;
    --label) label=$2; shift 2 ;;
    --out) out=$2; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
if [[ -z "$ami" ]]; then
  ami=$(aws ec2 describe-images --region "$EC2LIB_REGION" --owners self \
    --filters "Name=tag:cucina:image-family,Values=windows-worker" "Name=state,Values=available" \
    --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)
fi
[[ "$ami" == ami-* ]] || { echo "no windows-worker AMI" >&2; exit 1; }
case "$subnet_kind" in
  public) subnet=$(ec2lib_out public_subnet_id); EC2LIB_PUBLIC_IP=true ;;
  private) subnet=$(ec2lib_out private_subnet_id); EC2LIB_PUBLIC_IP=false ;;
  *) echo "--subnet public|private" >&2; exit 2 ;;
esac
export EC2LIB_PUBLIC_IP
sg=$(ec2lib_out sg_workers)
profile=$(ec2lib_out worker_instance_profile_name)
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
  # shellcheck disable=SC2016 # PowerShell source
  printf '%s\n' '<powershell>' \
    '$bb = "C:\ProgramData\cucina\bb"' \
    '@{ buildDirectoryPath = "B:\"; grpcServers = @(@{ listenPaths = @("C:/ProgramData/cucina/run/runner"); authenticationPolicy = @{ allow = @{} } }) } | ConvertTo-Json -Depth 8 | Set-Content -Encoding ASCII "$bb\runner.json"' \
    '@{ blobstore = @{ contentAddressableStorage = @{ grpc = @{ client = @{ address = "127.0.0.1:9" } } }; actionCache = @{ grpc = @{ client = @{ address = "127.0.0.1:9" } } } }; maximumMessageSizeBytes = 16777216; scheduler = @{ address = "127.0.0.1:9" }; buildDirectories = @(@{ virtual = @{ mount = @{ mountPath = "\\.\B:"; winfsp = @{} }; maximumExecutionTimeoutCompensation = "3600s"; maximumWritableFileUploadDelay = "60s"; caseInsensitive = $true }; runners = @(@{ endpoint = @{ address = "unix:C:/ProgramData/cucina/run/runner" }; concurrency = 1; maximumFilePoolFileCount = 10000; maximumFilePoolSizeBytes = 1073741824; platform = @{}; workerId = @{ node = "boot-measurement" } }) }); filePool = @{ blockDevice = @{ file = @{ path = "C:\bb\filepool\filepool"; sizeBytes = 1073741824 } } }; inputDownloadConcurrency = 10; outputUploadConcurrency = 11; directoryCache = @{ maximumCount = 1000; maximumSizeBytes = 1048576; cacheReplacementPolicy = "LEAST_RECENTLY_USED" } } | ConvertTo-Json -Depth 12 | Set-Content -Encoding ASCII "$bb\worker.json"' \
    '# images built before the agent alignment read bb_{worker,runner}.json' \
    'Copy-Item "$bb\runner.json" "$bb\bb_runner.json"; Copy-Item "$bb\worker.json" "$bb\bb_worker.json"' \
    'if (Get-Service cucina-boot -ErrorAction SilentlyContinue) { Start-Service cucina-boot } else { Start-ScheduledTask -TaskPath "\cucina\" -TaskName "cucina-boot" }' \
    '</powershell>' >"$userdata"
fi

probe=$(mktemp)
# shellcheck disable=SC2016 # PowerShell source
cat >"$probe" <<'PS'
function U([datetime]$d) { if ($d) { '{0:F3}' -f (([DateTimeOffset]$d.ToUniversalTime()).ToUnixTimeMilliseconds() / 1000.0) } }
$deadline = (Get-Date).AddMinutes(4)
while ((Get-Date) -lt $deadline -and -not (Test-Path 'C:\ProgramData\cucina\run\boot-complete') -and -not (Test-Path 'C:\ProgramData\cucina\run\bootstrap-failed') -and -not (Test-Path 'C:\ProgramData\cucina\run\not-a-worker')) { Start-Sleep -Seconds 1 }
if (Test-Path 'C:\ProgramData\cucina\bb\worker.json') {
  $deadline = (Get-Date).AddMinutes(3)
  while ((Get-Date) -lt $deadline -and (Get-Service cucina-bb-worker).Status -ne 'Running') { Start-Sleep -Seconds 1 }
  $deadline = (Get-Date).AddSeconds(30)
  while ((Get-Date) -lt $deadline -and -not (Test-Path 'B:\')) { Start-Sleep -Milliseconds 500 }
}
$os = Get-CimInstance Win32_OperatingSystem
"T_BOOT=$(U $os.LastBootUpTime)"
$log = Get-Content 'C:\ProgramData\cucina\logs\boot.log', 'C:\bb\log\boot.log' -ErrorAction SilentlyContinue
$start = $log | Where-Object { $_ -match 'boot: start' } | Select-Object -Last 1
$done = $log | Where-Object { $_ -match 'boot: (complete|no EC2 user data)' } | Select-Object -Last 1
if ($start) { "T_BOOTTASK_START=$(U ([datetime]::Parse($start.Split(' ')[0]).ToUniversalTime()))" }
if ($done) { "T_BOOTTASK_DONE=$(U ([datetime]::Parse($done.Split(' ')[0]).ToUniversalTime()))" }
$w = Get-Process bb_worker -ErrorAction SilentlyContinue | Select-Object -First 1
if ($w) { "T_BBWORKER=$(U $w.StartTime)" }
$ssm = Get-Process amazon-ssm-agent -ErrorAction SilentlyContinue | Select-Object -First 1
if ($ssm) { "T_SSMPROC=$(U $ssm.StartTime)" }
$ready = Select-String -Path 'C:\ProgramData\Amazon\EC2Launch\log\agent.log' -Pattern 'Windows is ready' -ErrorAction SilentlyContinue | Select-Object -Last 1
if ($ready -and $ready.Line -match '^(\S+ \S+)') { "T_READY=$(U ([datetime]::Parse($Matches[1]).ToUniversalTime()))" }
"SERVICES=" + ((Get-Service cucina-* | ForEach-Object { "$($_.Name):$($_.Status)" }) -join ',')
"WINFSP=$(Test-Path 'B:\')"
$cpu = (Get-Counter '\Processor(_Total)\% Processor Time' -SampleInterval 2 -MaxSamples 10 -ErrorAction SilentlyContinue).CounterSamples
if ($cpu) { "CPU_BUSY=" + [math]::Round(($cpu | Measure-Object CookedValue -Average).Average, 1) }
"BOOTLOG=" + (($log | Select-Object -Last 6) -join ' | ')
PS

trap 'rm -f "$probe" ${userdata:+"$userdata"}' EXIT
for i in $(seq 1 "$count"); do
  fl_state=$(aws ec2 describe-fast-launch-images --region "$EC2LIB_REGION" --image-ids "$ami" --query 'FastLaunchImages[0].State' --output text 2>/dev/null || echo none)
  t0=$(ec2lib_now)
  iid=$(ec2lib_launch "$ami" "$type" "$subnet" "$sg" "$profile" "cucina-boot-windows-worker" "$userdata")
  trap 'ec2lib_terminate "$iid" || true; rm -f "$probe" ${userdata:+"$userdata"}' EXIT
  ec2lib_log "measure-boot: windows #$i $iid ($type, $subnet_kind, $label, fast-launch=$fl_state)"
  if ! ssm=$(ec2lib_wait_ssm "$iid" "$t0" 1500); then
    ec2lib_log "measure-boot: $iid never came online in SSM"
    ec2lib_terminate "$iid"
    continue
  fi
  res=$(ec2lib_ssm_run "$iid" AWS-RunPowerShellScript "$probe" 600 | tr -d '\r' || true)
  ec2lib_terminate "$iid"
  kv() { sed -n "s/^$1=//p" <<<"$res" | head -n 1; }
  rel() { local v; v=$(kv "$1"); [[ -n "$v" ]] && python3 -c "print(round(float('$v') - $t0, 1))" || echo null; }
  json=$(jq -nc --arg ami "$ami" --arg type "$type" --arg subnet "$subnet_kind" --arg label "$label" --arg fl "$fl_state" \
    --argjson cfg "$with_config" --arg iid "$iid" --argjson ssm "$ssm" --argjson boot "$(rel T_BOOT)" \
    --argjson bstart "$(rel T_BOOTTASK_START)" --argjson bdone "$(rel T_BOOTTASK_DONE)" --argjson ready "$(rel T_READY)" \
    --argjson bbw "$(rel T_BBWORKER)" --argjson ssmproc "$(rel T_SSMPROC)" --arg services "$(kv SERVICES)" \
    --arg winfsp "$(kv WINFSP)" --arg cpu "$(kv CPU_BUSY)" --arg bootlog "$(kv BOOTLOG)" --arg at "$(date -u +%FT%TZ)" '{at: $at, family: "windows-worker",
      ami: $ami, type: $type, subnet: $subnet, label: $label, fast_launch_state: $fl, with_config: ($cfg == 1), instance: $iid,
      launch_to_os_boot_s: $boot, launch_to_ssm_process_s: $ssmproc, launch_to_ssm_online_s: $ssm,
      launch_to_ec2launch_ready_s: $ready, launch_to_boot_task_start_s: $bstart, launch_to_boot_task_done_s: $bdone,
      launch_to_bb_worker_s: $bbw, services: $services, winfsp_mounted: ($winfsp == "True"), cpu_busy_pct_20s: $cpu, boot_log: $bootlog}')
  echo "$json" | tee -a "$out"
done
