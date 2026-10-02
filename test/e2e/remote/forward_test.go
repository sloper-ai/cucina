// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build !windows

package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMain also supplies controlled local process fixtures. The launcher starts
// a child that inherits its output pipes, just as a CLI-launched tunnel does.
func TestMain(m *testing.M) {
	if role := os.Getenv("CUCINA_FORWARD_TEST_ROLE"); role != "" {
		if err := forwardingHelper(role); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type forwardEvent struct {
	Role    string `json:"role"`
	PID     int    `json:"pid"`
	Address string `json:"address,omitempty"`
}

type forwardChild struct {
	forwardEvent
	conn net.Conn
	gone chan struct{}
}

type forwardingFixture struct {
	listener net.Listener
	mu       sync.Mutex
	children []*forwardChild
	ready    chan string
	request  chan struct{}
	closed   bool
	done     chan struct{}
}

func newForwardingFixture(t *testing.T, mode string) *forwardingFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &forwardingFixture{listener: listener, ready: make(chan string, 1), request: make(chan struct{}, 1), done: make(chan struct{})}
	t.Cleanup(f.close)
	t.Setenv("CUCINA_FORWARD_TEST_CONTROL", listener.Addr().String())
	t.Setenv("CUCINA_FORWARD_TEST_MODE", mode)
	t.Setenv("BASH_ENV", "/dev/null")
	t.Setenv("ENV", "/dev/null")
	binary, err := os.Executable()
	require.NoError(t, err)
	bin := t.TempDir()
	for name, role := range map[string]string{"aws": "launcher", "python3": "receiver"} {
		script := "#!/bin/sh\nexport CUCINA_FORWARD_TEST_ROLE=" + role + "\nexec " + shQuote(binary) + " \"$@\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700))
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go f.observe(conn)
		}
	}()
	return f
}

func (f *forwardingFixture) observe(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	decoder := json.NewDecoder(conn)
	var event forwardEvent
	if decoder.Decode(&event) != nil {
		return
	}
	child := &forwardChild{forwardEvent: event, conn: conn, gone: make(chan struct{})}
	defer close(child.gone)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.children = append(f.children, child)
	f.mu.Unlock()
	if event.Role == "receiver" {
		select {
		case f.ready <- event.Address:
		case <-f.done:
			return
		}
	}
	if event.Role == "plugin" {
		// Each transfer waits for its own receiver, including after a round trip.
		var target string
		select {
		case target = <-f.ready:
		case <-f.done:
			return
		}
		if json.NewEncoder(conn).Encode(target) != nil {
			return
		}
	}
	for decoder.Decode(&event) == nil {
		if event.Role == "request" {
			select {
			case f.request <- struct{}{}:
			default:
			}
		}
	}
}

func (f *forwardingFixture) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.done)
	_ = f.listener.Close()
	for _, child := range f.children {
		_ = child.conn.Close()
	}
}

// Guards: T1 artifact-transfer regression — Put/Get must finish and reap their
// launcher and inherited-pipe child on success, cancellation and startup failure.
func TestBulkTransferOwnsTunnelProcesses(t *testing.T) {
	for _, mode := range []string{"put", "get", "private-put", "whole-tree-exit", "cancel", "startup-exit", "anchor-exit"} {
		t.Run(mode, func(t *testing.T) {
			f := newForwardingFixture(t, mode)
			h := localHost(t)
			h.bulk = &portForward{instanceID: "fixture", profile: "fixture", region: "fixture"}
			h.bulkThreshold = 256 << 10
			payload := make([]byte, (256<<10)+1)
			for i := range payload {
				payload[i] = byte(i % 251)
			}
			if mode == "private-put" {
				payload = []byte("synthetic private transfer fixture")
				h.t = privateTransport{&localTransport{}} // remote scripts execute locally; not the direct-copy host
			}
			src := filepath.Join(t.TempDir(), "source")
			require.NoError(t, os.WriteFile(src, payload, 0o600))
			dst := filepath.Join(h.workDir, "received")
			back := filepath.Join(t.TempDir(), "returned")
			if mode == "get" {
				require.NoError(t, os.WriteFile(dst, payload, 0o600))
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch mode {
				case "get":
					done <- h.Get(ctx, dst, back)
				case "private-put":
					done <- PutPrivate(ctx, h, src, dst, "")
				default:
					done <- h.Put(ctx, src, dst)
				}
			}()
			if mode == "cancel" {
				select {
				case <-f.request:
					cancel()
				case <-time.After(5 * time.Second):
					cancel()
					f.close()
					t.Fatal("fixture did not receive the transfer request")
				}
			}
			select {
			case err := <-done:
				if mode == "put" || mode == "get" || mode == "private-put" || mode == "whole-tree-exit" {
					require.NoError(t, err)
					result := dst
					if mode == "get" {
						result = back
					}
					got, err := os.ReadFile(result)
					require.NoError(t, err)
					require.Equal(t, payload, got)
				} else {
					require.Error(t, err)
					if mode == "cancel" {
						require.ErrorIs(t, err, context.Canceled)
					}
				}
			case <-time.After(5 * time.Second):
				result := dst
				if mode == "get" {
					result = back
				}
				transferred, readErr := os.ReadFile(result)
				cancel()
				f.close() // release only fixture children, including on the red version
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("transfer remained blocked after fixture cleanup")
				}
				if mode == "put" || mode == "get" || mode == "private-put" || mode == "whole-tree-exit" {
					require.NoError(t, readErr, "transfer must have finished before attributing a cleanup stall")
					require.Equal(t, payload, transferred)
				}
				t.Fatal("transfer blocked on a launcher descendant holding inherited output pipes")
			}
			f.mu.Lock()
			children := append([]*forwardChild(nil), f.children...)
			f.mu.Unlock()
			require.NotEmpty(t, children, "the public transfer must enter the tunnel path")
			for _, child := range children {
				select {
				case <-child.gone:
				case <-time.After(5 * time.Second):
					t.Fatalf("owned %s process remains after transfer return", child.Role)
				}
				if child.Role == "launcher" {
					_, err := syscall.Wait4(child.PID, nil, syscall.WNOHANG, nil)
					require.ErrorIs(t, err, syscall.ECHILD, "launcher must already be reaped")
				}
			}
		})
	}
}

