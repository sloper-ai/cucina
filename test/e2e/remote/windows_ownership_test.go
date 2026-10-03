// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build windows

package remote_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/remote"
)

func TestMain(m *testing.M) {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(99)
	}
	// exec.Command("aws", ...) preserves argv[0] as "aws" on Windows even
	// though lookup selects aws.exe. Dispatch by the actual fixture image.
	if strings.EqualFold(filepath.Base(executable), "aws.exe") {
		if err := tunnelFixture(); err != nil {
			if path := os.Getenv("CUCINA_OWNER_DIAGNOSTIC"); path != "" {
				text := err.Error()
				if len(text) > 2000 {
					text = text[:2000]
				}
				_ = os.WriteFile(path, []byte(text), 0o600)
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(99)
		}
		os.Exit(0)
	}
	if os.Getenv("CUCINA_OWNER_ROLE") != "" {
		if err := jobFixture(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(99)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		fmt.Fprintln(os.Stderr, "unrecognized fixture entry role; refusing to run the test suite")
		os.Exit(99)
	}
	os.Exit(m.Run())
}

func jobFixture() error {
	role := os.Getenv("CUCINA_OWNER_ROLE")
	if role == "payload" {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		child := exec.Command(binary)
		child.Env = append(os.Environ(), "CUCINA_OWNER_ROLE=child")
		if err := child.Start(); err != nil {
			return err
		}
		if os.Getenv("CUCINA_OWNER_MODE") == "completed" {
			return child.Process.Release()
		}
		return child.Wait()
	}
	if role != "child" && role != "unrelated" {
		return errors.New("unrecognized fixture child role")
	}
	conn, err := net.DialTimeout("tcp", os.Getenv("CUCINA_OWNER_CONTROL"), 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := json.NewEncoder(conn).Encode(map[string]any{"role": role, "pid": os.Getpid()}); err != nil {
		return err
	}
	var b [1]byte
	for {
		if _, err = io.ReadFull(conn, b[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if _, err = conn.Write(b[:]); err != nil {
			return err
		}
	}
}

func tunnelFixturePeer(role string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", os.Getenv("CUCINA_OWNER_CONTROL"), 10*time.Second)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(map[string]any{"role": role, "pid": os.Getpid()}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if ack[0] != 1 {
		_ = conn.Close()
		return nil, errors.New("invalid owned process handle acknowledgement")
	}
	return conn, nil
}

func tunnelFixture() error {
	if len(os.Args) < 2 || (os.Args[1] != "fixture-plugin" && (len(os.Args) < 3 || os.Args[1] != "ssm" || os.Args[2] != "start-session")) {
		return errors.New("unrecognized synthetic tunnel entry role")
	}
	if os.Args[1] == "fixture-plugin" && len(os.Args) != 3 {
		return errors.New("invalid synthetic plugin arguments")
	}
	role := "launcher"
	if len(os.Args) > 1 && os.Args[1] == "fixture-plugin" {
		role = "plugin"
	}
	control, err := tunnelFixturePeer(role)
	if err != nil {
		return err
	}
	defer func() { _ = control.Close() }()
	if len(os.Args) > 1 && os.Args[1] == "fixture-plugin" {
		var params map[string][]string
		if err := json.Unmarshal([]byte(os.Args[2]), &params); err != nil {
			return err
		}
		target, err := url.Parse("http://127.0.0.1:" + params["portNumber"][0])
		if err != nil {
			return err
		}
		listener, err := net.Listen("tcp", "127.0.0.1:"+params["localPortNumber"][0])
		if err != nil {
			return err
		}
		defer func() { _ = listener.Close() }()
		fmt.Println("Waiting for connections")
		server := &http.Server{Handler: httputil.NewSingleHostReverseProxy(target), ReadHeaderTimeout: 5 * time.Second}
		return server.Serve(listener)
	}
	for i, arg := range os.Args {
		if arg == "--parameters" && i+1 < len(os.Args) {
			cmd := exec.Command(os.Args[0], "fixture-plugin", os.Args[i+1])
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}
	}
	return errors.New("missing synthetic tunnel parameters")
}

// powerShellEvidence retains live stderr without an unbounded buffer. Keep the
// first fixed startup marker as well as the tail if a later error is verbose.
type powerShellEvidence struct {
	mu             sync.Mutex
	sequence       int
	phase, failure string
	began, ended   time.Time
	startElapsed   time.Duration
	stderr, prefix []byte
	truncated      bool
}

func (p *powerShellEvidence) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	const limit = 2000
	n := len(b)
	if len(p.prefix) < 256 {
		p.prefix = append(p.prefix, b[:min(n, 256-len(p.prefix))]...)
	}
	if len(p.stderr)+n > limit {
		p.truncated = true
	}
	if n >= limit {
		p.stderr = append(p.stderr[:0], b[n-limit:]...)
	} else {
		if drop := len(p.stderr) + n - limit; drop > 0 {
			p.stderr = p.stderr[:copy(p.stderr, p.stderr[drop:])]
		}
		p.stderr = append(p.stderr, b...)
	}
	return n, nil
}

func (p *powerShellEvidence) update(phase string, err, contextErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase = phase
	if phase == "waiting" {
		p.startElapsed = time.Since(p.began)
	} else {
		p.ended = time.Now()
	}
	if err != nil {
		p.failure = fmt.Sprintf("%v; context: %v", err, contextErr)
		if len(p.failure) > 500 {
			p.failure = p.failure[:500]
		}
	}
}

func (p *powerShellEvidence) snapshot() (diagnostic, stderr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stderr = string(p.stderr)
	if p.truncated {
		const gap = "\n[stderr truncated]\n"
		stderr = string(p.prefix) + gap + stderr[len(p.prefix)+len(gap):]
	}
	elapsed := time.Since(p.began)
	if !p.ended.IsZero() {
		elapsed = p.ended.Sub(p.began)
	}
	return fmt.Sprintf("PowerShell transport %d: phase=%s; start=%s; elapsed=%s; error=%s; stderr: %s", p.sequence, p.phase, p.startElapsed, elapsed, p.failure, stderr), stderr
}

// psSSM executes the public transport's real PowerShell/CIM scripts locally.
// It has no AWS client or credentials and retains results like the SSM port.
type psSSM struct {
	mu         sync.Mutex
	results    map[string]*ssm.GetCommandInvocationOutput
	transports []*powerShellEvidence
	calls      int
	active     int
}

func (s *psSSM) SendCommand(ctx context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", strings.Join(in.Parameters["commands"], "\n"))
	var out bytes.Buffer
	trace := &powerShellEvidence{phase: "starting", began: time.Now()}
	cmd.Stdout = &out
	cmd.Stderr = trace
	cmd.WaitDelay = time.Second
	s.mu.Lock()
	s.calls++
	trace.sequence = s.calls
	s.transports = append(s.transports, trace)
	if len(s.transports) > 8 {
		s.transports = s.transports[1:]
	}
	s.active++
	s.mu.Unlock()
	// An active SendCommand is not proof that CreateProcess returned. Preserve
	// the boundary before waiting on the exact process handle owned by cmd.
	err := cmd.Start()
	if err != nil {
		trace.update("start-failed", err, ctx.Err())
	} else {
		trace.update("waiting", nil, nil)
		err = cmd.Wait()
		trace.update("completed", err, ctx.Err())
	}
	_, stderr := trace.snapshot()
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	result := &ssm.GetCommandInvocationOutput{Status: ssmtypes.CommandInvocationStatusSuccess, StandardOutputContent: aws.String(out.String()), StandardErrorContent: aws.String(stderr)}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, err
		}
		result.Status = ssmtypes.CommandInvocationStatusFailed
		result.ResponseCode = int32(exit.ExitCode())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strconv.Itoa(len(s.results) + 1)
	s.results[id] = result
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String(id)}}, nil
}
func (s *psSSM) failures() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out strings.Builder
	fmt.Fprintf(&out, "active PowerShell transports=%d\n", s.active)
	for _, trace := range s.transports {
		diagnostic, _ := trace.snapshot()
		out.WriteString(diagnostic)
		out.WriteByte('\n')
	}
	// Eight traces, each with at most 2,000 stderr and 500 error bytes. Do not
	// tail-truncate the aggregate and discard an earlier startup boundary.
	return out.String()
}

