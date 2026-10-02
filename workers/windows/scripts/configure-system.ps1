# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Machine configuration shared by the Windows worker and windows-client images (base stage, R-POOL-5).

.DESCRIPTION
  * LongPathsEnabled=1, short roots (C:\bb), 8.3 names and last-access updates off.
  * Developer Mode (unprivileged symlinks with SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE).
  * Windows Update and other post-boot CPU hogs disabled (policies, services, scheduled tasks).
  * Microsoft Defender: path/process exclusions for build, cache and toolchain paths (DefenderMode=exclusions,
    the default) - see docs/operations/images.md for the Dev Drive alternative and the trade-off.
  * No error-reporting UI (a crashing test must not hang an action on a modal dialog).
  * EC2Launch v2 trimmed to the tasks a worker needs; SSM Agent kept and set to dual-stack endpoints.
  * Time sync against the Amazon Time Sync Service (169.254.169.123).
  Idempotent; safe to re-run.
#>
[CmdletBinding()]
param(
  [ValidateSet('exclusions', 'devdrive')][string]$DefenderMode = 'exclusions'
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

function Set-RegistryValue {
  param([string]$Path, [string]$Name, $Value, [string]$Type = 'DWord')
  if (-not (Test-Path $Path)) { New-Item -Path $Path -Force | Out-Null }
  New-ItemProperty -Path $Path -Name $Name -Value $Value -PropertyType $Type -Force | Out-Null
}

function Disable-ServiceIfPresent {
  param([string]$Name)
  $svc = Get-Service -Name $Name -ErrorAction SilentlyContinue
  if (-not $svc) { return }
  try {
    if ($svc.Status -ne 'Stopped') { Stop-Service -Name $Name -Force -ErrorAction Stop }
  } catch { Write-Output "note: could not stop ${Name}: $($_.Exception.Message)" }
  # Some services (e.g. WaaSMedicSvc, UsoSvc) refuse Set-Service; fall back to the registry Start value.
  try {
    Set-Service -Name $Name -StartupType Disabled -ErrorAction Stop
  } catch {
    try {
      Set-RegistryValue -Path "HKLM:\SYSTEM\CurrentControlSet\Services\$Name" -Name 'Start' -Value 4
    } catch { Write-Output "note: could not disable ${Name}: $($_.Exception.Message)" }
  }
  Write-Output "service ${Name}: disabled"
}

function Disable-TasksIfPresent {
  param([string]$TaskPath, [string]$TaskName = '*')
  $tasks = @(Get-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName -ErrorAction SilentlyContinue)
  foreach ($t in $tasks) {
    try {
      Disable-ScheduledTask -TaskPath $t.TaskPath -TaskName $t.TaskName -ErrorAction Stop | Out-Null
      Write-Output "task $($t.TaskPath)$($t.TaskName): disabled"
    } catch { Write-Output "note: could not disable task $($t.TaskPath)$($t.TaskName): $($_.Exception.Message)" }
  }
}

# --- File system: long paths, short roots, cheap metadata ------------------------------------------------
Set-RegistryValue -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' -Name 'LongPathsEnabled' -Value 1
& fsutil.exe behavior set disable8dot3 1 | Out-Null
& fsutil.exe behavior set disablelastaccess 1 | Out-Null
foreach ($d in @('C:\bb', 'C:\bb\bin', 'C:\bb\tmp', 'C:\bb\log', 'C:\ProgramData\cucina', 'C:\ProgramData\cucina\image', 'C:\tools\bin')) {
  New-Item -ItemType Directory -Force -Path $d | Out-Null
}
# C:\bb: SYSTEM/Administrators full, Users read+execute (no inheritance of C:\'s "Users may create folders").
& icacls.exe 'C:\bb' /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-32-545:(OI)(CI)RX' | Out-Null
# C:\bb\tmp is the TMP/TEMP shared by windows-client and workers (Bazel passes the client's TMP to MSVC actions).
& icacls.exe 'C:\bb\tmp' /grant '*S-1-5-32-545:(OI)(CI)M' | Out-Null

# --- Developer Mode (symlinks without SeCreateSymbolicLinkPrivilege) -------------------------------------
Set-RegistryValue -Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock' -Name 'AllowDevelopmentWithoutDevLicense' -Value 1
Set-RegistryValue -Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock' -Name 'AllowAllTrustedApps' -Value 1

# --- Windows Update off (automatic checks cost 50-99 % CPU right after boot) ------------------------------
$wu = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate'
Set-RegistryValue -Path "$wu\AU" -Name 'NoAutoUpdate' -Value 1
Set-RegistryValue -Path "$wu\AU" -Name 'AUOptions' -Value 1
Set-RegistryValue -Path "$wu\AU" -Name 'NoAutoRebootWithLoggedOnUsers' -Value 1
Set-RegistryValue -Path $wu -Name 'DisableWindowsUpdateAccess' -Value 1
Set-RegistryValue -Path $wu -Name 'DoNotConnectToWindowsUpdateInternetLocations' -Value 1
Set-RegistryValue -Path 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\DeliveryOptimization' -Name 'DODownloadMode' -Value 0
foreach ($s in @('wuauserv', 'UsoSvc', 'WaaSMedicSvc', 'DoSvc')) { Disable-ServiceIfPresent -Name $s }
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\WindowsUpdate\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\UpdateOrchestrator\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\WaaSMedic\'

# --- Other post-boot CPU/IO hogs ---------------------------------------------------------------------------
foreach ($s in @('DiagTrack', 'dmwappushservice', 'MapsBroker', 'SysMain', 'WSearch', 'edgeupdate', 'edgeupdatem',
    'MicrosoftEdgeElevationService', 'WerSvc', 'PcaSvc', 'lfsvc', 'RetailDemo', 'XblAuthManager', 'XblGameSave')) {
  Disable-ServiceIfPresent -Name $s
}
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Application Experience\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Customer Experience Improvement Program\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Defrag\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\DiskCleanup\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Server Manager\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Maintenance\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Windows Error Reporting\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Chkdsk\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Data Integrity Scan\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Feedback\Siuf\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Flighting\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\StateRepository\'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Windows Defender\' -TaskName 'Windows Defender Scheduled Scan'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Windows Defender\' -TaskName 'Windows Defender Cache Maintenance'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Windows Defender\' -TaskName 'Windows Defender Cleanup'
Disable-TasksIfPresent -TaskPath '\Microsoft\Windows\Windows Defender\' -TaskName 'Windows Defender Verification'
Disable-TasksIfPresent -TaskPath '\' -TaskName 'MicrosoftEdgeUpdateTaskMachine*'
# Automatic maintenance (defrag, cleanup, ngen idle tasks) never on ephemeral workers.
Set-RegistryValue -Path 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Schedule\Maintenance' -Name 'MaintenanceDisabled' -Value 1
Set-RegistryValue -Path 'HKLM:\SOFTWARE\Microsoft\ServerManager' -Name 'DoNotOpenServerManagerAtLogon' -Value 1
Set-RegistryValue -Path 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\DataCollection' -Name 'AllowTelemetry' -Value 0
& powercfg.exe /setactive SCHEME_MIN | Out-Null
& powercfg.exe /hibernate off | Out-Null
# Small memory dumps only: an "automatic" kernel dump would make the system-managed pagefile track RAM size.
Set-RegistryValue -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\CrashControl' -Name 'CrashDumpEnabled' -Value 3

# --- No modal error UI (a crashing test binary must fail, not hang) ----------------------------------------
Set-RegistryValue -Path 'HKLM:\SOFTWARE\Microsoft\Windows\Windows Error Reporting' -Name 'DontShowUI' -Value 1
Set-RegistryValue -Path 'HKLM:\SOFTWARE\Microsoft\Windows\Windows Error Reporting' -Name 'Disabled' -Value 1
Set-RegistryValue -Path 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\Windows Error Reporting' -Name 'Disabled' -Value 1
Set-RegistryValue -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Windows' -Name 'ErrorMode' -Value 2

# --- Microsoft Defender ----------------------------------------------------------------------------------
$mp = Get-Command -Name Add-MpPreference -ErrorAction SilentlyContinue
if ($mp) {
  $paths = @('C:\bb', 'C:\b', 'C:\BuildTools', "${env:ProgramFiles(x86)}\Windows Kits", "${env:ProgramFiles(x86)}\Microsoft Visual Studio",
    'C:\ProgramData\cucina', 'C:\tools', "$env:ProgramFiles\Git", 'B:\')
  $procs = @('bb_worker.exe', 'bb_runner.exe', 'cucina-worker-agent.exe', 'shawl.exe', 'bazel.exe', 'bazelisk.exe', 'java.exe',
    'cl.exe', 'link.exe', 'lib.exe', 'ml64.exe', 'rc.exe', 'mt.exe', 'cvtres.exe', 'mspdbsrv.exe', 'vctip.exe',
    'clang.exe', 'clang++.exe', 'clang-cl.exe', 'lld-link.exe', 'ld.lld.exe', 'llvm-ar.exe', 'python.exe', 'git.exe', 'bash.exe')
  try {
    Add-MpPreference -ExclusionPath $paths -ErrorAction Stop
    Add-MpPreference -ExclusionProcess $procs -ErrorAction Stop
    # No scheduled or catch-up scans and no signature update storms on ephemeral workers; real-time protection stays on.
    Set-MpPreference -DisableCatchupFullScan $true -DisableCatchupQuickScan $true -ScanScheduleDay 8 -RemediationScheduleDay 8 `
      -SignatureDisableUpdateOnStartupWithoutEngine $true -ScanAvgCPULoadFactor 10 -ErrorAction Stop
    Write-Output "defender: exclusions set (mode=$DefenderMode)"
  } catch {
    Write-Output "warning: Defender preferences not applied: $($_.Exception.Message)"
  }
} else {
  Write-Output 'defender: not installed'
}

# --- EC2Launch v2: keep only what a worker/client needs ----------------------------------------------------
# Drops setWallpaper (slow WMI queries on every boot) from the shipped agent-config.yml and keeps everything
# else: extendRootPartition (pools may enlarge the root volume), activateWindows (license-included AMIs),
# setDnsSuffix, setAdminAccount (random password, used by GetPasswordData), startSsm, and user data execution
# (EC2 Fast Launch requires it). Edited in place (not replaced) so the file stays valid for the installed
# EC2Launch version; `ec2launch validate` must accept the result, otherwise the original is restored.
$ec2lDir = 'C:\ProgramData\Amazon\EC2Launch\config'
$ec2lExe = Join-Path $env:ProgramFiles 'Amazon\EC2Launch\EC2Launch.exe'
$cfg = Join-Path $ec2lDir 'agent-config.yml'
if (Test-Path $cfg) {
  $orig = Join-Path $ec2lDir 'agent-config.yml.orig'
  if (-not (Test-Path $orig)) { Copy-Item -Force $cfg $orig }
  $lines = Get-Content -Path $orig
  $out = New-Object System.Collections.Generic.List[string]
  $skipIndent = -1
  foreach ($line in $lines) {
    $indent = $line.Length - $line.TrimStart().Length
    if ($skipIndent -ge 0) {
      if ($line.Trim().Length -eq 0) { continue }
      if ($indent -gt $skipIndent) { continue }
      $skipIndent = -1
    }
    if ($line -match '^\s*-\s*task:\s*setWallpaper\s*$') { $skipIndent = $indent; continue }
    $out.Add($line)
  }
  Set-Content -Path $cfg -Value $out -Encoding ASCII
  $valid = $true
  if (Test-Path $ec2lExe) {
    $ErrorActionPreference = 'Continue'
    $res = & $ec2lExe validate 2>&1
    $rc = $LASTEXITCODE
    $ErrorActionPreference = 'Stop'
    if ($rc -ne 0) { $valid = $false; Write-Output ('ec2launch validate failed: ' + ($res -join ' ')) }
  }
  if (-not $valid) {
    Copy-Item -Force $orig $cfg
    Write-Output 'warning: EC2Launch config restored to the original'
  } else {
    Write-Output 'ec2launch: agent-config.yml trimmed:'
    Get-Content $cfg | ForEach-Object { Write-Output "  $_" }
  }
} else {
  Write-Output 'warning: EC2Launch v2 agent-config.yml not found'
}

# --- SSM Agent: dual-stack endpoints (workers may only have IPv6 egress, R-DATA-4) -------------------------
$ssmDir = Join-Path $env:ProgramFiles 'Amazon\SSM'
if (Test-Path $ssmDir) {
  $ssmCfg = Join-Path $ssmDir 'amazon-ssm-agent.json'
  $ssmTemplate = Join-Path $ssmDir 'amazon-ssm-agent.json.template'
  $obj = $null
  foreach ($candidate in @($ssmCfg, $ssmTemplate)) {
    if ($null -eq $obj -and (Test-Path $candidate)) {
      $raw = Get-Content -Raw $candidate
      if ($raw -and $raw.Trim()) { $obj = $raw | ConvertFrom-Json }
    }
  }
  if ($null -eq $obj) { $obj = New-Object psobject }
  if ($null -eq $obj.PSObject.Properties['Agent']) { $obj | Add-Member -NotePropertyName Agent -NotePropertyValue (New-Object psobject) }
  $obj.Agent | Add-Member -Force -NotePropertyName UseDualStackEndpoint -NotePropertyValue $true
  ($obj | ConvertTo-Json -Depth 10) | Set-Content -Path $ssmCfg -Encoding ASCII
  Set-Service -Name AmazonSSMAgent -StartupType Automatic
  Write-Output 'ssm: dual-stack endpoints enabled'
} else {
  Write-Output 'warning: SSM Agent directory not found'
}

# --- Time sync: Amazon Time Sync Service --------------------------------------------------------------------
& w32tm.exe /config '/manualpeerlist:169.254.169.123,0x9' /syncfromflags:manual /reliable:yes /update | Out-Null
Set-Service -Name W32Time -StartupType Automatic
Restart-Service -Name W32Time
& w32tm.exe /resync /nowait | Out-Null
Write-Output ('time: ' + ((& w32tm.exe /query /source) -join ' '))

Write-Output 'configure-system: done'

exit 0
