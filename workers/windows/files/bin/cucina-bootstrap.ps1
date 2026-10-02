# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Bootstrap hook run at every boot before Buildbarn starts (Windows counterpart of the Linux unit's
  ExecStartPre=/opt/cucina/bin/cucina-bootstrap).

.DESCRIPTION
  * cucina-worker-agent.exe absent  -> nothing to do, exit 0 (images built before the agent existed).
  * agent present                   -> `cucina-worker-agent.exe bootstrap` (enrol with the instance identity document,
                                       fetch the certificate, render C:\ProgramData\cucina\bb\*.json); its exit code is
                                       returned unchanged, so a failing bootstrap fails closed (services are not started).
#>
[CmdletBinding()]
param(
  [string]$Agent = 'C:\bb\bin\cucina-worker-agent.exe'
)

# Continue: the agent logs to stderr, which must not turn into a terminating PowerShell error.
$ErrorActionPreference = 'Continue'
if (-not (Test-Path -LiteralPath $Agent)) {
  Write-Output "cucina-bootstrap: $Agent not installed; nothing to bootstrap"
  exit 0
}
$code = 1
try {
  $global:LASTEXITCODE = $null
  & $Agent bootstrap
  $code = $LASTEXITCODE
  if ($null -eq $code) { $code = 1 }   # the process never ran: fail closed
} catch {
  Write-Output "cucina-bootstrap: could not run ${Agent}: $($_.Exception.Message)"
  $code = 1
}
if ($code -ne 0) {
  Write-Output "cucina-bootstrap: '$Agent bootstrap' failed with exit code $code"
  exit $code
}
Write-Output 'cucina-bootstrap: ok'
exit 0
