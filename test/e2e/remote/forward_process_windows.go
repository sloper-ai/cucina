// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Start suspended so assignment to an owned Job Object precedes any child
// creation. Closing the job kills every descendant, including an orphan plugin.
type forwardProcess struct {
	cmd      *exec.Cmd
	mu       sync.Mutex
	job      windows.Handle
	assigned bool
}

func ownForwardProcess(cmd *exec.Cmd) (*forwardProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	return &forwardProcess{cmd: cmd, job: job}, nil
}

func (p *forwardProcess) started() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var startErr error
	handleErr := p.cmd.Process.WithHandle(func(handle uintptr) {
		if startErr = windows.AssignProcessToJobObject(p.job, windows.Handle(handle)); startErr != nil {
			return
		}
		p.assigned = true
		// Keep the original process handle live through thread lookup, so its
		// PID cannot identify an unrelated process after concurrent cancellation.
		startErr = resumeForwardProcess(uint32(p.cmd.Process.Pid))
	})
	return errors.Join(handleErr, startErr)
}

func resumeForwardProcess(pid uint32) error {
	// os/exec closes the primary thread handle. The suspended process has one
	// thread, which we reopen by exact owner PID to resume after job assignment.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, resumeErr := windows.ResumeThread(thread)
		closeErr := windows.CloseHandle(thread)
		return errors.Join(resumeErr, closeErr)
	}
	return fmt.Errorf("find suspended forwarding thread: %w", err)
}

func (p *forwardProcess) waitExited(ctx context.Context) error {
	var waitErr error
	handleErr := p.cmd.Process.WithHandle(func(handle uintptr) {
		for {
			if waitErr = ctx.Err(); waitErr != nil {
				return
			}
			var state uint32
			state, waitErr = windows.WaitForSingleObject(windows.Handle(handle), 200)
			if waitErr != nil || state == windows.WAIT_OBJECT_0 {
				return
			}
			if state != uint32(windows.WAIT_TIMEOUT) {
				waitErr = errors.New("unexpected owned process wait state")
				return
			}
		}
	})
	return errors.Join(handleErr, waitErr)
}

func (*forwardProcess) release() error                       { return nil }
func (p *forwardProcess) waitReap(ctx context.Context) error { return p.waitExited(ctx) }
func (p *forwardProcess) terminate() error                   { return p.kill() }
func (p *forwardProcess) kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := windows.TerminateJobObject(p.job, 1)
	if p.assigned && err == nil {
		return nil
	}
	// Cancellation may arrive before assignment, while the launcher is still
	// suspended. The os.Process handle still identifies exactly our process.
	if p.cmd.Process != nil {
		killErr := p.cmd.Process.Kill()
		if !errors.Is(killErr, os.ErrProcessDone) {
			err = errors.Join(err, killErr)
		}
	}
	return err
}

func (p *forwardProcess) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return windows.CloseHandle(p.job)
}
