// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode/utf16"

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

// psCommand runs a script with Windows PowerShell 5.1 (always present on
// Windows Server). The script travels as -EncodedCommand (base64 of UTF-16LE):
// fed through stdin with `-Command -`, PS 5.1 reads it like an interactive
// console and silently drops multi-line statements.
func psCommand(script string) ports.Command {
	return ports.Command{
		Path: powershell,
		Args: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-EncodedCommand", EncodePowerShell(script)},
	}
}

// EncodePowerShell returns the -EncodedCommand form of a script: base64 of its
// UTF-16LE encoding.
func EncodePowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// WindowsDisksScript lists the disks as compact JSON. Windows PowerShell 5.1
// safe: access paths are joined into one '|'-separated string ('|' cannot occur
// in a path), because 5.1 may serialise nested arrays as {"value":…,"Count":…}.
const WindowsDisksScript = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$disks = @(Get-Disk | ForEach-Object {
  $n = $_.Number
  $paths = @(Get-Partition -DiskNumber $n -ErrorAction SilentlyContinue | ForEach-Object { $_.AccessPaths } | Where-Object { $_ -and -not $_.StartsWith('\\?\') })
  New-Object -TypeName PSObject -Property @{
    Number = [int]$n; FriendlyName = [string]$_.FriendlyName; Model = [string]$_.Model; Size = [uint64]$_.Size
    PartitionStyle = [string]$_.PartitionStyle; IsBoot = [bool]$_.IsBoot; IsSystem = [bool]$_.IsSystem
    AccessPaths = [string]($paths -join '|')
  }
})
ConvertTo-Json -Compress -Depth 2 -InputObject $disks
`

// Disks implements Storage.
func (s WindowsStorage) Disks(ctx context.Context) ([]Disk, error) {
	res, err := run(ctx, s.Exec, psCommand(WindowsDisksScript))
	if err != nil {
		return nil, err
	}
	return ParseWindowsDisks(res.Stdout)
}

// psStrings accepts the shapes PowerShell gives a list of strings: one
// '|'-separated string, an array, null, or 5.1's {"value":[…],"Count":n}.
type psStrings []string

func (p *psStrings) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*p = nil
		for _, s := range strings.Split(one, "|") {
			if s != "" {
				*p = append(*p, s)
			}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err == nil {
		*p = many
		return nil
	}
	var wrapped struct{ Value []string }
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return err
	}
	*p = wrapped.Value
	return nil
}

// ParseWindowsDisks parses the JSON emitted by WindowsDisksScript: an array,
// a single object (PowerShell unrolled a one-element array) or 5.1's
// {"value":[…],"Count":n} wrapper; a UTF-8 byte order mark is ignored.
func ParseWindowsDisks(b []byte) ([]Disk, error) {
	type rawDisk struct {
		Number         int
		FriendlyName   string
		Model          string
		Size           uint64
		PartitionStyle string
		IsBoot         bool
		IsSystem       bool
		AccessPaths    psStrings
	}
	b = bytes.TrimSpace(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")))
	var raw []rawDisk
	switch {
	case len(b) == 0:
		return nil, errors.New("parse Get-Disk output: empty output")
	case b[0] == '[':
		if err := json.Unmarshal(b, &raw); err != nil {
			return nil, fmt.Errorf("parse Get-Disk output: %w", err)
		}
	default:
		var probe struct {
			Value *[]rawDisk `json:"value"`
		}
		if err := json.Unmarshal(b, &probe); err == nil && probe.Value != nil {
			raw = *probe.Value
			break
		}
		var one rawDisk
		if err := json.Unmarshal(b, &one); err != nil {
			return nil, fmt.Errorf("parse Get-Disk output: %w", err)
		}
		raw = []rawDisk{one}
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
		// An initialised disk is reused only when it is mounted (the image's
		// boot task or an earlier bootstrap formatted it); an initialised but
		// unmounted disk is left alone.
		d.Partitioned = r.PartitionStyle != "RAW" && len(d.MountPoints) == 0
		disks = append(disks, d)
	}
	return disks, nil
}

// WindowsPrepareScript initialises (GPT), partitions, formats (NTFS quick, 64 KiB
// clusters) and mounts disk at mountPoint; it is idempotent and Windows
// PowerShell 5.1 safe.
func WindowsPrepareScript(disk, mountPoint string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	return `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$n = ` + disk + `
$path = ` + q(strings.TrimSuffix(mountPoint, `\`)+`\`) + `
$d = Get-Disk -Number $n
if ($d.IsOffline) { Set-Disk -Number $n -IsOffline $false }
if ($d.IsReadOnly) { Set-Disk -Number $n -IsReadOnly $false }
if ([string]$d.PartitionStyle -eq 'RAW') { Initialize-Disk -Number $n -PartitionStyle GPT | Out-Null }
$p = Get-Partition -DiskNumber $n | Where-Object { [string]$_.Type -eq 'Basic' } | Select-Object -First 1
if (-not $p) { $p = New-Partition -DiskNumber $n -UseMaximumSize }
if (-not ($p | Get-Volume).FileSystem) {
  $p | Format-Volume -FileSystem NTFS -NewFileSystemLabel 'cucina-cache' -AllocationUnitSize 65536 -Confirm:$false -Force | Out-Null
}
New-Item -ItemType Directory -Force -Path $path | Out-Null
if (@(Get-Partition -DiskNumber $n | ForEach-Object { $_.AccessPaths }) -notcontains $path) {
  $p | Add-PartitionAccessPath -AccessPath $path
}
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
		if _, err := run(ctx, s.Exec, psCommand(WindowsPrepareScript(d.ID, mountPoint))); err != nil {
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
