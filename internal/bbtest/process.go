// SPDX-License-Identifier: FSL-1.1-ALv2

package bbtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ReadyTimeout bounds every readiness wait.
const ReadyTimeout = 20 * time.Second

// stopGrace is how long a process may take to exit after SIGTERM by default.
const stopGrace = 5 * time.Second

// Process is a running Buildbarn binary.
type Process struct {
	Name       string
	ConfigPath string
	stopGrace  time.Duration
	cmd        *exec.Cmd
	logs       *syncBuffer
	done       chan struct{}
	err        error
}

// Done is closed when the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err is the process's exit error; only valid after Done is closed.
func (p *Process) Err() error { return p.err }

// Logs returns everything the process wrote to stdout and stderr so far.
func (p *Process) Logs() string { return p.logs.String() }

// Readiness reports when a booted process can serve. It is called once and
// must block until ready or until ctx ends.
type Readiness func(ctx context.Context) error

// Option configures Start.
type Option func(*startOptions)

type startOptions struct {
	env       []string
	dir       string
	stopGrace time.Duration
}

// WithEnv adds KEY=VALUE environment variables.
func WithEnv(kv ...string) Option { return func(o *startOptions) { o.env = append(o.env, kv...) } }

// WithDir sets the working directory (default: a fresh temporary directory).
func WithDir(dir string) Option { return func(o *startOptions) { o.dir = dir } }

// WithStopGrace sets how long Stop waits after SIGTERM before SIGKILL (default
// 5 s). Give processes that own mounts time to unmount.
func WithStopGrace(d time.Duration) Option { return func(o *startOptions) { o.stopGrace = d } }

// Start runs binary with config (written to <dir>/<name>.json) without waiting
// for readiness. The process is stopped at test cleanup; if the test failed,
// the tail of its output is logged.
func Start(t testing.TB, name, binary string, config []byte, opts ...Option) *Process {
	t.Helper()
	o := startOptions{stopGrace: stopGrace}
	for _, opt := range opts {
		opt(&o)
	}
	if o.dir == "" {
		o.dir = t.TempDir()
	}
	configPath := filepath.Join(o.dir, name+".json")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatalf("write %s config: %v", name, err)
	}
	p := &Process{Name: name, ConfigPath: configPath, stopGrace: o.stopGrace, logs: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command(binary, configPath)
	p.cmd.Dir = o.dir
	p.cmd.Env = append(os.Environ(), o.env...)
	p.cmd.Stdout, p.cmd.Stderr = p.logs, p.logs
	setProcessGroup(p.cmd)
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		if err := p.StopContext(context.Background()); err != nil {
			t.Errorf("stop %s: %v", name, err)
		}
		if t.Failed() {
			t.Logf("--- %s output (tail) ---\n%s", name, Tail(p.Logs(), 60))
		}
	})
	return p
}

// Stop terminates the process (SIGTERM, then SIGKILL after a grace period).
// A process that cannot be reaped is reported by Start's test cleanup. Use
// StopContext when the caller needs the shutdown error before its own cleanup.
func (p *Process) Stop() { _ = p.StopContext(context.Background()) }

// StopContext terminates only this child and waits for it. Shutdown is bounded
// by the shorter of ctx and the configured grace plus five seconds to reap a
// killed child. In particular, cleanup must not wait forever on a child stuck
// on a dead filesystem. Repeated calls after exit do nothing.
func (p *Process) StopContext(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.stopGrace+stopGrace)
	defer cancel()
	select {
	case <-p.done:
		return nil
	default:
	}
	terminate(p.cmd)
	timer := time.NewTimer(p.stopGrace)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
	case <-ctx.Done():
	}
	kill(p.cmd)
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s did not exit after SIGKILL: %w", p.Name, ctx.Err())
	}
}

// KillContext abruptly terminates only this owned child and waits for it to be
// reaped, without requesting a graceful flush. Persistence tests use this after
// an observed checkpoint so Unix and Windows exercise the same crash boundary.
// The reap allowance is the same five seconds used by StopContext after a kill.
func (p *Process) KillContext(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, stopGrace)
	defer cancel()
	// Called only after done closes, which publishes cmd.Wait's result. An
	// exit status confirms reaping; pipe/Wait failures must remain visible.
	joined := func(killErr error) error {
		switch p.err.(type) { //nolint:errorlint // Cmd.Wait returns ExitError directly; a wrapped/joined error may also contain a wait failure.
		case nil, *exec.ExitError:
			return nil
		default:
			return fmt.Errorf("%s did not complete its owned wait: %w", p.Name, errors.Join(killErr, p.err))
		}
	}
	select {
	case <-p.done:
		return joined(nil)
	default:
	}
	// The OS child may already have exited while Cmd.Wait is still finishing.
	// Windows can report EINVAL/access denied in that gap; only our completed
	// wait, never a particular errno, establishes that cleanup succeeded.
	killErr := p.cmd.Process.Kill()
	select {
	case <-p.done:
		return joined(killErr)
	case <-ctx.Done():
		// Completion and cancellation may become ready together.
		select {
		case <-p.done:
			return joined(killErr)
		default:
		}
		return fmt.Errorf("%s was not reaped after an abrupt kill: %w", p.Name, errors.Join(killErr, ctx.Err()))
	}
}

