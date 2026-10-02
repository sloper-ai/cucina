// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/sloper-ai/cucina/internal/ports"
)

// EC2 NVMe model strings (/sys/block/nvmeXn1/device/model).
const (
	modelInstanceStore = "Amazon EC2 NVMe Instance Storage"
	modelEBS           = "Amazon Elastic Block Store"
)

// LinuxDisks lists whole NVMe disks from sysfs below sysRoot (Nitro instances
// expose instance store and EBS as NVMe; Xen-era xvd* devices are not
// supported). Mount points come from /proc/self/mountinfo, matched by
// major:minor so "/dev/root"-style sources do not matter.
func LinuxDisks(fs ports.FS, sysRoot string) ([]Disk, error) {
	blockDir := path.Join(sysRoot, "sys/block")
	names, err := fs.ListDir(blockDir)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", blockDir, err)
	}
	mounts, err := linuxMounts(fs, sysRoot)
	if err != nil {
		return nil, err
	}
	var disks []Disk
	for _, name := range names {
		if !strings.HasPrefix(name, "nvme") {
			continue
		}
		dir := path.Join(blockDir, name)
		d := Disk{ID: "/dev/" + name, Kind: DiskOther}
		if b, err := fs.ReadFile(path.Join(dir, "device/model")); err == nil {
			d.Model = strings.TrimSpace(string(b))
		}
		switch d.Model {
		case modelInstanceStore:
			d.Kind = DiskInstanceStore
		case modelEBS:
			d.Kind = DiskEBS
		}
		if b, err := fs.ReadFile(path.Join(dir, "size")); err == nil {
			if sectors, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
				d.SizeBytes = sectors * 512
			}
		}
		devs := []string{readTrim(fs, path.Join(dir, "dev"))}
		entries, _ := fs.ListDir(dir)
		for _, e := range entries {
			if !strings.HasPrefix(e, name) {
				continue
			}
			if ok, _ := fs.Exists(path.Join(dir, e, "partition")); ok {
				d.Partitioned = true
				devs = append(devs, readTrim(fs, path.Join(dir, e, "dev")))
			}
		}
		for _, dev := range devs {
			for _, mp := range mounts[dev] {
				d.MountPoints = append(d.MountPoints, mp)
				if mp == "/" {
					d.Root = true
				}
			}
		}
		disks = append(disks, d)
	}
	return disks, nil
}

func readTrim(fs ports.FS, p string) string {
	b, err := fs.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// linuxMounts maps "major:minor" to mount points from /proc/self/mountinfo.
func linuxMounts(fs ports.FS, sysRoot string) (map[string][]string, error) {
	p := path.Join(sysRoot, "proc/self/mountinfo")
	b, err := fs.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	out := map[string][]string{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		out[f[2]] = append(out[f[2]], unescapeMount(f[4]))
	}
	return out, nil
}

// unescapeMount decodes the octal escapes (\040 etc.) of mountinfo paths.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
