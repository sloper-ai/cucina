# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Static checks for the Windows image PowerShell: parse errors and PSScriptAnalyzer (errors + warnings).
  Runs under PowerShell 7 on the dev machine (`make -C workers/windows validate`); installs PSScriptAnalyzer for
  the current user only when it is missing.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$Path
)

$ErrorActionPreference = 'Stop'
$version = '1.24.0'
if (-not (Get-Module -ListAvailable -Name PSScriptAnalyzer | Where-Object { $_.Version -eq [version]$version })) {
  Install-Module -Name PSScriptAnalyzer -RequiredVersion $version -Scope CurrentUser -Force -AllowClobber
}
Import-Module PSScriptAnalyzer -RequiredVersion $version

# Rules that do not fit provisioning scripts run non-interactively by Packer/SSM/Task Scheduler:
#  - ShouldProcess/-WhatIf plumbing for internal helpers,
#  - Write-Host (not used, but harmless), plural nouns in helper names, BOM-less ASCII/UTF-8 files.
$exclude = @('PSUseShouldProcessForStateChangingFunctions', 'PSAvoidUsingWriteHost', 'PSUseSingularNouns', 'PSUseBOMForUnicodeEncodedFile')

$failed = $false
foreach ($f in Get-ChildItem -Path $Path -Recurse -Include *.ps1) {
  $tokens = $null; $errors = $null
  [System.Management.Automation.Language.Parser]::ParseFile($f.FullName, [ref]$tokens, [ref]$errors) | Out-Null
  foreach ($e in $errors) { $failed = $true; Write-Output ('{0}:{1}: parse error: {2}' -f $f.Name, $e.Extent.StartLineNumber, $e.Message) }
  if ($f.Name -eq 'winrm-bootstrap.ps1') { continue }   # EC2 user data wrapped in <powershell> tags
  foreach ($r in Invoke-ScriptAnalyzer -Path $f.FullName -Severity Error, Warning -ExcludeRule $exclude) {
    $failed = $true
    Write-Output ('{0}:{1}: {2} {3}' -f $f.Name, $r.Line, $r.RuleName, $r.Message)
  }
}
if ($failed) { exit 1 }
Write-Output 'lint: ok'
