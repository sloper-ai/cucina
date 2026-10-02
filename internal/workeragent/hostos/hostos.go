// SPDX-License-Identifier: FSL-1.1-ALv2

// Package hostos holds the production adapters of the ports the worker agent
// uses (ports.Exec, ports.FS, ports.Clock, ports.Rand) over the real operating
// system. They are deliberately thin; behaviour is specified by the ports and
// exercised by the agent's tests through fakes.
package hostos

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Exec runs commands with os/exec. A non-zero exit status is reported through
// ExecResult.ExitCode with a nil error; errors mean the command could not run.
type Exec struct{}

var _ ports.Exec = Exec{}

func command(ctx context.Context, c ports.Command) (*exec.Cmd, error) {
	if c.RunAs != nil {
		return nil, errors.New("hostos: RunAs is not supported by the worker agent")
	}
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Dir = c.Dir
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	return cmd, nil
}

// Run implements ports.Exec.
func (Exec) Run(ctx context.Context, c ports.Command) (ports.ExecResult, error) {
	cmd, err := command(ctx, c)
	if err != nil {
		return ports.ExecResult{}, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	res := ports.ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ctx.Err() == nil:
		res.ExitCode = ee.ExitCode()
		err = nil
	default:
		res.ExitCode = -1
	}
	return res, err
}

// Start implements ports.Exec.
func (Exec) Start(ctx context.Context, c ports.Command) (ports.Process, error) {
	cmd, err := command(ctx, c)
	if err != nil {
		return nil, err
	}
	p := &process{cmd: cmd}
	cmd.Stdout, cmd.Stderr = &p.stdout, &p.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.done = make(chan struct{})
	go func() { p.err = cmd.Wait(); close(p.done) }()
	return p, nil
}

type process struct {
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	done           chan struct{}
	err            error
}

func (p *process) PID() int { return p.cmd.Process.Pid }

func (p *process) Wait(ctx context.Context) (ports.ExecResult, error) {
	select {
	case <-ctx.Done():
		return ports.ExecResult{}, ctx.Err()
	case <-p.done:
	}
	res := ports.ExecResult{ExitCode: p.cmd.ProcessState.ExitCode(), Stdout: p.stdout.Bytes(), Stderr: p.stderr.Bytes()}
	var ee *exec.ExitError
	if p.err != nil && !errors.As(p.err, &ee) {
		return res, p.err
	}
	return res, nil
}

func (p *process) Signal(sig string) error { return signal(p.cmd.Process, sig) }

// FS is the real filesystem.
type FS struct{}

var _ ports.FS = FS{}

// ReadFile implements ports.FS.
func (FS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// WriteFileAtomic writes data to a temporary file in the target directory,
// syncs it and renames it over path, so readers never see partial content.
func (FS) WriteFileAtomic(path string, data []byte, mode uint32) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(os.FileMode(mode)); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
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
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// DiskUsage implements ports.FS.
func (FS) DiskUsage(path string) (used, free uint64, err error) { return diskUsage(path) }

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
	sort.Strings(names)
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
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Rand is a randomly seeded math/rand/v2 source with crypto/rand tokens. It is
// safe for concurrent use.
type Rand struct {
	mu sync.Mutex
	r  *rand.Rand
}

var _ ports.Rand = (*Rand)(nil)

// NewRand returns a Rand seeded from crypto/rand.
func NewRand() *Rand {
	var seed [32]byte
	_, _ = crand.Read(seed[:])
	return &Rand{r: rand.New(rand.NewChaCha8(seed))}
}

// Float64 implements ports.Rand.
func (r *Rand) Float64() float64 { r.mu.Lock(); defer r.mu.Unlock(); return r.r.Float64() }

// Int63n implements ports.Rand.
func (r *Rand) Int63n(n int64) int64 { r.mu.Lock(); defer r.mu.Unlock(); return r.r.Int64N(n) }

// Token implements ports.Rand.
func (r *Rand) Token(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}