// WaitReady blocks until ready succeeds, the process exits (an error carrying
// its output) or ReadyTimeout passes.
func (p *Process) WaitReady(ctx context.Context, ready Readiness) error {
	ctx, cancel := context.WithTimeout(ctx, ReadyTimeout)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- ready(ctx) }()
	select {
	case err := <-result:
		if err != nil {
			return fmt.Errorf("%s not ready: %w\n%s", p.Name, err, Tail(p.Logs(), 40))
		}
		return nil
	case <-p.done:
		cancel()
		return fmt.Errorf("%s exited during start-up (%w):\n%s", p.Name, p.err, Tail(p.Logs(), 40))
	}
}

// Boot starts the binary named by env (EnvStorage, …) and waits for ready
// (nil: no wait). It fails the test if the process cannot become ready.
func Boot(t testing.TB, env, name string, config []byte, ready Readiness, opts ...Option) *Process {
	t.Helper()
	p := Start(t, name, Binary(t, env), config, opts...)
	if ready != nil {
		if err := p.WaitReady(context.Background(), ready); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// BootStorage boots bb_storage (bb-storage 086b011, NEW `local` schema).
func BootStorage(t testing.TB, config []byte, ready Readiness, opts ...Option) *Process {
	t.Helper()
	return Boot(t, EnvStorage, "bb_storage", config, ready, opts...)
}

// BootScheduler boots bb_scheduler (bb-remote-execution 1a3be95).
func BootScheduler(t testing.TB, config []byte, ready Readiness, opts ...Option) *Process {
	t.Helper()
	return Boot(t, EnvScheduler, "bb_scheduler", config, ready, opts...)
}

// BootWorker boots bb_worker (bb-remote-execution 1a3be95, OLD `local` schema).
func BootWorker(t testing.TB, config []byte, ready Readiness, opts ...Option) *Process {
	t.Helper()
	return Boot(t, EnvWorker, "bb_worker", config, ready, opts...)
}

// BootRunner boots bb_runner (bb-remote-execution 1a3be95).
func BootRunner(t testing.TB, config []byte, ready Readiness, opts ...Option) *Process {
	t.Helper()
	return Boot(t, EnvRunner, "bb_runner", config, ready, opts...)
}

// GRPCReady is ready once a gRPC connection to target (host:port or
// unix:///path) reaches READY: the server listens and, with tlsConfig, the
// TLS handshake succeeds. Reconnects follow a fast backoff; no polling.
func GRPCReady(target string, tlsConfig *tls.Config) Readiness {
	return func(ctx context.Context) error {
		conn, err := Dial(target, tlsConfig)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		conn.Connect()
		for s := conn.GetState(); s != connectivity.Ready; s = conn.GetState() {
			if !conn.WaitForStateChange(ctx, s) {
				return fmt.Errorf("gRPC %s: %w (last state %s)", target, ctx.Err(), s)
			}
		}
		return nil
	}
}

// AllReady is ready once every listener is: a binary does not open its
// listeners at once, so wait for each one a test talks to.
func AllReady(readiness ...Readiness) Readiness {
	return func(ctx context.Context) error {
		for _, r := range readiness {
			if err := r(ctx); err != nil {
				return err
			}
		}
		return nil
	}
}

// Dial opens a lazily connecting gRPC client with a fast reconnect backoff,
// suited to children that are still starting.
func Dial(target string, tlsConfig *tls.Config, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	creds := insecure.NewCredentials()
	if tlsConfig != nil {
		creds = credentials.NewTLS(tlsConfig)
	}
	return grpc.NewClient(target, append([]grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 10 * time.Millisecond, Multiplier: 1.5, Jitter: 0.1, MaxDelay: 200 * time.Millisecond},
			MinConnectTimeout: 2 * time.Second,
		}),
	}, opts...)...)
}

// FreeAddr returns a loopback address with a currently free TCP port for a
// child to listen on. Ports come from [20000, 32768), below the ephemeral
// ranges of Linux, macOS and Windows, so outgoing connections (whose local
// ports the kernel takes from those ranges) cannot grab them before the
// child binds.
func FreeAddr(t testing.TB) string {
	t.Helper()
	for range 100 {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(20000+mathrand.IntN(32768-20000)))
		l, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = l.Close()
		return addr
	}
	t.Fatal("no free loopback port in [20000, 32768)")
	return ""
}

// ShortTempDir returns a fresh directory with a short absolute path, for UNIX
// sockets (sun_path is limited to 104 bytes on macOS); removed at cleanup.
func ShortTempDir(t testing.TB) string {
	t.Helper()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	base := os.TempDir()
	if len(base) > 60 {
		base = "/tmp"
	}
	dir := filepath.Join(base, "bbt"+hex.EncodeToString(suffix[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove short temporary directory: %v", err)
		}
	})
	return dir
}

// Tail returns the last n lines of s.
func Tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
