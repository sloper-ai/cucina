// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin

package workeragent

import (
	"context"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sloper-ai/cucina/internal/ports"
)

// NewHost returns the Host of the running OS.
func NewHost(ports.FS, string) Host { return darwinHost{} }

type darwinHost struct{}

func (darwinHost) BootID() (string, error) { return unix.Sysctl("kern.bootsessionuuid") }

func (darwinHost) Uptime() (time.Duration, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0, err
	}
	return time.Since(time.Unix(tv.Unix())), nil
}

func (darwinHost) MemoryBytes() (uint64, error) { return unix.SysctlUint64("hw.memsize") }

// protectKey is a no-op on Unix: the key file is written with mode 0600 in a
// 0700 directory.
func protectKey(string, []string) error { return nil }

func systemShuttingDown(context.Context, ports.Exec) bool { return false }