func (s *psSSM) jobDiagnostics(root string) string {
	var stages []string
	jobs, _ := filepath.Glob(filepath.Join(root, "jobs", "*"))
	for _, job := range jobs {
		var present []string
		for _, name := range []string{"owner", "ready", "start-error", "exit", "stop", "stopped"} {
			if _, err := os.Stat(filepath.Join(job, name)); err == nil {
				present = append(present, name)
			}
		}
		for _, role := range []string{"launcher", "supervisor"} {
			data, err := os.ReadFile(filepath.Join(job, role+"-stage"))
			if err == nil && len(data) <= 128 {
				fields := strings.Fields(string(data))
				if len(fields) == 2 {
					session, err := strconv.Atoi(fields[1])
					if err == nil && session >= 0 {
						switch fields[0] {
						case "runtime-load", "runtime-ready", "open-job", "assign-self", "payload-start", "payload-exit", "cim-create", "cim-created", "supervisor-ready":
							present = append(present, fmt.Sprintf("%s=%s(session=%d)", role, fields[0], session))
						}
					}
				}
			}
		}
		stages = append(stages, "job markers="+strings.Join(present, ","))
	}
	return s.failures() + "; " + strings.Join(stages, "; ")
}

func (s *psSSM) GetCommandInvocation(_ context.Context, in *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.results[aws.ToString(in.CommandId)]
	if !ok {
		return nil, &ssmtypes.InvocationDoesNotExist{}
	}
	return r, nil
}

