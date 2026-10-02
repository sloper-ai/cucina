// SPDX-License-Identifier: FSL-1.1-ALv2

package remote

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

type processNotice struct{}

func newProcessNotice() (*processNotice, error) { return &processNotice{}, nil }
func (*processNotice) bind(int) error           { return nil }
func (*processNotice) close() error             { return nil }
func (*processNotice) wait(ctx context.Context, pid int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Signo != 0 {
			return nil
		}
		if err := RealSleep(ctx, 20*time.Millisecond); err != nil {
			return err
		}
	}
}
