// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package hostos

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func diskUsage(path string) (used, free uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return 0, 0, &os.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: err}
	}
	return total - totalFree, avail, nil
}

func signal(p *os.Process, sig string) error {
	switch sig {
	case "KILL", "TERM":
		// Windows has no SIGTERM for arbitrary processes; services are stopped
		// through the service manager (shawl delivers CTRL_C to bb_worker).
		return p.Kill()
	default:
		return fmt.Errorf("hostos: unsupported signal %q on windows", sig)
	}
}
