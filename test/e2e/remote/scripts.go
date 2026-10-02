// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// resultMarker introduces the status line a foreground run prints; the user
// script's own output goes to files, so the transport never truncates it.
const resultMarker = "CUCINA-E2E-RESULT"

// Inline sizes of a foreground run's output (raw bytes, base64 adds a third):
// 12,000 + 4,000 bytes keep the transport response under SSM's 24,000
// character limit.
const (
	inlineStdout = 12000
	inlineStderr = 4000
)

// dialect renders the job/transfer scripts for one shell.
type dialect interface {
	join(dir, name string) string
	// foreground runs a script synchronously with output in dir and prints
	// the result marker plus the inlined output heads.
	foreground(dir, script string, o Opts) string
	// start launches a detached background job in dir.
	start(dir, id, script string, o Opts) string
	status(dir string) string
	// read prints base64 of up to max bytes of file from off.
	read(file string, off int64, max int) string
	// appendB64 appends decoded base64 to file.part (truncating first if first).
	appendB64(file, b64 string, first bool) string
	// commit renames file.part to file and prints its SHA-256.
	commit(file string) string
	sha256(file string) string
	size(file string) string
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------- POSIX sh

// sh renders scripts for Linux (SSM AWS-RunShellScript runs them as root) and
// macOS (local). Only POSIX sh, coreutils/BSD-compatible flags are used
// (`wc -c`, `head -c`, `tail -c +N`, `base64`), so the same scripts are
// exercised on the dev Mac by the integration test.
type sh struct{}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (sh) join(dir, name string) string { return strings.TrimSuffix(dir, "/") + "/" + name }

// cmdFile writes the user script, prefixed with environment and directory,
// into dir/cmd.sh and prints the command that runs it (optionally as a user).
func (d sh) cmdFile(dir, script string, o Opts) (write, run string) {
	var hdr strings.Builder
	hdr.WriteString("set -u\n")
	for _, k := range sortedKeys(o.Env) {
		fmt.Fprintf(&hdr, "export %s=%s\n", k, shQuote(o.Env[k]))
	}
	if o.Dir != "" {
		fmt.Fprintf(&hdr, "mkdir -p %s && cd %s || exit 97\n", shQuote(o.Dir), shQuote(o.Dir))
	}
	full := hdr.String() + script + "\n"
	q := shQuote(dir)
	write = fmt.Sprintf("mkdir -p %s && chmod 755 %s && printf '%%s' %s | base64 -d > %s/cmd.sh && chmod 644 %s/cmd.sh || exit 98\n",
		q, q, shQuote(b64(full)), q, q)
	run = fmt.Sprintf("/bin/bash %s/cmd.sh", q)
	if o.User != "" {
		run = fmt.Sprintf("sudo -u %s -H /bin/bash %s/cmd.sh", shQuote(o.User), q)
	}
	return write, run
}

func (d sh) foreground(dir, script string, o Opts) string {
	write, run := d.cmdFile(dir, script, o)
	q := shQuote(dir)
	return write + fmt.Sprintf(`%s </dev/null >%s/stdout 2>%s/stderr
rc=$?
echo "$rc" > %s/exit
echo "%s $rc $(wc -c <%s/stdout | tr -d ' ') $(wc -c <%s/stderr | tr -d ' ')"
head -c %d %s/stdout | base64 | tr -d '\n'; echo
head -c %d %s/stderr | base64 | tr -d '\n'; echo
`, run, q, q, q, resultMarker, q, q, inlineStdout, q, inlineStderr, q)
}

func (d sh) start(dir, id, script string, o Opts) string {
	write, run := d.cmdFile(dir, script, o)
	q := shQuote(dir)
	// pid and exit are written atomically (tmp + rename): a status poll must
	// never read a half-written file.
	runner := fmt.Sprintf("echo $$ > %s/pid.tmp && mv %s/pid.tmp %s/pid\n%s </dev/null >%s/stdout 2>%s/stderr\necho $? > %s/exit.tmp && mv %s/exit.tmp %s/exit\n",
		q, q, q, run, q, q, q, q, q)
	// systemd-run puts the job in its own unit (it survives the SSM agent and
	// its cgroup); elsewhere nohup + background is enough.
	return write + fmt.Sprintf(`printf '%%s' %s | base64 -d > %s/run.sh || exit 98
if command -v systemd-run >/dev/null 2>&1 && [ "$(id -u)" = 0 ]; then
  systemd-run --quiet --collect --unit=cucina-e2e-%s /bin/sh %s/run.sh || exit 99
else
  nohup /bin/sh %s/run.sh >/dev/null 2>&1 </dev/null &
fi
echo started
`, shQuote(b64(runner)), q, id, q, q)
}

func (sh) status(dir string) string {
	q := shQuote(dir)
	// Order matters: liveness is sampled before the exit file is checked (a
	// job that exits in between has already written its exit code, so it
	// reads as exited, never as lost), and output sizes are read last (once
	// the exit file exists, stdout/stderr are final). The liveness probe uses
	// only builtins (read, kill): a fork failure on a busy host must not make
	// a running job look lost.
	return fmt.Sprintf(`J=%s
alive=0; p=; [ -f "$J/pid" ] && read -r p <"$J/pid"; [ -n "$p" ] && kill -0 "$p" 2>/dev/null && alive=1
if [ -f "$J/exit" ]; then st="exited $(cat "$J/exit")"
elif [ ! -f "$J/pid" ]; then st="starting 0"
elif [ "$alive" = 1 ]; then st="running 0"
else st="lost 0"; fi
o=$(wc -c <"$J/stdout" 2>/dev/null | tr -d ' '); e=$(wc -c <"$J/stderr" 2>/dev/null | tr -d ' ')
echo "$st ${o:-0} ${e:-0}"
`, q)
}

func (sh) read(file string, off int64, max int) string {
	return fmt.Sprintf("tail -c +%d %s | head -c %d | base64 | tr -d '\\n'; echo\n", off+1, shQuote(file), max)
}

func (sh) appendB64(file, data string, first bool) string {
	redir := ">>"
	if first {
		redir = ">"
	}
	p := shQuote(file + ".part")
	return fmt.Sprintf("mkdir -p \"$(dirname %s)\" && printf '%%s' %s | base64 -d %s %s\n", p, shQuote(data), redir, p)
}

func (d sh) commit(file string) string {
	return fmt.Sprintf("mv %s %s && %s", shQuote(file+".part"), shQuote(file), d.sha256(file))
}

func (sh) sha256(file string) string {
	q := shQuote(file)
	return fmt.Sprintf("{ sha256sum %s 2>/dev/null || shasum -a 256 %s; } | cut -d' ' -f1\n", q, q)
}

func (sh) size(file string) string { return fmt.Sprintf("wc -c <%s | tr -d ' '\n", shQuote(file)) }

// ---------------------------------------------------------------- PowerShell

// ps renders Windows PowerShell 5.1 scripts (SSM AWS-RunPowerShellScript runs
// them as SYSTEM). Background jobs are created through Win32_Process (CIM) so
// they leave the SSM agent's job object and survive the command.
type ps struct{}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (ps) join(dir, name string) string { return strings.TrimSuffix(dir, `\`) + `\` + name }

func (d ps) cmdFile(dir, script string, o Opts) string {
	var hdr strings.Builder
	hdr.WriteString("$ErrorActionPreference = 'Stop'\n$ProgressPreference = 'SilentlyContinue'\n")
	for _, k := range sortedKeys(o.Env) {
		fmt.Fprintf(&hdr, "$env:%s = %s\n", k, psQuote(o.Env[k]))
	}
	if o.Dir != "" {
		fmt.Fprintf(&hdr, "New-Item -ItemType Directory -Force -Path %s | Out-Null\nSet-Location %s\n", psQuote(o.Dir), psQuote(o.Dir))
	}
	// A native command's exit code becomes the script's; cmdlet failures
	// throw (exit 1) under ErrorActionPreference=Stop.
	full := hdr.String() + script + "\nexit $LASTEXITCODE\n"
	q := psQuote(dir)
	return fmt.Sprintf("New-Item -ItemType Directory -Force -Path %s | Out-Null\n[IO.File]::WriteAllText((Join-Path %s 'cmd.ps1'), [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String(%s)))\n",
		q, q, psQuote(b64(full)))
}

const psHead64 = `function Get-Head64([string]$f, [int]$n) {
  $fs = [IO.File]::Open($f, 'Open', 'Read', 'ReadWrite'); try {
    $b = New-Object byte[] ([Math]::Min([int64]$n, $fs.Length)); [void]$fs.Read($b, 0, $b.Length); [Convert]::ToBase64String($b)
  } finally { $fs.Close() } }
`

func psRunCmd(dir string) string {
	q := psQuote(dir)
	return fmt.Sprintf(`$p = Start-Process -FilePath powershell.exe -ArgumentList '-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',(Join-Path %s 'cmd.ps1') -RedirectStandardOutput (Join-Path %s 'stdout') -RedirectStandardError (Join-Path %s 'stderr') -NoNewWindow -Wait -PassThru`, q, q, q)
}

func (d ps) foreground(dir, script string, o Opts) string {
	q := psQuote(dir)
	return d.cmdFile(dir, script, o) + psRunCmd(dir) + fmt.Sprintf(`
$rc = $p.ExitCode
Set-Content -Path (Join-Path %s 'exit') -Value $rc
$o = (Get-Item (Join-Path %s 'stdout')).Length; $e = (Get-Item (Join-Path %s 'stderr')).Length
"%s $rc $o $e"
%sGet-Head64 (Join-Path %s 'stdout') %d
Get-Head64 (Join-Path %s 'stderr') %d
`, q, q, q, resultMarker, psHead64, q, inlineStdout, q, inlineStderr)
}

func (d ps) start(dir, id, script string, o Opts) string {
	q := psQuote(dir)
	runner := fmt.Sprintf("Set-Content -Path (Join-Path %s 'pid.tmp') -Value $PID\nMove-Item -Force (Join-Path %s 'pid.tmp') (Join-Path %s 'pid')\n%s\nSet-Content -Path (Join-Path %s 'exit.tmp') -Value $p.ExitCode\nMove-Item -Force (Join-Path %s 'exit.tmp') (Join-Path %s 'exit')\n",
		q, q, q, psRunCmd(dir), q, q, q)
	return d.cmdFile(dir, script, o) + fmt.Sprintf(`[IO.File]::WriteAllText((Join-Path %s 'run.ps1'), [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String(%s)))
$cl = 'powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + (Join-Path %s 'run.ps1') + '"'
$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{ CommandLine = $cl }
if ($r.ReturnValue -ne 0) { throw "Win32_Process.Create failed: $($r.ReturnValue)" }
"started"
`, q, psQuote(b64(runner)), q)
}

func (ps) status(dir string) string {
	q := psQuote(dir)
	// Same ordering as the sh variant: liveness, then exit file, then sizes.
	return fmt.Sprintf(`$J = %s
function Len($n) { $f = Join-Path $J $n; if (Test-Path $f) { (Get-Item $f).Length } else { 0 } }
$alive = $false
if (Test-Path (Join-Path $J 'pid')) { $alive = [bool](Get-Process -Id ([int](Get-Content (Join-Path $J 'pid') -Raw).Trim()) -ErrorAction SilentlyContinue) }
if (Test-Path (Join-Path $J 'exit')) { $st = "exited $((Get-Content (Join-Path $J 'exit') -Raw).Trim())" }
elseif (-not (Test-Path (Join-Path $J 'pid'))) { $st = 'starting 0' }
elseif ($alive) { $st = 'running 0' }
else { $st = 'lost 0' }
"$st $(Len 'stdout') $(Len 'stderr')"
`, q)
}

func (ps) read(file string, off int64, max int) string {
	return fmt.Sprintf(`$fs = [IO.File]::Open(%s, 'Open', 'Read', 'ReadWrite'); try { [void]$fs.Seek(%d, 'Begin'); $b = New-Object byte[] %d; $n = $fs.Read($b, 0, %d); [Convert]::ToBase64String($b, 0, $n) } finally { $fs.Close() }
`, psQuote(file), off, max, max)
}

func (ps) appendB64(file, data string, first bool) string {
	mode := "Append"
	if first {
		mode = "Create"
	}
	p := psQuote(file + ".part")
	return fmt.Sprintf(`New-Item -ItemType Directory -Force -Path (Split-Path -Parent %s) | Out-Null
$b = [Convert]::FromBase64String(%s); $fs = [IO.File]::Open(%s, '%s', 'Write'); try { $fs.Write($b, 0, $b.Length) } finally { $fs.Close() }
`, p, psQuote(data), p, mode)
}

func (d ps) commit(file string) string {
	return fmt.Sprintf("Move-Item -Force %s %s\n%s", psQuote(file+".part"), psQuote(file), d.sha256(file))
}

func (ps) sha256(file string) string {
	return fmt.Sprintf("(Get-FileHash -Algorithm SHA256 -LiteralPath %s).Hash.ToLower()\n", psQuote(file))
}

func (ps) size(file string) string {
	return fmt.Sprintf("(Get-Item -LiteralPath %s).Length\n", psQuote(file))
}
