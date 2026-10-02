// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package workeragent

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/sloper-ai/cucina/internal/ports"
)

// NewHost returns the Host of the running OS.
func NewHost(ports.FS, string) Host { return windowsHost{} }

type windowsHost struct{}

// BootID is empty on Windows; State compares the boot time instead.
func (windowsHost) BootID() (string, error) { return "", nil }

func (windowsHost) Uptime() (time.Duration, error) { return windows.DurationSinceBoot(), nil }

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func (windowsHost) MemoryBytes() (uint64, error) {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return ms.TotalPhys, nil
}

// protectKey replaces the key file's inherited DACL (C:\ProgramData grants
// Users read access by inheritance) with a protected one: full control for
// SYSTEM and Administrators, read for each extra reader (account name or
// SID, for a low-privilege worker service account).
func protectKey(path string, readers []string) error {
	sddl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	for _, r := range readers {
		sid, err := windows.StringToSid(r)
		if err != nil {
			if sid, _, _, err = windows.LookupSID("", r); err != nil {
				return fmt.Errorf("key reader %q: %w", r, err)
			}
		}
		sddl += "(A;;FR;;;" + sid.String() + ")"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(strings.TrimSpace(path), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

var procGetSystemMetrics = windows.NewLazySystemDLL("user32.dll").NewProc("GetSystemMetrics")

const smShuttingDown = 0x2000

// systemShuttingDown reports whether Windows is shutting down (SM_SHUTTINGDOWN).
func systemShuttingDown(context.Context, ports.Exec) bool {
	if procGetSystemMetrics.Find() != nil {
		return false
	}
	r, _, _ := procGetSystemMetrics.Call(smShuttingDown)
	return r != 0
}
