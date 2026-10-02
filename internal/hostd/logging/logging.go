// SPDX-License-Identifier: FSL-1.1-ALv2

// Package logging sets up hostd's structured logs (R-MAC-5, R-OBS-4): JSON
// lines (log/slog) to a size-rotated file under /Library/Logs/Cucina (user
// mode: ~/Library/Logs/Cucina) and, on darwin, to unified logging with
// subsystem ai.sloper.cucina (oslog_darwin.go).
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Subsystem is the unified-logging subsystem.
const Subsystem = "ai.sloper.cucina"

// RotatingFile is an io.Writer that rotates path when it exceeds MaxBytes,
// keeping Keep old files (path.1 … path.Keep).
type RotatingFile struct {
	Path     string
	MaxBytes int64
	Keep     int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Write implements io.Writer.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.MaxBytes > 0 && r.size+int64(len(p)) > r.MaxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) open() error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *RotatingFile) rotate() error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	keep := max(r.Keep, 1)
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.Path, i), fmt.Sprintf("%s.%d", r.Path, i+1))
	}
	if err := os.Rename(r.Path, r.Path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return r.open()
}

// Close closes the current file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// ParseLevel maps debug|info|warn|error.
func ParseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// fanout sends records to several handlers.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(a []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(a)
	}
	return out
}

func (f fanout) WithGroup(n string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(n)
	}
	return out
}

// Setup returns the hostd logger: JSON to the rotated file (5 × 20 MiB) and to
// unified logging; extra (e.g. stderr in user mode) also receives JSON lines.
func Setup(dir string, level *slog.LevelVar, extra io.Writer) (*slog.Logger, io.Closer, error) {
	rf := &RotatingFile{Path: filepath.Join(dir, "hostd.log"), MaxBytes: 20 << 20, Keep: 5}
	opts := &slog.HandlerOptions{Level: level}
	hs := fanout{slog.NewJSONHandler(rf, opts)}
	if extra != nil {
		hs = append(hs, slog.NewJSONHandler(extra, opts))
	}
	if h := newOSLogHandler("hostd", opts); h != nil {
		hs = append(hs, h)
	}
	return slog.New(hs).With("component", "hostd"), rf, nil
}
