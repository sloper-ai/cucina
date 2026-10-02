// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/workeragent"
)

// Guards: R-CACHE-2 / R-POOL-4 L1 placement — auto prefers instance-store
// NVMe, then an ephemeral EBS data volume, then no device (bbconfig chooses
// memory or the root disk); explicit requests degrade visibly; the OS disk and
// partitioned disks are never formatted.
func TestPlanPlacement(t *testing.T) {
	root := workeragent.Disk{ID: "/dev/nvme0n1", Kind: workeragent.DiskEBS, SizeBytes: 12 << 30, Root: true, Partitioned: true, MountPoints: []string{"/"}}
	nvme := func(id string, size uint64) workeragent.Disk {
		return workeragent.Disk{ID: id, Kind: workeragent.DiskInstanceStore, SizeBytes: size}
	}
	data := workeragent.Disk{ID: "/dev/nvme2n1", Kind: workeragent.DiskEBS, SizeBytes: 100 << 30}
	cases := []struct {
		name         string
		requested    string
		disks        []workeragent.Disk
		want, device string
		fallback     bool
	}{
		{"auto with instance store", "auto", []workeragent.Disk{root, nvme("/dev/nvme1n1", 237e9), data}, "instance-store", "/dev/nvme1n1", false},
		{"auto, empty request", "", []workeragent.Disk{root, data}, "ebs", "/dev/nvme2n1", false},
		{"auto without devices", "auto", []workeragent.Disk{root}, "auto", "", false},
		{"largest instance-store device", "auto", []workeragent.Disk{nvme("/dev/nvme1n1", 100e9), nvme("/dev/nvme3n1", 200e9)}, "instance-store", "/dev/nvme3n1", false},
		{"device the image already mounted wins", "auto", []workeragent.Disk{nvme("/dev/nvme1n1", 100e9),
			{ID: "/dev/nvme3n1", Kind: workeragent.DiskInstanceStore, SizeBytes: 50e9, MountPoints: []string{"/var/lib/cucina/ephemeral"}}}, "instance-store", "/dev/nvme3n1", false},
		{"explicit ebs", "ebs", []workeragent.Disk{root, nvme("/dev/nvme1n1", 237e9), data}, "ebs", "/dev/nvme2n1", false},
		{"instance-store requested, type has none", "instance-store", []workeragent.Disk{root, data}, "ebs", "/dev/nvme2n1", true},
		{"ebs requested, nothing attached", "ebs", []workeragent.Disk{root}, "auto", "", true},
		{"memory requested", "memory", []workeragent.Disk{root, nvme("/dev/nvme1n1", 237e9)}, "memory", "", false},
		{"vm-disk requested", "vm-disk", nil, "vm-disk", "", false},
		{"root disk is never chosen", "ebs", []workeragent.Disk{{ID: "/dev/nvme0n1", Kind: workeragent.DiskEBS, SizeBytes: 8 << 30, Root: true, MountPoints: []string{"/"}}}, "auto", "", true},
		{"partitioned data disk is left alone", "auto", []workeragent.Disk{{ID: "/dev/nvme2n1", Kind: workeragent.DiskEBS, SizeBytes: 1 << 30, Partitioned: true}}, "auto", "", false},
		{"unknown placement", "tmpfs", []workeragent.Disk{data}, "ebs", "/dev/nvme2n1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := workeragent.PlanPlacement(tc.requested, tc.disks)
			require.Equal(t, tc.want, p.Placement)
			if tc.device == "" {
				require.Nil(t, p.Disk)
			} else {
				require.NotNil(t, p.Disk)
				require.Equal(t, tc.device, p.Disk.ID)
			}
			require.Equal(t, tc.fallback, p.Fallback != "", "fallback %q", p.Fallback)
		})
	}
}
