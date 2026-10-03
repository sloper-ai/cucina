// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import "fmt"

// The remote CIM supervisor and its stopper share a private, unpredictable job
// name and mutex. Neither cleanup nor liveness opens a numeric PID for killing.
const psOwnedJobRuntime = `if (-not ('CucinaOwnedJob' -as [type])) { Add-Type -TypeDefinition @'
// SPDX-License-Identifier: FSL-1.1-ALv2
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
public static class CucinaOwnedJob {
 [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern IntPtr CreateJobObject(IntPtr attr,string name);
 [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern IntPtr OpenJobObject(uint access,bool inherit,string name);
 [DllImport("kernel32.dll", SetLastError=true)] static extern bool AssignProcessToJobObject(IntPtr job,IntPtr process);
 [DllImport("kernel32.dll")] static extern IntPtr GetCurrentProcess();
 [DllImport("kernel32.dll", SetLastError=true)] static extern bool TerminateJobObject(IntPtr job,uint code);
 [DllImport("kernel32.dll", SetLastError=true)] static extern bool CloseHandle(IntPtr handle);
 [DllImport("kernel32.dll", SetLastError=true)] static extern bool QueryInformationJobObject(IntPtr job,int kind,out Accounting info,uint length,IntPtr returned);
 [DllImport("kernel32.dll", SetLastError=true)] static extern IntPtr OpenProcess(uint access,bool inherit,int pid);
 [DllImport("kernel32.dll", SetLastError=true)] static extern bool IsProcessInJob(IntPtr process,IntPtr job,out bool member);
 [StructLayout(LayoutKind.Sequential)] struct Accounting {
  public long User,Kernel,PeriodUser,PeriodKernel;
  public uint PageFaults,Total,Active,Terminated;
 }
 public static IntPtr Create(string name) {
  IntPtr h=CreateJobObject(IntPtr.Zero,name);int e=Marshal.GetLastWin32Error();
  if(h==IntPtr.Zero) throw new Win32Exception(e);
  if(e==183) {CloseHandle(h);throw new InvalidOperationException("owned job name already exists");}
  return h;
 }
 public static IntPtr Open(string name) {return OpenWithAccess(name,12);}
 public static IntPtr OpenForAssignment(string name) {return OpenWithAccess(name,1);}
 static IntPtr OpenWithAccess(string name,uint access) {
  IntPtr h=OpenJobObject(access,false,name);int e=Marshal.GetLastWin32Error();
  if(h==IntPtr.Zero && e!=2) throw new Win32Exception(e);
  return h;
 }
 public static void AssignSelf(IntPtr h) {
  if(!AssignProcessToJobObject(h,GetCurrentProcess())) throw new Win32Exception(Marshal.GetLastWin32Error());
 }
 public static void Stop(IntPtr h) {
  if(!TerminateJobObject(h,1)) throw new Win32Exception(Marshal.GetLastWin32Error());
 }
 public static uint Active(IntPtr h) {
  Accounting info;
  if(!QueryInformationJobObject(h,1,out info,(uint)Marshal.SizeOf(typeof(Accounting)),IntPtr.Zero)) throw new Win32Exception(Marshal.GetLastWin32Error());
  return info.Active;
 }
 public static bool Contains(IntPtr h,int pid) {
  IntPtr p=OpenProcess(4096,false,pid);int e=Marshal.GetLastWin32Error();
  if(p==IntPtr.Zero) {if(e==87) return false;throw new Win32Exception(e);}
  try {bool member;if(!IsProcessInJob(p,h,out member)) throw new Win32Exception(Marshal.GetLastWin32Error());return member;}
  finally {CloseHandle(p);}
 }
 public static void Close(IntPtr h) {if(h!=IntPtr.Zero && !CloseHandle(h)) throw new Win32Exception(Marshal.GetLastWin32Error());}
}
'@ }
function Lock-OwnedGate($gate) {
 try { if (-not $gate.WaitOne(5000)) { throw 'owned job gate timeout' } }
 catch [Threading.AbandonedMutexException] { }
}
function New-OwnedGate([string]$name) {
 $acl=New-Object Security.AccessControl.MutexSecurity
 foreach($sid in @([Security.Principal.WindowsIdentity]::GetCurrent().User.Value,'S-1-5-18','S-1-5-32-544')) {
  $identity=New-Object Security.Principal.SecurityIdentifier($sid)
  $acl.AddAccessRule((New-Object Security.AccessControl.MutexAccessRule($identity,'FullControl','Allow')))
 }
 $created=$false
 $gate=New-Object Threading.Mutex($false,($name+'.gate'),[ref]$created,$acl)
 return $gate
}
`

