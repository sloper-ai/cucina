// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Build information, set at link time (Bazel x_defs / -ldflags -X).
var (
	Version = "dev"
	Commit  = ""
)

// SystemClock is the production ports.Clock.
type SystemClock struct{}

// Now implements ports.Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// After implements ports.Clock.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Sleep implements ports.Clock.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SystemRand is the production ports.Rand: a ChaCha8 stream seeded from
// crypto/rand, safe for concurrent use.
type SystemRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

// NewSystemRand returns a randomly seeded source.
func NewSystemRand() *SystemRand {
	var seed [32]byte
	_, _ = cryptorand.Read(seed[:])
	return &SystemRand{r: rand.New(rand.NewChaCha8(seed))}
}

// Float64 implements ports.Rand.
func (s *SystemRand) Float64() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.Float64()
}

// Int63n implements ports.Rand.
func (s *SystemRand) Int63n(n int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.Int64N(n)
}

// Token implements ports.Rand.
func (s *SystemRand) Token(n int) string {
	b := make([]byte, n)
	s.mu.Lock()
	for i := 0; i < n; i += 8 {
		var w [8]byte
		binary.LittleEndian.PutUint64(w[:], s.r.Uint64())
		copy(b[i:], w[:])
	}
	s.mu.Unlock()
	return hex.EncodeToString(b)
}

var (
	_ ports.Clock = SystemClock{}
	_ ports.Rand  = (*SystemRand)(nil)
)

// ParseLevel maps the configured log level to slog.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// TeeWriter forwards log output to an optional second writer installed later
// (the management API's in-memory log ring for support bundles).
type TeeWriter struct{ w atomic.Pointer[io.Writer] }

// Set installs the second writer.
func (t *TeeWriter) Set(w io.Writer) { t.w.Store(&w) }

// Write implements io.Writer; it never fails.
func (t *TeeWriter) Write(p []byte) (int, error) {
	if w := t.w.Load(); w != nil {
		_, _ = (*w).Write(p)
	}
	return len(p), nil
}

// LogTee receives a copy of everything the process logs.
var LogTee = &TeeWriter{}

// SetupLogging installs JSON slog logging (R-OBS-4) as the process default and
// bridges controller-runtime (logr) and client-go (klog) into it.
func SetupLogging(level string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	w = io.MultiWriter(w, LogTee)
	lv := new(slog.LevelVar)
	lv.Set(ParseLevel(level))
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv})
	l := slog.New(h) // subsystems add "component"
	slog.SetDefault(l)
	ctrl.SetLogger(logr.FromSlogHandler(l.Handler()))
	klog.SetSlogLogger(l)
	return l
}
