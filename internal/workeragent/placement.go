// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"fmt"
	"sort"

	"github.com/sloper-ai/cucina/internal/bbconfig"
)

// L1 placements (WorkerSettings.l1_placement), shared with internal/bbconfig.
const (
	PlacementAuto          = bbconfig.PlacementAuto
	PlacementInstanceStore = bbconfig.PlacementInstanceStore
	PlacementEBS           = bbconfig.PlacementEBS
	PlacementMemory        = bbconfig.PlacementMemory
	PlacementVMDisk        = bbconfig.PlacementVMDisk
)

// PlacementPlan is the outcome of PlanPlacement.
type PlacementPlan struct {
	// Placement is what the renderer is asked for: instance-store or ebs (with
	// Disk set), or auto/memory/vm-disk (no device; bbconfig then chooses
	// between memory and the root disk and sizes everything).
	Placement string
	// Disk is the device to format and mount at the cache root.
	Disk *Disk
	// Fallback explains why the requested placement was not honoured ("" if it was).
	Fallback string
}

// PlanPlacement resolves WorkerSettings.l1_placement against the machine's
// disks (R-CACHE-2, R-POOL-4): auto prefers an instance-store NVMe device,
// then an attached ephemeral EBS data volume, then no device (memory when RAM
// allows, else the root disk — decided and sized by internal/bbconfig). An
// explicit instance-store or ebs request whose device is missing degrades
// along the same order and says so in Fallback: the pool keeps working and the
// log line makes the misconfiguration visible. Disks holding the OS or
// carrying partition tables are never chosen. Among eligible disks of a kind,
// one that is already mounted (by the image's format unit or an earlier
// bootstrap of this boot) wins, then the largest.
func PlanPlacement(requested string, disks []Disk) PlacementPlan {
	if requested == "" {
		requested = PlacementAuto
	}
	pick := func(kind DiskKind) *Disk {
		var cands []Disk
		for _, d := range disks {
			if d.Kind == kind && !d.Root && !d.Partitioned && d.SizeBytes > 0 {
				cands = append(cands, d)
			}
		}
		if len(cands) == 0 {
			return nil
		}
		sort.SliceStable(cands, func(i, j int) bool {
			mi, mj := len(cands[i].MountPoints) > 0, len(cands[j].MountPoints) > 0
			if mi != mj {
				return mi
			}
			if cands[i].SizeBytes != cands[j].SizeBytes {
				return cands[i].SizeBytes > cands[j].SizeBytes
			}
			return cands[i].ID < cands[j].ID
		})
		d := cands[0]
		return &d
	}
	auto := func() PlacementPlan {
		if d := pick(DiskInstanceStore); d != nil {
			return PlacementPlan{Placement: PlacementInstanceStore, Disk: d}
		}
		if d := pick(DiskEBS); d != nil {
			return PlacementPlan{Placement: PlacementEBS, Disk: d}
		}
		return PlacementPlan{Placement: PlacementAuto}
	}
	switch requested {
	case PlacementAuto:
		return auto()
	case PlacementMemory, PlacementVMDisk:
		return PlacementPlan{Placement: requested}
	case PlacementInstanceStore:
		if d := pick(DiskInstanceStore); d != nil {
			return PlacementPlan{Placement: requested, Disk: d}
		}
		p := auto()
		p.Fallback = "no instance-store NVMe device on this instance type"
		return p
	case PlacementEBS:
		if d := pick(DiskEBS); d != nil {
			return PlacementPlan{Placement: requested, Disk: d}
		}
		p := auto()
		p.Fallback = "no ephemeral EBS data volume attached"
		return p
	default:
		p := auto()
		p.Fallback = fmt.Sprintf("unknown l1_placement %q", requested)
		return p
	}
}

// GiB is 2^30 bytes.
const GiB = uint64(1) << 30
