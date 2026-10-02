// SPDX-License-Identifier: FSL-1.1-ALv2

// Package remote runs commands on the campaign's machines without inbound
// ports: the Linux and Windows client VMs through SSM Run Command (and SSM
// port forwarding for large files), and the dev Mac directly. Every host
// offers the same operations — synchronous runs with complete (chunked)
// output capture, background jobs that survive the transport (long Bazel
// builds) with status polling, and file transfer — implemented once as small
// POSIX-sh and PowerShell scripts executed by a transport.
package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// OS names.
const (
	Linux   = "linux"
	Windows = "windows"
	Darwin  = "darwin"
)

// Opts tune a command.
type Opts struct {
	// Timeout bounds a synchronous Run (default 10 min).
	Timeout time.Duration
	// User runs the command as this user (Linux: sudo -u; ignored elsewhere).
	User string
	// Env adds environment variables.
	Env map[string]string
	// Dir is the working directory.
	Dir string
}

// Result is a finished command.
type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Duration time.Duration
}

// Err returns a descriptive error for a non-zero exit, nil otherwise.
func (r Result) Err() error {
	if r.ExitCode == 0 {
		return nil
	}
	return &ExitError{Code: r.ExitCode, Stderr: tail(string(r.Stderr), 2000), Stdout: tail(string(r.Stdout), 1000)}
}

// ExitError is a non-zero exit.
type ExitError struct {
	Code           int
	Stdout, Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit status %d: %s%s", e.Code, e.Stderr, e.Stdout)
}

// Job is a background job.
type Job struct {
	ID  string `json:"id"`
	Dir string `json:"dir"`
}

// JobState values.
const (
	JobStarting = "starting"
	JobRunning  = "running"
	JobExited   = "exited"
	JobLost     = "lost" // process gone without an exit code (reboot, kill)
)

// JobStatus is a polled job state.
type JobStatus struct {
	State       string
	ExitCode    int
	StdoutBytes int64
	StderrBytes int64
}

// ErrJobLost is returned by Wait when a job vanished.
var ErrJobLost = errors.New("background job lost")

// Host is one machine.
type Host interface {
	Name() string
	OS() string
	// WorkDir is the host's scratch root for jobs and transfers.
	WorkDir() string
	// Run executes a script (sh on Linux/macOS, PowerShell on Windows) and
	// returns its complete output.
	Run(ctx context.Context, script string, o Opts) (Result, error)
	// Start launches the script as a detached background job.
	Start(ctx context.Context, script string, o Opts) (Job, error)
	// Status polls a job.
	Status(ctx context.Context, j Job) (JobStatus, error)
	// Read returns up to max bytes of a job's "stdout" or "stderr" from off.
	Read(ctx context.Context, j Job, stream string, off int64, max int) ([]byte, error)
	// Put copies a local file to the host; Get copies a host file locally.
	// Both verify SHA-256 end to end.
	Put(ctx context.Context, local, remote string) error
	Get(ctx context.Context, remote, local string) error
}

// Sleeper waits between polls; tests inject a fake.
type Sleeper func(ctx context.Context, d time.Duration) error

// RealSleep waits for d or until ctx is done.
func RealSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Wait polls a job every `every` until it exits, ctx ends or it is lost. A
// job is declared lost only when two consecutive polls say so: one poll on a
// host under process pressure is not enough evidence to abandon a build.
func Wait(ctx context.Context, h Host, j Job, every time.Duration, sleep Sleeper) (JobStatus, error) {
	if sleep == nil {
		sleep = RealSleep
	}
	lost := 0
	for {
		st, err := h.Status(ctx, j)
		if err != nil {
			return st, err
		}
		switch st.State {
		case JobExited:
			return st, nil
		case JobLost:
			if lost++; lost >= 2 {
				return st, fmt.Errorf("%s: job %s: %w", h.Name(), j.ID, ErrJobLost)
			}
		default:
			lost = 0
		}
		if err := sleep(ctx, every); err != nil {
			return st, err
		}
	}
}

