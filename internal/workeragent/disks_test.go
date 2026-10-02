// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent_test

import (
	"context"
	"encoding/base64"
	"path"
	"slices"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
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
// classified the same way (boot disk excluded, a mounted volume reused, an
// initialised but unmounted disk left alone), in every shape Windows
// PowerShell 5.1 emits (joined access paths, arrays, a single object, the
// {"value":…,"Count":…} wrapper, a byte order mark).
func TestParseWindowsDisks(t *testing.T) {
	out := `[{"Number":0,"FriendlyName":"NVMe Amazon Elastic B","Model":"Amazon Elastic Block Store","Size":32212254720,"PartitionStyle":"GPT","IsBoot":true,"IsSystem":true,"AccessPaths":"C:\\"},
	{"Number":1,"FriendlyName":"NVMe Amazon EC2 NVMe","Model":"Amazon EC2 NVMe Instance Storage","Size":118111600640,"PartitionStyle":"RAW","IsBoot":false,"IsSystem":false,"AccessPaths":""},
	{"Number":2,"FriendlyName":"NVMe Amazon Elastic B","Model":"","Size":107374182400,"PartitionStyle":"GPT","IsBoot":false,"IsSystem":false,"AccessPaths":"D:\\|C:\\bb\\data\\"},
	{"Number":3,"FriendlyName":"NVMe Amazon Elastic B","Model":"","Size":107374182400,"PartitionStyle":"GPT","IsBoot":false,"IsSystem":false,"AccessPaths":{"value":[],"Count":0}}]`
	disks, err := workeragent.ParseWindowsDisks(append([]byte("\xef\xbb\xbf"), out...))
	require.NoError(t, err)
	require.Len(t, disks, 4)
	require.True(t, disks[0].Root)
	require.Equal(t, workeragent.DiskInstanceStore, disks[1].Kind)
	require.False(t, disks[1].Partitioned)
	require.Equal(t, workeragent.DiskEBS, disks[2].Kind)
	require.Equal(t, []string{"D:", `C:\bb\data`}, disks[2].MountPoints)
	require.False(t, disks[2].Partitioned, "a mounted volume (the image's or ours) is reused")
	require.True(t, disks[3].Partitioned, "an initialised but unmounted disk is left alone")

	plan := workeragent.PlanPlacement("ebs", disks)
	require.Equal(t, "2", plan.Disk.ID)

	for name, doc := range map[string]string{
		"single object": `{"Number":0,"FriendlyName":"x","Model":"","Size":1,"PartitionStyle":"GPT","IsBoot":true,"IsSystem":true,"AccessPaths":["C:\\"]}`,
		"value wrapper": `{"value":[{"Number":0,"FriendlyName":"x","Model":"","Size":1,"PartitionStyle":"GPT","IsBoot":true,"IsSystem":true,"AccessPaths":null}],"Count":1}`,
	} {
		single, err := workeragent.ParseWindowsDisks([]byte(doc))
		require.NoError(t, err, name)
		require.Len(t, single, 1, name)
		require.True(t, single[0].Root, name)
	}
	_, err = workeragent.ParseWindowsDisks(nil)
	require.Error(t, err, "empty output (the PS 5.1 stdin bug) is an error, not zero disks")
}

// decodePowerShell independently decodes an -EncodedCommand argument.
func decodePowerShell(t *testing.T, arg string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(arg)
	require.NoError(t, err)
	require.Zero(t, len(b)%2)
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

// Guards: the Windows worker blocker (images agent, Windows Server 2025 /
// PowerShell 5.1): multi-line scripts fed through stdin never ran, so Disks()
// saw empty output and every bootstrap powered off. Scripts must travel as
// -EncodedCommand, non-interactively, with nothing on stdin.
func TestWindowsStorageRunsPowerShellEncoded(t *testing.T) {
	clock, rnd := fakes.NewClock(agenttest.Epoch), fakes.NewRand(5)
	ex := fakes.NewExec(clock, rnd)
	var prepared []string
	ex.Handle("powershell.exe", func(_ context.Context, c ports.Command) (ports.ExecResult, error) {
		require.Nil(t, c.Stdin, "nothing on stdin")
		require.Subset(t, c.Args, []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"})
		i := slices.Index(c.Args, "-EncodedCommand")
		require.GreaterOrEqual(t, i, 0, "script passed with -EncodedCommand")
		require.Len(t, c.Args, i+2, "-EncodedCommand is the last switch")
		switch script := decodePowerShell(t, c.Args[i+1]); script {
		case workeragent.WindowsDisksScript:
			// One disk: PS 5.1 unrolls the array into a single object.
			return ports.ExecResult{Stdout: []byte(`{"Number":1,"FriendlyName":"NVMe Amazon EC2 NVMe","Model":"Amazon EC2 NVMe Instance Storage",` +
				`"Size":118111600640,"PartitionStyle":"RAW","IsBoot":false,"IsSystem":false,"AccessPaths":""}` + "\r\n")}, nil
		case workeragent.WindowsPrepareScript("1", `C:\bb\ephemeral`):
			prepared = append(prepared, "1")
			return ports.ExecResult{}, nil
		default:
			return ports.ExecResult{ExitCode: 1, Stderr: []byte("unexpected script")}, nil
		}
	})
	s := workeragent.WindowsStorage{Exec: ex, FS: fakes.NewFS(clock, rnd, 200<<30)}
	disks, err := s.Disks(context.Background())
	require.NoError(t, err)
	require.Len(t, disks, 1)
	plan := workeragent.PlanPlacement("auto", disks)
	require.Equal(t, workeragent.PlacementInstanceStore, plan.Placement)
	path, free, err := s.Prepare(context.Background(), plan, `C:\bb\ephemeral`)
	require.NoError(t, err)
	require.Equal(t, `C:\bb\ephemeral`, path)
	require.Positive(t, free)
	require.Equal(t, []string{"1"}, prepared, "the blank instance-store disk was initialised and mounted")
	require.Contains(t, workeragent.WindowsPrepareScript("1", `C:\b'b`), `'C:\b''b\'`, "paths are PowerShell-quoted")
}
