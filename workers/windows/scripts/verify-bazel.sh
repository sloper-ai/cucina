#!/usr/bin/env bash
# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Proves that rules_cc's autodetected MSVC toolchain works with the image's VS Build Tools (R-POOL-5): launches
# an instance from the newest windows-base AMI (or --ami), installs nothing (Bazelisk is in the image), builds and
# tests the probe workspace in workers/windows/verify/hello with the §10.2 flags and the pins
# BAZEL_VC / BAZEL_VC_FULL_VERSION / BAZEL_WINSDK_FULL_VERSION from C:\ProgramData\cucina\image\toolchain.json,
# checks that the compile action used exactly those MSVC/SDK directories, then terminates the instance.
#
#   verify-bazel.sh [--ami AMI] [--type c7i.2xlarge] [--result FILE]
# FILE's first line is "pass ..." or "fail ..." (the worker build's optional sysprep gate reads it).
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
source "$here/../../linux/scripts/ec2lib.sh"

ami="" type=c7i.2xlarge
logs=${CUCINA_DEV_STORAGE:?}/logs/images
result="$logs/verify-bazel.result"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ami) ami=$2; shift 2 ;;
    --type) type=$2; shift 2 ;;
    --result) result=$2; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
mkdir -p "$logs"
rm -f "$result"
if [[ -z "$ami" ]]; then
  ami=$(aws ec2 describe-images --region "$EC2LIB_REGION" --owners self \
    --filters "Name=tag:cucina:image-family,Values=windows-base" "Name=state,Values=available" \
    --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)
fi
[[ "$ami" == ami-* ]] || { echo "no windows-base AMI found" >&2; exit 1; }

# PowerShell run through SSM (as SYSTEM): recreate the probe workspace, build, test, inspect the compile action.
probe="$here/../verify/hello"
script=$(mktemp)
trap 'rm -f "$script"' EXIT
# shellcheck disable=SC2016,SC2028 # PowerShell source, not shell expansions
{
  echo '$ErrorActionPreference = "Continue"; $ProgressPreference = "SilentlyContinue"'
  echo '$ws = "C:\b\probe"; Remove-Item -Recurse -Force $ws -ErrorAction SilentlyContinue; New-Item -ItemType Directory -Force $ws | Out-Null'
  for pair in "MODULE.bazel.probe:MODULE.bazel" "BUILD.bazel.probe:BUILD.bazel" "bazelrc.probe:.bazelrc" \
    "bazelversion.probe:.bazelversion" "hello.cc:hello.cc" "hello_test.cc:hello_test.cc"; do
    printf '[IO.File]::WriteAllBytes((Join-Path $ws "%s"), [Convert]::FromBase64String("%s"))\n' \
      "${pair#*:}" "$(base64 <"$probe/${pair%%:*}" | tr -d '\n')"
  done
  cat <<'PS'
$tc = Get-Content -Raw C:\ProgramData\cucina\image\toolchain.json | ConvertFrom-Json
$env:BAZELISK_HOME = 'C:\b\bazelisk'
$env:BAZEL_SH = 'C:\Program Files\Git\usr\bin\bash.exe'
$env:Path = 'C:\tools\bin;C:\Program Files\Git\cmd;' + $env:Path
Set-Location $ws
$repoEnv = @("--repo_env=BAZEL_VC=$($tc.bazel.BAZEL_VC)", "--repo_env=BAZEL_VC_FULL_VERSION=$($tc.bazel.BAZEL_VC_FULL_VERSION)",
  "--repo_env=BAZEL_WINSDK_FULL_VERSION=$($tc.bazel.BAZEL_WINSDK_FULL_VERSION)")
$fail = @()
$t0 = Get-Date
& bazel.exe version 2>&1 | Select-String 'Build label' | ForEach-Object { "bazel: $_" }
& bazel.exe build @repoEnv //:hello 2>&1 | Select-Object -Last 15
if ($LASTEXITCODE -ne 0) { $fail += 'build' }
$run = & "$ws\bazel-bin\hello.exe" 2>&1
"run: $run"
if ($run -notmatch 'hello from rules_cc') { $fail += 'run' }
& bazel.exe test @repoEnv //:hello_test 2>&1 | Select-Object -Last 8
if ($LASTEXITCODE -ne 0) { $fail += 'test' }
$aq = ((& bazel.exe aquery @repoEnv 'mnemonic("CppCompile", //:hello)' --output=text 2>$null) -join "`n").Replace('\\', '/').Replace('\', '/')
$msvcDir = "MSVC/$($tc.bazel.BAZEL_VC_FULL_VERSION)/bin/HostX64/x64/cl.exe"
$sdkInc = "Include/$($tc.bazel.BAZEL_WINSDK_FULL_VERSION)/um"
if ($aq -match [regex]::Escape($msvcDir)) { "aquery: compiler $msvcDir" } else { $fail += 'compiler-path'; "aquery: compiler path not found; first lines:"; ($aq -split "`n" | Select-Object -First 12) }
# INCLUDE/LIB reach cl.exe through the action environment (not the command line): read them from aquery's proto.
$aqj = ((& bazel.exe aquery @repoEnv 'mnemonic("CppCompile", //:hello)' --output=jsonproto 2>$null) -join "`n") | ConvertFrom-Json
$inc = ''
foreach ($a in @($aqj.actions)) {
  foreach ($e in @($a.environmentVariables)) { if ($e -and $e.key -eq 'INCLUDE') { $inc = [string]$e.value } }
}
"include env: $($inc.Substring(0, [Math]::Min(400, $inc.Length)))"
# Collapse separators: the toolchain writes some components with doubled backslashes ("10\\include").
if (($inc -replace '[\\/]+', '/') -match [regex]::Escape($sdkInc)) { "aquery: sdk include $sdkInc" } else { $fail += 'sdk-path' }
"elapsed: {0:N0} s" -f ((Get-Date) - $t0).TotalSeconds
"toolchain: MSVC $($tc.msvc_version) (cl $($tc.cl_exe_version)), SDK $($tc.windows_sdk_version), VS $($tc.vs_display_version)"
if ($fail.Count -eq 0) { 'RESULT: pass' } else { 'RESULT: fail ' + ($fail -join ',') }
& bazel.exe shutdown 2>$null | Out-Null
PS
} >"$script"

subnet=$(ec2lib_out public_subnet_id)
sg=$(ec2lib_out sg_builders)
profile=$(ec2lib_out client_instance_profile_name)
t0=$(ec2lib_now)
iid=$(ec2lib_launch "$ami" "$type" "$subnet" "$sg" "$profile" cucina-verify-bazel)
trap 'ec2lib_terminate "$iid" || true; rm -f "$script"' EXIT
ec2lib_log "verify-bazel: launched $iid from $ami ($type)"
ssm_s=$(ec2lib_wait_ssm "$iid" "$t0" 1200) || { echo "fail ssm-timeout $ami" >"$result"; exit 1; }
ec2lib_log "verify-bazel: SSM online after ${ssm_s}s; running probe"
out="$logs/verify-bazel-$(date -u +%Y%m%dT%H%M%SZ).log"
ec2lib_ssm_run "$iid" AWS-RunPowerShellScript "$script" 3600 >"$out" || true
grep -E '^(bazel:|run:|aquery:|include env:|elapsed:|toolchain:|RESULT:)' "$out" || tail -n 30 "$out"
if grep -q '^RESULT: pass' "$out"; then
  echo "pass $ami $(grep '^toolchain:' "$out" | head -1)" >"$result"
else
  echo "fail $ami $(grep '^RESULT:' "$out" | head -1)" >"$result"
  exit 1
fi