// Startup tracing already defines these helpers. Keep one copy per script,
// including the encoded supervisor, to stay below Windows' command-line limit.
const psOwnedJob = psOwnedJobRuntime + psProtectJobDirectory

// Boundary names below are fixed literals, never paths, identities or payloads.
// stderr remains available before the directory is safe for diagnostic files.
const psOwnedBoundary = `function Write-OB([string]$boundary) {
 try { [Console]::Error.WriteLine('owned '+$boundary) } catch { }
}
`

const psProtectJobDirectory = psOwnedBoundary + `function Protect-OwnedDirectory([string]$path) {
 Write-OB 'protect/begin'
 Write-OB 'mkdir/begin'
 [void][IO.Directory]::CreateDirectory($path)
 Write-OB 'mkdir/end'
 Write-OB 'clr-attributes/begin'
 try {
  $oda=[IO.File]::GetAttributes($path)
  Write-OB ('clr-attributes dir='+[bool]($oda -band 16)+' reparse='+[bool]($oda -band 1024))
 } catch { Write-OB 'clr-attributes unavailable' }
 Write-OB 'clr-attributes/end'
 Write-OB 'attributes/begin'
 if ((Get-Item -LiteralPath $path).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'owned job directory is a reparse point' }
 Write-OB 'attributes/end'
 Write-OB 'acl-new/begin'
 $acl=New-Object Security.AccessControl.DirectorySecurity
 Write-OB 'acl-new/end'
 Write-OB 'acl-inheritance/begin'
 $acl.SetAccessRuleProtection($true,$false)
 Write-OB 'acl-inheritance/end'
 Write-OB 'identity/begin'
 $ownedDirectorySIDs=@([Security.Principal.WindowsIdentity]::GetCurrent().User.Value,'S-1-5-18','S-1-5-32-544')
 Write-OB 'identity/end'
 foreach($sid in $ownedDirectorySIDs) {
  Write-OB 'sid/begin'
  $identity=New-Object Security.Principal.SecurityIdentifier($sid)
  Write-OB 'sid/end'
  Write-OB 'rule/begin'
  $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($identity,'FullControl','ContainerInherit,ObjectInherit','None','Allow')))
  Write-OB 'rule/end'
 }
 Write-OB 'acl-apply/begin'
 Set-Acl -LiteralPath $path -AclObject $acl
 Write-OB 'acl-apply/end'
 Write-OB 'protect/end'
}
`

func windowsJobName(id string) string { return `Local\CucinaE2E-` + id }

// Protected stage records contain only fixed phases and an integer session ID.
// Launcher-only runtime metadata contains versions, path-class counts and cache/
// layout classifications, never paths, principals, environment values or payloads.
func psStartupTrace(dir, role string) string {
	// Emit only a fixed role marker before directory/ACL preparation. If that
	// preparation stalls, the transport can still distinguish script entry from
	// process/PowerShell startup. Do not create an unprotected diagnostic file.
	entry := fmt.Sprintf("try { [Console]::Error.WriteLine(%s) } catch { }\n", psQuote("owned startup "+role+"-entry"))
	if role == "launcher" {
		// CLR-only observations: no command discovery, imports or environment
		// changes. Path classes/counts and candidate existence are not authority.
		entry += `try {
 [Console]::Error.WriteLine('owned runtime ps='+$PSVersionTable.PSVersion+' clr='+[Environment]::Version)
 $odm=@(0,0,0,0)
 foreach($ode in ([Environment]::GetEnvironmentVariable('PSModulePath') -split ';')) {
  if (!$ode) { continue }
  if ($ode.TrimEnd([char[]]'\/') -ieq ($PSHOME+'\Modules')) { $odm[0]++ }
  elseif ($ode -match '[\\/]WindowsPowerShell[\\/]') { $odm[1]++ }
  elseif ($ode -match '[\\/]PowerShell[\\/]') { $odm[2]++ }
  else { $odm[3]++ }
 }
 $odc=[Environment]::GetEnvironmentVariable('PSModuleAnalysisCachePath'); $odk='custom'
 if (!$odc) { $odk='default'; $odc=[IO.Path]::Combine([Environment]::GetFolderPath('LocalApplicationData'),'Microsoft\Windows\PowerShell\ModuleAnalysisCache') }
 [Console]::Error.WriteLine('owned module-classes='+($odm -join ',')+' cache='+$odk+','+[IO.File]::Exists($odc))
 [Console]::Error.WriteLine('owned layout manifest='+[IO.File]::Exists($PSHOME+'\Modules\Microsoft.PowerShell.Management\Microsoft.PowerShell.Management.psd1')+' dll='+[IO.File]::Exists($PSHOME+'\Microsoft.PowerShell.Commands.Management.dll'))
} catch { try { [Console]::Error.WriteLine('owned runtime metadata unavailable') } catch { } }
`
	}
	return "$ErrorActionPreference='Stop'\n" + entry + psProtectJobDirectory + fmt.Sprintf(`
$J=%s
function Report-OwnedTraceFailure {
 try { [Console]::Error.WriteLine('owned startup diagnostics unavailable') } catch { }
}
$script:OwnedTraceReady=$false
Write-OB 'protect-call/begin'
try { Protect-OwnedDirectory $J; $script:OwnedTraceReady=$true } catch { Report-OwnedTraceFailure }
Write-OB 'protect-call/end'
function Write-OwnedStage([string]$phase) {
 if (-not $script:OwnedTraceReady) { return }
 $staging=$null; $previous=$null
 try {
  if ($phase -cnotin @('runtime-load','runtime-ready','open-job','assign-self','payload-start','payload-exit','cim-create','cim-created','supervisor-ready')) { throw 'invalid owned startup phase' }
  Write-OB 'trace-session/begin'
  $record=$phase+' '+[string]([Diagnostics.Process]::GetCurrentProcess().SessionId)
  Write-OB 'trace-session/end'
  if ($record.Length -gt 96) { throw 'owned startup record exceeds its bound' }
  Write-OB 'trace-path/begin'
  $path=Join-Path $J %s
  $staging=$path+'.tmp'; $previous=$path+'.previous'
  Write-OB 'trace-path/end'
  Write-OB 'trace-write/begin'
  [IO.File]::WriteAllText($staging,$record)
  Write-OB 'trace-write/end'
  Write-OB 'trace-publish/begin'
  if ([IO.File]::Exists($path)) {
   # PS5 marshals a null backup argument as an empty path. Supply a real,
   # private same-directory backup so replacement remains atomic.
   [IO.File]::Replace($staging,$path,$previous)
  } else { [IO.File]::Move($staging,$path) }
  Write-OB 'trace-publish/end'
 } catch { Report-OwnedTraceFailure } finally {
  foreach($file in @($staging,$previous)) {
   try { if ($file -and [IO.File]::Exists($file)) { [IO.File]::Delete($file) } } catch { Report-OwnedTraceFailure }
  }
 }
}
Write-OwnedStage 'runtime-load'
`, psQuote(dir), psQuote(role+"-stage"))
}

