# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Worker stage (R-POOL-5/7, R-SEC-5): pinned bb_worker/bb_runner in C:\bb\bin, shawl-wrapped services, the boot
  orchestration task, the bootstrap hook and the dead-man switch task.

.DESCRIPTION
  Services (Buildbarn and agent services are demand-start; cucina-boot.ps1 starts them after the bootstrap hook):
    cucina-bb-runner     shawl -> bb_runner.exe C:\ProgramData\cucina\bb\runner.json, runs as the virtual account
                         NT SERVICE\cucina-bb-runner (low privilege; build actions inherit it); stop kills the tree.
    cucina-bb-worker     shawl -> bb_worker.exe C:\ProgramData\cucina\bb\worker.json, LocalSystem (WinFSP Mount
                         Manager mounts, L1/filePool); depends on cucina-bb-runner; stop = Ctrl-C (drain), 110 s grace (Spot notice = 120 s).
    cucina-worker-agent  shawl -> cucina-worker-agent.exe supervise, LocalSystem; only started when installed.
    cucina-boot          shawl -> powershell cucina-boot.ps1, LocalSystem, automatic start, runs once per boot.
  Scheduled task (SYSTEM): cucina-deadman (every minute, armed at startup).
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$PinsFile,
  [Parameter(Mandatory)][string]$ImageVersion,
  [Parameter(Mandatory)][string]$Generation,
  [string]$StagingDir = 'C:\Windows\Temp\cucina',
  [ValidateSet('exclusions', 'devdrive')][string]$DefenderMode = 'exclusions'
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Get-VerifiedFile {
  param([Parameter(Mandatory)][string]$Url, [Parameter(Mandatory)][string]$Sha256, [Parameter(Mandatory)][string]$OutFile)
  for ($attempt = 1; $attempt -le 5; $attempt++) {
    try { Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $OutFile; break }
    catch { if ($attempt -eq 5) { throw }; Start-Sleep -Seconds (5 * $attempt) }
  }
  $actual = (Get-FileHash -Algorithm SHA256 -Path $OutFile).Hash
  if ($actual -ne $Sha256.ToUpperInvariant()) {
    Remove-Item -Force $OutFile
    throw "SHA-256 mismatch for ${Url}: expected $Sha256, got $actual"
  }
}

function Invoke-Native {
  param([string]$FilePath, [string[]]$Arguments, [int[]]$OkCodes = @(0))
  $ErrorActionPreference = 'Continue'
  $out = & $FilePath @Arguments 2>&1
  $code = $LASTEXITCODE
  $ErrorActionPreference = 'Stop'
  if ($OkCodes -notcontains $code) { throw "$FilePath $($Arguments -join ' ') failed ($code): $($out -join ' ')" }
  return $out
}

$pins = (Get-Content -Raw $PinsFile | ConvertFrom-Json).pins
$bin = 'C:\bb\bin'
$state = 'C:\ProgramData\cucina'
$shawl = Join-Path $bin 'shawl.exe'
if (-not (Test-Path $shawl)) { throw "shawl missing ($shawl): build the worker stage from the base AMI" }

# --- Directories and ACLs ------------------------------------------------------------------------------
# SIDs: S-1-5-18 SYSTEM, S-1-5-32-544 Administrators, S-1-5-32-545 Users.
$logs = "$state\logs"
foreach ($d in @("$bin", 'C:\bb\run', 'C:\bb\cache', 'C:\bb\filepool', 'C:\bb\tmp', $state, "$state\bb", "$state\pki", "$state\run",
    "$state\image", $logs, "$logs\bb-worker", "$logs\bb-runner", "$logs\agent", "$logs\boot-service")) {
  New-Item -ItemType Directory -Force -Path $d | Out-Null
}
Invoke-Native icacls.exe @($state, '/inheritance:r', '/grant:r', '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F', '*S-1-5-32-545:(OI)(CI)RX') | Out-Null
# Private keys and the L1/filePool backing files: SYSTEM and Administrators only.
foreach ($d in @("$state\pki", 'C:\bb\cache', 'C:\bb\filepool')) {
  Invoke-Native icacls.exe @($d, '/inheritance:r', '/grant:r', '*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F') | Out-Null
}