func forwardingHelper(role string) error {
	conn, err := net.Dial("tcp", os.Getenv("CUCINA_FORWARD_TEST_CONTROL"))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	var announcedAddress string
	announce := func(name string) error {
		return json.NewEncoder(conn).Encode(forwardEvent{Role: name, PID: os.Getpid(), Address: announcedAddress})
	}
	if role == "launcher" {
		if err := announce(role); err != nil {
			return err
		}
		if os.Getenv("CUCINA_FORWARD_TEST_MODE") == "startup-exit" {
			return fmt.Errorf("fixture launcher exited before readiness")
		}
		if os.Getenv("CUCINA_FORWARD_TEST_MODE") == "anchor-exit" {
			// This fixture's direct parent is its newly created ownership anchor.
			return syscall.Kill(os.Getppid(), syscall.SIGKILL)
		}
		cmd := exec.Command(os.Args[0], os.Args[1:]...)
		cmd.Env = append(os.Environ(), "CUCINA_FORWARD_TEST_ROLE=plugin")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	var address string
	var target *url.URL
	var handler http.Handler
	done := make(chan struct{})
	if role == "receiver" {
		args := os.Args[1:]
		if len(args) != 6 {
			return fmt.Errorf("invalid receiver fixture arguments")
		}
		path, token := args[2], args[4]
		// The real receiver lives on another machine. Map its requested port
		// to an ephemeral local port instead of colliding with host services.
		address = "127.0.0.1:0"
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/"+token {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			defer close(done)
			if r.Method == http.MethodPut {
				b, err := io.ReadAll(r.Body)
				if err != nil || os.WriteFile(path, b, 0o600) != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			} else {
				http.ServeFile(w, r, path)
			}
		})
	} else {
		signal.Ignore(syscall.SIGTERM) // prove the bounded forced-cleanup path
		var params map[string][]string
		for i, arg := range os.Args {
			if arg == "--parameters" && i+1 < len(os.Args) {
				if err := json.Unmarshal([]byte(os.Args[i+1]), &params); err != nil {
					return err
				}
			}
		}
		address = "127.0.0.1:" + params["localPortNumber"][0]
		u, err := url.Parse("http://127.0.0.1:" + params["portNumber"][0])
		if err != nil {
			return err
		}
		target = u
		proxy := httputil.NewSingleHostReverseProxy(target)
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if os.Getenv("CUCINA_FORWARD_TEST_MODE") == "cancel" {
				_ = announce("request")
				<-r.Context().Done()
				return
			}
			proxy.ServeHTTP(w, r)
			if os.Getenv("CUCINA_FORWARD_TEST_MODE") == "whole-tree-exit" {
				close(done)
			}
		})
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	announcedAddress = listener.Addr().String()
	if err := announce(role); err != nil {
		_ = listener.Close()
		return err
	}
	if role == "plugin" {
		var ready string
		if err := json.NewDecoder(conn).Decode(&ready); err != nil {
			_ = listener.Close()
			return err
		}
		target.Host = ready
		_, _ = fmt.Fprintln(os.Stdout, "Waiting for connections on", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	released, shutdown := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		close(released)
	}()
	go func() {
		select {
		case <-done:
			_ = server.Shutdown(context.Background())
		case <-released:
			_ = server.Close()
		}
		close(shutdown)
	}()
	_ = server.Serve(listener)
	<-shutdown // do not exit the fixture before the HTTP response has flushed
	return nil
}
