# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  R-ARTIFACT: install and verify the mandatory legal payload in both Windows image stages.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$Source,
  [string]$Destination = 'C:\ProgramData\cucina\doc'
)
Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$inputs = @(
  @{ Source = Join-Path $Source 'LICENSE.md'; Name = 'LICENSE.md' },
  @{ Source = Join-Path $Source 'THIRD_PARTY_NOTICES.md'; Name = 'THIRD_PARTY_NOTICES.md' }
)
$licenses = @(Get-ChildItem -LiteralPath (Join-Path $Source 'licenses') -File -Filter '*.txt')
if ($licenses.Count -eq 0) { throw 'Redistributed license texts are required' }
foreach ($license in $licenses) {
  $inputs += @{ Source = $license.FullName; Name = Join-Path 'licenses' $license.Name }
}
# Check all inputs before writing anything, so missing payloads fail the image build rather than silently inheriting
# an outdated or absent notice set from the base AMI.
foreach ($item in $inputs) {
  if (-not (Test-Path -LiteralPath $item.Source -PathType Leaf) -or (Get-Item -LiteralPath $item.Source).Length -eq 0) {
    throw "Missing or empty legal payload: $($item.Source)"
  }
}
New-Item -ItemType Directory -Force -Path $Destination, (Join-Path $Destination 'licenses') | Out-Null
foreach ($item in $inputs) {
  $target = Join-Path $Destination $item.Name
  Copy-Item -LiteralPath $item.Source -Destination $target -Force
  if ((Get-FileHash -LiteralPath $item.Source -Algorithm SHA256).Hash -ne (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash) {
    throw "Legal payload copy changed bytes: $($item.Name)"
  }
}
Write-Output "Installed $($inputs.Count) legal documents in $Destination"
