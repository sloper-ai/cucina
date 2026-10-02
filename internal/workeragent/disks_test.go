// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent_test

import (
	"path"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/workeragent"
	"github.com/sloper-ai/cucina/internal/workeragent/agenttest"
)

// Guards: R-POOL-4 / R-CACHE-2 — the agent recognises EC2 NVMe instance store
// and EBS volumes from sysfs (model strings), finds the OS disk by the
// major:minor of "/" (whatever /proc/self/mountinfo calls its source) and
// sees existing mounts.
func TestLinuxDisks(t *testing.T) {
	fs := fakes.NewFS(fakes.NewClock(agenttest.Epoch), fakes.NewRand(1), 0)
	put := func(p, content string) {
		require.NoError(t, fs.MkdirAll(path.Dir(p), 0o755))
		require.NoError(t, fs.WriteFileAtomic(p, []byte(content), 0o644))
	}
	const sys = "/fixture"
	put(sys+"/sys/block/nvme0n1/device/model", "Amazon Elastic Block Store              \n")
	put(sys+"/sys/block/nvme0n1/size", "25165824\n")
	put(sys+"/sys/block/nvme0n1/dev", "259:0\n")
	put(sys+"/sys/block/nvme0n1/nvme0n1p1/partition", "1\n")
	put(sys+"/sys/block/nvme0n1/nvme0n1p1/dev", "259:1\n")
	put(sys+"/sys/block/nvme1n1/device/model", "Amazon EC2 NVMe Instance Storage        \n")
	put(sys+"/sys/block/nvme1n1/size", "462890200\n")
	put(sys+"/sys/block/nvme1n1/dev", "259:2\n")
	put(sys+"/sys/block/nvme2n1/device/model", "Amazon Elastic Block Store\n")
	put(sys+"/sys/block/nvme2n1/size", "209715200\n")
	put(sys+"/sys/block/nvme2n1/dev", "259:3\n")
	put(sys+"/sys/block/loop0/size", "0\n")
	put(sys+"/proc/self/mountinfo", "26 1 259:1 / / rw,relatime shared:1 - ext4 /dev/root rw,discard\n"+
		"97 26 259:3 / /mnt/scratch\\040space rw,noatime shared:44 - ext4 /dev/nvme2n1 rw\n")

	disks, err := workeragent.LinuxDisks(fs, sys)
	require.NoError(t, err)
	require.Equal(t, []workeragent.Disk{
		{ID: "/dev/nvme0n1", Kind: workeragent.DiskEBS, Model: "Amazon Elastic Block Store", SizeBytes: 12 << 30, Root: true, Partitioned: true, MountPoints: []string{"/"}},
		{ID: "/dev/nvme1n1", Kind: workeragent.DiskInstanceStore, Model: "Amazon EC2 NVMe Instance Storage", SizeBytes: 462890200 * 512},
		{ID: "/dev/nvme2n1", Kind: workeragent.DiskEBS, Model: "Amazon Elastic Block Store", SizeBytes: 100 << 30, MountPoints: []string{"/mnt/scratch space"}},
	}, disks)
	plan := workeragent.PlanPlacement("auto", disks)
	require.Equal(t, "/dev/nvme1n1", plan.Disk.ID)
}

// Guards: R-POOL-5 / R-CACHE-2 on Windows — the Get-Disk probe output is
// classified the same way (boot disk excluded, a disk already holding only
// the cache root reused, multi-volume disks left alone).
func TestParseWindowsDisks(t *testing.T) {
	out := `[{"Number":0,"FriendlyName":"NVMe Amazon Elastic B","Model":"Amazon Elastic Block Store","Size":32212254720,"PartitionStyle":"GPT","IsBoot":true,"IsSystem":true,"AccessPaths":["C:\\"]},
	{"Number":1,"FriendlyName":"NVMe Amazon EC2 NVMe","Model":"Amazon EC2 NVMe Instance Storage","Size":118111600640,"PartitionStyle":"RAW","IsBoot":false,"IsSystem":false,"AccessPaths":[]},
	{"Number":2,"FriendlyName":"NVMe Amazon Elastic B","Model":"","Size":107374182400,"PartitionStyle":"GPT","IsBoot":false,"IsSystem":false,"AccessPaths":["C:\\bb\\data\\"]},
	{"Number":3,"FriendlyName":"NVMe Amazon Elastic B","Model":"","Size":107374182400,"PartitionStyle":"GPT","IsBoot":false,"IsSystem":false,"AccessPaths":["D:\\","E:\\"]}]`
	disks, err := workeragent.ParseWindowsDisks([]byte(out))
	require.NoError(t, err)
	require.Len(t, disks, 4)
	require.True(t, disks[0].Root)
	require.Equal(t, workeragent.DiskInstanceStore, disks[1].Kind)
	require.False(t, disks[1].Partitioned)
	require.Equal(t, workeragent.DiskEBS, disks[2].Kind)
	require.Equal(t, []string{`C:\bb\data`}, disks[2].MountPoints)
	require.False(t, disks[2].Partitioned, "a single mounted volume (ours, or the image's) stays eligible")
	require.True(t, disks[3].Partitioned, "a disk with several volumes is not ours")

	plan := workeragent.PlanPlacement("ebs", disks)
	require.Equal(t, "2", plan.Disk.ID)

	single, err := workeragent.ParseWindowsDisks([]byte(`{"Number":0,"FriendlyName":"x","Model":"","Size":1,"PartitionStyle":"GPT","IsBoot":true,"IsSystem":true,"AccessPaths":null}`))
	require.NoError(t, err, "PowerShell may unroll a one-element array")
	require.Len(t, single, 1)
}
