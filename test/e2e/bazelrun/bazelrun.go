// SPDX-License-Identifier: FSL-1.1-ALv2

// Package bazelrun runs Bazel on a campaign host with the §10.2 flags and the
// §10.4 collector flags, then brings the artifacts back to the dev Mac and
// parses them: the build event stream (wall time, runner counts, critical
// path, network bytes, heap), the compact execution log (per-spawn queue/
// setup/execution time, input/output bytes, routing), the --profile trace,
// and the Bazel server's peak memory (VmHWM on Linux via /proc, PeakWorkingSet64
// on Windows via Get-Process).
package bazelrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/collect/bep"
	"github.com/sloper-ai/cucina/test/e2e/collect/execlog"
	"github.com/sloper-ai/cucina/test/e2e/collect/profile"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// Invocation is one Bazel command on a host.
type Invocation struct {
	// Name identifies the invocation in artifact paths ("T1-build").
	Name string
	Host remote.Host
	// Workspace is the checkout on the host (the Abseil overlay or Cucina).
	Workspace string
	// Bazel is the client binary (default "bazel"; bazelisk honours .bazelversion).
	Bazel string
	// Startup options precede the command (e.g. --output_user_root=C:/b).
	Startup []string
	Command string   // build | test | clean | aquery | ...
	Args    []string // flags, then "--", then targets
	// FreshServer shuts the server down first so VmHWM/PeakWorkingSet64
	// cover exactly this invocation.
	FreshServer bool
	// Collect adds the BEP, compact execution log, profile and memory
	// profile flags and fetches/parses them afterwards.
	Collect bool
	// User runs Bazel as this user on Linux (never root: rules refuse it).
	User string
	// Env is the complete environment Bazel sees: the build event stream
	// records the client environment (--client_env), so it is kept minimal
	// on purpose (no credentials can leak into artifacts).
	Env map[string]string
	// Unset removes variables the Windows job inherits from the machine
	// environment (e.g. BAZEL_SH for cross configurations); elsewhere Bazel
	// starts from an empty environment anyway.
	Unset []string
	// Poll is the job status interval (default 15 s).
	Poll time.Duration
}

// Outcome is what one invocation produced.
type Outcome struct {
	Name      string        `json:"name"`
	Command   string        `json:"command"`
	ExitCode  int           `json:"exitCode"`
	Wall      time.Duration `json:"wall"`
	ServerPID string        `json:"serverPid,omitempty"`
	// PeakServerRSSBytes is VmHWM (Linux) or PeakWorkingSet64 (Windows) of the
	// Bazel server after the command; 0 where neither exists (macOS).
	PeakServerRSSBytes int64            `json:"peakServerRssBytes"`
	BEP                *bep.Summary     `json:"bep,omitempty"`
	ExecLog            *execlog.Summary `json:"execLog,omitempty"`
	Profile            *profile.Summary `json:"profile,omitempty"`
	// TimeToFirstRemoteAction is the first remote spawn's start minus the
	// build start (§10.4).
	TimeToFirstRemoteAction time.Duration `json:"timeToFirstRemoteAction,omitempty"`
	// LocalDir holds the fetched raw artifacts on the dev Mac.
	LocalDir string `json:"localDir"`
	// Log is the tail of Bazel's console output.
	Log string `json:"log,omitempty"`
	// ExecLogPath is the local path of the compact execution log, for
	// routing checks that need per-spawn data.
	ExecLogPath string `json:"execLogPath,omitempty"`
}

// Succeeded reports Bazel exit code 0.
func (o *Outcome) Succeeded() bool { return o.ExitCode == 0 }

// Artifact file names on the host (inside the invocation's output dir).
const (
	bepFile     = "bep.bin"
	execLogFile = "exec.log.zst"
	profileFile = "profile.json.gz"
	memProfile  = "memory.txt"
	consoleFile = "console.log"
	outcomeFile = "outcome.json"
)

