// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// processOwner is the sole waiter/signaller for a command. Exit notification does
// not reap: the child reserves its PID/group until finish has sent its LAST signal.
// Use exec.Command, never CommandContext, whose watchdog would race final reaping.
type processOwner struct {
	cmd         *exec.Cmd
	tree        *forwardProcess
	exited      chan struct{}
	exitErr     error // published by closing exited
	cancelWatch context.CancelFunc
	mu          sync.Mutex
	released    bool
	result      error
}

func startOwned(cmd *exec.Cmd) (*processOwner, error) {
	tree, err := ownForwardProcess(cmd)
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, errors.Join(err, tree.close())
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &processOwner{cmd: cmd, tree: tree, exited: make(chan struct{}), cancelWatch: cancel}
	if err = tree.started(); err != nil {
		p.exitErr = err
		close(p.exited)
		return nil, errors.Join(fmt.Errorf("prepare owned process notification: %w", err), p.finish(true))
	}
	go func() { p.exitErr = tree.waitExited(ctx); close(p.exited) }()
	return p, nil
}

// finish arbitrates normal completion against cancellation. Once released is
// set, no path can signal this numeric identity again, including a late cancel.
func (p *processOwner) finish(stop bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return p.result
	}
	select {
	case <-p.exited:
		if p.exitErr != nil {
			if errors.Is(p.exitErr, syscall.ECHILD) {
				p.released = true
				p.cancelWatch()
				p.result = errors.Join(fmt.Errorf("owned child reservation lost: %w", p.exitErr), p.tree.close())
				return p.result // never signal an identity another waiter may have released
			}
			stop = true // notification failure is not proof of successful completion
		}
	default:
		if !stop {
			return errors.New("owned process has not reported exit")
		}
	}
	var cleanupErr error
	select {
	case <-p.exited:
		if p.exitErr != nil {
			cleanupErr = fmt.Errorf("owned process notification failed: %w", p.exitErr)
		}
	default:
	}
	if stop {
		cleanupErr = errors.Join(cleanupErr, p.tree.terminate())
		grace := time.NewTimer(250 * time.Millisecond)
		select {
		case <-p.exited:
		case <-grace.C:
		}
		grace.Stop()
		// Even if the launcher exited first, its unreaped PID still reserves the
		// group identity while descendants are killed. There is no probe-then-kill.
		if err := p.tree.kill(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("terminate owned process tree: %w", err))
		}
	} else if err := p.tree.release(); err != nil {
		cleanupErr = fmt.Errorf("release owned process anchor: %w", err)
	}
	p.released = true // prohibit every further signal BEFORE the only reaping wait
	p.cancelWatch()
	select {
	case <-p.exited:
	case <-time.After(time.Second):
		p.result = errors.Join(cleanupErr, errors.New("owned process notifier did not stop"))
		return p.result // keep its fd reserved rather than close/reuse during a syscall
	}
	reapContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.tree.waitReap(reapContext); err != nil {
		p.result = errors.Join(cleanupErr, fmt.Errorf("owned anchor did not report exit: %w", err), p.tree.close())
		return p.result
	}
	waited := make(chan error, 1)
	go func() { waited <- p.cmd.Wait() }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	select {
	case err := <-waited:
		if !stop {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	case <-deadline.C:
		cleanupErr = errors.Join(cleanupErr, errors.New("owned process did not reap after final termination"))
	}
	p.result = errors.Join(cleanupErr, p.tree.close())
	return p.result
}
