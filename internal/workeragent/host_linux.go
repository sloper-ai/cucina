// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux

package workeragent

import (
	"context"
	"strings"

	"github.com/sloper-ai/cucina/internal/ports"
)

// NewHost returns the Host of the running OS.
func NewHost(fs ports.FS, sysRoot string) Host { return LinuxHost{FS: fs, SysRoot: sysRoot} }

// protectKey is a no-op on Unix: the key file is written with mode 0600 in a
// 0700 directory.
func protectKey(string, []string) error { return nil }

// systemShuttingDown reports whether systemd is stopping the system.
func systemShuttingDown(ctx context.Context, ex ports.Exec) bool {
	res, err := ex.Run(ctx, ports.Command{Path: "systemctl", Args: []string{"is-system-running"}})
	return err == nil && strings.TrimSpace(string(res.Stdout)) == "stopping"
}
