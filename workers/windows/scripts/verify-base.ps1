# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  End of the base stage: precompile .NET assemblies (so ngen does not burn CPU after every boot), smoke-test the
  MSVC toolchain (vcvars64 + cl + link + run), and write C:\ProgramData\cucina\image\image-base.json with every
  installed version (R-VER-1).
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$ImageVersion,
  [string]$ToolchainJson = 'C:\ProgramData\cucina\image\toolchain.json',
  [string]$ComponentsJson = 'C:\ProgramData\cucina\image\base-components.json',
  [string]$Out = 'C:\ProgramData\cucina\image\image-base.json'
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# --- .NET native images: drain the queue now (VS installs queue many assemblies) ---------------------------
foreach ($fw in @('Framework64', 'Framework')) {
  $ngen = Join-Path $env:windir "Microsoft.NET\$fw\v4.0.30319\ngen.exe"
  if (Test-Path $ngen) {
    $t = Measure-Command { & $ngen executeQueuedItems /nologo /silent | Out-Null }
    Write-Output ("ngen {0}: {1:N0} s" -f $fw, $t.TotalSeconds)
  }
}
Get-ScheduledTask -TaskPath '\Microsoft\Windows\.NET Framework\' -ErrorAction SilentlyContinue |
  ForEach-Object { Disable-ScheduledTask -TaskPath $_.TaskPath -TaskName $_.TaskName | Out-Null }

# --- MSVC smoke test ---------------------------------------------------------------------------------------
$tc = Get-Content -Raw $ToolchainJson | ConvertFrom-Json
$vcvars = Join-Path $tc.bazel.BAZEL_VC 'Auxiliary\Build\vcvars64.bat'
if (-not (Test-Path $vcvars)) { throw "vcvars64.bat missing: $vcvars" }
$smoke = Join-Path $env:TEMP 'cucina-msvc-smoke'
New-Item -ItemType Directory -Force -Path $smoke | Out-Null
@'
#include <cstdio>
#include <windows.h>
int main() { std::printf("hello from MSVC %d, WINVER 0x%04x\n", _MSC_FULL_VER, WINVER); return 0; }
'@ | Set-Content -Path (Join-Path $smoke 'hello.cc') -Encoding ASCII
@"
@echo off
call "$vcvars" >nul
cd /d "$smoke" || exit /b 1
cl /nologo /EHsc /MD /std:c++17 hello.cc /Fe:hello.exe >build.log 2>&1 || exit /b 2
hello.exe
"@ | Set-Content -Path (Join-Path $smoke 'smoke.cmd') -Encoding ASCII
$out = & cmd.exe /d /c (Join-Path $smoke 'smoke.cmd')
if ($LASTEXITCODE -ne 0) {
  Get-Content (Join-Path $smoke 'build.log') -ErrorAction SilentlyContinue | Select-Object -Last 30
  throw "MSVC smoke test failed (exit $LASTEXITCODE)"
}
Write-Output "msvc smoke: $out"
Remove-Item -Recurse -Force $smoke

# --- Version inventory -------------------------------------------------------------------------------------
$nt = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
$ec2launch = Join-Path $env:ProgramFiles 'Amazon\EC2Launch\EC2Launch.exe'
$ec2lVersion = $null
if (Test-Path $ec2launch) { $ec2lVersion = ((& $ec2launch version) -join ' ').Trim() }
$ssm = Join-Path $env:ProgramFiles 'Amazon\SSM\amazon-ssm-agent.exe'
$ssmVersion = $null
if (Test-Path $ssm) { $ssmVersion = ((& $ssm -version) -join ' ').Trim() }
$c = Get-PSDrive -Name C
$inventory = [ordered]@{
  image_version   = $ImageVersion
  os              = [ordered]@{
    product   = $nt.ProductName
    display   = $nt.DisplayVersion
    build     = "$($nt.CurrentBuild).$($nt.UBR)"
    edition   = $nt.EditionID
  }
  ec2launch       = $ec2lVersion
  ssm_agent       = $ssmVersion
  powershell      = $PSVersionTable.PSVersion.ToString()
  toolchain       = $tc
  components      = (Get-Content -Raw $ComponentsJson | ConvertFrom-Json)
  disk_c_used_gib = [math]::Round($c.Used / 1GB, 1)
  disk_c_free_gib = [math]::Round($c.Free / 1GB, 1)
}
$json = $inventory | ConvertTo-Json -Depth 6
$json | Set-Content -Encoding UTF8 -Path $Out
Write-Output $json
# Packer's WinRM file download is unreliable (empty files), so the inventory also goes to the build log as
# base64 chunks; the Makefile reassembles CUCINA_INVENTORY:<n>:<chunk> lines into the local artifact.
$b64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($json))
for ($i = 0; $i * 76 -lt $b64.Length; $i++) {
  Write-Output ('CUCINA_INVENTORY:{0:D4}:{1}' -f $i, $b64.Substring($i * 76, [Math]::Min(76, $b64.Length - $i * 76)))
}
exit 0
