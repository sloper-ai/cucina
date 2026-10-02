// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build windows

package bazelrun_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

	"github.com/sloper-ai/cucina/test/e2e/bazelrun"
	"github.com/sloper-ai/cucina/test/e2e/remote"
)

// The test executable also models a Bazel client with a persistent server.
// The server exits only when the test closes its loopback connection; neither
// readiness nor completion depends on a synchronization sleep.
func TestMain(m *testing.M) {
	if strings.EqualFold(filepath.Base(os.Args[0]), "bazel.exe") {
		code, err := syntheticBazel(os.Args[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 99
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func syntheticBazel(args []string) (int, error) {
	root := os.Getenv("CUCINA_WAIT_TEST_ROOT")
	if len(args) == 1 && args[0] == "--fixture-server" {
		conn, err := net.DialTimeout("tcp", os.Getenv("CUCINA_WAIT_TEST_ADDR"), 10*time.Second)
		if err != nil {
			return 0, err
		}
		if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return 0, errors.Join(err, conn.Close())
		}
		if _, err := fmt.Fprintln(conn, os.Getpid()); err != nil {
			return 0, errors.Join(err, conn.Close())
		}
		_, err = conn.Read(make([]byte, 1))
		if errors.Is(err, io.EOF) {
			err = nil
		}
		return 0, errors.Join(err, conn.Close())
	}
	for _, arg := range args {
		switch arg {
		case "clean", "shutdown":
			return 0, nil
		case "info":
			pid, err := os.ReadFile(filepath.Join(root, "server.pid"))
			if err != nil {
				return 0, err
			}
			fmt.Println(string(pid))
			return 0, nil
		case "build", "test":
			data, err := json.Marshal(args)
			if err != nil {
				return 0, err
			}
			if err := os.WriteFile(filepath.Join(root, arg+".args.json"), data, 0o600); err != nil {
				return 0, err
			}
			if arg == "test" {
				return 3, nil
			}
			executable, err := os.Executable()
			if err != nil {
				return 0, err
			}
			server := exec.Command(executable, "--fixture-server")
			if err := server.Start(); err != nil {
				return 0, err
			}
			if err := os.WriteFile(filepath.Join(root, "server.pid"), []byte(strconv.Itoa(server.Process.Pid)), 0o600); err != nil {
				return 0, err
			}
			if err := server.Process.Release(); err != nil {
				return 0, err
			}
			code, err := strconv.Atoi(os.Getenv("CUCINA_WAIT_BUILD_EXIT"))
			return code, err
		}
	}
	return 0, fmt.Errorf("unexpected synthetic Bazel arguments: %q", args)
}

// localPowerShellSSM executes the real Windows scripts at the public SSM port.
// It keeps finished command results like SSM, but never makes a network request
// or requires AWS credentials. Only the fixture server uses a loopback socket.
type localPowerShellSSM struct {
	mu      sync.Mutex
	results map[string]*ssm.GetCommandInvocationOutput
}

func (s *localPowerShellSSM) SendCommand(ctx context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", strings.Join(in.Parameters["commands"], "\n"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	result := &ssm.GetCommandInvocationOutput{
		Status:                ssmtypes.CommandInvocationStatusSuccess,
		StandardOutputContent: aws.String(stdout.String()),
		StandardErrorContent:  aws.String(stderr.String()),
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, err
		}
		result.Status, result.ResponseCode = ssmtypes.CommandInvocationStatusFailed, int32(exit.ExitCode())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strconv.Itoa(len(s.results) + 1)
	s.results[id] = result
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String(id)}}, nil
}

func (s *localPowerShellSSM) GetCommandInvocation(_ context.Context, in *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, ok := s.results[aws.ToString(in.CommandId)]
	if !ok {
		return nil, &ssmtypes.InvocationDoesNotExist{}
	}
	return result, nil
}

