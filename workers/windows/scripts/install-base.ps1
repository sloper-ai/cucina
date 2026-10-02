# SPDX-License-Identifier: FSL-1.1-ALv2
<#
.SYNOPSIS
  Installs the pinned base components of the Windows images: VC++ Redistributable (x64), WinFSP, Git for
  Windows, shawl and Bazelisk. Every download is verified against the SHA-256 in workers/windows/versions.json.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File install-base.ps1 -PinsFile C:\Windows\Temp\cucina\versions.json
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$PinsFile,
  [string]$WorkDir = (Join-Path $env:SystemRoot 'Temp\cucina-base'),
  [string]$VersionsOut = 'C:\ProgramData\cucina\image\base-components.json'
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
    try { Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $OutFile; break }
    catch { if ($attempt -eq 5) { throw }; Start-Sleep -Seconds (5 * $attempt) }
  }
  $actual = (Get-FileHash -Algorithm SHA256 -Path $OutFile).Hash
  if ($actual -ne $Sha256.ToUpperInvariant()) {
    Remove-Item -Force $OutFile
    throw "SHA-256 mismatch for ${Url}: expected $Sha256, got $actual"
  }
}

function Invoke-Installer {
  param([string]$FilePath, [string]$Arguments, [int[]]$OkCodes = @(0, 3010))
  $p = Start-Process -FilePath $FilePath -ArgumentList $Arguments -Wait -PassThru -NoNewWindow
  if ($OkCodes -notcontains $p.ExitCode) { throw "$FilePath $Arguments failed with exit code $($p.ExitCode)" }
  return $p.ExitCode
}

function Add-MachinePath {
  param([string]$Dir)
  $current = [Environment]::GetEnvironmentVariable('Path', 'Machine')
  if (($current -split ';') -notcontains $Dir) {
    [Environment]::SetEnvironmentVariable('Path', ($current.TrimEnd(';') + ';' + $Dir), 'Machine')
  }
}

$pins = (Get-Content -Raw $PinsFile | ConvertFrom-Json).pins
New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null
$result = [ordered]@{}

# --- VC++ Redistributable x64 (cross-built MSVC-ABI tests link /MD by default) -----------------------------
$f = Join-Path $WorkDir 'VC_redist.x64.exe'
Get-VerifiedFile -Url $pins.vc_redist.url -Sha256 $pins.vc_redist.sha256 -OutFile $f
# 1638 = a newer or identical version is already installed (VS Build Tools installs the same package).
$code = Invoke-Installer -FilePath $f -Arguments '/install /quiet /norestart' -OkCodes @(0, 1638, 3010)
$redist = Get-ItemProperty -Path 'HKLM:\SOFTWARE\Microsoft\VisualStudio\14.0\VC\Runtimes\X64' -ErrorAction SilentlyContinue
$result.vc_redist_x64 = $(if ($redist) { $redist.Version } else { "unknown (installer exit $code)" })
Write-Output "vc_redist x64: $($result.vc_redist_x64)"

# --- WinFSP (bb_worker virtual build directory, R-CACHE-3) -------------------------------------------------
$f = Join-Path $WorkDir 'winfsp.msi'
Get-VerifiedFile -Url $pins.winfsp.url -Sha256 $pins.winfsp.sha256 -OutFile $f
Invoke-Installer -FilePath 'msiexec.exe' -Arguments "/i `"$f`" /qn /norestart /l*v `"$WorkDir\winfsp-install.log`"" | Out-Null
# Let a non-administrator bb_worker create Mount Manager mounts (\\.\B:), see bb-remote-execution virtual.proto.
$winfspKey = 'HKLM:\SOFTWARE\WOW6432Node\WinFsp'
if (-not (Test-Path $winfspKey)) { New-Item -Path $winfspKey -Force | Out-Null }
New-ItemProperty -Path $winfspKey -Name 'MountUseMountmgrFromFSD' -Value 1 -PropertyType DWord -Force | Out-Null
$winfspDll = Join-Path ${env:ProgramFiles(x86)} 'WinFsp\bin\winfsp-x64.dll'
if (-not (Test-Path $winfspDll)) { throw "WinFSP installation incomplete: $winfspDll missing" }
# ProductVersion is the marketing name ("2025"); FileVersion carries the release (2.1.25156).
$result.winfsp = (Get-Item $winfspDll).VersionInfo.FileVersion
Write-Output "winfsp: $($result.winfsp)"