func psStartOwnedJob(dir, id, prepare string) string {
	name := psQuote(windowsJobName(id))
	q := psQuote(dir)
	runner := psStartupTrace(dir, "supervisor") + psOwnedJobRuntime + fmt.Sprintf(`
$J=%s; $name=%s
Write-OwnedStage 'runtime-ready'
$startupPhase='runtime-ready'
$gate=New-OwnedGate $name
$h=[IntPtr]::Zero
try {
 Lock-OwnedGate $gate
 try {
  if (Test-Path -LiteralPath (Join-Path $J 'stop')) { return }
  $startupPhase='open-job'
  $h=[CucinaOwnedJob]::OpenForAssignment($name)
  if ($h -eq [IntPtr]::Zero) { throw 'owned job missing before payload assignment' }
  $startupPhase='assign-self'
  [CucinaOwnedJob]::AssignSelf($h)
  [IO.File]::WriteAllText((Join-Path $J 'pid'),[string]$PID)
  [IO.File]::WriteAllText((Join-Path $J 'ready'),'owned')
 } finally { $gate.ReleaseMutex() }
 $startupPhase='payload-start'
 Write-OwnedStage $startupPhase
 %s
 $startupPhase='payload-exit'
 Lock-OwnedGate $gate
 try {
  if (Test-Path -LiteralPath (Join-Path $J 'stop')) { throw 'job was canceled before completion' }
  [IO.File]::WriteAllText((Join-Path $J 'exit.tmp'),[string]$p.ExitCode)
  Move-Item -Force -LiteralPath (Join-Path $J 'exit.tmp') -Destination (Join-Path $J 'exit')
 } finally { $gate.ReleaseMutex() }
 Write-OwnedStage $startupPhase
} catch {
 Write-OwnedStage $startupPhase
 [IO.File]::WriteAllText((Join-Path $J 'start-error'),'owned supervisor failed')
 throw
} finally { [CucinaOwnedJob]::Close($h);$gate.Dispose() }
`, q, name, psRunCmd(dir))
	return psStartupTrace(dir, "launcher") + psOwnedJobRuntime + fmt.Sprintf(`
$J=%s; $name=%s
Write-OwnedStage 'runtime-ready'
Protect-OwnedDirectory $J
$gate=New-OwnedGate $name
$h=[IntPtr]::Zero
try {
 Lock-OwnedGate $gate
 try {
  if (Test-Path -LiteralPath (Join-Path $J 'stop')) { throw 'job canceled before startup' }
  $h=[CucinaOwnedJob]::Create($name)
  [IO.File]::WriteAllText((Join-Path $J 'owner'),$name)
 } finally { $gate.ReleaseMutex() }
 %s
 [IO.File]::WriteAllText((Join-Path $J 'run.ps1'),[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String(%s)))
 $cl='powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "'+(Join-Path $J 'run.ps1')+'"'
 Write-OwnedStage 'cim-create'
 $r=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$cl}
 if ($r.ReturnValue -ne 0) { throw 'owned supervisor creation failed' }
 $deadline=[DateTime]::UtcNow.AddSeconds(20)
 Write-OwnedStage 'cim-created'
 while (-not (Test-Path -LiteralPath (Join-Path $J 'ready'))) {
  if ((Test-Path -LiteralPath (Join-Path $J 'start-error')) -or (Test-Path -LiteralPath (Join-Path $J 'stop'))) { throw 'owned supervisor did not start' }
  if ([DateTime]::UtcNow -ge $deadline) { throw 'owned supervisor startup timeout' }
  Start-Sleep -Milliseconds 25
 }
 Write-OwnedStage 'supervisor-ready'
 'started'
} catch {
 Lock-OwnedGate $gate
 try {
  [IO.File]::WriteAllText((Join-Path $J 'stop'),'startup failed')
  if ($h -ne [IntPtr]::Zero) { [CucinaOwnedJob]::Stop($h) }
 } finally { $gate.ReleaseMutex() }
 throw
} finally { [CucinaOwnedJob]::Close($h);$gate.Dispose() }
`, q, name, prepare, psQuote(b64(runner)))
}

