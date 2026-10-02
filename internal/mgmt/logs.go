// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/domain"
)

const (
	defaultTailLines = 200
	maxTailLines     = 2000
	// ssmOutputBudget keeps one invocation's output below the 24,000 characters SSM
	// GetCommandInvocation returns inline.
	ssmOutputBudget = 22000
	// maxVMLogBytes bounds a Tart worker log snapshot.
	maxVMLogBytes = 8 << 20
)

// logUnit is where a worker unit logs (contract with the worker images, see
// docs/dev/mgmt.md "Worker log locations").
type logUnit struct {
	systemd    string // Linux journald unit
	windowsLog string // Windows log file (WinSW)
}

var logUnits = map[string]logUnit{
	"bb-worker": {"bb-worker.service", `C:\ProgramData\cucina\logs\bb-worker.log`},
	"bb-runner": {"bb-runner.service", `C:\ProgramData\cucina\logs\bb-runner.log`},
	"agent":     {"cucina-worker-agent.service", `C:\ProgramData\cucina\logs\cucina-worker-agent.log`},
}

// journalCursor is the shape of a journald cursor ("s=…;i=…;b=…;m=…;t=…;x=…"); only
// cursors of this shape are ever put into a script.
var journalCursor = regexp.MustCompile(`^[A-Za-z0-9=;_-]{1,512}$`)

// linuxTailScript tails a unit's journal: the last `lines` entries, or everything
// after cursor, bounded to ssmOutputBudget bytes, followed by "-- cursor: …".
func linuxTailScript(u logUnit, lines int, cursor string) string {
	sel := fmt.Sprintf("-n %d", lines)
	if cursor != "" {
		sel = "--after-cursor='" + cursor + "'"
	}
	return fmt.Sprintf("journalctl --no-pager --quiet -o short-iso-precise --show-cursor -u %s %s 2>&1 | tail -c %d",
		u.systemd, sel, ssmOutputBudget)
}

