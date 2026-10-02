// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// jobTransport keeps local ownership behind the same public Host operations.
// SSM transports continue to use remotely owned systemd units / Windows jobs.
type jobTransport interface {
	startJob(context.Context, Job, string, Opts) (Job, error)
	jobStatus(Job) (JobStatus, error)
	stopJob(context.Context, Job) error
}

type localJob struct {
	mu    sync.Mutex
	job   Job
	owner *processOwner
	state JobStatus
	err   error
}

func (t *localTransport) startJob(ctx context.Context, j Job, script string, o Opts) (Job, error) {
	if err := ctx.Err(); err != nil {
		return j, err
	}
	if err := os.MkdirAll(j.Dir, 0o700); err != nil {
		return j, err
	}
	write, run := (sh{}).cmdFile(j.Dir, script, o)
	q := shQuote(j.Dir)
	body := write + fmt.Sprintf("echo $$ > %s/pid.tmp && mv %s/pid.tmp %s/pid\n%s </dev/null >%s/stdout 2>%s/stderr\nrc=$?\nprintf '%%s\\n' \"$rc\" > %s/exit.tmp && mv %s/exit.tmp %s/exit\n", q, q, q, run, q, q, q, q, q)
	cmd := exec.Command("/bin/sh", "-c", body)
	owner, err := startOwned(cmd)
	if err != nil {
		return j, err
	}
	entry := &localJob{job: j, owner: owner, state: JobStatus{State: JobRunning}}
	t.mu.Lock()
	if t.jobs == nil {
		t.jobs = make(map[string]*localJob)
	}
	t.jobs[j.ID] = entry
	t.mu.Unlock()
	go func() { <-owner.exited; entry.mu.Lock(); defer entry.mu.Unlock(); entry.complete() }()
	if err := ctx.Err(); err != nil {
		return j, errors.Join(err, t.stopJob(context.WithoutCancel(ctx), j))
	}
	return j, nil
}

func (t *localTransport) findJob(j Job) (*localJob, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.jobs[j.ID]
	if entry == nil || entry.job.Dir != j.Dir {
		return nil, errors.New("local job has no live ownership record; refusing persisted PID cleanup")
	}
	return entry, nil
}

// complete and stop publish terminal state under the same lock. A late stop
// cannot kill a successful build's deliberately persistent Bazel server.
func (j *localJob) complete() {
	if j.state.State != JobRunning {
		return
	}
	data, readErr := os.ReadFile(filepath.Join(j.job.Dir, "exit"))
	code, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if readErr == nil && parseErr == nil && j.owner.exitErr == nil {
		j.err = j.owner.finish(false)
		j.state = JobStatus{State: JobExited, ExitCode: code}
	} else {
		j.err = j.owner.finish(true)
		j.state = JobStatus{State: JobLost}
	}
}
func (t *localTransport) jobStatus(j Job) (JobStatus, error) {
	entry, err := t.findJob(j)
	if err != nil {
		return JobStatus{}, err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	select {
	case <-entry.owner.exited:
		entry.complete()
	default:
	}
	st := entry.state
	if f, err := os.Stat(filepath.Join(j.Dir, "stdout")); err == nil {
		st.StdoutBytes = f.Size()
	}
	if f, err := os.Stat(filepath.Join(j.Dir, "stderr")); err == nil {
		st.StderrBytes = f.Size()
	}
	return st, entry.err
}
func (t *localTransport) stopJob(ctx context.Context, j Job) error {
	entry, err := t.findJob(j)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.state.State != JobRunning {
		return entry.err
	}
	select {
	case <-entry.owner.exited:
		entry.complete()
		return entry.err
	default:
	}
	entry.err = entry.owner.finish(true)
	entry.state = JobStatus{State: JobLost}
	return entry.err
}