func (inv *Invocation) bazel() string {
	if inv.Bazel != "" {
		return inv.Bazel
	}
	if inv.Host.OS() == remote.Windows {
		return "bazel.exe"
	}
	return "bazel"
}

func (inv *Invocation) outDir() string {
	sep := "/"
	if inv.Host.OS() == remote.Windows {
		sep = `\`
	}
	return strings.TrimSuffix(inv.Host.WorkDir(), sep) + sep + "bazel-runs" + sep + inv.Name
}

// FullArgs returns the command-line arguments after the startup options:
// the command, the caller's flags and, with Collect, the collector flags
// (placed before a "--" target separator if present).
func (inv *Invocation) FullArgs() []string {
	args := append([]string{inv.Command}, inv.Args...)
	if !inv.Collect {
		return args
	}
	sep := "/"
	if inv.Host.OS() == remote.Windows {
		sep = `\`
	}
	d := inv.outDir() + sep
	collect := []string{
		"--build_event_binary_file=" + d + bepFile,
		"--execution_log_compact_file=" + d + execLogFile,
		"--profile=" + d + profileFile,
		"--memory_profile=" + d + memProfile,
	}
	for i, a := range args {
		if a == "--" {
			return append(append(append([]string{}, args[:i]...), collect...), args[i:]...)
		}
	}
	return append(args, collect...)
}

// Script renders the host script (sh or PowerShell).
func (inv *Invocation) Script() (string, error) {
	for _, a := range append(append([]string{}, inv.Startup...), inv.FullArgs()...) {
		if strings.ContainsAny(a, " \t\n\"'") && inv.Host.OS() == remote.Windows {
			return "", fmt.Errorf("argument %q needs quoting: put it into a .bazelrc instead", a)
		}
	}
	if inv.Host.OS() == remote.Windows {
		return inv.psScript(), nil
	}
	return inv.shScript(), nil
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// environment preserves only the local CLI's non-secret profile selection
// in addition to explicit invocation variables. The lead may keep campaign
// credentials in a private CUCINA_CONFIG_DIR; env -i must not send the helper
// to a different default profile. Remote clients use their own login state.
func (inv *Invocation) environment() map[string]string {
	env := map[string]string{}
	for k, v := range inv.Env {
		env[k] = v
	}
	if inv.Host.Name() == "dev-mac" {
		for _, k := range []string{"CUCINA_CONFIG_DIR", "CUCINA_PROFILE"} {
			if _, ok := env[k]; !ok {
				if v := os.Getenv(k); v != "" {
					env[k] = v
				}
			}
		}
	}
	return env
}

func (inv *Invocation) shScript() string {
	var b strings.Builder
	out := inv.outDir()
	bz := inv.bazel()
	startup := make([]string, len(inv.Startup))
	for i, s := range inv.Startup {
		startup[i] = shq(s)
	}
	args := inv.FullArgs()
	for i := range args {
		args[i] = shq(args[i])
	}
	// A minimal, explicit environment: HOME/PATH/USER from the job shell plus
	// the caller's variables (which win).
	envs := []string{"env", "-i", `HOME="$HOME"`, `PATH="$PATH"`, `USER="${USER:-$(id -un)}"`, "LANG=C.UTF-8"}
	clientEnv := inv.environment()
	for _, k := range sortedKeys(clientEnv) {
		envs = append(envs, shq(k+"="+clientEnv[k]))
	}
	fmt.Fprintf(&b, "set -u\nOUT=%s\nmkdir -p \"$OUT\"\ncd %s || exit 2\n", shq(out), shq(inv.Workspace))
	run := strings.Join(envs, " ") + " " + shq(bz) + " " + strings.Join(startup, " ")
	if inv.FreshServer {
		fmt.Fprintf(&b, "%s shutdown >/dev/null 2>&1 || true\n", run)
	}
	fmt.Fprintf(&b, "start=$(date +%%s)\n%s %s >\"$OUT/%s\" 2>&1\nrc=$?\nend=$(date +%%s)\n", run, strings.Join(args, " "), consoleFile)
	fmt.Fprintf(&b, "pid=$(%s info server_pid 2>/dev/null | tail -n 1)\n", run)
	b.WriteString(`hwm=0
if [ -n "$pid" ] && [ -r "/proc/$pid/status" ]; then hwm=$(awk '/^VmHWM:/ {print $2 * 1024}' "/proc/$pid/status"); fi
`)
	fmt.Fprintf(&b, "printf '{\"exit\":%%s,\"start\":%%s,\"end\":%%s,\"serverPid\":\"%%s\",\"peakRssBytes\":%%s}\\n' \"$rc\" \"$start\" \"$end\" \"$pid\" \"${hwm:-0}\" > \"$OUT/%s\"\n", outcomeFile)
	b.WriteString("tail -c 4000 \"$OUT/" + consoleFile + "\"\n")
	return b.String()
}

func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (inv *Invocation) psScript() string {
	var b strings.Builder
	out := inv.outDir()
	quote := func(xs []string) string {
		q := make([]string, len(xs))
		for i, x := range xs {
			q[i] = psq(x)
		}
		return "@(" + strings.Join(q, ", ") + ")"
	}
	// Native commands report through exit codes; with 'Stop', Windows
	// PowerShell 5.1 turns redirected native stderr into terminating errors.
	fmt.Fprintf(&b, "$ErrorActionPreference = 'Continue'\n$out = %s\nNew-Item -ItemType Directory -Force -Path $out | Out-Null\nSet-Location %s\n", psq(out), psq(inv.Workspace))
	// BEP includes client_env: keep only the Windows process/runtime paths,
	// not inherited AWS credentials, tokens or arbitrary service variables.
	b.WriteString(`$keep = @('SYSTEMROOT','WINDIR','COMSPEC','TEMP','TMP','USERPROFILE','HOMEDRIVE','HOMEPATH','APPDATA','LOCALAPPDATA','PROGRAMDATA','PROGRAMFILES','PROGRAMFILES(X86)','PROGRAMW6432','PROCESSOR_ARCHITECTURE','PATH','PATHEXT','NUMBER_OF_PROCESSORS','USERNAME','USERDOMAIN','BAZEL_SH')
Get-ChildItem Env: | Where-Object { $keep -notcontains $_.Name.ToUpperInvariant() } | ForEach-Object { Remove-Item -LiteralPath ('Env:' + $_.Name) }
`)
	for _, k := range inv.Unset {
		fmt.Fprintf(&b, "Remove-Item -ErrorAction SilentlyContinue %s\n", psq(`Env:\`+k))
	}
	for _, k := range sortedKeys(inv.Env) {
		fmt.Fprintf(&b, "$env:%s = %s\n", k, psq(inv.Env[k]))
	}
	fmt.Fprintf(&b, "$bazel = %s\n$startup = %s\n", psq(inv.bazel()), quote(inv.Startup))
	if inv.FreshServer {
		b.WriteString("& $bazel @startup shutdown 2>$null | Out-Null\n")
	}
	fmt.Fprintf(&b, `$bazelArgs = $startup + %s
$sw = [Diagnostics.Stopwatch]::StartNew()
$p = Start-Process -FilePath $bazel -ArgumentList $bazelArgs -RedirectStandardOutput (Join-Path $out 'console.out') -RedirectStandardError (Join-Path $out '%s') -NoNewWindow -PassThru
$null = $p.Handle
$p.WaitForExit()
$rc = $p.ExitCode
$sw.Stop()
$serverPid = (& $bazel @startup info server_pid 2>$null | Select-Object -Last 1)
$peak = 0
if ($serverPid) { $proc = Get-Process -Id ([int]$serverPid) -ErrorAction SilentlyContinue; if ($proc) { $peak = $proc.PeakWorkingSet64 } }
$end = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
@{ exit = $rc; start = ($end - [int]$sw.Elapsed.TotalSeconds); end = $end; serverPid = "$serverPid"; peakRssBytes = $peak } | ConvertTo-Json -Compress | Set-Content -Encoding ASCII (Join-Path $out '%s')
$c = Get-Content -Raw (Join-Path $out '%s'); if ($c.Length -gt 4000) { $c.Substring($c.Length - 4000) } else { $c }
exit 0
`, quote(inv.FullArgs()), consoleFile, outcomeFile, consoleFile)
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Run executes the invocation as a background job, waits, fetches the
// artifacts into localDir and parses them.
func Run(ctx context.Context, inv Invocation, localDir string) (*Outcome, error) {
	script, err := inv.Script()
	if err != nil {
		return nil, err
	}
	poll := inv.Poll
	if poll == 0 {
		poll = 15 * time.Second
	}
	_, res, err := remote.RunJob(ctx, inv.Host, script, remote.Opts{User: inv.User}, poll, nil)
	if err != nil {
		return nil, fmt.Errorf("%s on %s: %w", inv.Name, inv.Host.Name(), err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("%s on %s: wrapper exit %d: %s", inv.Name, inv.Host.Name(), res.ExitCode, string(res.Stderr))
	}
	o := &Outcome{Name: inv.Name, Command: inv.Command, LocalDir: localDir, Log: string(res.Stdout)}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return o, err
	}
	sep := "/"
	if inv.Host.OS() == remote.Windows {
		sep = `\`
	}
	remoteDir := inv.outDir() + sep
	fetch := func(name string) (string, error) {
		local := filepath.Join(localDir, name)
		return local, inv.Host.Get(ctx, remoteDir+name, local)
	}
	oc, err := fetch(outcomeFile)
	if err != nil {
		return o, fmt.Errorf("%s: fetch outcome: %w", inv.Name, err)
	}
	if err := parseOutcome(oc, o); err != nil {
		return o, err
	}
	if !inv.Collect {
		return o, nil
	}
	if p, err := fetch(bepFile); err == nil {
		if o.BEP, err = bep.ReadFile(p); err != nil {
			return o, fmt.Errorf("%s: %w", inv.Name, err)
		}
	} else {
		return o, fmt.Errorf("%s: fetch BEP: %w", inv.Name, err)
	}
	if p, err := fetch(execLogFile); err == nil {
		l, err := execlog.ReadFile(p)
		if err != nil {
			return o, fmt.Errorf("%s: %w", inv.Name, err)
		}
		s := l.Summarize()
		o.ExecLog, o.ExecLogPath = &s, p
		if first := l.FirstRemoteStart(); !first.IsZero() && o.BEP != nil && !o.BEP.Started.IsZero() {
			o.TimeToFirstRemoteAction = first.Sub(o.BEP.Started)
		}
	}
	if p, err := fetch(profileFile); err == nil {
		if o.Profile, err = profile.ReadFile(p, 25); err != nil {
			return o, fmt.Errorf("%s: %w", inv.Name, err)
		}
	}
	_, _ = fetch(memProfile)
	if o.BEP != nil && o.BEP.WallTime > 0 {
		o.Wall = o.BEP.WallTime
	}
	return o, nil
}

func parseOutcome(path string, o *Outcome) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw struct {
		Exit         int             `json:"exit"`
		Start        int64           `json:"start"`
		End          int64           `json:"end"`
		ServerPID    json.RawMessage `json:"serverPid"`
		PeakRSSBytes json.Number     `json:"peakRssBytes"`
	}
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(string(b), "\ufeff")))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("outcome %s: %w", path, err)
	}
	o.ExitCode = raw.Exit
	o.Wall = time.Duration(raw.End-raw.Start) * time.Second
	o.ServerPID = strings.Trim(string(raw.ServerPID), `"`)
	if n, err := strconv.ParseFloat(raw.PeakRSSBytes.String(), 64); err == nil {
		o.PeakServerRSSBytes = int64(n)
	}
	return nil
}
