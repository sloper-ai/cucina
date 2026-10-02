// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin || linux

package privdrop

import (
	"fmt"
	"os"
	"syscall"
)

// DropAndExec is the trampoline body: set supplementary groups, gid and uid,
// verify the drop is irreversible, then exec the program with the inherited
// environment. It only returns on error.
func DropAndExec(t TrampolineArgs) error {
	groups := make([]int, 0, len(t.Groups)+1)
	groups = append(groups, int(t.GID))
	for _, g := range t.Groups {
		groups = append(groups, int(g))
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("drop-exec: setgroups: %w", err)
	}
	if err := syscall.Setgid(int(t.GID)); err != nil {
		return fmt.Errorf("drop-exec: setgid: %w", err)
	}
	if err := syscall.Setuid(int(t.UID)); err != nil {
		return fmt.Errorf("drop-exec: setuid: %w", err)
	}
	if os.Getuid() != int(t.UID) || os.Geteuid() != int(t.UID) || os.Getgid() != int(t.GID) {
		return fmt.Errorf("drop-exec: credentials not applied (uid=%d euid=%d gid=%d)", os.Getuid(), os.Geteuid(), os.Getgid())
	}
	if syscall.Setuid(0) == nil {
		return fmt.Errorf("drop-exec: privilege drop is reversible; refusing to continue")
	}
	return syscall.Exec(t.Argv[0], t.Argv, os.Environ())
}