type ownedPeer struct {
	role    string
	pid     int
	conn    net.Conn
	process *os.Process // acquired while the fixture is alive and before it may proceed
	exited  chan struct{}
	waitErr error
}

type transferProgress struct {
	done    <-chan struct{}
	failure func() string
}

func acceptPeer(t *testing.T, l net.Listener, transfer ...transferProgress) *ownedPeer {
	t.Helper()
	require.NoError(t, l.(*net.TCPListener).SetDeadline(time.Now().Add(15*time.Second)))
	type accepted struct {
		conn net.Conn
		err  error
	}
	acceptedPeer := make(chan accepted, 1)
	go func() { c, err := l.Accept(); acceptedPeer <- accepted{c, err} }()
	var failed <-chan struct{}
	if len(transfer) != 0 {
		failed = transfer[0].done
	}
	var result accepted
	select {
	case result = <-acceptedPeer:
	case <-failed:
		_ = l.(*net.TCPListener).SetDeadline(time.Now())
		result = <-acceptedPeer
		if result.conn != nil {
			_ = result.conn.Close()
		}
		t.Fatalf("transfer ended before the owned process handshake: %s", transfer[0].failure())
	}
	c, err := result.conn, result.err
	if err != nil && len(transfer) != 0 {
		t.Fatalf("owned process handshake: %v; %s", err, transfer[0].failure())
	}
	require.NoError(t, err)
	var info struct {
		Role string
		PID  int
	}
	require.NoError(t, json.NewDecoder(c).Decode(&info))
	process, err := os.FindProcess(info.PID)
	require.NoError(t, err)
	p := &ownedPeer{role: info.Role, pid: info.PID, conn: c, process: process, exited: make(chan struct{})}
	go func() { _, p.waitErr = process.Wait(); close(p.exited) }() // exactly one waiter on this retained handle
	t.Cleanup(func() {
		_ = c.Close() // controlled persistent children now exit normally
		select {
		case <-p.exited:
			require.NoError(t, p.waitErr)
		case <-time.After(5 * time.Second):
			_ = p.process.Kill() // the retained Windows handle, never a re-resolved PID
			select {
			case <-p.exited:
			case <-time.After(5 * time.Second):
				t.Error("fixture process did not stop after held-handle cleanup")
			}
			t.Error("fixture process did not exit after its control connection closed")
		}
	})
	if p.role == "launcher" || p.role == "plugin" {
		_, err = c.Write([]byte{1})
		require.NoError(t, err)
	}
	return p
}

func (p *ownedPeer) assertExited(t *testing.T) {
	t.Helper()
	select {
	case <-p.exited:
		require.NoError(t, p.waitErr)
	case <-time.After(5 * time.Second):
		t.Fatalf("owned %s process survived cleanup", p.role)
	}
}

