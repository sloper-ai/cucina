// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
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
	return &scriptHost{name: "local", os: "darwin", workDir: t.TempDir(), t: localTransport{}, d: sh{}, putChunk: 7000}
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
