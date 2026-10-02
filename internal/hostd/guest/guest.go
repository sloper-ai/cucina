// SPDX-License-Identifier: FSL-1.1-ALv2

// Package guest builds the commands and payloads of the in-VM contract
// (docs/dev/hostd.md §1): readiness probe, image manifest, console user,
// the per-boot configuration bundle (a tar stream sent over stdin, never argv)
// and the activation script that bootstraps the Buildbarn launchd jobs. It is
// pure; the VM manager runs the commands through ports.VMRuntime.GuestExec.
package guest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Paths and labels inside the VM (the contract with the golden image).
const (
	ManifestPath   = "/usr/local/cucina/image.json"
	ConfigRoot     = "/private/etc/cucina" // /etc/cucina
	WorkerLabel    = "ai.sloper.cucina.bb-worker"
	RunnerLabel    = "ai.sloper.cucina.bb-runner"
	WorkerPlist    = "/usr/local/cucina/launchd/" + WorkerLabel + ".plist"
	RunnerPlist    = "/usr/local/cucina/launchd/" + RunnerLabel + ".plist"
	RunnerSocket   = "/var/run/cucina/runner.sock"
	BuildRoot      = "/Volumes/cucina/build"
	CacheRoot      = "/Volumes/cucina/cache"
	TempRoot       = "/Volumes/cucina/tmp"
	ConfigDir      = "/etc/cucina/bb"
	PKIDir         = "/etc/cucina/pki"
	ManifestSchema = 1
)

// Manifest is /usr/local/cucina/image.json (schema 1).
type Manifest struct {
	Schema       int    `json:"schema"`
	ImageVersion string `json:"imageVersion"`
	MacOS        string `json:"macos"`
	Xcode        struct {
		Version              string `json:"version"`
		Build                string `json:"build"`
		DeveloperDir         string `json:"developerDir"`
		XcodeVersionOverride string `json:"xcodeVersionOverride"`
	} `json:"xcode"`
	Buildbarn string `json:"buildbarn"`
	BuildUser string `json:"buildUser"`
	// WorkerUser runs bb_worker (the worker plist's UserName); empty = BuildUser.
	// "root" is required for NFSv4 virtual build directories.
	WorkerUser string `json:"workerUser"`
}

// Worker returns the bb_worker user name.
func (m Manifest) Worker() string {
	if m.WorkerUser == "" {
		return m.BuildUser
	}
	return m.WorkerUser
}

// ParseManifest parses and validates the image manifest.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("image manifest %s: %w", ManifestPath, err)
	}
	if m.Schema != ManifestSchema {
		return m, fmt.Errorf("image manifest %s: unsupported schema %d (want %d)", ManifestPath, m.Schema, ManifestSchema)
	}
	if m.BuildUser == "" || m.BuildUser == "root" || m.ImageVersion == "" {
		return m, fmt.Errorf("image manifest %s: buildUser (non-root) and imageVersion are required", ManifestPath)
	}
	return m, nil
}

// Console is the owner of /dev/console (the auto-logged-in user).
type Console struct {
	User string
	UID  int
	GID  int
}

// ParseConsole parses `stat -f '%Su %u %g' /dev/console`.
func ParseConsole(out []byte) (Console, error) {
	f := strings.Fields(string(out))
	if len(f) != 3 {
		return Console{}, fmt.Errorf("unexpected console owner %q", strings.TrimSpace(string(out)))
	}
	uid, err1 := strconv.Atoi(f[1])
	gid, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return Console{}, fmt.Errorf("unexpected console owner %q", strings.TrimSpace(string(out)))
	}
	return Console{User: f[0], UID: uid, GID: gid}, nil
}

func sudo(args ...string) ports.Command {
	return ports.Command{Path: "/usr/bin/sudo", Args: append([]string{"-n", "--"}, args...)}
}

// ProbeCmd checks that the guest agent answers.
func ProbeCmd() ports.Command { return ports.Command{Path: "/usr/bin/true"} }

// ManifestCmd reads the image manifest.
func ManifestCmd() ports.Command {
	return ports.Command{Path: "/bin/cat", Args: []string{ManifestPath}}
}

