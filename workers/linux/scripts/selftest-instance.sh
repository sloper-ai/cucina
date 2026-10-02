#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Launches one instance from a worker AMI (Linux or Windows, taken from the AMI's cucina:os tag) without user data,
# waits for SSM, runs `cucina-worker-agent selftest` and checks that the boot ended in the "not a worker" state
# (bootstrap exit 2: Buildbarn down, instance up), then terminates the instance. Prints the selftest JSON.
#
#   selftest-instance.sh --ami AMI [--type TYPE] [--subnet public|private]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
source "$here/ec2lib.sh"

ami="" type="" subnet_kind=public
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ami) ami=$2; shift 2 ;;
    --type) type=$2; shift 2 ;;
    --subnet) subnet_kind=$2; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ "$ami" == ami-* ]] || { echo "--ami is required" >&2; exit 2; }
read -r os arch < <(aws ec2 describe-images --region "$EC2LIB_REGION" --image-ids "$ami" --output text \
  --query "Images[0].[Tags[?Key=='cucina:os'].Value | [0], Architecture]")
if [[ -z "$type" ]]; then
  case "$os/$arch" in
    windows/*) type=c7a.xlarge ;;
    */arm64) type=c7g.large ;;
    *) type=c7i.large ;;
  esac
fi
case "$subnet_kind" in
  public) subnet=$(ec2lib_out public_subnet_id); EC2LIB_PUBLIC_IP=true ;;
  private) subnet=$(ec2lib_out private_subnet_id); EC2LIB_PUBLIC_IP=false ;;
  *) echo "--subnet public|private" >&2; exit 2 ;;
esac
export EC2LIB_PUBLIC_IP

script=$(mktemp)
if [[ "$os" == windows ]]; then
  doc=AWS-RunPowerShellScript
  # shellcheck disable=SC2016 # PowerShell source
  cat >"$script" <<'PS'
$deadline = (Get-Date).AddMinutes(4)
while ((Get-Date) -lt $deadline -and -not (Test-Path 'C:\ProgramData\cucina\run\not-a-worker') -and -not (Test-Path 'C:\ProgramData\cucina\run\bootstrap-failed')) { Start-Sleep -Seconds 2 }
& 'C:\bb\bin\cucina-worker-agent.exe' selftest --log-file= --out C:\Windows\Temp\selftest.json 2>&1 | Out-Null
"SELFTEST_EXIT=$LASTEXITCODE"
"SELFTEST=" + ((Get-Content -Raw C:\Windows\Temp\selftest.json) -replace '\s+', ' ')
"NOT_A_WORKER=$(Test-Path 'C:\ProgramData\cucina\run\not-a-worker')"
"SERVICES=" + ((Get-Service cucina-* | ForEach-Object { "$($_.Name):$($_.Status)" }) -join ',')
"BOOTLOG=" + ((Get-Content 'C:\ProgramData\cucina\logs\boot.log' -ErrorAction SilentlyContinue | Select-Object -Last 5) -join ' | ')
PS
else
  doc=AWS-RunShellScript
  cat >"$script" <<'SH'
timeout 120 systemctl is-system-running --wait >/dev/null 2>&1 || true
# The fastest images reach `running` before chrony's first sync (~5-8 s after boot); wait like a real smoke test.
chronyc waitsync 30 0.1 >/dev/null 2>&1 || true
rc=0; /opt/cucina/bin/cucina-worker-agent selftest --out /tmp/selftest.json >/dev/null 2>&1 || rc=$?
echo "SELFTEST_EXIT=$rc"
echo "SELFTEST=$(tr -s ' \n' ' ' </tmp/selftest.json)"
echo "NOT_A_WORKER=$(test -e /run/cucina/not-a-worker && echo True || echo False)"
echo "SERVICES=$(systemctl is-active bb-runner bb-worker cucina-worker-agent | tr '\n' ',')"
echo "BOOTLOG=$(journalctl -b -u bb-runner -o cat --no-pager | grep -E 'not a worker|bootstrap' | tail -n 3 | cut -c 1-200 | tr '\n' '|')"
SH
fi

t0=$(ec2lib_now)
iid=$(ec2lib_launch "$ami" "$type" "$subnet" "$(ec2lib_out sg_workers)" "$(ec2lib_out worker_instance_profile_name)" "cucina-selftest-$os")
trap 'ec2lib_terminate "$iid" || true; rm -f "$script"' EXIT
ec2lib_log "selftest-instance: $iid ($os/$arch, $type) from $ami"
ssm=$(ec2lib_wait_ssm "$iid" "$t0" 1500)
ec2lib_log "selftest-instance: SSM online after ${ssm}s"
out=$(ec2lib_ssm_run "$iid" "$doc" "$script" 600 | tr -d '\r')
kv() { sed -n "s/^$1=//p" <<<"$out" | head -n 1; }
sed -n 's/^SELFTEST=//p' <<<"$out" | jq -c '{ok, version, checks: [.checks[] | {name, ok, detail: (.detail | .[0:80])}]}'
echo "not-a-worker: $(kv NOT_A_WORKER); services: $(kv SERVICES)"
echo "boot: $(kv BOOTLOG)"
[[ "$(kv SELFTEST_EXIT)" == 0 && "$(kv NOT_A_WORKER)" == True ]]
