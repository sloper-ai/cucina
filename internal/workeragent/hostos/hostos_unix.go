// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build unix

package hostos

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func diskUsage(path string) (used, free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, &os.PathError{Op: "statfs", Path: path, Err: err}
	}
	bs := uint64(st.Bsize) //nolint:gosec // block size is positive
	return (st.Blocks - st.Bfree) * bs, st.Bavail * bs, nil
}

func signal(p *os.Process, sig string) error {
	switch sig {
	case "TERM":
		return p.Signal(syscall.SIGTERM)
	case "INT":
		return p.Signal(syscall.SIGINT)
	case "KILL":
		return p.Kill()
	default:
		return fmt.Errorf("hostos: unsupported signal %q", sig)
	}
}
