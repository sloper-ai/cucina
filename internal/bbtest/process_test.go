// SPDX-License-Identifier: FSL-1.1-ALv2

package bbtest

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/fakes"
)

// Guards: the 03b0997 boottest port collision — allocations remain exclusive
// across independent fixture processes before TCP bind and across a restart.
func TestFreeAddrLease(t *testing.T) {
	const helperEnv = "CUCINA_BBTEST_ADDR_HELPER"
	newAllocator := func() AddrAllocator { return AddrAllocator{Rand: fakes.NewRand(1)} }
	if result := os.Getenv(helperEnv); result != "" {
		addr := newAllocator().FreeAddr(t)
		require.NoError(t, os.WriteFile(result, []byte(addr), 0o600))
		return
	}

	executable, err := os.Executable()
	require.NoError(t, err)
	otherProcess := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		result := filepath.Join(dir, "address")
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestFreeAddrLease$")
		// Independent test sandboxes/output bases need not share a temp directory.
		// The helper must not overwrite the parent Bazel XML result, either.
		cmd.Env = append(os.Environ(), helperEnv+"="+result, "XML_OUTPUT_FILE=",
			"TMPDIR="+dir, "TMP="+dir, "TEMP="+dir, "TEST_TMPDIR="+dir)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		data, err := os.ReadFile(result)
		require.NoError(t, err)
		return string(data)
	}

	owned := newAllocator().FreeAddr(t)
	for _, phase := range []string{"before TCP bind", "after TCP stop"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "after TCP stop" {
				listener, err := net.Listen("tcp4", owned)
				require.NoError(t, err, "the lease must allow the real child to bind TCP")
				require.NoError(t, listener.Close())
			}
			// The same seed forces the conflicting candidate without relying on
			// random collisions, concurrent scheduling or sleeps.
			if got := newAllocator().FreeAddr(t); got == owned {
				t.Errorf("independent allocator reused live fixture address %s", owned)
			}
			if got := otherProcess(t); got == owned {
				t.Errorf("independent process reused live fixture address %s", owned)
			}
		})
	}
}

// Guards the reviewed KillContext exit/reap race: a signal error does not
// decide whether our owned child finished. The bounded Cmd.Wait publication
// must be joined, non-exit Wait failures must survive even on an already-done
// call, and a failed join must retain the original kill error. The child is
// really started and reaped first; synctest controls only the publication gap
// between os.Process.Wait and bbtest's done channel, not OS process timing.
func TestKillContextJoinsOwnedCompletion(t *testing.T) {
	const helperEnv = "CUCINA_BBTEST_KILL_EXIT_HELPER"
	if os.Getenv(helperEnv) == "exit" {
		t.Fatal("intentional helper-child exit")
	}

	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestKillContextJoinsOwnedCompletion$")
	// The helper's intentional failure must not overwrite the parent Bazel XML.
	cmd.Env = append(os.Environ(), helperEnv+"=exit", "XML_OUTPUT_FILE=")
	childExit := cmd.Run()
	var exit *exec.ExitError
	require.ErrorAs(t, childExit, &exit, "the owned helper must have exited and been reaped")
	// Windows Wait already releases the process. On Unix, Release puts this
	// already-reaped object into the equivalent unusable signal state. No
	// numeric PID lookup, external process signal or live child is involved.
	err = cmd.Process.Release()
	require.True(t, err == nil || errors.Is(err, syscall.EINVAL), "release already-reaped child: %v", err)
	killCause := cmd.Process.Kill()
	require.Error(t, killCause, "a released owned process must not accept another signal")
	for errors.Unwrap(killCause) != nil {
		killCause = errors.Unwrap(killCause)
	}
	waitFailure := errors.New("fixture Cmd.Wait completion failure")

	for _, tc := range []struct {
		name          string
		alreadyDone   bool
		publish       bool
		waitErr       error
		wantWaitError bool
		wantTimeout   bool
	}{
		{name: "already joined normal exit", alreadyDone: true},
		{name: "already joined exit status", alreadyDone: true, waitErr: childExit},
		{name: "already joined wait error", alreadyDone: true, waitErr: waitFailure, wantWaitError: true},
		{name: "exit status does not hide a joined wait error", alreadyDone: true, waitErr: errors.Join(childExit, waitFailure), wantWaitError: true},
		{name: "released handle before normal completion publication", publish: true},
		{name: "released handle before exit-status publication", publish: true, waitErr: childExit},
		{name: "released handle before wait-error publication", publish: true, waitErr: waitFailure, wantWaitError: true},
		{name: "kill error and no confirmed join", wantTimeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := &Process{Name: "owned-exited-child", cmd: cmd, done: make(chan struct{}), err: tc.waitErr}
				if tc.alreadyDone {
					close(p.done)
				}
				result := make(chan error, 1)
				started := time.Now()
				go func() { result <- p.KillContext(context.Background()) }()
				synctest.Wait()
				var got error
				returned := false
				select {
				case got = <-result:
					returned = true
				default:
				}
				if !tc.alreadyDone {
					require.False(t, returned, "a kill error must not bypass the owned completion join")
				}
				if tc.publish {
					close(p.done)
				}
				if !returned {
					got = <-result
				}
				switch {
				case tc.wantTimeout:
					require.ErrorIs(t, got, context.DeadlineExceeded)
					require.ErrorIs(t, got, killCause, "keep the original kill failure when the join cannot be confirmed")
					require.Equal(t, 5*time.Second, time.Since(started), "the reap bound must not increase")
				case tc.wantWaitError:
					require.ErrorIs(t, got, waitFailure)
				default:
					require.NoError(t, got)
				}
			})
		})
	}
}
