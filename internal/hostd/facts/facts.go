// SPDX-License-Identifier: FSL-1.1-ALv2

// Package facts gathers host facts (R-MAC-5: model, chip, cores, RAM, disk,
// macOS, Tart and agent versions, FileVault) and the hardware serial number
// (the enrollment admission key, R-SEC-3) through ports.Exec. Parsers are pure.
package facts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Facts are the host facts plus the serial number.
type Facts struct {
	Serial       string
	Model        string
	Chip         string
	Cores        int
	MemoryGiB    int
	DiskFreeGiB  int
	MacOSVersion string
	TartVersion  string
	AgentVersion string
	FileVault    string // off | on | unknown
	Hostname     string
}

// Proto converts to the wire type.
func (f Facts) Proto(site string, labels map[string]string) *cucinav1.HostFacts {
	return &cucinav1.HostFacts{
		Model: f.Model, Chip: f.Chip, Cores: uint32(f.Cores), MemoryGib: uint32(f.MemoryGiB),
		DiskFreeGib: uint32(f.DiskFreeGiB), MacosVersion: f.MacOSVersion, TartVersion: f.TartVersion,
		AgentVersion: f.AgentVersion, Filevault: f.FileVault, Site: site, Labels: labels,
	}
}

// ParseHardware parses `system_profiler SPHardwareDataType -json`.
func ParseHardware(out []byte) (model, chip, serial string, memGiB int, err error) {
	var doc struct {
		SPHardwareDataType []struct {
			MachineName  string `json:"machine_name"`
			MachineModel string `json:"machine_model"`
			ChipType     string `json:"chip_type"`
			Serial       string `json:"serial_number"`
			Memory       string `json:"physical_memory"`
		} `json:"SPHardwareDataType"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return "", "", "", 0, fmt.Errorf("system_profiler: %w", err)
	}
	if len(doc.SPHardwareDataType) == 0 {
		return "", "", "", 0, errors.New("system_profiler: no hardware entry")
	}
	h := doc.SPHardwareDataType[0]
	model = strings.TrimSpace(h.MachineName + " (" + h.MachineModel + ")")
	if h.MachineModel == "" {
		model = h.MachineName
	}
	if f := strings.Fields(h.Memory); len(f) == 2 && f[1] == "GB" {
		memGiB, _ = strconv.Atoi(f[0])
	}
	return model, h.ChipType, h.Serial, memGiB, nil
}

var ioregSerialRe = regexp.MustCompile(`"IOPlatformSerialNumber"\s*=\s*"([^"]+)"`)

// ParseIORegSerial parses `ioreg -rd1 -c IOPlatformExpertDevice`.
func ParseIORegSerial(out []byte) (string, error) {
	m := ioregSerialRe.FindSubmatch(out)
	if m == nil {
		return "", errors.New("ioreg: IOPlatformSerialNumber not found")
	}
	return string(m[1]), nil
}

// ParseFileVault parses `fdesetup status`.
func ParseFileVault(out []byte) string {
	s := string(out)
	switch {
	case strings.Contains(s, "FileVault is Off"):
		return "off"
	case strings.Contains(s, "FileVault is On"):
		return "on"
	}
	return "unknown"
}

// Gatherer collects facts.
type Gatherer struct {
	Exec         ports.Exec
	FS           ports.FS
	DiskPath     string // volume holding TART_HOME / the L2 cache
	TartVersion  func(context.Context) (string, error)
	AgentVersion string
}

func (g Gatherer) out(ctx context.Context, path string, args ...string) ([]byte, error) {
	res, err := g.Exec.Run(ctx, ports.Command{Path: path, Args: args})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("%s: exit %d: %s", path, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return res.Stdout, nil
}

// Gather collects every fact; individual failures leave fields empty except
// the serial number, which is required.
func (g Gatherer) Gather(ctx context.Context) (Facts, error) {
	f := Facts{AgentVersion: g.AgentVersion, FileVault: "unknown"}
	if out, err := g.out(ctx, "/usr/sbin/system_profiler", "SPHardwareDataType", "-json"); err == nil {
		f.Model, f.Chip, f.Serial, f.MemoryGiB, _ = ParseHardware(out)
	}
	if f.Serial == "" {
		out, err := g.out(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
		if err != nil {
			return f, fmt.Errorf("reading the serial number: %w", err)
		}
		if f.Serial, err = ParseIORegSerial(out); err != nil {
			return f, err
		}
	}
	if out, err := g.out(ctx, "/usr/sbin/sysctl", "-n", "hw.ncpu"); err == nil {
		f.Cores, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}
	if f.MemoryGiB == 0 {
		if out, err := g.out(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize"); err == nil {
			if b, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil {
				f.MemoryGiB = int(b >> 30)
			}
		}
	}
	if out, err := g.out(ctx, "/usr/bin/sw_vers", "-productVersion"); err == nil {
		f.MacOSVersion = strings.TrimSpace(string(out))
	}
	if out, err := g.out(ctx, "/usr/bin/fdesetup", "status"); err == nil {
		f.FileVault = ParseFileVault(out)
	}
	if out, err := g.out(ctx, "/bin/hostname"); err == nil {
		f.Hostname = strings.TrimSpace(string(out))
	}
	if g.FS != nil && g.DiskPath != "" {
		if _, free, err := g.FS.DiskUsage(g.DiskPath); err == nil {
			f.DiskFreeGiB = int(free >> 30)
		}
	}
	if g.TartVersion != nil {
		if v, err := g.TartVersion(ctx); err == nil {
			f.TartVersion = v
		}
	}
	if f.Serial == "" {
		return f, errors.New("serial number is empty")
	}
	return f, nil
}
