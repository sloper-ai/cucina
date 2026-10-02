// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// LinuxHost reads boot identity, uptime and memory from procfs below SysRoot.
// It is plain file parsing, so it compiles (and is tested) on every OS.
type LinuxHost struct {
	FS      ports.FS
	SysRoot string
}

var _ Host = LinuxHost{}

// BootID implements Host (/proc/sys/kernel/random/boot_id).
func (h LinuxHost) BootID() (string, error) {
	b, err := h.FS.ReadFile(path.Join(h.SysRoot, "proc/sys/kernel/random/boot_id"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Uptime implements Host (/proc/uptime, CLOCK_BOOTTIME: immune to wall-clock steps).
func (h LinuxHost) Uptime() (time.Duration, error) {
	b, err := h.FS.ReadFile(path.Join(h.SysRoot, "proc/uptime"))
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("/proc/uptime is empty")
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/uptime: %w", err)
	}
	return time.Duration(sec * float64(time.Second)), nil
}

// MemoryBytes implements Host (MemTotal from /proc/meminfo).
func (h LinuxHost) MemoryBytes() (uint64, error) {
	b, err := h.FS.ReadFile(path.Join(h.SysRoot, "proc/meminfo"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) >= 1 {
				kb, err := strconv.ParseUint(f[0], 10, 64)
				if err != nil {
					return 0, fmt.Errorf("/proc/meminfo: %w", err)
				}
				return kb * 1024, nil
			}
		}
	}
	return 0, errors.New("/proc/meminfo has no MemTotal")
}

// chown changes the owner of a directory bbconfig wants owned by the build
// user (macOS bb_runner socket directory). Not used on Windows.
func chown(path string, uid, gid int) error {
	if err := os.Chown(path, uid, gid); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return err
	}
	return nil
}