# --- Git for Windows (Bazel repository rules, bash for genrules; identical path on client and worker) -------
$f = Join-Path $WorkDir 'Git-64-bit.exe'
Get-VerifiedFile -Url $pins.git.url -Sha256 $pins.git.sha256 -OutFile $f
$inf = Join-Path $WorkDir 'git.inf'
@'
[Setup]
Lang=default
Dir=C:\Program Files\Git
Group=Git
NoIcons=1
SetupType=default
Components=gitlfs,assoc,assoc_sh
Tasks=
EditorOption=Notepad
DefaultBranchOption=main
PathOption=Cmd
SSHOption=OpenSSH
TortoiseOption=false
CURLOption=WinSSL
CRLFOption=CRLFCommitAsIs
BashTerminalOption=ConHost
GitPullBehaviorOption=Merge
UseCredentialManager=Disabled
PerformanceTweaksFSCache=Enabled
EnableSymlinks=Enabled
EnableFSMonitor=Disabled
'@ | Set-Content -Path $inf -Encoding ASCII
Invoke-Installer -FilePath $f -Arguments "/VERYSILENT /NORESTART /NOCANCEL /SP- /SUPPRESSMSGBOXES /CLOSEAPPLICATIONS /LOADINF=`"$inf`" /LOG=`"$WorkDir\git-install.log`"" | Out-Null
$gitExe = Join-Path $env:ProgramFiles 'Git\cmd\git.exe'
if (-not (Test-Path $gitExe)) { throw "Git installation incomplete: $gitExe missing" }
& $gitExe config --system core.longpaths true
& $gitExe config --system core.symlinks true
& $gitExe config --system core.autocrlf false
$result.git = ((& $gitExe --version) -replace '^git version ', '').Trim()
Write-Output "git: $($result.git)"

# --- shawl (service wrapper: stop = Ctrl-C so bb_worker drains) --------------------------------------------
$f = Join-Path $WorkDir 'shawl.zip'
Get-VerifiedFile -Url $pins.shawl.url -Sha256 $pins.shawl.sha256 -OutFile $f
$unz = Join-Path $WorkDir 'shawl'
if (Test-Path $unz) { Remove-Item -Recurse -Force $unz }
Expand-Archive -Path $f -DestinationPath $unz -Force
New-Item -ItemType Directory -Force -Path 'C:\bb\bin' | Out-Null
Copy-Item -Force (Join-Path $unz 'shawl.exe') 'C:\bb\bin\shawl.exe'
$result.shawl = ((& 'C:\bb\bin\shawl.exe' --version) -join ' ').Trim()
Write-Output "shawl: $($result.shawl)"

# --- Bazelisk as C:\tools\bin\bazel.exe (windows-client; harmless on workers) -------------------------------
New-Item -ItemType Directory -Force -Path 'C:\tools\bin' | Out-Null
Get-VerifiedFile -Url $pins.bazelisk.url -Sha256 $pins.bazelisk.sha256 -OutFile 'C:\tools\bin\bazel.exe'
Add-MachinePath -Dir 'C:\tools\bin'
$result.bazelisk = $pins.bazelisk.version
Write-Output "bazelisk: $($result.bazelisk)"

New-Item -ItemType Directory -Force -Path (Split-Path -Parent $VersionsOut) | Out-Null
($result | ConvertTo-Json -Depth 4) | Set-Content -Encoding UTF8 -Path $VersionsOut
Remove-Item -Recurse -Force $WorkDir -ErrorAction SilentlyContinue
Write-Output 'install-base: done'

exit 0