func psOwnedJobStatus(dir string) string {
	return "$ErrorActionPreference='Stop'\n" + psOwnedJob + fmt.Sprintf(`
$J=%s
$name='Local\CucinaE2E-'+(Split-Path -Leaf $J)
function Len($n) { $f=Join-Path $J $n; if (Test-Path -LiteralPath $f) { (Get-Item -LiteralPath $f).Length } else { 0 } }
$gate=New-OwnedGate $name
$h=[IntPtr]::Zero
try {
 Lock-OwnedGate $gate
 try {
  if (Test-Path -LiteralPath (Join-Path $J 'exit')) { $st='exited '+(Get-Content -LiteralPath (Join-Path $J 'exit') -Raw).Trim() }
  elseif (Test-Path -LiteralPath (Join-Path $J 'stopped')) { $st='lost 0' }
  elseif (-not (Test-Path -LiteralPath (Join-Path $J 'pid'))) { $st='starting 0' }
  else {
   $h=[CucinaOwnedJob]::Open($name)
   if ($h -ne [IntPtr]::Zero -and [CucinaOwnedJob]::Contains($h,[int](Get-Content -LiteralPath (Join-Path $J 'pid') -Raw).Trim())) { $st='running 0' } else { $st='lost 0' }
  }
  "$st $(Len 'stdout') $(Len 'stderr')"
 } finally { $gate.ReleaseMutex() }
} finally { [CucinaOwnedJob]::Close($h);$gate.Dispose() }
`, psQuote(dir))
}

func psStopOwnedJob(j Job) string {
	return "$ErrorActionPreference='Stop'\n" + psOwnedJob + fmt.Sprintf(`
$J=%s; $name=%s
Protect-OwnedDirectory $J
$gate=New-OwnedGate $name
$h=[IntPtr]::Zero
try {
 Lock-OwnedGate $gate
 try {
  if (Test-Path -LiteralPath (Join-Path $J 'exit')) { return }
  [IO.File]::WriteAllText((Join-Path $J 'stop'),'requested')
  if (Test-Path -LiteralPath (Join-Path $J 'owner')) {
   if ((Get-Content -LiteralPath (Join-Path $J 'owner') -Raw) -ne $name) { throw 'owned job identity mismatch' }
   $h=[CucinaOwnedJob]::Open($name)
  }
  if ($h -ne [IntPtr]::Zero) {
   [CucinaOwnedJob]::Stop($h)
   $deadline=[DateTime]::UtcNow.AddSeconds(5)
   while ([CucinaOwnedJob]::Active($h) -ne 0) {
    if ([DateTime]::UtcNow -ge $deadline) { throw 'owned job processes did not stop' }
    Start-Sleep -Milliseconds 25
   }
  }
  # An early cancellation leaves a marker checked under this SAME gate before
  # CIM assignment/payload launch. No future payload can start after this stop.
  [IO.File]::WriteAllText((Join-Path $J 'stopped'),'confirmed')
 } finally { $gate.ReleaseMutex() }
} finally { [CucinaOwnedJob]::Close($h);$gate.Dispose() }
`, psQuote(j.Dir), psQuote(windowsJobName(j.ID)))
}
