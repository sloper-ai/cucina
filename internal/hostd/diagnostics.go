// SPDX-License-Identifier: FSL-1.1-ALv2

package hostd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/hostd/lifecycle"
	"github.com/sloper-ai/cucina/internal/hostd/redact"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Diagnostics limits.
const (
	FollowLimit      = 10 * time.Minute // a followed log stream ends after this
	followPoll       = 2 * time.Second
	defaultTailLines = 200
	maxTailLines     = 10000
	logChunk         = 64 << 10
)

// vmLogFiles maps CollectDiagnostics.unit to the in-VM log file (in-VM contract §1.1).
var vmLogFiles = map[string]string{
	"bb-worker": "/var/log/cucina/bb_worker.log",
	"bb-runner": "/var/log/cucina/bb_runner.log",
}

// sendLog streams data as redacted LogData chunks (certificates, keys and
// tokens never leave the host).
func (a *Agent) sendLog(id string, send func(*cucinav1.HostMessage), data []byte, last bool) {
	data = redact.Bytes(data, a.o.Config.SiteEnrollmentToken)
	for {
		n := min(len(data), logChunk)
		chunk := data[:n]
		data = data[n:]
		send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_Log{Log: &cucinav1.LogData{
			CommandId: id, Data: chunk, Last: last && len(data) == 0}}})
		if len(data) == 0 {
			return
		}
	}
}

// collectDiagnostics answers CollectDiagnostics: the full bundle, or one VM's
// unit log (tail_lines, optionally followed for up to FollowLimit).
func (a *Agent) collectDiagnostics(ctx context.Context, id string, cd *cucinav1.CollectDiagnostics, send func(*cucinav1.HostMessage)) error {
	if cd.GetVmName() == "" && cd.GetUnit() == "" {
		a.sendLog(id, send, a.diagnostics(ctx, cd.GetIncludeVmLogs()), true)
		return nil
	}
	lines := int(cd.GetTailLines())
	if lines <= 0 {
		lines = defaultTailLines
	}
	lines = min(lines, maxTailLines)
	var read func(ctx context.Context, off int64) ([]byte, int64, error) // bytes from off, new offset
	var tail func(ctx context.Context) ([]byte, int64, error)
	switch unit := cd.GetUnit(); {
	case unit == "agent":
		path := filepath.Join(a.o.LogDir, "hostd.log")
		tail = func(context.Context) ([]byte, int64, error) { return tailFile(path, lines) }
		read = func(_ context.Context, off int64) ([]byte, int64, error) { return readFrom(path, off) }
	case vmLogFiles[unit] != "":
		if cd.GetVmName() == "" {
			return fmt.Errorf("%w: unit %s needs vm_name", lifecycle.ErrUnknownVM, unit)
		}
		known := false
		for _, vm := range a.vmm.Inventory() {
			known = known || vm.GetName() == cd.GetVmName()
		}
		if !known {
			return fmt.Errorf("%w: %s", lifecycle.ErrUnknownVM, cd.GetVmName())
		}
		tn, file := a.o.Config.VMNamePrefix+cd.GetVmName(), vmLogFiles[unit]
		tail = func(ctx context.Context) ([]byte, int64, error) { return a.guestTail(ctx, tn, file, lines) }
		read = func(ctx context.Context, off int64) ([]byte, int64, error) {
			return a.guestReadFrom(ctx, tn, file, off)
		}
	default:
		return fmt.Errorf("unknown unit %q (bb-worker | bb-runner | agent)", cd.GetUnit())
	}
	data, off, err := tail(ctx)
	if err != nil {
		return err
	}
	a.sendLog(id, send, data, !cd.GetFollow())
	if !cd.GetFollow() {
		return nil
	}
	deadline := a.o.Clock.Now().Add(FollowLimit)
	for a.o.Clock.Now().Before(deadline) {
		if err := a.o.Clock.Sleep(ctx, followPoll); err != nil {
			break
		}
		more, next, err := read(ctx, off)
		if err != nil {
			continue // the VM may be restarting; keep following until the limit
		}
		off = next
		if len(more) > 0 {
			a.sendLog(id, send, more, false)
		}
	}
	a.sendLog(id, send, nil, true)
	return nil
}

// guestTail returns the last lines of a guest file and its size.
func (a *Agent) guestTail(ctx context.Context, vm, file string, lines int) ([]byte, int64, error) {
	res, err := a.o.Runtime.GuestExec(ctx, vm, ports.Command{Path: "/bin/sh", Args: []string{"-c",
		`stat -f %z "$1" && tail -n "$2" "$1"`, "sh", file, strconv.Itoa(lines)}})
	if err != nil {
		return nil, 0, err
	}
	if res.ExitCode != 0 {
		return nil, 0, fmt.Errorf("reading %s: exit %d: %s", file, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	size, rest, _ := bytes.Cut(res.Stdout, []byte("\n"))
	n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("unexpected stat output %q", size)
	}
	return rest, n, nil
}

// guestReadFrom returns up to 256 KiB of a guest file from off and the new offset.
func (a *Agent) guestReadFrom(ctx context.Context, vm, file string, off int64) ([]byte, int64, error) {
	res, err := a.o.Runtime.GuestExec(ctx, vm, ports.Command{Path: "/bin/sh", Args: []string{"-c",
		`tail -c +"$2" "$1" | head -c 262144`, "sh", file, strconv.FormatInt(off+1, 10)}})
	if err != nil {
		return nil, off, err
	}
	if res.ExitCode != 0 {
		return nil, off, fmt.Errorf("reading %s: exit %d", file, res.ExitCode)
	}
	return res.Stdout, off + int64(len(res.Stdout)), nil
}

// tailFile returns the last lines of a host file and its size.
func tailFile(path string, lines int) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	const window = 4 << 20
	start := max(st.Size()-window, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	b, err := io.ReadAll(io.LimitReader(f, window))
	if err != nil {
		return nil, 0, err
	}
	parts := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return append(bytes.Join(parts, []byte("\n")), '\n'), st.Size(), nil
}

// readFrom returns up to 256 KiB of a host file from off and the new offset.
func readFrom(path string, off int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, off, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, off, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 256<<10))
	return b, off + int64(len(b)), err
}
