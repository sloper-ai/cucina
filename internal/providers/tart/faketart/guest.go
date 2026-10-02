// SPDX-License-Identifier: FSL-1.1-ALv2

package faketart

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/sloper-ai/cucina/internal/ports"
)

// GuestFile is one file in the emulated guest.
type GuestFile struct {
	Data []byte
	Mode int64
	UID  int
	GID  int
}

// Guest emulates the parts of a golden image that hostd touches through
// `tart exec` (docs/dev/hostd.md §1): the image manifest, the console user,
// tar extraction under /private/etc/cucina, the activation script that
// bootstraps the Buildbarn launchd jobs, `launchctl print` and the busy probe.
// State is observable for tests (Files, Running); nothing records call order.
type Guest struct {
	mu sync.Mutex
	// ConsoleUser/UID/GID is the auto-logged-in build user ("" = still at loginwindow).
	ConsoleUser string
	ConsoleUID  int
	ConsoleGID  int
	Files       map[string]GuestFile
	running     map[string]bool
	// Busy makes the busy probe report a running action.
	Busy bool
	// SpotlightOff is set by the activation script.
	SpotlightOff bool
}

// DefaultImageJSON is the image manifest of a v1 golden image.
const DefaultImageJSON = `{"schema":1,"imageVersion":"27.0-27A266a-test","macos":"27.0",` +
	`"xcode":{"version":"27.0","build":"27A266a"},"buildbarn":"20260930T173749Z-1a3be95","buildUser":"admin"}`

// NewGuest returns a guest built from a v1 golden image with build user admin (uid 501).
func NewGuest() *Guest {
	return &Guest{
		ConsoleUser: "admin", ConsoleUID: 501, ConsoleGID: 20,
		Files: map[string]GuestFile{
			"/usr/local/cucina/image.json":                               {Data: []byte(DefaultImageJSON), Mode: 0o644},
			"/usr/local/cucina/launchd/ai.sloper.cucina.bb-worker.plist": {Data: []byte("<plist/>"), Mode: 0o644},
			"/usr/local/cucina/launchd/ai.sloper.cucina.bb-runner.plist": {Data: []byte("<plist/>"), Mode: 0o644},
		},
		running: map[string]bool{},
	}
}

// Boot resets per-boot state: launchd jobs are not started at boot (§1.2) and
// /var/run is empty.
func (g *Guest) Boot() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = map[string]bool{}
	for p := range g.Files {
		if strings.HasPrefix(p, "/var/run/") {
			delete(g.Files, p)
		}
	}
}

// Shutdown stops every job.
func (g *Guest) Shutdown() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = map[string]bool{}
}

// Running reports whether a launchd job (by label) runs.
func (g *Guest) Running(label string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running[label]
}

// File returns a guest file.
func (g *Guest) File(p string) (GuestFile, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.Files[p]
	return f, ok
}

// SetFile writes a guest file (test setup, e.g. a broken image manifest).
func (g *Guest) SetFile(p string, data []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Files[p] = GuestFile{Data: data, Mode: 0o644}
}

// Handle answers one `tart exec` argv.
func (g *Guest) Handle(argv []string, stdin []byte) ports.ExecResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	root := false
	if len(argv) >= 2 && argv[0] == "/usr/bin/sudo" && argv[1] == "-n" {
		root = true
		argv = argv[2:]
		if len(argv) > 0 && argv[0] == "--" {
			argv = argv[1:]
		}
	}
	if len(argv) == 0 {
		return fail(1, "usage")
	}
	switch argv[0] {
	case "/usr/bin/true":
		return ok("")
	case "/bin/cat":
		if len(argv) != 2 {
			return fail(1, "usage: cat file")
		}
		f, found := g.Files[argv[1]]
		if !found {
			return fail(1, "cat: %s: No such file or directory", argv[1])
		}
		return ok(string(f.Data))
	case "/usr/bin/stat":
		if len(argv) == 4 && argv[1] == "-f" && argv[3] == "/dev/console" {
			if g.ConsoleUser == "" {
				return ok("root 0 0\n")
			}
			return ok(fmt.Sprintf("%s %d %d\n", g.ConsoleUser, g.ConsoleUID, g.ConsoleGID))
		}
		return fail(1, "stat: unsupported")
	case "/usr/bin/tar":
		if !root {
			return fail(1, "tar: Permission denied")
		}
		return g.extract(argv, stdin)
	case "/bin/sh":
		// Read-only log scripts of hostd's diagnostics (positional args after "sh").
		if len(argv) == 6 && argv[1] == "-c" && argv[3] == "sh" {
			return g.logScript(argv[2], argv[4], argv[5])
		}
		if !root {
			return fail(1, "sh: Permission denied")
		}
		if len(argv) != 3 || argv[1] != "-c" {
			return fail(2, "sh: unsupported")
		}
		if strings.Contains(argv[2], "pgrep -P") {
			if g.Busy {
				return ok("")
			}
			return ports.ExecResult{ExitCode: 1}
		}
		return g.activate(argv[2])
	case "/bin/launchctl":
		if len(argv) == 3 && argv[1] == "print" {
			label := argv[2][strings.LastIndex(argv[2], "/")+1:]
			if g.running[label] {
				return ok(fmt.Sprintf("%s = {\n\tstate = running\n}\n", argv[2]))
			}
			return fail(113, "Could not find service %q in domain for system", label)
		}
		return fail(1, "launchctl: unsupported")
	case "/usr/bin/pgrep":
		if g.Busy {
			return ok("4242\n")
		}
		return ports.ExecResult{ExitCode: 1}
	}
	return fail(127, "%s: command not found", argv[0])
}

