// SPDX-License-Identifier: FSL-1.1-ALv2

// Package privdrop builds the command lines hostd uses to run `tart` as the
// dedicated non-root user (R-MAC-2, ADR 0701) and implements the tiny
// trampoline that performs the drop.
//
// Virtualization.framework VMs fail when started as root and need the user's
// unlocked login keychain, so hostd (a root LaunchDaemon) runs every tart
// invocation as the auto-logged-in `cucina` user:
//
//	/bin/launchctl asuser <uid> <hostd> drop-exec --uid <uid> --gid <gid> --groups <g,…> -- <tart> <args…>
//
// `launchctl asuser` (root only) moves the child into the user's Mach bootstrap
// namespace and security audit session, where securityd finds the unlocked
// login keychain; the trampoline then sets groups, gid and uid (irreversibly)
// and execs tart. Secrets such as TART_REGISTRY_PASSWORD travel only in the
// environment (argv is visible to every local user). The construction is pure
// so it is unit-tested (R-TEST-6); the root path itself is exercised by MT-001.
package privdrop

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Mode selects the privilege-drop mechanism.
type Mode string

const (
	// ModeNone runs commands as the current user (user mode on a developer Mac).
	ModeNone Mode = "none"
	// ModeAsUser is the default for the root daemon: launchctl asuser + trampoline.
	ModeAsUser Mode = "asuser"
	// ModeSetuid drops credentials directly in the child (Orchard's approach); kept
	// as a fallback for MT-001 comparisons.
	ModeSetuid Mode = "setuid"
)

// ParseMode parses a mode name.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeNone, ModeAsUser, ModeSetuid:
		return Mode(s), nil
	}
	return "", fmt.Errorf("unknown privilege-drop mode %q (none|asuser|setuid)", s)
}

// User is the target account.
type User struct {
	Name   string
	UID    uint32
	GID    uint32
	Groups []uint32
	Home   string
}

// RunAs converts a User into the port type.
func (u User) RunAs() *ports.RunAs { return &ports.RunAs{User: u.Name, UID: u.UID, GID: u.GID} }

// BaseEnv is the explicit environment of commands run as u (nothing inherited
// from root). extra (KEY=VALUE) is appended, e.g. TART_HOME.
func BaseEnv(u User, extra ...string) []string {
	env := []string{
		"HOME=" + u.Home,
		"USER=" + u.Name,
		"LOGNAME=" + u.Name,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=en_US.UTF-8",
	}
	return append(env, extra...)
}

// Spec is the concrete process to start.
type Spec struct {
	Path string
	Args []string
	Env  []string
	// Credential, when non-nil, must be applied by the starter (ModeSetuid).
	Credential *ports.RunAs
	Groups     []uint32
}

// Wrap turns a command with RunAs into the process to start for the given
// mode. self is the absolute path of the hostd binary (the trampoline).
func Wrap(c ports.Command, mode Mode, self string, groups []uint32) (Spec, error) {
	if c.RunAs == nil || mode == ModeNone {
		return Spec{Path: c.Path, Args: c.Args, Env: c.Env}, nil
	}
	if c.RunAs.UID == 0 {
		return Spec{}, errors.New("privdrop: refusing to run as uid 0")
	}
	switch mode {
	case ModeSetuid:
		return Spec{Path: c.Path, Args: c.Args, Env: c.Env, Credential: c.RunAs, Groups: groups}, nil
	case ModeAsUser:
		if self == "" || !strings.HasPrefix(self, "/") {
			return Spec{}, fmt.Errorf("privdrop: trampoline path must be absolute (got %q)", self)
		}
		uid := strconv.FormatUint(uint64(c.RunAs.UID), 10)
		args := []string{"asuser", uid, self, "drop-exec",
			"--uid", uid,
			"--gid", strconv.FormatUint(uint64(c.RunAs.GID), 10),
			"--groups", joinIDs(groups),
			"--", c.Path}
		args = append(args, c.Args...)
		return Spec{Path: "/bin/launchctl", Args: args, Env: c.Env}, nil
	}
	return Spec{}, fmt.Errorf("privdrop: unknown mode %q", mode)
}

func joinIDs(ids []uint32) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(uint64(id), 10))
	}
	return strings.Join(parts, ",")
}

// TrampolineArgs is the parsed `drop-exec` command line.
type TrampolineArgs struct {
	UID, GID uint32
	Groups   []uint32
	Argv     []string // program and arguments
}

// ParseTrampoline parses `drop-exec --uid U --gid G --groups a,b -- prog args…`
// (the arguments after the subcommand name).
func ParseTrampoline(args []string) (TrampolineArgs, error) {
	var t TrampolineArgs
	seenUID, seenGID := false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			t.Argv = args[i+1:]
			if len(t.Argv) == 0 || !strings.HasPrefix(t.Argv[0], "/") {
				return t, errors.New("drop-exec: an absolute program path must follow --")
			}
			if !seenUID || !seenGID {
				return t, errors.New("drop-exec: --uid and --gid are required")
			}
			if t.UID == 0 {
				return t, errors.New("drop-exec: refusing to run as uid 0")
			}
			return t, nil
		case "--uid", "--gid", "--groups":
			if i+1 >= len(args) {
				return t, fmt.Errorf("drop-exec: %s needs a value", args[i])
			}
			v := args[i+1]
			i++
			switch args[i-1] {
			case "--uid":
				n, err := strconv.ParseUint(v, 10, 32)
				if err != nil {
					return t, fmt.Errorf("drop-exec: bad uid %q", v)
				}
				t.UID, seenUID = uint32(n), true
			case "--gid":
				n, err := strconv.ParseUint(v, 10, 32)
				if err != nil {
					return t, fmt.Errorf("drop-exec: bad gid %q", v)
				}
				t.GID, seenGID = uint32(n), true
			case "--groups":
				if v == "" {
					continue
				}
				for _, p := range strings.Split(v, ",") {
					n, err := strconv.ParseUint(p, 10, 32)
					if err != nil {
						return t, fmt.Errorf("drop-exec: bad group %q", p)
					}
					t.Groups = append(t.Groups, uint32(n))
				}
			}
		default:
			return t, fmt.Errorf("drop-exec: unexpected argument %q", args[i])
		}
	}
	return t, errors.New("drop-exec: missing -- program")
}
