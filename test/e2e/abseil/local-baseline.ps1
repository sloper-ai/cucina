# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Local baseline for NFR-P2 and NFR-X2 on Windows: build, then test,
# //absl/... in a prepared Abseil checkout WITHOUT remote execution, from a
# cold output base. Prints one JSON line; logs and the BEP go to -Out.
#
# Usage: local-baseline.ps1 -Workspace C:\e2e\abseil -Out C:\e2e\baseline [-BazelArgs @('--config=lane-windows')]
param([Parameter(Mandatory)][string]$Workspace, [Parameter(Mandatory)][string]$Out, [string[]]$BazelArgs = @())
$ErrorActionPreference = 'Continue'
New-Item -ItemType Directory -Force -Path $Out | Out-Null
Set-Location $Workspace
$ob = "$Workspace.local-ob"
& bazel.exe "--output_base=$ob" clean --expunge 2>$null | Out-Null
$t0 = Get-Date
$b = Start-Process -FilePath bazel.exe -ArgumentList (@("--output_base=$ob", 'build') + $BazelArgs + @('//absl/...')) -RedirectStandardOutput "$Out\build.out" -RedirectStandardError "$Out\build.log" -NoNewWindow -Wait -PassThru
$t1 = Get-Date
$t = Start-Process -FilePath bazel.exe -ArgumentList (@("--output_base=$ob", 'test') + $BazelArgs + @("--build_event_json_file=$Out\bep.json", '//absl/...')) -RedirectStandardOutput "$Out\test.out" -RedirectStandardError "$Out\test.log" -NoNewWindow -Wait -PassThru
$t2 = Get-Date
@{ buildExit = $b.ExitCode; testExit = $t.ExitCode; buildSeconds = [int]($t1 - $t0).TotalSeconds; testSeconds = [int]($t2 - $t1).TotalSeconds;
   wallSeconds = [int]($t2 - $t0).TotalSeconds; cpus = [Environment]::ProcessorCount } | ConvertTo-Json -Compress
