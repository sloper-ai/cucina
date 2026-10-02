# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Dead-man switch for EC2 Windows workers (R-POOL-7). Runs every minute as SYSTEM (scheduled task
  `cucina-deadman`, armed from boot) and powers the machine off - which terminates it, because pools launch with
  InstanceInitiatedShutdownBehavior=terminate - when any of these holds:

    * idle:     now - last-activity > idle_limit_seconds     (default 1800 = 30 min)
    * contact:  now - last-contact  > contact_limit_seconds  (default  600 = 10 min, scheduler unreachable)
    * uptime:   uptime              > max_uptime_seconds     (default 43200 = 12 h)

  last-activity / last-contact are files in C:\ProgramData\cucina\run whose modification time the worker agent
  refreshes (content is informational). A missing file, or one older than the current boot, counts as "at boot",
  so a worker that never bootstraps or never reaches the scheduler still powers itself off. This works without
  the controller and without the agent.

  Overrides: C:\ProgramData\cucina\deadman.json {"enabled", "idle_limit_seconds", "contact_limit_seconds",
  "max_uptime_seconds", "not_a_worker_limit_seconds"} (the agent syncs the pool's limits into it); a file
  C:\ProgramData\cucina\run\deadman-disabled pauses it until the next boot (break-glass debugging). An instance the
  boot task marked "not a worker" (run\not-a-worker: no EC2 user data, e.g. EC2 Fast Launch preparation) is only held
  to not_a_worker_limit_seconds (default 1800, like the agent's not-bootstrapped rule) and the uptime limit.
  -DryRun evaluates and prints without powering off.
#>
[CmdletBinding()]
param(
  [string]$StateRoot = 'C:\ProgramData\cucina',
  [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$run = Join-Path $StateRoot 'run'

$cfg = [ordered]@{ enabled = $true; idle_limit_seconds = 1800; contact_limit_seconds = 600; max_uptime_seconds = 43200
  not_a_worker_limit_seconds = 1800 }
$cfgFile = Join-Path $StateRoot 'deadman.json'
if (Test-Path $cfgFile) {
  try {
    $o = Get-Content -Raw $cfgFile | ConvertFrom-Json
    foreach ($k in @($cfg.Keys)) {
      $p = $o.PSObject.Properties[$k]
      if ($null -ne $p) { $cfg[$k] = $p.Value }
    }
  } catch {
    Write-Output "deadman: ignoring unreadable ${cfgFile}: $($_.Exception.Message)"
  }
}

$now = [DateTime]::UtcNow
$boot = (Get-CimInstance -ClassName Win32_OperatingSystem).LastBootUpTime.ToUniversalTime()

function Get-Mark {
  param([string]$Name)
  $f = Join-Path $run $Name
  if (Test-Path $f) {
    $t = (Get-Item $f).LastWriteTimeUtc
    if ($t -gt $boot) { return $t }
  }
  return $boot
}

$lastActivity = Get-Mark -Name 'last-activity'
$lastContact = Get-Mark -Name 'last-contact'
$uptime = ($now - $boot).TotalSeconds
$idle = ($now - $lastActivity).TotalSeconds
$noContact = ($now - $lastContact).TotalSeconds

$reasons = @()
if ($uptime -gt [double]$cfg.max_uptime_seconds) { $reasons += ('uptime {0:N0}s > {1}s' -f $uptime, $cfg.max_uptime_seconds) }
if (Test-Path (Join-Path $run 'not-a-worker')) {
  if ($uptime -gt [double]$cfg.not_a_worker_limit_seconds) { $reasons += ('not a worker, up {0:N0}s > {1}s' -f $uptime, $cfg.not_a_worker_limit_seconds) }
} else {
  if ($idle -gt [double]$cfg.idle_limit_seconds) { $reasons += ('idle {0:N0}s > {1}s' -f $idle, $cfg.idle_limit_seconds) }
  if ($noContact -gt [double]$cfg.contact_limit_seconds) { $reasons += ('no scheduler contact {0:N0}s > {1}s' -f $noContact, $cfg.contact_limit_seconds) }
}

$status = [ordered]@{
  evaluated_at = $now.ToString('o'); boot = $boot.ToString('o'); uptime_s = [int]$uptime; idle_s = [int]$idle
  no_contact_s = [int]$noContact; enabled = [bool]$cfg.enabled; reasons = $reasons
}
if (Test-Path $run) { ($status | ConvertTo-Json -Compress) | Set-Content -Path (Join-Path $run 'deadman-status.json') }

if ($reasons.Count -eq 0) { if ($DryRun) { Write-Output 'deadman: ok' }; exit 0 }
$why = $reasons -join '; '
if (-not [bool]$cfg.enabled -or (Test-Path (Join-Path $run 'deadman-disabled'))) {
  Write-Output "deadman: would power off ($why) but is disabled"
  exit 0
}
if ($DryRun) { Write-Output "deadman: would power off: $why"; exit 0 }

Add-Content -Path (Join-Path $StateRoot 'logs\deadman.log') -Value ('{0} power off: {1}' -f $now.ToString('o'), $why)
try { Write-EventLog -LogName Application -Source 'cucina' -EventId 7 -EntryType Warning -Message "cucina dead-man switch: power off ($why)" } catch { Write-Verbose 'no event source' }
& shutdown.exe /s /t 0 /f /d p:4:1 /c "cucina dead-man: $why"
exit 0
