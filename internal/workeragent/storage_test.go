// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent_test

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/workeragent"
	"github.com/sloper-ai/cucina/internal/workeragent/agenttest"
)

// fakeBlockDevices models blkid/mkfs.ext4/mount over a set of block devices
// and /proc/self/mountinfo in the fake filesystem.
type fakeBlockDevices struct {
	fs        *fakes.FS
	sysRoot   string
	fsType    map[string]string // device -> filesystem signature ("" = blank)
	mountinfo []string
}

func (b *fakeBlockDevices) writeMountinfo(t *testing.T) {
	require.NoError(t, b.fs.WriteFileAtomic(path.Join(b.sysRoot, "proc/self/mountinfo"), []byte(strings.Join(b.mountinfo, "\n")+"\n"), 0o444))
}

func (b *fakeBlockDevices) install(t *testing.T, ex *fakes.Exec) {
	ex.Handle("blkid", func(_ context.Context, c ports.Command) (ports.ExecResult, error) {
		if typ := b.fsType[c.Args[len(c.Args)-1]]; typ != "" {
			return ports.ExecResult{Stdout: []byte(typ + "\n")}, nil
		}
		return ports.ExecResult{ExitCode: 2}, nil
	})
	ex.Handle("mkfs.ext4", func(_ context.Context, c ports.Command) (ports.ExecResult, error) {
		b.fsType[c.Args[len(c.Args)-1]] = "ext4"
		return ports.ExecResult{}, nil
	})
	ex.Handle("mount", func(_ context.Context, c ports.Command) (ports.ExecResult, error) {
		dev, mp := c.Args[len(c.Args)-2], c.Args[len(c.Args)-1]
		if b.fsType[dev] != "ext4" {
			return ports.ExecResult{ExitCode: 32, Stderr: []byte("wrong fs type")}, nil
		}
		b.mountinfo = append(b.mountinfo, "120 26 259:9 / "+mp+" rw,noatime - ext4 "+dev+" rw")
		b.writeMountinfo(t)
		return ports.ExecResult{}, nil
	})
}

// Guards: R-POOL-4 / R-CACHE-2 on Linux — the agent reuses what the image's
// format unit mounted (a single device or a RAID 0 array of several
// instance-store devices), formats and mounts a blank device itself, and never
// mounts a device it does not understand.
func TestLinuxStoragePrepare(t *testing.T) {
	const mnt = "/var/lib/cucina/ephemeral"
	cases := []struct {
		name     string
		disk     workeragent.Disk
		fsType   string
		mounted  []string // mountinfo lines present before
		wantPath string
		wantErr  bool
		wantExt4 bool
	}{
		{name: "device the image mounted", disk: workeragent.Disk{ID: "/dev/nvme1n1", MountPoints: []string{mnt}}, fsType: "ext4",
			mounted: []string{"100 26 259:2 / " + mnt + " rw,noatime - ext4 /dev/nvme1n1 rw"}, wantPath: mnt},
		{name: "array the image mounted", disk: workeragent.Disk{ID: "/dev/nvme1n1"}, fsType: "linux_raid_member",
			mounted: []string{"101 26 9:127 / " + mnt + " rw,noatime - ext4 /dev/md127 rw"}, wantPath: mnt},
		{name: "blank device", disk: workeragent.Disk{ID: "/dev/nvme2n1"}, wantPath: mnt, wantExt4: true},
		{name: "existing filesystem is kept", disk: workeragent.Disk{ID: "/dev/nvme2n1"}, fsType: "ext4", wantPath: mnt},
		{name: "raid member without its array", disk: workeragent.Disk{ID: "/dev/nvme1n1"}, fsType: "linux_raid_member", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock, rnd := fakes.NewClock(agenttest.Epoch), fakes.NewRand(3)
			fs, ex := fakes.NewFS(clock, rnd, 100<<30), fakes.NewExec(clock, rnd)
			const sys = "/sysroot"
			require.NoError(t, fs.MkdirAll(sys+"/proc/self", 0o755))
			b := &fakeBlockDevices{fs: fs, sysRoot: sys, fsType: map[string]string{tc.disk.ID: tc.fsType},
				mountinfo: append([]string{"26 1 259:1 / / rw - ext4 /dev/root rw"}, tc.mounted...)}
			b.writeMountinfo(t)
			b.install(t, ex)
			s := workeragent.LinuxStorage{Exec: ex, FS: fs, SysRoot: sys}
			disk := tc.disk
			got, free, err := s.Prepare(context.Background(), workeragent.PlacementPlan{Placement: workeragent.PlacementInstanceStore, Disk: &disk}, mnt)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantPath, got)
			require.Positive(t, free)
			mounted, err := fs.ReadFile(sys + "/proc/self/mountinfo")
			require.NoError(t, err)
			require.Contains(t, string(mounted), " "+mnt+" ", "something is mounted where L1 goes")
			require.Equal(t, tc.wantExt4, tc.fsType == "" && b.fsType[disk.ID] == "ext4", "only blank devices are formatted")
		})
	}
}