// ChunkSize is the raw byte size of one output chunk read over a transport
// whose responses are limited (SSM returns at most 24,000 characters: 16 KiB
// base64-encodes to 21,848).
const ChunkSize = 16 << 10

// ReadAll reads a job stream completely in chunks.
func ReadAll(ctx context.Context, h Host, j Job, stream string, size int64) ([]byte, error) {
	out := make([]byte, 0, size)
	for int64(len(out)) < size {
		b, err := h.Read(ctx, j, stream, int64(len(out)), ChunkSize)
		if err != nil {
			return out, err
		}
		if len(b) == 0 {
			break
		}
		out = append(out, b...)
	}
	return out, nil
}

// RunJob runs a script as a background job and waits for it, returning the
// complete output — the pattern for long builds that outlive one transport
// call.
func RunJob(ctx context.Context, h Host, script string, o Opts, every time.Duration, sleep Sleeper) (Job, Result, error) {
	start := time.Now()
	j, err := h.Start(ctx, script, o)
	if err != nil {
		return j, Result{}, err
	}
	st, err := Wait(ctx, h, j, every, sleep)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		stopErr := StopJob(cleanup, h, j)
		cancel()
		return j, Result{}, errors.Join(err, stopErr)
	}
	stdout, err := ReadAll(ctx, h, j, "stdout", st.StdoutBytes)
	if err != nil {
		return j, Result{}, err
	}
	stderr, err := ReadAll(ctx, h, j, "stderr", st.StderrBytes)
	if err != nil {
		return j, Result{}, err
	}
	return j, Result{ExitCode: st.ExitCode, Stdout: stdout, Stderr: stderr, Duration: time.Since(start)}, nil
}

// StopJob terminates a detached job's process tree. It is called with an
// independent bounded context after cancellation, so cancelling Wait cannot
// abandon an active remote build. Linux SSM jobs have a dedicated systemd
// unit; local jobs retain the original process owner, and Windows jobs retain
// kernel Job Object ownership. Persisted numeric PIDs are never authority to kill.
func StopJob(ctx context.Context, h Host, j Job) error {
	if j.ID == "" || j.Dir == "" {
		return fmt.Errorf("cannot stop an unidentified job")
	}
	if sh, ok := h.(*scriptHost); ok {
		if t, ok := sh.t.(jobTransport); ok {
			return t.stopJob(ctx, j)
		}
		sh.jobsMu.Lock()
		owned, exists := sh.jobs[j.ID]
		sh.jobsMu.Unlock()
		if !exists || owned != j {
			return errors.New("job has no launch ownership record; refusing stale cleanup")
		}
	} else {
		return errors.New("host cannot prove job ownership")
	}
	var script string
	if h.OS() == Windows {
		script = psStopOwnedJob(j)
	} else {
		script = fmt.Sprintf(`set -eu
J=%s
unit=%s
[ ! -f "$J/exit" ] || exit 0
if command -v systemctl >/dev/null 2>&1 && [ "$(id -u)" = 0 ] && systemctl is-active --quiet "$unit"; then
  systemctl stop "$unit"
else
  printf 'job has no owned systemd unit; refusing persisted PID cleanup\n' >&2
  exit 1
fi
`, shQuote(j.Dir), shQuote("cucina-e2e-"+j.ID))
	}
	r, err := h.Run(ctx, script, Opts{Timeout: time.Minute})
	if err != nil {
		return err
	}
	if err = r.Err(); err != nil {
		return err
	}
	for {
		s, err := h.Status(ctx, j)
		if err != nil {
			return err
		}
		if s.State == JobExited || s.State == JobLost {
			return nil
		}
		if err := RealSleep(ctx, time.Second); err != nil {
			return fmt.Errorf("job did not stop: %w", err)
		}
	}
}

func newJobID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