# --- Binaries ------------------------------------------------------------------------------------------
Get-VerifiedFile -Url $pins.buildbarn.bb_worker.url -Sha256 $pins.buildbarn.bb_worker.sha256 -OutFile (Join-Path $bin 'bb_worker.exe')
Get-VerifiedFile -Url $pins.buildbarn.bb_runner.url -Sha256 $pins.buildbarn.bb_runner.sha256 -OutFile (Join-Path $bin 'bb_runner.exe')
foreach ($f in @('cucina-boot.ps1', 'cucina-bootstrap.ps1', 'cucina-deadman.ps1', 'cucina-format-data-volume.ps1')) {
  Copy-Item -Force (Join-Path $StagingDir "bin\$f") (Join-Path $bin $f)
}
# The agent arrives as cucina-worker-agent.zip.part-* (Packer's WinRM upload fails on large files), a .zip or an .exe.
$parts = @(Get-ChildItem -Path (Join-Path $StagingDir 'bin') -Filter 'cucina-worker-agent.zip.part-*' -File -ErrorAction SilentlyContinue | Sort-Object Name)
if ($parts.Count -gt 0) {
  $zipPath = Join-Path $StagingDir 'bin\cucina-worker-agent.zip'
  $outStream = [IO.File]::Create($zipPath)
  try { foreach ($p in $parts) { $bytes = [IO.File]::ReadAllBytes($p.FullName); $outStream.Write($bytes, 0, $bytes.Length) } } finally { $outStream.Dispose() }
}
foreach ($zip in @(Get-ChildItem -Path (Join-Path $StagingDir 'bin') -Filter 'cucina-worker-agent*.zip' -File -ErrorAction SilentlyContinue)) {
  Expand-Archive -Path $zip.FullName -DestinationPath (Join-Path $StagingDir 'bin') -Force
}
$agentStaged = @(Get-ChildItem -Path (Join-Path $StagingDir 'bin') -Filter 'cucina-worker-agent*.exe' -File -ErrorAction SilentlyContinue)
$agentInstalled = $agentStaged.Count -gt 0
if ($agentInstalled) {
  Copy-Item -Force $agentStaged[0].FullName (Join-Path $bin 'cucina-worker-agent.exe')
  Write-Output ('worker agent: ' + ((& (Join-Path $bin 'cucina-worker-agent.exe') version) -join ' '))
}
Write-Output "binaries installed (worker agent: $agentInstalled)"