// Guards: NFR-P2/NFR-X2 and T4 — a completed Windows Bazel client must yield
// baseline/outcome evidence while its persistent server remains alive; the
// background wrapper must not defer its exit record until that server stops.
func TestWindowsCommandsDoNotWaitForBazelServer(t *testing.T) {
	for _, kind := range []string{"baseline", "invocation"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			executable, err := os.Executable()
			require.NoError(t, err)
			binary, err := os.ReadFile(executable)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, "bazel.exe"), binary, 0o755))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, listener.Close()) })
			server := make(chan net.Conn, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					server <- conn
				}
			}()
			env := map[string]string{
				"PATH":                   root + ";" + os.Getenv("PATH"),
				"CUCINA_WAIT_TEST_ROOT":  root,
				"CUCINA_WAIT_TEST_ADDR":  listener.Addr().String(),
				"CUCINA_WAIT_BUILD_EXIT": "0",
			}
			api := &localPowerShellSSM{results: map[string]*ssm.GetCommandInvocationOutput{}}
			host, err := remote.NewSSMHost(remote.SSMHostConfig{Name: "windows-fixture", OS: remote.Windows, InstanceID: "fixture", WorkDir: root}, api)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			type result struct {
				baseline remote.Result
				outcome  *bazelrun.Outcome
				err      error
			}
			finished := make(chan result, 1)
			go func() {
				if kind == "baseline" {
					path := os.Getenv("BASELINE_PS1")
					if path == "" {
						path = "../abseil/local-baseline.ps1"
					}
					path, err := filepath.Abs(path)
					if err != nil {
						finished <- result{err: err}
						return
					}
					script := fmt.Sprintf("& '%s' -Workspace '%s' -Out '%s' -BazelArgs @('--config=lane-windows')", strings.ReplaceAll(path, "'", "''"), root, filepath.Join(root, "baseline-out"))
					_, res, err := remote.RunJob(ctx, host, script, remote.Opts{Env: env}, time.Millisecond, nil)
					finished <- result{baseline: res, err: err}
					return
				}
				env["CUCINA_WAIT_BUILD_EXIT"] = "7"
				out, err := bazelrun.Run(ctx, bazelrun.Invocation{Name: "wait-fixture", Host: host, Workspace: root, Command: "build", Args: []string{"--config=lane-windows", "//absl/..."}, Env: env, Poll: time.Millisecond}, filepath.Join(root, "collected"))
				finished <- result{outcome: out, err: err}
			}()
			var conn net.Conn
			select {
			case conn = <-server:
			case res := <-finished:
				t.Fatalf("wrapper ended before starting its server: %+v", res)
			case <-ctx.Done():
				t.Fatal("synthetic Bazel server never became ready")
			}
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			pidText, err := bufio.NewReader(conn).ReadString('\n')
			require.NoError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(pidText))
			require.NoError(t, err)
			process, err := os.FindProcess(pid)
			require.NoError(t, err)
			release := sync.OnceFunc(func() {
				require.NoError(t, conn.Close())
				state, err := process.Wait()
				require.NoError(t, err)
				require.True(t, state.Success(), "fixture server did not exit cleanly")
			})
			defer release()
			var res result
			select {
			case res = <-finished:
			case <-time.After(10 * time.Second):
				// Release only this synthetic server before failing; the old
				// process-tree wait can then unwind without orphaned jobs.
				release()
				select {
				case <-finished:
				case <-ctx.Done():
				}
				t.Fatal("completed Bazel client was held by its persistent server")
			}
			require.NoError(t, res.err)
			if kind == "baseline" {
				require.Zero(t, res.baseline.ExitCode)
				var outcome struct{ BuildExit, TestExit int }
				require.NoError(t, json.Unmarshal(bytes.TrimSpace(res.baseline.Stdout), &outcome))
				require.Zero(t, outcome.BuildExit)
				require.Equal(t, 3, outcome.TestExit)
			} else {
				require.NotNil(t, res.outcome)
				require.Equal(t, 7, res.outcome.ExitCode)
				require.NotEmpty(t, res.outcome.ServerPID)
				require.Positive(t, res.outcome.PeakServerRSSBytes)
			}
			commands := []string{"build"}
			if kind == "baseline" {
				commands = append(commands, "test")
			}
			for _, command := range commands {
				data, err := os.ReadFile(filepath.Join(root, command+".args.json"))
				require.NoError(t, err)
				var args []string
				require.NoError(t, json.Unmarshal(data, &args))
				require.Contains(t, args, "--config=lane-windows")
				require.Contains(t, args, "//absl/...")
			}
		})
	}
}