// ConsoleCmd reads the console owner.
func ConsoleCmd() ports.Command {
	return ports.Command{Path: "/usr/bin/stat", Args: []string{"-f", "%Su %u %g", "/dev/console"}}
}

// File is one file of the configuration bundle (path relative to ConfigRoot).
type File struct {
	Path string
	Data []byte
	Mode int64
	UID  int
	GID  int
}

// Bundle is the per-boot configuration pushed into the VM.
type Bundle struct {
	WorkerJSON []byte
	RunnerJSON []byte
	VMJSON     []byte
	KeyPEM     []byte
	CertPEM    []byte
	CAPEM      []byte
}

// Files lays the bundle out per §1.2 step 5: configs root-owned and world
// readable, the PKI directory and files owned by the bb_worker user (worker).
func (b Bundle) Files(worker Console) []File {
	return []File{
		{Path: "bb/worker.json", Data: b.WorkerJSON, Mode: 0o644},
		{Path: "bb/runner.json", Data: b.RunnerJSON, Mode: 0o644},
		{Path: "vm.json", Data: b.VMJSON, Mode: 0o644},
		{Path: "pki/worker.key", Data: b.KeyPEM, Mode: 0o600, UID: worker.UID, GID: worker.GID},
		{Path: "pki/worker.crt", Data: b.CertPEM, Mode: 0o644, UID: worker.UID, GID: worker.GID},
		{Path: "pki/ca.crt", Data: b.CAPEM, Mode: 0o644, UID: worker.UID, GID: worker.GID},
	}
}

// IDCmd prints a user's uid and gid ("uid=600(builder) gid=20(staff) …").
func IDCmd(user string) ports.Command {
	return ports.Command{Path: "/usr/bin/id", Args: []string{user}}
}

var idRe = regexp.MustCompile(`uid=(\d+)\(([^)]*)\) gid=(\d+)`)

// ParseID parses `id <user>`.
func ParseID(out []byte) (Console, error) {
	m := idRe.FindSubmatch(out)
	if m == nil {
		return Console{}, fmt.Errorf("unexpected id output %q", strings.TrimSpace(string(out)))
	}
	uid, _ := strconv.Atoi(string(m[1]))
	gid, _ := strconv.Atoi(string(m[3]))
	return Console{User: string(m[2]), UID: uid, GID: gid}, nil
}

