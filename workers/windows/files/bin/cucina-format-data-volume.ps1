# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Formats and mounts a raw local data disk for L1 / filePool (R-CACHE-2), Windows counterpart of
  cucina-format-instance-store.service. By default only NVMe instance store is touched (it is always raw at
  launch); -IncludeEbs also takes a raw EBS data volume (for the agent, which owns the gp3/memory choice).

.DESCRIPTION
  Format: NTFS by default; with the image's defender_mode = devdrive (or -Format devdrive) a Dev Drive
  (ReFS, trusted, Defender in performance mode) is created instead, falling back to plain ReFS if this
  Windows build cannot create Dev Drives. The result is written to C:\ProgramData\cucina\run\data-volume.json
  ({"path","filesystem","dev_drive","size_bytes","source"}); without a raw disk nothing is mounted.
#>
[CmdletBinding()]
param(
  [ValidateSet('', 'ntfs', 'devdrive')][string]$Format = '',
  [string]$DriveLetter = 'D',
  [switch]$IncludeEbs,
  [string]$StateRoot = 'C:\ProgramData\cucina'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

if (-not $Format) {
  $Format = 'ntfs'
  $imageJson = Join-Path $StateRoot 'image.json'
  if (Test-Path $imageJson) {
    $img = Get-Content -Raw $imageJson | ConvertFrom-Json
    $mode = $img.PSObject.Properties['defender_mode']
    if ($null -ne $mode -and $mode.Value -eq 'devdrive') { $Format = 'devdrive' }
  }
}

$candidates = @(Get-Disk | Where-Object {
    $_.PartitionStyle -eq 'RAW' -and -not $_.IsBoot -and -not $_.IsSystem -and
    ($IncludeEbs -or $_.FriendlyName -match 'Instance Stor' -or $_.Model -match 'Instance Stor')
  } | Sort-Object -Property Size -Descending)
if ($candidates.Count -eq 0) {
  Write-Output 'no raw data disk; nothing mounted'
  exit 0
}
$disk = $candidates[0]
$source = $(if ($disk.FriendlyName -match 'Instance Stor' -or $disk.Model -match 'Instance Stor') { 'instance-store' } else { 'ebs' })
Initialize-Disk -Number $disk.Number -PartitionStyle GPT
$letterFree = -not (Get-PSDrive -Name $DriveLetter -ErrorAction SilentlyContinue)
if ($letterFree) {
  $part = New-Partition -DiskNumber $disk.Number -UseMaximumSize -DriveLetter $DriveLetter
} else {
  $part = New-Partition -DiskNumber $disk.Number -UseMaximumSize -AssignDriveLetter
}
$fs = 'NTFS'
$devDrive = $false
if ($Format -eq 'devdrive') {
  try {
    Format-Volume -Partition $part -DevDrive -NewFileSystemLabel 'cucina-data' -Confirm:$false -Force | Out-Null
    $fs = 'ReFS'
    $devDrive = $true
  } catch {
    Write-Output "Dev Drive not available ($($_.Exception.Message)); formatting plain ReFS"
    Format-Volume -Partition $part -FileSystem ReFS -NewFileSystemLabel 'cucina-data' -Confirm:$false -Force | Out-Null
    $fs = 'ReFS'
  }
} else {
  Format-Volume -Partition $part -FileSystem NTFS -AllocationUnitSize 65536 -NewFileSystemLabel 'cucina-data' -Confirm:$false -Force | Out-Null
}
$letter = (Get-Partition -DiskNumber $disk.Number -PartitionNumber $part.PartitionNumber).DriveLetter
$path = "${letter}:\"
& icacls.exe $path /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
$info = [ordered]@{ path = $path; filesystem = $fs; dev_drive = $devDrive; size_bytes = [int64]$disk.Size; source = $source; disk_number = $disk.Number }
New-Item -ItemType Directory -Force -Path (Join-Path $StateRoot 'run') | Out-Null
($info | ConvertTo-Json -Compress) | Set-Content -Path (Join-Path $StateRoot 'run\data-volume.json')
Write-Output ("mounted {0} {1} ({2:N0} GiB, {3}{4})" -f $source, $path, ($disk.Size / 1GB), $fs, $(if ($devDrive) { ', Dev Drive' } else { '' }))
exit 0