func assertAlive(t *testing.T, p *ownedPeer) {
	t.Helper()
	require.NoError(t, p.conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err := p.conn.Write([]byte{42})
	require.NoError(t, err)
	var b [1]byte
	_, err = io.ReadFull(p.conn, b[:])
	require.NoError(t, err, "control process was terminated instead of remaining alive")
	require.Equal(t, byte(42), b[0])
}

// Guards: T1/T4 transport ownership — remote CIM jobs and the local tunnel own
// only their kernel Job Objects; completion and stale IDs never kill a bystander.
func TestWindowsOwnedJobLifecycle(t *testing.T) {
	for _, mode := range []string{"cancel", "completed", "bulk"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			binary, err := os.Executable()
			require.NoError(t, err)
			data, err := os.ReadFile(binary)
			require.NoError(t, err)
			exe := filepath.Join(root, "fixture.exe")
			require.NoError(t, os.WriteFile(exe, data, 0o700))
			api := &psSSM{results: map[string]*ssm.GetCommandInvocationOutput{}}
			cfg := remote.SSMHostConfig{Name: "owned-fixture", OS: remote.Windows, InstanceID: "fixture", WorkDir: root}
			if mode == "bulk" {
				cfg.Profile = "fixture"
				cfg.Region = "fixture"
				require.NoError(t, os.WriteFile(filepath.Join(root, "aws.exe"), data, 0o700))
				t.Setenv("PATH", root+";"+os.Getenv("PATH"))
			}
			h, err := remote.NewSSMHost(cfg, api)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if mode == "bulk" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				defer func() { _ = listener.Close() }()
				t.Setenv("CUCINA_OWNER_CONTROL", listener.Addr().String())
				diagnostic := filepath.Join(root, "tunnel-error.txt")
				t.Setenv("CUCINA_OWNER_DIAGNOSTIC", diagnostic)
				payload := bytes.Repeat([]byte("owned-transfer"), 24000)
				src := filepath.Join(root, "source.bin")
				dst := filepath.Join(root, "destination.bin")
				require.NoError(t, os.WriteFile(src, payload, 0o600))
				done := make(chan struct{})
				var transferErr error
				go func() { transferErr = h.Put(ctx, src, dst); close(done) }()
				t.Cleanup(func() {
					cancel()
					select {
					case <-done:
					case <-time.After(20 * time.Second):
						t.Error("owned transfer did not finish cleanup")
					}
				})
				progress := transferProgress{done: done, failure: func() string {
					text, _ := os.ReadFile(diagnostic)
					select {
					case <-done:
						return fmt.Sprintf("transfer=%v; fixture=%s; PowerShell=%s", transferErr, text, api.failures())
					default:
						return fmt.Sprintf("transfer still active; fixture=%s; PowerShell=%s", text, api.failures())
					}
				}}
				launcher := acceptPeer(t, listener, progress)
				plugin := acceptPeer(t, listener, progress)
				require.Equal(t, "launcher", launcher.role)
				require.Equal(t, "plugin", plugin.role)
				select {
				case <-done:
					require.NoError(t, transferErr, api.failures())
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				launcher.assertExited(t)
				plugin.assertExited(t)
				got, err := os.ReadFile(dst)
				require.NoError(t, err)
				require.Equal(t, payload, got)
				return
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer func() { _ = listener.Close() }()
			unrelated := exec.Command(exe)
			unrelated.Env = append(os.Environ(), "CUCINA_OWNER_ROLE=unrelated", "CUCINA_OWNER_CONTROL="+listener.Addr().String())
			require.NoError(t, unrelated.Start())
			unrelatedDone := make(chan struct{})
			go func() { _ = unrelated.Wait(); close(unrelatedDone) }()
			peerObserved := false
			t.Cleanup(func() {
				if !peerObserved {
					_ = unrelated.Process.Kill()
				}
				select {
				case <-unrelatedDone:
				case <-time.After(5 * time.Second):
					_ = unrelated.Process.Kill()
					t.Error("unrelated fixture did not join")
				}
			}) // peer cleanup below closes control and joins first
			bystander := acceptPeer(t, listener)
			peerObserved = true
			j, err := h.Start(ctx, "& '"+strings.ReplaceAll(exe, "'", "''")+"'", remote.Opts{Env: map[string]string{"CUCINA_OWNER_ROLE": "payload", "CUCINA_OWNER_MODE": mode, "CUCINA_OWNER_CONTROL": listener.Addr().String()}})
			require.NoError(t, err, api.jobDiagnostics(root))
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
				defer stop()
				_ = remote.StopJob(cleanup, h, j)
			})
			child := acceptPeer(t, listener)
			if mode == "completed" {
				st, err := remote.Wait(ctx, h, j, time.Millisecond, func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
				require.NoError(t, err)
				require.Equal(t, 0, st.ExitCode)
				// Model a reused old PID in the persisted record without real PID churn.
				require.NoError(t, os.WriteFile(filepath.Join(j.Dir, "pid"), []byte(strconv.Itoa(bystander.pid)), 0o600))
				require.NoError(t, remote.StopJob(ctx, h, j))
				assertAlive(t, child)
			} else {
				require.NoError(t, remote.StopJob(ctx, h, j))
				child.assertExited(t)
				require.NoError(t, child.conn.SetReadDeadline(time.Now().Add(5*time.Second)))
				_, err := child.conn.Read(make([]byte, 1))
				require.Error(t, err)
				var ne net.Error
				if errors.As(err, &ne) {
					require.False(t, ne.Timeout(), "owned child survived cancellation")
				}
			}
			assertAlive(t, bystander)
		})
	}
}