# --- Services (shawl) ----------------------------------------------------------------------------------
function Install-ShawlService {
  param([string]$Name, [string]$DisplayName, [string]$Description, [string]$LogUnit, [int]$StopTimeoutMs,
    [string[]]$Command, [string]$Dependencies = '', [switch]$KillTree)
  if (Get-Service -Name $Name -ErrorAction SilentlyContinue) {
    Invoke-Native sc.exe @('stop', $Name) -OkCodes @(0, 1062) | Out-Null
    Invoke-Native sc.exe @('delete', $Name) | Out-Null
    Start-Sleep -Seconds 2
  }
  # Logs (management API contract): command output in C:\ProgramData\cucina\logs\<unit>\<unit>_rCURRENT.log, rotated at
  # 50 MB with one old file kept, and reachable as C:\ProgramData\cucina\logs\<unit>.log (symlink to the current file).
  $a = @('add', '--name', $Name, '--cwd', 'C:\bb', '--log-dir', "$logs\$LogUnit", '--log-as', 'shawl', '--log-cmd-as', $LogUnit,
    '--log-rotate', 'bytes=52428800', '--log-retain', '1', '--stop-timeout', "$StopTimeoutMs", '--restart-delay', '2000')
  if ($Dependencies) { $a += @('--dependencies', $Dependencies) }
  if ($KillTree) { $a += '--kill-process-tree' }
  $a += '--'
  $a += $Command
  Invoke-Native $shawl $a | Out-Null
  Invoke-Native sc.exe @('config', $Name, 'start=', 'demand', 'DisplayName=', $DisplayName) | Out-Null
  Invoke-Native sc.exe @('description', $Name, $Description) | Out-Null
  Invoke-Native sc.exe @('failure', $Name, 'reset=', '3600', 'actions=', 'restart/5000/restart/5000/restart/10000') | Out-Null
  Invoke-Native sc.exe @('failureflag', $Name, '1') | Out-Null
  $link = "$logs\$LogUnit.log"
  # del also removes a dangling link (Test-Path is false for those); mklink keeps the target relative.
  Invoke-Native cmd.exe @('/c', "if exist `"$link`" del /f /q `"$link`"") -OkCodes @(0, 1) | Out-Null
  Invoke-Native cmd.exe @('/c', 'mklink', $link, "$LogUnit\$($LogUnit)_rCURRENT.log") | Out-Null
}

Install-ShawlService -Name 'cucina-bb-runner' -DisplayName 'Cucina Buildbarn runner' `
  -Description 'bb_runner: executes build actions (low-privilege virtual account). Started by cucina-boot.' `
  -LogUnit 'bb-runner' -StopTimeoutMs 30000 -KillTree `
  -Command @("$bin\bb_runner.exe", "$state\bb\runner.json")
Install-ShawlService -Name 'cucina-bb-worker' -DisplayName 'Cucina Buildbarn worker' `
  -Description 'bb_worker: scheduler client, WinFSP build directory, L1 cache. Stop sends Ctrl-C (drain).' `
  -LogUnit 'bb-worker' -StopTimeoutMs 110000 -Dependencies 'cucina-bb-runner' `
  -Command @("$bin\bb_worker.exe", "$state\bb\worker.json")
Install-ShawlService -Name 'cucina-worker-agent' -DisplayName 'Cucina worker agent' `
  -Description 'cucina-worker-agent supervise (dead-man, activity/contact marks, Spot drain). Started by cucina-boot when installed.' `
  -LogUnit 'agent' -StopTimeoutMs 10000 `
  -Command @("$bin\cucina-worker-agent.exe", 'supervise', '--log-file=')

# bb_runner (and therefore every build action) runs as a per-service virtual account: no password, a SID that is
# derived from the service name (stable across sysprep), member of Users only.
$runnerAccount = 'NT SERVICE\cucina-bb-runner'
Invoke-Native sc.exe @('config', 'cucina-bb-runner', 'obj=', $runnerAccount) | Out-Null
$runnerSid = (New-Object System.Security.Principal.NTAccount($runnerAccount)).Translate([System.Security.Principal.SecurityIdentifier]).Value
# The runner socket lives in the agent's run directory (internal/bbconfig RunDir = C:\ProgramData\cucina\run).
foreach ($d in @("$logs\bb-runner", "$state\run", 'C:\bb\tmp')) {
  Invoke-Native icacls.exe @($d, '/grant', "*${runnerSid}:(OI)(CI)M") | Out-Null
}
# Symlink creation inside the WinFSP build directory needs SeCreateSymbolicLinkPrivilege (Developer Mode only
# covers callers that pass SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE).
$tmp = Join-Path $env:TEMP 'cucina-secpol'
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
Invoke-Native secedit.exe @('/export', '/cfg', "$tmp\export.inf", '/areas', 'USER_RIGHTS') | Out-Null
$line = Get-Content "$tmp\export.inf" | Where-Object { $_ -match '^SeCreateSymbolicLinkPrivilege\s*=' }
$holders = $(if ($line) { ($line -split '=', 2)[1].Trim() } else { '*S-1-5-32-544' })
if ($holders -notmatch [regex]::Escape($runnerSid)) { $holders = "$holders,*$runnerSid" }
@"
[Unicode]
Unicode=yes
[Version]
signature="`$CHICAGO`$"
Revision=1
[Privilege Rights]
SeCreateSymbolicLinkPrivilege = $holders
"@ | Set-Content -Path "$tmp\symlink.inf" -Encoding Unicode
Invoke-Native secedit.exe @('/configure', '/db', "$tmp\symlink.sdb", '/cfg', "$tmp\symlink.inf", '/areas', 'USER_RIGHTS', '/quiet') | Out-Null
Remove-Item -Recurse -Force $tmp
Write-Output "runner account $runnerAccount ($runnerSid): SeCreateSymbolicLinkPrivilege granted"

# --- Event log source, scheduled tasks -----------------------------------------------------------------
if (-not [System.Diagnostics.EventLog]::SourceExists('cucina')) { New-EventLog -LogName Application -Source 'cucina' }
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
$psArgs = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File'

# Keep boot orchestration after Windows setup, using the validated startup task (ADR 0302). The automatic-service
# variant produced Fast Launch instances stuck in Windows setup; faster SCM startup is not safe evidence of readiness.
if (Get-Service -Name 'cucina-boot' -ErrorAction SilentlyContinue) {
  Stop-Service -Name 'cucina-boot' -Force -ErrorAction SilentlyContinue
  Invoke-Native sc.exe @('delete', 'cucina-boot') | Out-Null
  Start-Sleep -Seconds 2
}
$bootTrigger = New-ScheduledTaskTrigger -AtStartup
$bootSettings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
  -ExecutionTimeLimit (New-TimeSpan -Minutes 10) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName 'cucina-boot' -TaskPath '\cucina\' -Force -Principal $principal -Settings $bootSettings `
  -Trigger $bootTrigger -Action (New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "$psArgs $bin\cucina-boot.ps1") | Out-Null

# Armed only from the next boot: a startup trigger that repeats every minute (never during the image build).
$dmTrigger = New-ScheduledTaskTrigger -AtStartup
$dmTrigger.Delay = 'PT1M'
$dmTrigger.Repetition = (New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 1)).Repetition
$dmSettings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
  -ExecutionTimeLimit (New-TimeSpan -Minutes 2) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName 'cucina-deadman' -TaskPath '\cucina\' -Force -Principal $principal -Settings $dmSettings `
  -Trigger $dmTrigger -Action (New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "$psArgs $bin\cucina-deadman.ps1") | Out-Null
