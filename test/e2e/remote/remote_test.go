// SPDX-License-Identifier: FSL-1.1-ALv2
//go:build !windows

package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Integration tier (localhost only): the POSIX job/transfer scripts the SSM
// transport sends to the Linux client are executed for real by /bin/sh on
// the machine running the test. Guards the e2e task's remote plumbing
// requirements: complete output capture beyond the transport's 24,000
// character limit, background jobs with status polling (incl. a job that
// dies without an exit code), and SHA-256-verified chunked file transfer.

func localHost(t *testing.T) *scriptHost {
	return &scriptHost{name: "local", os: "darwin", workDir: t.TempDir(), t: &localTransport{}, d: sh{}, putChunk: 7000}
}

// busyPoll re-polls immediately: the jobs below finish in milliseconds, and
// tests never sleep (R-TEST-5.3); ctx bounds the loop.
func busyPoll(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func TestRunCapturesCompleteOutput(t *testing.T) {
	h := localHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	work := t.TempDir()
	// 30,000 bytes of stdout: above the inlined 12,000-byte head, so the rest
	// comes back in chunks; environment and working directory are applied.
	res, err := h.Run(ctx, `head -c 30000 /dev/zero | tr '\0' 'x'; echo "it's $GREETING from $(pwd)" >&2; exit 3`,
		Opts{Env: map[string]string{"GREETING": "o'hai"}, Dir: work})
	require.NoError(t, err)
	require.Equal(t, 3, res.ExitCode)
	require.Equal(t, strings.Repeat("x", 30000), string(res.Stdout))
	gotWork, _ := filepath.EvalSymlinks(work)
	require.Contains(t, string(res.Stderr), "it's o'hai from ")
	require.Contains(t, string(res.Stderr), filepath.Base(gotWork))
	require.Error(t, res.Err())
}

func TestBackgroundJobs(t *testing.T) {
	h := localHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, res, err := RunJob(ctx, h, "seq 1 5000; echo done >&2; exit 4", Opts{}, time.Second, busyPoll)
	require.NoError(t, err)
	require.Equal(t, 4, res.ExitCode)
	require.Equal(t, 5000, strings.Count(string(res.Stdout), "\n"))
	require.Equal(t, "done\n", string(res.Stderr))

	// The job's supervising shell is killed: no exit code is ever written.
	j, err := h.Start(ctx, "kill -9 $PPID", Opts{})
	require.NoError(t, err)
	_, err = Wait(ctx, h, j, time.Second, busyPoll)
	require.ErrorIs(t, err, ErrJobLost)

	// Cancelling a detached build must stop it, not only stop polling. The
	// long-lived subprocess is our fake workload, not a test synchronization sleep.
	cancelCtx, stop := context.WithCancel(ctx)
	j, _, err = RunJob(cancelCtx, h, "exec sleep 600", Opts{}, time.Second, func(context.Context, time.Duration) error { stop(); return context.Canceled })
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = StopJob(cleanup, h, j)
	})
	require.ErrorIs(t, err, context.Canceled)
	st, err := h.Status(ctx, j)
	require.NoError(t, err)
	require.Contains(t, []string{JobExited, JobLost}, st.State, "cancelled workload must no longer run")
}

// Guards: cancellation ownership — stale on-disk PID/group data must never
// target a different process after the original job is lost. Replacing the
// recorded identity models PID reuse without churning global OS process IDs.
func TestLostJobDoesNotUseRecycledIdentity(t *testing.T) {
	h := localHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	j, err := h.Start(ctx, "kill -9 $PPID", Opts{})
	require.NoError(t, err)
	_, err = Wait(ctx, h, j, time.Millisecond, busyPoll)
	require.ErrorIs(t, err, ErrJobLost)

	unrelated := exec.Command("/bin/sh", "-c", "read -r release")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := unrelated.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, unrelated.Start())
	t.Cleanup(func() {
		_ = input.Close()
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait() // the test holds this identity unreaped until cleanup
	})
	require.NoError(t, os.WriteFile(filepath.Join(j.Dir, "group"), fmt.Appendf(nil, "%d\n", unrelated.Process.Pid), 0o600))
	_ = StopJob(ctx, h, j) // stale/unowned cleanup may fail, but must not signal it
	fresh := NewLocal("new harness", h.WorkDir())
	require.Error(t, StopJob(ctx, fresh, j), "a new harness cannot recover kill authority from a PID file")
	_, err = input.Write([]byte("release\n"))
	require.NoError(t, err, "a different process must remain untouched by stale job cleanup")
	require.NoError(t, unrelated.Wait(), "the unrelated control process must exit normally")
}

// privateTransport exercises the SSM-side scripts on localhost without an
// AWS connection; the bulk fake observes filesystem state before any bytes.
type privateTransport struct{ *localTransport }

type privateBulk struct{ secure bool }

func (b *privateBulk) put(_ context.Context, _ *scriptHost, local, dst string) error {
	d, err := os.Stat(filepath.Dir(dst))
	if err != nil {
		return err
	}
	p, err := os.Stat(dst + ".part")
	if err != nil {
		return err
	}
	b.secure = d.Mode().Perm() == 0o700 && p.Mode().Perm() == 0o600
	if !b.secure {
		return errors.New("secret transfer started before permissions were private")
	}
	data, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst+".part", data, 0o600); err != nil {
		return err
	}
	return os.Rename(dst+".part", dst)
}
func (*privateBulk) get(context.Context, *scriptHost, string, string) error {
	return errors.New("not a download")
}

// Guards §12: private destination permissions apply before transfer, including
// retrying over a pre-existing world-readable staging file, not just afterwards.
func TestPutPrivateBeforeTransfer(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "fixture")
			require.NoError(t, os.WriteFile(src, []byte("not-a-real-key"), 0o600))
			dst := filepath.Join(root, "secrets", "key")
			if existing {
				require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
				require.NoError(t, os.WriteFile(dst+".part", nil, 0o644))
			}
			bulk := &privateBulk{}
			h := &scriptHost{name: "private-test", os: Linux, workDir: root, t: privateTransport{&localTransport{}}, d: sh{}, bulk: bulk}
			require.NoError(t, PutPrivate(context.Background(), h, src, dst, ""))
			require.True(t, bulk.secure)
			st, err := os.Stat(dst)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
		})
	}
}

func TestTransferRoundTrip(t *testing.T) {
	h := localHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	src := filepath.Join(t.TempDir(), "payload.bin")
	data := make([]byte, 50_001) // > 7 upload chunks and > 3 download chunks
	_, _ = rand.Read(data)
	require.NoError(t, os.WriteFile(src, data, 0o644))

	remote := filepath.Join(h.workDir, "in", "payload.bin")
	require.NoError(t, h.Put(ctx, src, remote))
	back := filepath.Join(t.TempDir(), "back.bin")
	require.NoError(t, h.Get(ctx, remote, back))
	got, err := os.ReadFile(back)
	require.NoError(t, err)
	require.True(t, bytes.Equal(data, got))

	empty := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(empty, nil, 0o644))
	require.NoError(t, h.Put(ctx, empty, filepath.Join(h.workDir, "empty")))
}
