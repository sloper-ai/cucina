// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Storage discovers disks and makes the volume chosen by PlanPlacement usable.
type Storage interface {
	Disks(ctx context.Context) ([]Disk, error)
	// Prepare returns the path where plan.Disk's file system is mounted and the
	// free bytes there. It reuses what is already mounted — the disk itself, or
	// whatever the image mounted at mountPoint (for example a RAID 0 of several
	// instance-store devices) — and otherwise formats a blank device and mounts
	// it at mountPoint.
	Prepare(ctx context.Context, plan PlacementPlan, mountPoint string) (path string, freeBytes uint64, err error)
}

// run executes a command and turns a non-zero exit status into an error that
// carries (bounded) stderr.
func run(ctx context.Context, ex ports.Exec, c ports.Command) (ports.ExecResult, error) {
	res, err := ex.Run(ctx, c)
	if err != nil {
		return res, fmt.Errorf("%s: %w", c.Path, err)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(string(res.Stderr))
		if len(msg) > 512 {
			msg = msg[:512] + "…"
		}
		return res, fmt.Errorf("%s %s: exit %d: %s", c.Path, strings.Join(c.Args, " "), res.ExitCode, msg)
	}
	return res, nil
}

// LinuxStorage formats (ext4 without a journal: the cache dies with the
// instance) and mounts the chosen device. The images' format unit usually did
// it already for instance store (R-POOL-4: "an ExecStartPre … formats instance
// store when present"); the agent covers EBS data volumes and images without
// that unit.
type LinuxStorage struct {
	Exec    ports.Exec
	FS      ports.FS
	SysRoot string
}

// Disks implements Storage.
func (s LinuxStorage) Disks(context.Context) ([]Disk, error) { return LinuxDisks(s.FS, s.SysRoot) }

// Prepare implements Storage.
func (s LinuxStorage) Prepare(ctx context.Context, plan PlacementPlan, mountPoint string) (string, uint64, error) {
	d := plan.Disk
	if d == nil {
		return "", 0, errors.New("no device to prepare")
	}
	target := ""
	switch {
	case len(d.MountPoints) > 0:
		target = d.MountPoints[0]
	default:
		mounted, err := linuxIsMountPoint(s.FS, s.SysRoot, mountPoint)
		if err != nil {
			return "", 0, err
		}
		if mounted {
			target = mountPoint
			break
		}
		if err := s.FS.MkdirAll(mountPoint, 0o755); err != nil {
			return "", 0, err
		}
		res, err := s.Exec.Run(ctx, ports.Command{Path: "blkid", Args: []string{"-p", "-o", "value", "-s", "TYPE", d.ID}})
		if err != nil {
			return "", 0, fmt.Errorf("blkid: %w", err)
		}
		fsType := strings.TrimSpace(string(res.Stdout))
		switch {
		case res.ExitCode == 2 || (res.ExitCode == 0 && fsType == ""): // no signature: a blank device
			if _, err := run(ctx, s.Exec, ports.Command{Path: "mkfs.ext4", Args: []string{
				"-F", "-q", "-m", "0", "-O", "^has_journal",
				"-E", "nodiscard,lazy_itable_init=1,lazy_journal_init=1",
				"-L", "cucina-cache", d.ID,
			}}); err != nil {
				return "", 0, err
			}
		case res.ExitCode != 0:
			return "", 0, fmt.Errorf("blkid %s: exit %d", d.ID, res.ExitCode)
		case fsType == "linux_raid_member":
			return "", 0, fmt.Errorf("%s is a RAID member but no array is mounted at %s", d.ID, mountPoint)
		}
		if _, err := run(ctx, s.Exec, ports.Command{Path: "mount", Args: []string{"-o", "noatime", d.ID, mountPoint}}); err != nil {
			return "", 0, err
		}
		target = mountPoint
	}
	_, free, err := s.FS.DiskUsage(target)
	if err != nil {
		return "", 0, fmt.Errorf("disk usage of %s: %w", target, err)
	}
	return target, free, nil
}

// linuxIsMountPoint reports whether p is a mount point (/proc/self/mountinfo).
func linuxIsMountPoint(fs ports.FS, sysRoot, p string) (bool, error) {
	mi := path.Join(sysRoot, "proc/self/mountinfo")
	b, err := fs.ReadFile(mi)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", mi, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 5 && unescapeMount(f[4]) == p {
			return true, nil
		}
	}
	return false, nil
}

// WindowsStorage initialises, formats (NTFS, quick) and mounts the chosen disk
// through PowerShell's Storage module when the image's boot task has not
// (cucina-format-data-volume.ps1 mounts instance store as D:). The Dev Drive /
// Defender trade-off belongs to the image; the agent only exposes the roots.
type WindowsStorage struct {
	Exec ports.Exec
	FS   ports.FS
}

const powershell = "powershell.exe"

func psCommand(script string) ports.Command {
	return ports.Command{
		Path:  powershell,
		Args:  []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", "-"},
		Stdin: []byte(script),
	}
}