// windowsTailScript reads a unit's log file from a byte offset (-1: the last
// ssmOutputBudget bytes), printing "-- more" when the file has more and
// "-- offset: N" for the next poll.
func windowsTailScript(u logUnit, offset int64) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$p = '%s'
if (-not (Test-Path -LiteralPath $p)) { [Console]::Out.Write("-- no log file $p`+"`n"+`"); exit 0 }
$fs = [System.IO.File]::Open($p, 'Open', 'Read', 'ReadWrite, Delete')
try {
  $len = $fs.Length
  $off = [int64]%d
  if ($off -lt 0 -or $off -gt $len) { $off = [Math]::Max([int64]0, $len - %d) }
  $n = [int][Math]::Min([int64]%d, $len - $off)
  $buf = New-Object byte[] $n
  [void]$fs.Seek($off, 'Begin')
  $r = 0
  while ($r -lt $n) { $k = $fs.Read($buf, $r, $n - $r); if ($k -le 0) { break }; $r += $k }
  [Console]::Out.Write([System.Text.Encoding]::UTF8.GetString($buf, 0, $r))
  if ($off + $r -lt $len) { [Console]::Out.Write("`+"`n"+`-- more") }
  [Console]::Out.Write("`+"`n"+`-- offset: " + ($off + $r) + "`+"`n"+`")
} finally { $fs.Close() }
`, u.windowsLog, offset, ssmOutputBudget, ssmOutputBudget)
}

// parseTail splits a script's output into log data and the position for the next
// poll; more reports that output was cut (Linux: older entries were dropped;
// Windows: the next poll continues).
func parseTail(out []byte, windows bool) (data []byte, next string, more bool) {
	if !windows && len(out) >= ssmOutputBudget {
		// tail -c cut the output: the first line is partial.
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
		more = true
	}
	lines := strings.SplitAfter(string(out), "\n")
	var b strings.Builder
	for _, l := range lines {
		t := strings.TrimRight(l, "\r\n")
		switch {
		case !windows && strings.HasPrefix(t, "-- cursor: "):
			if c := strings.TrimPrefix(t, "-- cursor: "); journalCursor.MatchString(c) {
				next = c
			}
		case windows && strings.HasPrefix(t, "-- offset: "):
			if n, err := strconv.ParseInt(strings.TrimPrefix(t, "-- offset: "), 10, 64); err == nil && n >= 0 {
				next = strconv.FormatInt(n, 10)
			}
		case windows && t == "-- more":
			more = true
		case t == "-- No entries --":
		default:
			b.WriteString(l)
		}
	}
	return []byte(b.String()), next, more
}

// lastLines keeps the last n lines of data.
func lastLines(data []byte, n int) []byte {
	trimmed := bytes.TrimRight(data, "\n")
	count := 0
	for i := len(trimmed) - 1; i >= 0; i-- {
		if trimmed[i] == '\n' {
			count++
			if count == n {
				return data[i+1:]
			}
		}
	}
	return data
}

// StreamWorkerLogs tails a worker's unit log: EC2 workers through SSM Run Command
// (follow by polling, rate-limited across all streams, each poll ≤ 22 KB), Tart
// workers through their host's diagnostics. Output is redacted.
func (s *Server) StreamWorkerLogs(req *cucinav1.StreamWorkerLogsRequest, stream grpc.ServerStreamingServer[cucinav1.LogChunk]) error {
	ctx := stream.Context()
	unit := req.GetUnit()
	if unit == "" {
		unit = "bb-worker"
	}
	u, ok := logUnits[unit]
	if !ok {
		return invalid("unit must be bb-worker, bb-runner or agent")
	}
	lines := int(min(req.GetTailLines(), maxTailLines))
	if lines <= 0 {
		lines = defaultTailLines
	}
	w, pool, err := s.findWorker(ctx, req.GetNode())
	if err != nil {
		return err
	}
	release, err := s.logStreams.acquire("log and diagnostics streams")
	if err != nil {
		return err
	}
	defer release()
	provider := pool.Spec.Provider
	if provider == "" {
		provider = domain.Provider(pool.Resource.Spec.Provider)
	}
	switch provider {
	case domain.ProviderEC2:
		return s.streamInstanceLogs(ctx, stream, w, poolOS(pool.Spec) == "windows", u, lines, req.GetFollow())
	case domain.ProviderTart:
		return s.streamVMLogs(ctx, stream, w, unit, lines, req.GetFollow())
	}
	return status.Errorf(codes.FailedPrecondition, "pool %q has no log source for provider %q", pool.Name(), provider)
}

func (s *Server) streamInstanceLogs(ctx context.Context, stream grpc.ServerStreamingServer[cucinav1.LogChunk],
	w Worker, windows bool, u logUnit, lines int, follow bool,
) error {
	if s.deps.Shell == nil {
		return notConfigured("SSM access to EC2 workers")
	}
	deadline := time.Now().Add(s.opts.LogFollowLimit)
	next := ""
	for first := true; ; first = false {
		if err := s.ssm.wait(ctx); err != nil {
			return streamEnd(ctx)
		}
		var script string
		if windows {
			offset := int64(-1)
			if next != "" {
				offset, _ = strconv.ParseInt(next, 10, 64)
			}
			script = windowsTailScript(u, offset)
		} else {
			script = linuxTailScript(u, lines, next)
		}
		out, err := s.deps.Shell.RunScript(ctx, w.ID, windows, script)
		if err != nil {
			return fail("SSM", err)
		}
		data, pos, more := parseTail(out, windows)
		if pos != "" {
			next = pos
		}
		if first {
			data = lastLines(data, lines)
		} else if more && !windows {
			data = append([]byte("[… output truncated: more was logged than one poll returns …]\n"), data...)
		}
		done := !follow || !time.Now().Before(deadline)
		if follow && done {
			data = append(data, "[follow limit reached; run the command again to continue]\n"...)
		}
		if len(data) > 0 || done {
			if err := stream.Send(&cucinav1.LogChunk{Data: RedactBytes(data), Last: done}); err != nil {
				return err
			}
		}
		if done {
			return nil
		}
		if windows && more {
			continue // the file has more: fetch it right away (still rate-limited)
		}
		t := time.NewTimer(s.opts.LogPollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return streamEnd(ctx)
		case <-s.done:
			t.Stop()
			return errStopping
		case <-t.C:
		}
	}
}

// streamVMLogs returns a snapshot of a Tart worker's log through its host.
func (s *Server) streamVMLogs(ctx context.Context, stream grpc.ServerStreamingServer[cucinav1.LogChunk],
	w Worker, unit string, lines int, follow bool,
) error {
	if s.deps.HostAdmin == nil {
		return notConfigured("Mac host administration")
	}
	hostRef, vm, ok := strings.Cut(w.ID, "/")
	if !ok {
		return status.Errorf(codes.FailedPrecondition, "Tart worker %q has no <host>/<vm> node name", w.ID)
	}
	if w.Host != "" {
		hostRef = w.Host
	}
	h, err := s.resolveHost(ctx, hostRef)
	if err != nil {
		return err
	}
	rc, err := s.deps.HostAdmin.Diagnostics(ctx, h.Spec.Serial, DiagnosticsRequest{
		IncludeVMLogs: true, VM: vm, Unit: unit, TailLines: lines,
	})
	if err != nil {
		return fail("collecting VM log", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxVMLogBytes))
	if err != nil {
		return fail("reading VM log", err)
	}
	data = RedactBytes(data)
	if follow {
		data = append(data, "\n[follow is not available for Tart workers yet; this is a snapshot]\n"...)
	}
	return s.sendChunks(ctx, stream, bytes.NewReader(data))
}