Write-Output 'startup tasks: \cucina\cucina-boot and \cucina\cucina-deadman'

# --- Defender: worker-specific exclusions ----------------------------------------------------------------
try {
  Add-MpPreference -ExclusionPath @('C:\bb', 'B:\', 'D:\', "$state") -ExclusionProcess @('bb_worker.exe', 'bb_runner.exe', 'shawl.exe', 'cucina-worker-agent.exe') -ErrorAction Stop
} catch { Write-Output "warning: Defender exclusions not applied: $($_.Exception.Message)" }

# --- Image metadata (read by the agent; also recorded in the build artifacts) -------------------------------
$base = $null
$baseJson = Join-Path $state 'image\image-base.json'
if (Test-Path $baseJson) { $base = Get-Content -Raw $baseJson | ConvertFrom-Json }
@{ enabled = $true; idle_limit_seconds = 1800; contact_limit_seconds = 600; max_uptime_seconds = 43200 } |
  ConvertTo-Json | Set-Content -Path "$state\deadman.json" -Encoding ASCII
$image = [ordered]@{
  family        = 'windows-worker'
  image_version = $ImageVersion
  generation    = $Generation
  defender_mode = $DefenderMode
  buildbarn     = [ordered]@{ bb_remote_execution = $pins.buildbarn.bb_remote_execution; bb_worker_sha256 = $pins.buildbarn.bb_worker.sha256; bb_runner_sha256 = $pins.buildbarn.bb_runner.sha256 }
  shawl         = $pins.shawl.version
  worker_agent  = $agentInstalled
  paths         = [ordered]@{
    bin = $bin; bb_config_dir = "$state\bb"; pki_dir = "$state\pki"; run_dir = "$state\run"; logs = $logs
    bb_worker_config = "$state\bb\worker.json"; bb_runner_config = "$state\bb\runner.json"; env_file = "$state\env"
    runner_socket_dir = "$state\run"; tmp = 'C:\bb\tmp'
    build_mount = '\\.\B:'; data_volume_info = "$state\run\data-volume.json"
  }
  services      = [ordered]@{ runner = 'cucina-bb-runner'; worker = 'cucina-bb-worker'; agent = 'cucina-worker-agent'; runner_account = $runnerAccount; runner_sid = $runnerSid }
  base          = $base
}
($image | ConvertTo-Json -Depth 8) | Set-Content -Path "$state\image.json" -Encoding UTF8
Write-Output 'install-worker: done'
exit 0