const windowsDisksScript = `$ErrorActionPreference = 'Stop'
$disks = @(Get-Disk | ForEach-Object {
  $n = $_.Number
  [pscustomobject]@{
    Number = $n; FriendlyName = $_.FriendlyName; Model = $_.Model; Size = [uint64]$_.Size
    PartitionStyle = "$($_.PartitionStyle)"; IsBoot = [bool]$_.IsBoot; IsSystem = [bool]$_.IsSystem
    AccessPaths = @(Get-Partition -DiskNumber $n -ErrorAction SilentlyContinue | ForEach-Object { $_.AccessPaths } | Where-Object { $_ -and -not $_.StartsWith('\\?\') })
  }
})
ConvertTo-Json -Compress -Depth 3 -InputObject $disks
`

// Disks implements Storage.
func (s WindowsStorage) Disks(ctx context.Context) ([]Disk, error) {
	res, err := run(ctx, s.Exec, psCommand(windowsDisksScript))
	if err != nil {
		return nil, err
	}
	return ParseWindowsDisks(res.Stdout)
}

// ParseWindowsDisks parses the JSON emitted by the Get-Disk probe script.
func ParseWindowsDisks(b []byte) ([]Disk, error) {
	var raw []struct {
		Number         int
		FriendlyName   string
		Model          string
		Size           uint64
		PartitionStyle string
		IsBoot         bool
		IsSystem       bool
		AccessPaths    []string
	}
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' { // a single object if PowerShell unrolled the array
		b = append(append([]byte{'['}, b...), ']')
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse Get-Disk output: %w", err)
	}
	disks := make([]Disk, 0, len(raw))
	for _, r := range raw {
		name := r.Model + " " + r.FriendlyName
		d := Disk{
			ID: strconv.Itoa(r.Number), Model: strings.TrimSpace(r.Model), SizeBytes: r.Size,
			Root: r.IsBoot || r.IsSystem, Kind: DiskOther,
		}
		switch {
		case strings.Contains(name, "Instance Stor") || strings.Contains(name, "EC2 NVMe"):
			d.Kind = DiskInstanceStore
		case strings.Contains(name, "Elastic Block Store") || strings.Contains(name, "Elastic B"):
			d.Kind = DiskEBS
		}
		for _, ap := range r.AccessPaths {
			if ap = strings.TrimSuffix(ap, `\`); ap != "" {
				d.MountPoints = append(d.MountPoints, ap)
			}
		}
		// An initialised disk stays eligible only with exactly one access path:
		// the volume the image's boot task (or an earlier bootstrap) created.
		// Multi-volume or unmounted partitioned disks are left alone.
		d.Partitioned = r.PartitionStyle != "RAW" && (len(d.MountPoints) == 0 || !allEqualFold(d.MountPoints))
		disks = append(disks, d)
	}
	return disks, nil
}

func allEqualFold(ps []string) bool {
	for _, p := range ps[1:] {
		if !strings.EqualFold(p, ps[0]) {
			return false
		}
	}
	return true
}

func windowsPrepareScript(disk, mountPoint string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	return `$ErrorActionPreference = 'Stop'
$n = ` + disk + `
$path = ` + q(strings.TrimSuffix(mountPoint, `\`)+`\`) + `
$d = Get-Disk -Number $n
if ($d.IsOffline) { Set-Disk -Number $n -IsOffline $false }
if ($d.IsReadOnly) { Set-Disk -Number $n -IsReadOnly $false }
if ("$($d.PartitionStyle)" -eq 'RAW') { Initialize-Disk -Number $n -PartitionStyle GPT | Out-Null }
$p = Get-Partition -DiskNumber $n | Where-Object { "$($_.Type)" -eq 'Basic' } | Select-Object -First 1
if (-not $p) { $p = New-Partition -DiskNumber $n -UseMaximumSize }
if (-not ($p | Get-Volume).FileSystem) {
  $p | Format-Volume -FileSystem NTFS -NewFileSystemLabel 'cucina-cache' -AllocationUnitSize 65536 -Confirm:$false -Force | Out-Null
}
New-Item -ItemType Directory -Force -Path $path | Out-Null
$p | Add-PartitionAccessPath -AccessPath $path
`
}

// Prepare implements Storage.
func (s WindowsStorage) Prepare(ctx context.Context, plan PlacementPlan, mountPoint string) (string, uint64, error) {
	d := plan.Disk
	if d == nil {
		return "", 0, errors.New("no device to prepare")
	}
	target := mountPoint
	if len(d.MountPoints) > 0 {
		target = d.MountPoints[0]
		if len(target) == 2 && target[1] == ':' {
			target += `\` // a drive letter
		}
	} else {
		if _, err := strconv.Atoi(d.ID); err != nil {
			return "", 0, fmt.Errorf("windows disk id %q is not a disk number", d.ID)
		}
		if _, err := run(ctx, s.Exec, psCommand(windowsPrepareScript(d.ID, mountPoint))); err != nil {
			return "", 0, err
		}
	}
	_, free, err := s.FS.DiskUsage(target)
	if err != nil {
		return "", 0, fmt.Errorf("disk usage of %s: %w", target, err)
	}
	return target, free, nil
}

// DirStorage has no devices: L1 lives below the state root (macOS VMs,
// vm-disk/memory placements, unsupported OSes).
type DirStorage struct{}

// Disks implements Storage.
func (DirStorage) Disks(context.Context) ([]Disk, error) { return nil, nil }

// Prepare implements Storage.
func (DirStorage) Prepare(context.Context, PlacementPlan, string) (string, uint64, error) {
	return "", 0, errors.New("directory storage cannot prepare a device")
}
