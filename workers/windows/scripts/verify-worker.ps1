# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Worker-stage checks before sysprep:
    1. service definitions (accounts, start mode, dependency) and scheduled tasks;
    2. the bootstrap hook succeeds trivially without the agent; the dead-man switch evaluates (dry run);
    3. live smoke test with a throwaway configuration: cucina-bb-runner starts as its virtual account and serves
       its socket, cucina-bb-worker starts as LocalSystem and mounts the WinFSP build directory (B:), and stopping
       the worker through the SCM (shawl -> Ctrl-C) completes within the grace period.
  The throwaway configuration, logs and runtime files are removed afterwards. Writes a JSON report.
#>
[CmdletBinding()]
param(
  [string]$Report = 'C:\ProgramData\cucina\image\worker-verify.json'
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$state = 'C:\ProgramData\cucina'
$result = [ordered]@{}

function Assert-True { param([bool]$Condition, [string]$Message) if (-not $Condition) { throw "verify-worker: $Message" } }

# --- 1. Definitions ----------------------------------------------------------------------------------------
$runner = Get-CimInstance -ClassName Win32_Service -Filter "Name='cucina-bb-runner'"
$worker = Get-CimInstance -ClassName Win32_Service -Filter "Name='cucina-bb-worker'"
$agent = Get-CimInstance -ClassName Win32_Service -Filter "Name='cucina-worker-agent'"
Assert-True ($null -ne $runner -and $null -ne $worker -and $null -ne $agent) 'services missing'
Assert-True ($runner.StartName -eq 'NT SERVICE\cucina-bb-runner') "runner account is $($runner.StartName)"
Assert-True ($worker.StartName -eq 'LocalSystem') "worker account is $($worker.StartName)"
Assert-True ($runner.StartMode -eq 'Manual' -and $worker.StartMode -eq 'Manual' -and $agent.StartMode -eq 'Manual') 'services must be demand-start'
$deps = @((Get-Service -Name 'cucina-bb-worker').ServicesDependedOn | ForEach-Object { $_.Name })
Assert-True ($deps -contains 'cucina-bb-runner') 'cucina-bb-worker must depend on cucina-bb-runner'
Assert-True ($null -ne (Get-ScheduledTask -TaskPath '\cucina\' -TaskName 'cucina-deadman' -ErrorAction SilentlyContinue)) 'task cucina-deadman missing'
$boot = Get-CimInstance -ClassName Win32_Service -Filter "Name='cucina-boot'"
Assert-True ($null -ne $boot -and $boot.StartMode -eq 'Auto' -and $boot.StartName -eq 'LocalSystem') 'service cucina-boot must be automatic, LocalSystem'
$result.services = 'ok'

# --- 2. Hook and dead-man ----------------------------------------------------------------------------------
if (-not (Test-Path 'C:\bb\bin\cucina-worker-agent.exe')) {
  & powershell.exe -NoProfile -ExecutionPolicy Bypass -File 'C:\bb\bin\cucina-bootstrap.ps1' | Out-Null
  Assert-True ($LASTEXITCODE -eq 0) 'bootstrap hook must succeed without the agent'
  $result.bootstrap_hook = 'ok (agent absent)'
} else {
  $result.bootstrap_hook = 'skipped (agent installed)'
}
$dm = (& powershell.exe -NoProfile -ExecutionPolicy Bypass -File 'C:\bb\bin\cucina-deadman.ps1' -DryRun) -join ' '
Assert-True ($LASTEXITCODE -eq 0) 'dead-man dry run failed'
$result.deadman_dry_run = $dm

# cucina-worker-agent selftest (IMDS, disks, time sync, WinFSP, Buildbarn binaries); w32time may need a moment.
$agentExe = 'C:\bb\bin\cucina-worker-agent.exe'
if (Test-Path $agentExe) {
  $ok = $false
  for ($i = 0; $i -lt 6 -and -not $ok; $i++) {
    if ($i -gt 0) { Start-Sleep -Seconds 20 }
    $ErrorActionPreference = 'Continue'
    & $agentExe selftest --log-file= --out "$state\image\selftest.json" 2>&1 | Out-Null
    $ok = ($LASTEXITCODE -eq 0)
    $ErrorActionPreference = 'Stop'
  }
  $st = Get-Content -Raw "$state\image\selftest.json" | ConvertFrom-Json
  $result.agent_selftest = @($st.checks | ForEach-Object { '{0}={1}: {2}' -f $_.name, $_.ok, $_.Detail })
  Assert-True $ok ("agent selftest failed: " + ($result.agent_selftest -join '; '))
}

# --- 3. Live smoke test ------------------------------------------------------------------------------------
$runnerCfg = @{
  buildDirectoryPath = 'B:\'
  grpcServers        = @(@{ listenPaths = @('C:/ProgramData/cucina/run/runner'); authenticationPolicy = @{ allow = @{} } })
}
$workerCfg = @{
  blobstore                 = @{
    contentAddressableStorage = @{ grpc = @{ client = @{ address = '127.0.0.1:9' } } }
    actionCache               = @{ grpc = @{ client = @{ address = '127.0.0.1:9' } } }
  }
  maximumMessageSizeBytes   = 16777216
  scheduler                 = @{ address = '127.0.0.1:9' }
  buildDirectories          = @(@{
      virtual = @{
        mount                              = @{ mountPath = '\\.\B:'; winfsp = @{} }
        maximumExecutionTimeoutCompensation = '3600s'
        maximumWritableFileUploadDelay     = '60s'
        caseInsensitive                    = $true
      }
      runners = @(@{
          endpoint                  = @{ address = 'unix:C:/ProgramData/cucina/run/runner' }
          concurrency               = 1
          maximumFilePoolFileCount  = 10000
          maximumFilePoolSizeBytes  = 1073741824
          platform                  = @{}
          workerId                  = @{ node = 'image-smoke-test' }
        })
    })
  filePool                  = @{ blockDevice = @{ file = @{ path = 'C:\bb\filepool\filepool'; sizeBytes = 1073741824 } } }
  inputDownloadConcurrency  = 10
  outputUploadConcurrency   = 11
  directoryCache            = @{ maximumCount = 1000; maximumSizeBytes = 1048576; cacheReplacementPolicy = 'LEAST_RECENTLY_USED' }
}
($runnerCfg | ConvertTo-Json -Depth 10) | Set-Content -Encoding ASCII -Path "$state\bb\runner.json"
($workerCfg | ConvertTo-Json -Depth 10) | Set-Content -Encoding ASCII -Path "$state\bb\worker.json"

function Get-LogTail {
  param([string]$Dir)
  # [string] casts drop the provider NoteProperties that Get-Content attaches (they would bloat ConvertTo-Json).
  Get-ChildItem $Dir -File -ErrorAction SilentlyContinue | Sort-Object LastWriteTime | Select-Object -Last 2 |
    ForEach-Object { Get-Content $_.FullName -Tail 8 } | ForEach-Object { [string]$_ }
}

try {
  Start-Service -Name 'cucina-bb-runner'
  (Get-Service 'cucina-bb-runner').WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
  Start-Sleep -Seconds 10
  $runnerProc = Get-CimInstance Win32_Process -Filter "Name='bb_runner.exe'"
  Assert-True ($null -ne $runnerProc) ("bb_runner.exe not running:`n" + ((Get-LogTail "$state\logs\bb-runner") -join "`n"))
  $owner = Invoke-CimMethod -InputObject @($runnerProc)[0] -MethodName GetOwner
  $result.runner_process_owner = "$($owner.Domain)\$($owner.User)"
  Assert-True (Test-Path "$state\run\runner") "runner socket $state\run\runner not created"

  $t0 = Get-Date
  Start-Service -Name 'cucina-bb-worker'
  $mounted = $false
  for ($i = 0; $i -lt 60 -and -not $mounted; $i++) { Start-Sleep -Seconds 1; $mounted = Test-Path 'B:\' }
  $result.winfsp_mount_seconds = [math]::Round(((Get-Date) - $t0).TotalSeconds, 1)
  Assert-True $mounted ("WinFSP build directory B:\ not mounted:`n" + ((Get-LogTail "$state\logs\bb-worker") -join "`n"))
  Assert-True ($null -ne (Get-CimInstance Win32_Process -Filter "Name='bb_worker.exe'")) 'bb_worker.exe not running'
  $result.worker_running = $true

  $t0 = Get-Date
  Stop-Service -Name 'cucina-bb-worker'
  (Get-Service 'cucina-bb-worker').WaitForStatus('Stopped', [TimeSpan]::FromSeconds(150))
  $result.worker_stop_seconds = [math]::Round(((Get-Date) - $t0).TotalSeconds, 1)
  Stop-Service -Name 'cucina-bb-runner'
  (Get-Service 'cucina-bb-runner').WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
  $result.smoke = 'ok'
} finally {
  foreach ($s in @('cucina-bb-worker', 'cucina-bb-runner')) {
    if ((Get-Service $s).Status -ne 'Stopped') { Stop-Service -Name $s -Force -ErrorAction SilentlyContinue }
  }
  $result.worker_log_tail = @(Get-LogTail "$state\logs\bb-worker")
  # The stable per-unit paths of the log contract must resolve to the current files.
  $result.log_links = @('bb-worker', 'bb-runner') | ForEach-Object { '{0}={1}' -f $_, (Test-Path "$state\logs\$_.log") }
  Remove-Item -Force "$state\bb\*.json", "$state\run\*" -ErrorAction SilentlyContinue
  Start-Sleep -Seconds 2
  Get-ChildItem "$state\logs\bb-worker", "$state\logs\bb-runner", "$state\logs\agent", 'C:\bb\run', 'C:\bb\filepool', 'C:\bb\cache', "$state\run" -Recurse -File -ErrorAction SilentlyContinue |
    Remove-Item -Force -ErrorAction SilentlyContinue
  Remove-Item -Force "$state\logs\boot.log", "$state\logs\deadman.log" -ErrorAction SilentlyContinue
  ($result | ConvertTo-Json -Depth 5) | Set-Content -Encoding UTF8 -Path $Report
}
Write-Output ($result | ConvertTo-Json -Depth 5)
exit 0
