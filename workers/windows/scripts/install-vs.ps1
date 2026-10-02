# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Installs Visual Studio Build Tools (C++ workload + Windows SDK) pinned to one channel manifest, or verifies
  an existing installation, and records the resulting toolchain versions.

.DESCRIPTION
  Shared by the Windows worker AMI (base stage) and the windows-client image so that the toolchain paths and
  versions on clients and workers are identical (R-POOL-5, §10.2). Idempotent: when the requested components
  are already installed at -InstallPath from the pinned channel version, nothing is reinstalled.

  Pinning: the bootstrapper is the fixed-version bootstrapper of the pinned release (SHA-256 verified) and both
  --channelUri and --installChannelUri point at a local, SHA-256-verified copy of that release's channel
  manifest, so the installer cannot drift to a newer release and never self-updates the product.

  Writes a JSON document (-VersionsOut) with the values Bazel needs:
  BAZEL_VC, BAZEL_VC_FULL_VERSION, BAZEL_WINSDK_FULL_VERSION.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File install-vs.ps1            # pinned defaults (same as versions.json)
  powershell -ExecutionPolicy Bypass -File install-vs.ps1 -VerifyOnly
#>
[CmdletBinding()]
param(
  [string]$BootstrapperUrl = 'https://download.visualstudio.microsoft.com/download/pr/e5f740e0-92f9-49d7-ab3b-5d17b84108fd/9203cc1fa53bc4c6254723cdd4bc35e6a658253b98c90c863b0b4a962808254e/vs_BuildTools.exe',
  [string]$BootstrapperSha256 = '9203cc1fa53bc4c6254723cdd4bc35e6a658253b98c90c863b0b4a962808254e',
  [string]$ChannelManifestUrl = 'https://download.visualstudio.microsoft.com/download/pr/e5f740e0-92f9-49d7-ab3b-5d17b84108fd/8061f93be2aee4bfacc85dbebbd174a9d6571882c151a492b21bfc71b87c13ca/VisualStudio.18.Release.chman',
  [string]$ChannelManifestSha256 = '4b7622ddec2ef331171698019538642be0df63162f2924ccb18110ef4f75d867',
  [string]$ExpectedDisplayVersion = '18.10.3',
  [string]$InstallPath = 'C:\BuildTools',
  [string[]]$Add = @(
    'Microsoft.VisualStudio.Workload.VCTools',
    'Microsoft.VisualStudio.Component.VC.Tools.x86.x64',
    'Microsoft.VisualStudio.Component.Windows11SDK.28000'),
  [string]$VersionsOut = 'C:\ProgramData\cucina\image\toolchain.json',
  [string]$WorkDir = (Join-Path $env:SystemRoot 'Temp\cucina-vs'),
  # Optional workers/windows/versions.json: its pins.vs values override the defaults above.
  [string]$PinsFile = '',
  [switch]$VerifyOnly
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Get-VerifiedFile {
  param([Parameter(Mandatory)][string]$Url, [Parameter(Mandatory)][string]$Sha256, [Parameter(Mandatory)][string]$OutFile)
  if (Test-Path $OutFile) {
    if ((Get-FileHash -Algorithm SHA256 -Path $OutFile).Hash -eq $Sha256.ToUpperInvariant()) { return }
    Remove-Item -Force $OutFile
  }
  for ($attempt = 1; $attempt -le 5; $attempt++) {
    try {
      Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $OutFile
      break
    } catch {
      if ($attempt -eq 5) { throw }
      Start-Sleep -Seconds (5 * $attempt)
    }
  }
  $actual = (Get-FileHash -Algorithm SHA256 -Path $OutFile).Hash
  if ($actual -ne $Sha256.ToUpperInvariant()) {
    Remove-Item -Force $OutFile
    throw "SHA-256 mismatch for ${Url}: expected $Sha256, got $actual"
  }
}

function Get-VsWhere {
  $p = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
  if (Test-Path $p) { return $p }
  return $null
}

function Get-InstalledInstance {
  $vswhere = Get-VsWhere
  if (-not $vswhere) { return $null }
  $vswhereArgs = @('-products', '*', '-format', 'json', '-utf8', '-nologo')
  foreach ($c in $Add) { $vswhereArgs += @('-requires', $c) }
  $json = & $vswhere @vswhereArgs
  if ($LASTEXITCODE -ne 0 -or -not $json) { return $null }
  $instances = @($json | ConvertFrom-Json)
  foreach ($i in $instances) {
    if ([IO.Path]::GetFullPath($i.installationPath).TrimEnd('\') -ieq [IO.Path]::GetFullPath($InstallPath).TrimEnd('\')) { return $i }
  }
  return $null
}

function Get-ToolchainVersions {
  param($Instance)
  $vcDir = Join-Path $InstallPath 'VC'
  $defaultTxt = Join-Path $vcDir 'Auxiliary\Build\Microsoft.VCToolsVersion.default.txt'
  $msvc = $null
  if (Test-Path $defaultTxt) { $msvc = (Get-Content -Raw $defaultTxt).Trim() }
  if (-not $msvc) {
    $msvc = (Get-ChildItem -Directory (Join-Path $vcDir 'Tools\MSVC') | Sort-Object { [version]$_.Name } | Select-Object -Last 1).Name
  }
  $kits = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10'
  $sdks = @(Get-ChildItem -Directory (Join-Path $kits 'Include') -ErrorAction SilentlyContinue |
      Where-Object { $_.Name -match '^10\.0\.\d+\.\d+$' -and (Test-Path (Join-Path $_.FullName 'um\windows.h')) } |
      Sort-Object { [version]$_.Name })
  $sdk = $null
  if ($sdks.Count -gt 0) { $sdk = $sdks[-1].Name }
  $redist = Get-ItemProperty -Path 'HKLM:\SOFTWARE\Microsoft\VisualStudio\14.0\VC\Runtimes\X64' -ErrorAction SilentlyContinue
  $clExe = Join-Path $vcDir "Tools\MSVC\$msvc\bin\Hostx64\x64\cl.exe"
  $clVersion = $null
  if (Test-Path $clExe) { $clVersion = (Get-Item $clExe).VersionInfo.ProductVersion }
  return [ordered]@{
    vs_display_version     = $Instance.catalog.productDisplayVersion
    vs_installation_version = $Instance.installationVersion
    vs_channel_id          = $Instance.channelId
    vs_install_path        = $InstallPath
    msvc_version           = $msvc
    cl_exe_version         = $clVersion
    windows_sdk_version    = $sdk
    windows_sdk_versions   = @($sdks | ForEach-Object { $_.Name })
    vc_redist_x64_version  = $(if ($redist) { $redist.Version } else { $null })
    bazel = [ordered]@{
      BAZEL_VC                  = $vcDir
      BAZEL_VC_FULL_VERSION     = $msvc
      BAZEL_WINSDK_FULL_VERSION = $sdk
    }
  }
}

if ($PinsFile) {
  $vs = (Get-Content -Raw $PinsFile | ConvertFrom-Json).pins.vs
  $BootstrapperUrl = $vs.bootstrapper_url
  $BootstrapperSha256 = $vs.bootstrapper_sha256
  $ChannelManifestUrl = $vs.channel_manifest_url
  $ChannelManifestSha256 = $vs.channel_manifest_sha256
  $ExpectedDisplayVersion = $vs.product_display_version
  $InstallPath = $vs.install_path
  $Add = @($vs.add)
}

New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null
$instance = Get-InstalledInstance
$upToDate = $instance -and ($instance.catalog.productDisplayVersion -eq $ExpectedDisplayVersion)

if ($upToDate) {
  Write-Output "VS Build Tools $($instance.catalog.productDisplayVersion) with the requested components already present at $InstallPath"
} elseif ($VerifyOnly) {
  throw "VS Build Tools $ExpectedDisplayVersion with components [$($Add -join ', ')] not found at $InstallPath"
} else {
  $bootstrapper = Join-Path $WorkDir 'vs_BuildTools.exe'
  $chman = Join-Path $WorkDir 'VisualStudio.chman'
  Get-VerifiedFile -Url $BootstrapperUrl -Sha256 $BootstrapperSha256 -OutFile $bootstrapper
  Get-VerifiedFile -Url $ChannelManifestUrl -Sha256 $ChannelManifestSha256 -OutFile $chman
  $vsArgs = @('--quiet', '--wait', '--norestart', '--nocache',
    '--installPath', $InstallPath,
    '--channelUri', $chman,
    '--installChannelUri', $chman)
  foreach ($c in $Add) { $vsArgs += @('--add', $c) }
  Write-Output ("Installing VS Build Tools {0}: {1}" -f $ExpectedDisplayVersion, ($Add -join ', '))
  $started = Get-Date
  # Start-Process joins the argument array verbatim (PowerShell 5.1), so quote anything with spaces.
  $argLine = ($vsArgs | ForEach-Object { if ($_ -match '\s') { '"{0}"' -f $_ } else { $_ } }) -join ' '
  $p = Start-Process -FilePath $bootstrapper -ArgumentList $argLine -Wait -PassThru -NoNewWindow
  $code = $p.ExitCode
  Write-Output ("VS installer exit code {0} after {1:N1} min" -f $code, ((Get-Date) - $started).TotalMinutes)
  if ($code -ne 0 -and $code -ne 3010) {
    $logs = Get-ChildItem -Path $env:TEMP, (Join-Path $env:SystemRoot 'Temp') -Filter 'dd_*.log' -ErrorAction SilentlyContinue |
      Sort-Object LastWriteTime | Select-Object -Last 3
    foreach ($l in $logs) { Write-Output "--- $($l.FullName)"; Get-Content $l.FullName -Tail 40 }
    throw "VS Build Tools installation failed with exit code $code"
  }
  $instance = Get-InstalledInstance
  if (-not $instance) { throw "VS Build Tools installation reported success but vswhere cannot find the components at $InstallPath" }
  if ($instance.catalog.productDisplayVersion -ne $ExpectedDisplayVersion) {
    throw "Installed VS version $($instance.catalog.productDisplayVersion) differs from the pinned $ExpectedDisplayVersion"
  }
  if ($code -eq 3010) { Write-Output 'A reboot is required to complete the installation.' }
}

$versions = Get-ToolchainVersions -Instance $instance
foreach ($k in @('msvc_version', 'windows_sdk_version')) {
  if (-not $versions[$k]) { throw "Could not determine $k after installation" }
}
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $VersionsOut) | Out-Null
($versions | ConvertTo-Json -Depth 5) | Set-Content -Encoding UTF8 -Path $VersionsOut
Write-Output ($versions | ConvertTo-Json -Depth 5)

exit 0
