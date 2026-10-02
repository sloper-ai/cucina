# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Boot orchestration for EC2 Windows workers; runs once at every startup as LocalSystem (automatic one-shot service
  `cucina-boot`, wrapped by shawl).
  Windows has no ExecStartPre, so this script provides the same ordering as the Linux systemd units:

    1. reset per-boot runtime state (C:\ProgramData\cucina\run, the counterpart of /run/cucina);
    2. format and mount a raw NVMe instance-store disk when the instance type has one (cucina-format-data-volume.ps1);
    3. run the bootstrap hook (cucina-bootstrap.ps1) - fail closed: on error nothing is started;
    4. start cucina-worker-agent (supervise) when installed, then cucina-bb-runner and cucina-bb-worker (the worker
       service depends on the runner service) when their configuration exists, with C:\ProgramData\cucina\env (written
       by the agent) as their environment.
  Bootstrap exit code 2 = no EC2 user data at all (EC2 Fast Launch preparation, image build): "not a worker" - nothing
  is started, the instance stays up, the dead-man switch only applies its uptime limits.

  Services are demand-start; nothing Buildbarn-related runs before this script has bootstrapped the machine.
  Idempotent: re-running it on a booted worker only (re)starts what is not running.
#>
[CmdletBinding()]
param(
  [string]$Root = 'C:\bb',
  [string]$StateRoot = 'C:\ProgramData\cucina'
)

$ErrorActionPreference = 'Stop'
$log = Join-Path $StateRoot 'logs\boot.log'
$run = Join-Path $StateRoot 'run'
$bbConfig = Join-Path $StateRoot 'bb'

function Write-BootLog {
  param([string]$Message)
  $line = '{0} {1}' -f [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ss.fffZ'), $Message
  Add-Content -Path $log -Value $line
  Write-Output $line
}

function Start-ServiceChecked {
  param([string]$Name)
  $svc = Get-Service -Name $Name
  if ($svc.Status -ne 'Running') {
    Start-Service -Name $Name
    $svc.WaitForStatus('Running', [TimeSpan]::FromSeconds(60))
  }
  Write-BootLog "started $Name"
}

try {
  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $log) | Out-Null
  $os = Get-CimInstance -ClassName Win32_OperatingSystem
  $uptime = [int]((Get-Date) - $os.LastBootUpTime).TotalSeconds
  Write-BootLog "boot: start (uptime ${uptime}s)"

  # 1. Runtime state never survives a reboot (and never leaks from an AMI or a Fast Launch snapshot).
  foreach ($d in @($run, (Join-Path $Root 'run'))) {
    # $Root\run holds the bb_worker <-> bb_runner socket; a stale socket file would block the listener.
    if (Test-Path $d) { Get-ChildItem -Path $d -Force | Remove-Item -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $d | Out-Null
  }
  Set-Content -Path (Join-Path $run 'boot-started') -Value ([DateTime]::UtcNow.ToString('o'))

  # 2. Instance store (L1 / filePool candidates); no-op when the type has none. Not fatal: the agent can
  #    still choose an EBS data volume or memory.
  #    With the agent installed it places and formats L1 itself (NTFS); the image only pre-formats a Dev Drive.
  $ErrorActionPreference = 'Continue'   # stderr of child processes must not become terminating errors
  $devDrive = $false
  $imageJson = Join-Path $StateRoot 'image.json'
  if (Test-Path $imageJson) { $devDrive = ((Get-Content -Raw $imageJson | ConvertFrom-Json).defender_mode -eq 'devdrive') }
  if ($devDrive -or -not (Test-Path (Join-Path $Root 'bin\cucina-worker-agent.exe'))) {
    $fmt = & powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File (Join-Path $Root 'bin\cucina-format-data-volume.ps1') 2>&1
    $fmtCode = $LASTEXITCODE
    foreach ($l in @($fmt)) { Write-BootLog "format: $l" }
    if ($fmtCode -ne 0) { Write-BootLog "format: exit $fmtCode (ignored)" }
  }

  # 3. Bootstrap hook in its own process (its failure must not take this script's state with it).
  $hook = Join-Path $Root 'bin\cucina-bootstrap.ps1'
  $out = & powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $hook 2>&1
  $code = $LASTEXITCODE
  $ErrorActionPreference = 'Stop'
  foreach ($l in @($out)) { Write-BootLog "bootstrap: $l" }
  if ($code -eq 2) {
    Set-Content -Path (Join-Path $run 'not-a-worker') -Value ([DateTime]::UtcNow.ToString('o'))
    Write-BootLog 'boot: no EC2 user data: not a worker; Buildbarn services not started, instance stays up'
    exit 0
  }
  if ($code -ne 0) {
    Set-Content -Path (Join-Path $run 'bootstrap-failed') -Value "exit $code"
    Write-BootLog "boot: bootstrap failed (exit $code); Buildbarn services NOT started"
    exit 1
  }

  # Service environment from the agent's env file (KEY='value' lines, systemd EnvironmentFile syntax).
  $envFile = Join-Path $StateRoot 'env'
  if (Test-Path $envFile) {
    $vars = @()
    foreach ($l in Get-Content $envFile) {
      if ($l -match '^\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
        $v = $Matches[2].Trim()
        if ($v.Length -ge 2 -and (($v[0] -eq "'" -and $v[-1] -eq "'") -or ($v[0] -eq '"' -and $v[-1] -eq '"'))) { $v = $v.Substring(1, $v.Length - 2) }
        $vars += ('{0}={1}' -f $Matches[1], $v)
      }
    }
    foreach ($svc in @('cucina-bb-runner', 'cucina-bb-worker')) {
      Set-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\$svc" -Name Environment -Type MultiString -Value $vars
    }
    Write-BootLog "boot: service environment from $envFile ($($vars.Count) variables)"
  }

  # 4. Services.
  if (Test-Path (Join-Path $Root 'bin\cucina-worker-agent.exe')) { Start-ServiceChecked -Name 'cucina-worker-agent' }
  $workerCfg = Join-Path $bbConfig 'worker.json'
  $runnerCfg = Join-Path $bbConfig 'runner.json'
  if ((Test-Path $workerCfg) -and (Test-Path $runnerCfg)) {
    Start-ServiceChecked -Name 'cucina-bb-runner'
    Start-ServiceChecked -Name 'cucina-bb-worker'
  } else {
    Write-BootLog "boot: no Buildbarn configuration in $bbConfig; bb services not started"
  }
  Set-Content -Path (Join-Path $run 'boot-complete') -Value ([DateTime]::UtcNow.ToString('o'))
  Write-BootLog 'boot: complete'
  exit 0
} catch {
  Write-BootLog "boot: error: $($_.Exception.Message)"
  exit 1
}
