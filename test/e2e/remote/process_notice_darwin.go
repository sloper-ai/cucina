// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

type processNotice struct{ fd int }

func newProcessNotice() (*processNotice, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	return &processNotice{fd: fd}, nil
}
func (p *processNotice) bind(pid int) error {
	changes := []unix.Kevent_t{{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}}
	for {
		_, err := unix.Kevent(p.fd, changes, nil, &unix.Timespec{})
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}
func (p *processNotice) wait(ctx context.Context, pid int) error {
	events := make([]unix.Kevent_t, 1)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.Kevent(p.fd, nil, events, &unix.Timespec{Nsec: 200000000})
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		e := events[0]
		if e.Flags&unix.EV_ERROR != 0 {
			return fmt.Errorf("owned process kqueue: %w", unix.Errno(e.Data))
		}
		if e.Ident != uint64(pid) || e.Filter != unix.EVFILT_PROC || e.Fflags&unix.NOTE_EXIT == 0 {
			return errors.New("unexpected owned process exit notification")
		}
		return nil // NOTE_EXIT observes the still-unreaped child; it does not wait/reap
	}
}
func (p *processNotice) close() error { return unix.Close(p.fd) }