// logScript emulates `stat -f %z f && tail -n N f` and `tail -c +OFF f | head -c 262144`.
func (g *Guest) logScript(script, file, arg string) ports.ExecResult {
	f, found := g.Files[file]
	if !found {
		return fail(1, "stat: %s: stat: No such file or directory", file)
	}
	n, err := strconv.Atoi(arg)
	if err != nil {
		return fail(2, "sh: bad argument")
	}
	switch {
	case strings.Contains(script, "stat -f %z"):
		lines := strings.SplitAfter(string(f.Data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		return ok(fmt.Sprintf("%d\n%s", len(f.Data), strings.Join(lines, "")))
	case strings.Contains(script, "tail -c +"):
		off := n - 1
		if off >= len(f.Data) {
			return ok("")
		}
		out := f.Data[off:]
		if len(out) > 262144 {
			out = out[:262144]
		}
		return ok(string(out))
	}
	return fail(2, "sh: unsupported script")
}

// AppendFile appends to a guest file (tests: a growing log).
func (g *Guest) AppendFile(p string, data []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.Files[p]
	f.Data = append(f.Data, data...)
	if f.Mode == 0 {
		f.Mode = 0o644
	}
	g.Files[p] = f
}

func (g *Guest) extract(argv []string, stdin []byte) ports.ExecResult {
	dir := ""
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "-C" {
			dir = argv[i+1]
		}
	}
	if dir == "" {
		return fail(1, "tar: -C required by the emulator")
	}
	tr := tar.NewReader(bytes.NewReader(stdin))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(1, "tar: %v", err)
		}
		if strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, "..") {
			return fail(1, "tar: refusing unsafe path %q", h.Name)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return fail(1, "tar: %v", err)
		}
		g.Files[path.Join(dir, h.Name)] = GuestFile{Data: data, Mode: h.Mode, UID: h.Uid, GID: h.Gid}
	}
	return ok("")
}

// activate interprets the activation script of docs/dev/hostd.md §1.2: every
// `launchctl bootstrap <domain> <plist>` line starts the job named by the
// plist, provided the files the job needs exist.
func (g *Guest) activate(script string) ports.ExecResult {
	if strings.Contains(script, "mdutil -a -i off") {
		g.SpotlightOff = true
	}
	for _, line := range strings.Split(script, "\n") {
		fields := strings.Fields(line)
		for i := 0; i+3 < len(fields); i++ {
			if fields[i] != "launchctl" || fields[i+1] != "bootstrap" {
				continue
			}
			domain, plist := fields[i+2], strings.TrimSuffix(fields[i+3], ";")
			if domain != "system" && domain != "gui/"+strconv.Itoa(g.ConsoleUID) {
				return fail(5, "Bootstrap failed: 5: Input/output error (domain %s)", domain)
			}
			if _, found := g.Files[plist]; !found {
				return fail(5, "Bootstrap failed: 5: Input/output error (%s missing)", plist)
			}
			label := strings.TrimSuffix(path.Base(plist), ".plist")
			if label == "ai.sloper.cucina.bb-worker" {
				for _, need := range []string{"/private/etc/cucina/bb/worker.json", "/private/etc/cucina/pki/worker.key",
					"/private/etc/cucina/pki/worker.crt", "/private/etc/cucina/pki/ca.crt"} {
					if _, found := g.Files[need]; !found {
						return fail(78, "bb_worker: %s: no such file (job exited)", need)
					}
				}
			}
			if label == "ai.sloper.cucina.bb-runner" {
				if _, found := g.Files["/private/etc/cucina/bb/runner.json"]; !found {
					return fail(78, "bb_runner: runner.json missing (job exited)")
				}
			}
			g.running[label] = true
		}
	}
	return ok("")
}
