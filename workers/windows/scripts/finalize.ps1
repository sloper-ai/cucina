# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Last Packer step: remove the build-time WinRM listener and staging files, then either
  * -Mode reset   (base stage): `ec2launch reset` without sysprep, so instances launched from the AMI run
                  EC2Launch's first-boot tasks again (new random Administrator password, user data), then shut down;
  * -Mode sysprep (worker stage): generalise with `ec2launch sysprep --shutdown` (R-POOL-5, Fast Launch).

.DESCRIPTION
  The work runs detached (scheduled task as SYSTEM, a few seconds later) so that this WinRM command returns
  successfully before the listener disappears. Packer is configured with disable_stop_instance = true and waits
  for the instance to stop on its own. A pre-flight check fails the build early if the EC2Launch CLI does not
  support the required verbs/flags.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][ValidateSet('reset', 'sysprep')][string]$Mode
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'

$ec2launch = Join-Path $env:ProgramFiles 'Amazon\EC2Launch\EC2Launch.exe'
if (-not (Test-Path $ec2launch)) { throw "EC2Launch v2 not found at $ec2launch" }

# Pre-flight: the verb and flags we rely on must exist in this EC2Launch version (--clean is optional).
$verb = $Mode
$ErrorActionPreference = 'Continue'   # native stderr must not become a terminating error here
$help = (& $ec2launch $verb --help 2>&1 | Out-String)
$ErrorActionPreference = 'Stop'
$ec2lArgs = $verb
if ($Mode -eq 'sysprep') {
  if ($help -notmatch '--shutdown') { throw "ec2launch sysprep does not support --shutdown; help was:`n$help" }
  $ec2lArgs += ' --shutdown'
}
if ($help -match '--clean') { $ec2lArgs += ' --clean' }
Write-Output "finalize: will run 'ec2launch $ec2lArgs'"

$work = @"
`$ErrorActionPreference = 'Continue'
Start-Sleep -Seconds 15
Start-Transcript -Path 'C:\Windows\Temp\cucina-finalize.log' -Force | Out-Null
Get-ChildItem -Path WSMan:\localhost\Listener -ErrorAction SilentlyContinue | Remove-Item -Recurse -Force
Get-NetFirewallRule -Name 'cucina-packer-winrm-https' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
Get-ChildItem Cert:\LocalMachine\My | Where-Object { `$_.FriendlyName -eq 'cucina-packer-winrm' } | Remove-Item -Force
Remove-Item -Recurse -Force 'C:\Windows\Temp\cucina', 'C:\Windows\Temp\cucina-vs', 'C:\Windows\Temp\cucina-base', 'C:\Windows\Temp\packer-*' -ErrorAction SilentlyContinue
Remove-Item -Force 'C:\Windows\Temp\cucina-winrm-bootstrap.log' -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName 'cucina-finalize' -Confirm:`$false -ErrorAction SilentlyContinue
& '$ec2launch' $ec2lArgs
Write-Output "ec2launch exit code: `$LASTEXITCODE"
Stop-Transcript | Out-Null
$(if ($Mode -eq 'reset') { "Start-Sleep -Seconds 5; & shutdown.exe /s /t 0 /f /d p:4:1 /c 'cucina image capture'" } else { "# sysprep shuts the instance down" })
"@
$workFile = 'C:\Windows\Temp\cucina-finalize.ps1'
Set-Content -Path $workFile -Value $work -Encoding ASCII

$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File $workFile"
$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(30)
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
Register-ScheduledTask -TaskName 'cucina-finalize' -Action $action -Trigger $trigger -Principal $principal -Force | Out-Null
Start-ScheduledTask -TaskName 'cucina-finalize'
Write-Output "finalize ($Mode) scheduled; the instance shuts itself down shortly"
