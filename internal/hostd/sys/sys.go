// SPDX-License-Identifier: FSL-1.1-ALv2

// Package sys holds hostd's production adapters for the generic ports: Exec
// (os/exec with the R-MAC-2 privilege drop), FS and Clock.
package sys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/sloper-ai/cucina/internal/hostd/privdrop"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Exec runs processes, applying privdrop for commands with RunAs.
type Exec struct {
	Mode   privdrop.Mode
	Self   string   // absolute path of the hostd binary (trampoline)
	Groups []uint32 // supplementary groups of the RunAs user
	// MaxOutput bounds the captured stdout/stderr of Run per stream (default 4 MiB).
	MaxOutput int
}

var _ ports.Exec = (*Exec)(nil)

func (e *Exec) build(c ports.Command, detach bool) (*exec.Cmd, error) {
	spec, err := privdrop.Wrap(c, e.Mode, e.Self, e.Groups)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	}
	cmd.Dir = c.Dir
	attr := &syscall.SysProcAttr{Setsid: detach}
	if spec.Credential != nil {
		attr.Credential = &syscall.Credential{Uid: spec.Credential.UID, Gid: spec.Credential.GID, Groups: spec.Groups}
	}
	cmd.SysProcAttr = attr
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	return cmd, nil
}

// capped keeps the first max bytes written to it.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (e *Exec) max() int {
	if e.MaxOutput > 0 {
		return e.MaxOutput
	}
	return 4 << 20
}

// Run runs a command to completion. A non-zero exit is reported in ExitCode
// with a nil error; errors mean the process could not run.
func (e *Exec) Run(ctx context.Context, c ports.Command) (ports.ExecResult, error) {
	cmd, err := e.build(c, false)
	if err != nil {
		return ports.ExecResult{}, err
	}
	stdout, stderr := &capped{max: e.max()}, &capped{max: e.max()}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return ports.ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-done:
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case werr = <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			werr = <-done
		}
		return ports.ExecResult{ExitCode: -1, Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes()}, ctx.Err()
	}
	res := ports.ExecResult{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes()}
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, werr
}

// Start starts a long-running child in its own session so it survives a hostd
// crash (crash-only restarts must not kill VMs). Output keeps the last 64 KiB.
func (e *Exec) Start(_ context.Context, c ports.Command) (ports.Process, error) {
	cmd, err := e.build(c, true)
	if err != nil {
		return nil, err
	}
	p := &process{cmd: cmd, done: make(chan struct{})}
	p.stdout, p.stderr = &tail{max: 64 << 10}, &tail{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		err := cmd.Wait()
		p.res = ports.ExecResult{Stdout: p.stdout.bytes(), Stderr: p.stderr.bytes()}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			p.res.ExitCode = ee.ExitCode()
		} else {
			p.err = err
		}
		close(p.done)
	}()
	return p, nil
}

type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

type process struct {
	cmd            *exec.Cmd
	done           chan struct{}
	res            ports.ExecResult
	err            error
	stdout, stderr *tail
}

func (p *process) PID() int { return p.cmd.Process.Pid }

func (p *process) Wait(ctx context.Context) (ports.ExecResult, error) {
	select {
	case <-p.done:
		return p.res, p.err
	case <-ctx.Done():
		return ports.ExecResult{}, ctx.Err()
	}
}

func (p *process) Signal(sig string) error {
	s, ok := map[string]syscall.Signal{"TERM": syscall.SIGTERM, "INT": syscall.SIGINT, "KILL": syscall.SIGKILL, "HUP": syscall.SIGHUP}[sig]
	if !ok {
		return fmt.Errorf("unknown signal %q", sig)
	}
	return p.cmd.Process.Signal(s)
}

// FS is the local filesystem.
type FS struct{}

var _ ports.FS = FS{}

// ReadFile implements ports.FS.
func (FS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// WriteFileAtomic writes via a temp file in the same directory, fsync, rename.
func (FS) WriteFileAtomic(path string, data []byte, mode uint32) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(os.FileMode(mode)); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MkdirAll implements ports.FS.
func (FS) MkdirAll(path string, mode uint32) error { return os.MkdirAll(path, os.FileMode(mode)) }

// Remove implements ports.FS.
func (FS) Remove(path string) error { return os.Remove(path) }

// RemoveAll implements ports.FS.
func (FS) RemoveAll(path string) error { return os.RemoveAll(path) }

// Rename implements ports.FS.
func (FS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// Exists implements ports.FS.
func (FS) Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// DiskUsage implements ports.FS.
func (FS) DiskUsage(path string) (used, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total := uint64(st.Blocks) * uint64(st.Bsize)
	free = uint64(st.Bavail) * uint64(st.Bsize)
	return total - uint64(st.Bfree)*uint64(st.Bsize), free, nil
}

// ListDir implements ports.FS.
func (FS) ListDir(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// Clock is the system clock.
type Clock struct{}

var _ ports.Clock = Clock{}

// Now implements ports.Clock.
func (Clock) Now() time.Time { return time.Now() }

// After implements ports.Clock.
func (Clock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Sleep implements ports.Clock.
func (Clock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
