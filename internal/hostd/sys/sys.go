// SPDX-License-Identifier: FSL-1.1-ALv2

// Package sys holds hostd's production adapters for the generic ports: Exec
// (os/exec with the R-MAC-2 privilege drop), FS and Clock.
package sys

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sloper-ai/cucina/internal/hostd/privdrop"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Exec runs processes on Darwin/Linux, applying privdrop for commands with
// RunAs. Other platforms reject Run/Start with errors.ErrUnsupported before
// starting a child; portable FS/Clock consumers do not require this adapter.
type Exec struct {
	Mode   privdrop.Mode
	Self   string   // absolute path of the hostd binary (trampoline)
	Groups []uint32 // supplementary groups of the RunAs user
	// MaxOutput bounds the captured stdout/stderr of Run per stream (default 4 MiB).
	MaxOutput int
}

var _ ports.Exec = (*Exec)(nil)

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