// Tar builds the tar stream (sorted, fixed timestamps: reproducible).
func Tar(files []File, mtime time.Time) ([]byte, error) {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, f := range sorted {
		if strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") || f.Path == "" {
			return nil, fmt.Errorf("bundle path %q must be relative", f.Path)
		}
		if i := strings.LastIndex(f.Path, "/"); i > 0 && !dirs[f.Path[:i]] {
			dirs[f.Path[:i]] = true
			mode := int64(0o755)
			uid, gid := 0, 0
			if f.Path[:i] == "pki" {
				mode, uid, gid = 0o700, f.UID, f.GID
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: f.Path[:i] + "/", Mode: mode,
				Uid: uid, Gid: gid, ModTime: mtime}); err != nil {
				return nil, err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.Path, Mode: f.Mode, Size: int64(len(f.Data)),
			Uid: f.UID, Gid: f.GID, ModTime: mtime}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ExtractCmd pushes a tar stream (stdin) into ConfigRoot as root.
func ExtractCmd(tarStream []byte) ports.Command {
	c := sudo("/usr/bin/tar", "-x", "-p", "-f", "-", "-C", ConfigRoot)
	c.Stdin = tarStream
	return c
}

// Dir is a directory the activation script creates (bbconfig's WorkerPlan.Directories).
type Dir struct {
	Path           string
	Mode           uint32
	BuildUserOwned bool
}

var safePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// ActivationScript is the §1.2 step 6 script: create the planned directories
// with their owners (build user, or the bb_worker user) and modes, then (re)bootstrap the runner LaunchAgent in the
// build user's GUI session and the worker LaunchDaemon. bootout is
// asynchronous, so each bootstrap is retried briefly.
func ActivationScript(c, worker Console, dirs []Dir) (string, error) {
	uid, gid := strconv.Itoa(c.UID), strconv.Itoa(c.GID)
	wowner := strconv.Itoa(worker.UID) + ":" + strconv.Itoa(worker.GID)
	lines := []string{
		"set -eu",
		"mdutil -a -i off >/dev/null 2>&1 || true",
	}
	for _, d := range dirs {
		if !safePath.MatchString(d.Path) || strings.Contains(d.Path, "..") {
			return "", fmt.Errorf("unsafe directory path %q", d.Path)
		}
		owner := wowner
		if d.BuildUserOwned {
			owner = uid + ":" + gid
		}
		lines = append(lines, fmt.Sprintf("mkdir -p '%s' && chown %s '%s' && chmod %o '%s'", d.Path, owner, d.Path, d.Mode&0o7777, d.Path))
	}
	// The parent belongs to root even when both daemons are unprivileged:
	// actions must never replace paths that launchd subsequently opens as root.
	// Lock the parent first; reject planted links/non-files instead of opening,
	// truncating or changing ownership of their targets (including hardlinks).
	lines = append(lines,
		`[ ! -L /var/log/cucina ] || { printf '%s\n' 'unsafe log directory' >&2; exit 1; }`,
		"install -d -o 0 -g 0 -m 0755 /var/log/cucina",
		"chmod -N /var/log/cucina", // ownership/mode do not remove macOS ACL grants
		`prepare_log() {
  if [ -L "$2" ] || { [ -e "$2" ] && [ ! -f "$2" ]; }; then
    printf 'unsafe log file: %s\n' "$2" >&2
    return 1
  fi
  if [ -e "$2" ]; then
    [ "$(stat -f %l "$2")" = 1 ] || { printf 'hardlinked log file: %s\n' "$2" >&2; return 1; }
  else
    (umask 077; set -C; : > "$2")
  fi
  chmod -N "$2"
  chown "$1" "$2"
  chmod 0644 "$2"
}`,
		"prepare_log "+wowner+" /var/log/cucina/bb_worker.log",
		"prepare_log "+uid+":"+gid+" /var/log/cucina/bb_runner.log",
		"launchctl bootout gui/"+uid+"/"+RunnerLabel+" 2>/dev/null || true",
		"launchctl bootout system/"+WorkerLabel+" 2>/dev/null || true",
		"n=0; until launchctl bootstrap gui/"+uid+" "+RunnerPlist+"; do n=$((n+1)); [ $n -lt 10 ] || exit 1; sleep 1; done",
		"n=0; until launchctl bootstrap system "+WorkerPlist+"; do n=$((n+1)); [ $n -lt 10 ] || exit 1; sleep 1; done",
	)
	return strings.Join(lines, "\n") + "\n", nil
}

// ActivateCmd runs the activation script as root.
func ActivateCmd(c, worker Console, dirs []Dir) (ports.Command, error) {
	script, err := ActivationScript(c, worker, dirs)
	if err != nil {
		return ports.Command{}, err
	}
	return sudo("/bin/sh", "-c", script), nil
}

// WorkerStatusCmd prints the worker job; ParseRunning interprets it.
func WorkerStatusCmd() ports.Command { return sudo("/bin/launchctl", "print", "system/"+WorkerLabel) }

// ParseRunning reports whether `launchctl print` shows a running job.
func ParseRunning(res ports.ExecResult) bool {
	return res.ExitCode == 0 && strings.Contains(string(res.Stdout), "state = running")
}

// BusyProbeCmd reports (exit 0) whether bb_runner has a child process, i.e. an
// action is executing (dead-man idle guard for long actions).
func BusyProbeCmd() ports.Command {
	return sudo("/bin/sh", "-c", `p=$(pgrep -x bb_runner) && pgrep -P "$p" >/dev/null`)
}

// ErrNotLoggedIn means the build user's GUI session does not exist yet.
var ErrNotLoggedIn = errors.New("build user is not logged in yet")
