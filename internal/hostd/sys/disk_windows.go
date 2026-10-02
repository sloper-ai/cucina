// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package sys

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// DiskUsage implements ports.FS for portable hostlink/fake-runtime consumers
// on ordinary Windows volumes. GetDiskFreeSpaceEx can return quota-adjusted
// capacity but volume-wide free space. Reject inconsistent accounting rather
// than underflow or fabricate usage. Full physical-volume accounting under
// quotas requires a separate volume-length query and is not implemented here.
// See https://learn.microsoft.com/windows/win32/api/fileapi/nf-fileapi-getdiskfreespaceexw.
func (FS) DiskUsage(path string) (used, free uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var available, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &available, &total, &totalFree); err != nil {
		return 0, 0, &os.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: err}
	}
	if totalFree > total {
		return 0, 0, &os.PathError{Op: "DiskUsage", Path: path, Err: fmt.Errorf("quota-adjusted capacity is smaller than volume free space: %w", errors.ErrUnsupported)}
	}
	return total - totalFree, available, nil
}
