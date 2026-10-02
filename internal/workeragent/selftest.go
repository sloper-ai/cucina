// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"context"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// SelftestReport is the JSON document `cucina-worker-agent selftest` writes
// (image smoke tests assert on OK and on the check names).
type SelftestReport struct {
	OK      bool          `json:"ok"`
	Version string        `json:"version"`
	OS      string        `json:"os"`
	Checks  []CheckResult `json:"checks"`
}

// CheckResult is one selftest check.
type CheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Selftest checks what a worker image must provide before bootstrap can work.
type Selftest struct {
	Paths   Paths
	GOOS    string
	Version string
	IMDS    IMDS
	Storage Storage
	Exec    ports.Exec
	FS      ports.FS
	// BinDir holds bb_worker and bb_runner (default /usr/local/bin, C:\bb\bin).
	BinDir string
	// BuildUser is the unprivileged action user bootstrap requires ("" = none).
	BuildUser string
}

// Run executes every check (each bounded by a short timeout).
func (t *Selftest) Run(ctx context.Context) SelftestReport {
	r := SelftestReport{OK: true, Version: t.Version, OS: t.GOOS}
	add := func(name string, f func(context.Context) (string, error)) {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		detail, err := f(cctx)
		c := CheckResult{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			c.Detail = strings.TrimSpace(detail + " " + err.Error())
			r.OK = false
		}
		r.Checks = append(r.Checks, c)
	}
	add("imds", t.checkIMDS)
	add("disks", t.checkDisks)
	add("time-sync", t.checkTimeSync)
	add("virtual-filesystem", t.checkVirtualFS)
	add("buildbarn-binaries", t.checkBinaries)
	if t.BuildUser != "" {
		add("build-user", t.checkBuildUser)
	}
	return r
}

func (t *Selftest) checkIMDS(ctx context.Context) (string, error) {
	_, doc, _, err := t.IMDS.Identity(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("instance %s (%s) in %s", doc.InstanceID, doc.InstanceType, doc.AvailabilityZone), nil
}

func (t *Selftest) checkDisks(ctx context.Context) (string, error) {
	disks, err := t.Storage.Disks(ctx)
	if err != nil {
		return "", err
	}
	plan := PlanPlacement(PlacementAuto, disks)
	var parts []string
	for _, d := range disks {
		parts = append(parts, fmt.Sprintf("%s:%s:%dGiB%s", d.ID, d.Kind, d.SizeBytes/GiB, map[bool]string{true: ":root"}[d.Root]))
	}
	detail := fmt.Sprintf("auto L1 placement %s", plan.Placement)
	if plan.Disk != nil {
		detail += " on " + plan.Disk.ID
	}
	if len(parts) > 0 {
		detail += "; disks " + strings.Join(parts, ",")
	}
	return detail, nil
}

func (t *Selftest) checkTimeSync(ctx context.Context) (string, error) {
	switch t.GOOS {
	case "linux":
		res, err := t.Exec.Run(ctx, ports.Command{Path: "timedatectl", Args: []string{"show", "-p", "NTPSynchronized", "--value"}})
		if err == nil && strings.TrimSpace(string(res.Stdout)) == "yes" {
			return "NTPSynchronized=yes", nil
		}
		res, err = run(ctx, t.Exec, ports.Command{Path: "chronyc", Args: []string{"-n", "tracking"}})
		if err != nil {
			return "", err
		}
		if strings.Contains(string(res.Stdout), "Leap status     : Normal") {
			return "chrony tracking: leap status normal", nil
		}
		return "", fmt.Errorf("clock not synchronised: %s", firstLine(res.Stdout))
	case "windows":
		res, err := run(ctx, t.Exec, ports.Command{Path: "w32tm", Args: []string{"/query", "/status"}})
		if err != nil {
			return "", err
		}
		return firstLine(res.Stdout), nil
	default:
		return "not checked on " + t.GOOS, nil
	}
}

func (t *Selftest) checkVirtualFS(context.Context) (string, error) {
	var p string
	switch t.GOOS {
	case "linux":
		p = "/dev/fuse"
	case "windows":
		p = `C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`
	default:
		return "NFSv4 client is part of the OS", nil
	}
	ok, err := t.FS.Exists(p)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%s is missing", p)
	}
	return p, nil
}

func (t *Selftest) checkBinaries(context.Context) (string, error) {
	dir, ext := t.BinDir, ""
	if t.GOOS == "windows" {
		ext = ".exe"
	}
	var missing []string
	for _, b := range []string{"bb_worker", "bb_runner"} {
		p := t.Paths.Style.Join(dir, b+ext)
		if ok, err := t.FS.Exists(p); err != nil || !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	return dir, nil
}

func (t *Selftest) checkBuildUser(context.Context) (string, error) {
	u, err := user.Lookup(t.BuildUser)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s uid=%s gid=%s", u.Username, u.Uid, u.Gid), nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
