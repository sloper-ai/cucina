// SPDX-License-Identifier: FSL-1.1-ALv2

package porttest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// VMRuntimeHarness configures RunVMRuntime.
type VMRuntimeHarness struct {
	Runtime ports.VMRuntime
	// Image is a small image reference the runtime can clone.
	Image string
	// BootWait bounds IP(): enough for the image to boot.
	BootWait time.Duration
	// TwoVMLimit runs the check that a third VM is refused (needs three
	// clones and two running VMs; heavy on a real Mac).
	TwoVMLimit bool
}

// RunVMRuntime checks the ports.VMRuntime contract (R-MAC-3): clone, run,
// stop keeping the disk, delete, the two-VM limit and error sentinels. VM
// names are unique and every VM is deleted at the end.
func RunVMRuntime(t *testing.T, newRuntime func(t *testing.T) VMRuntimeHarness) {
	ctx := context.Background()
	clone := func(t *testing.T, h VMRuntimeHarness) string {
		name := uniq(t, "porttest")
		require.NoError(t, h.Runtime.Clone(ctx, h.Image, name, 0))
		t.Cleanup(func() {
			_ = h.Runtime.Stop(ctx, name, 30*time.Second)
			_ = h.Runtime.Delete(ctx, name)
		})
		return name
	}
	state := func(t *testing.T, h VMRuntimeHarness, name string) (ports.TartState, bool) {
		vms, err := h.Runtime.List(ctx)
		require.NoError(t, err)
		for _, v := range vms {
			if v.Name == name {
				return v.State, true
			}
		}
		return "", false
	}

	t.Run("CloneRunStopKeepsDisk", func(t *testing.T) {
		h := newRuntime(t)
		name := clone(t, h)
		require.ErrorIs(t, h.Runtime.Clone(ctx, h.Image, name, 0), ports.ErrVMExists)
		st, ok := state(t, h, name)
		require.True(t, ok)
		require.Equal(t, ports.TartStopped, st)
		require.NoError(t, h.Runtime.Run(ctx, name, ports.RunOptions{NoGraphics: true, RootDiskOpts: "caching=cached,sync=none"}))
		st, _ = state(t, h, name)
		require.Equal(t, ports.TartRunning, st)
		ip, err := h.Runtime.IP(ctx, name, h.BootWait)
		require.NoError(t, err)
		require.True(t, ip.IsValid())
		require.NoError(t, h.Runtime.Stop(ctx, name, 30*time.Second))
		st, ok = state(t, h, name)
		require.True(t, ok, "a stopped VM keeps its disk")
		require.Equal(t, ports.TartStopped, st)
	})

	t.Run("DeleteNeedsAStoppedVM", func(t *testing.T) {
		h := newRuntime(t)
		name := clone(t, h)
		require.NoError(t, h.Runtime.Run(ctx, name, ports.RunOptions{NoGraphics: true}))
		require.Error(t, h.Runtime.Delete(ctx, name), "deleting a running VM must fail")
		require.NoError(t, h.Runtime.Stop(ctx, name, 30*time.Second))
		require.NoError(t, h.Runtime.Delete(ctx, name))
		_, ok := state(t, h, name)
		require.False(t, ok)
	})

	t.Run("MissingVMIsReported", func(t *testing.T) {
		h := newRuntime(t)
		require.ErrorIs(t, h.Runtime.Run(ctx, "porttest-missing", ports.RunOptions{}), ports.ErrVMNotFound)
		require.ErrorIs(t, h.Runtime.Stop(ctx, "porttest-missing", time.Second), ports.ErrVMNotFound)
		_, err := h.Runtime.GuestExec(ctx, "porttest-missing", ports.Command{Path: "/usr/bin/true"})
		require.ErrorIs(t, err, ports.ErrVMNotFound)
	})

	t.Run("AtMostTwoVMsRun", func(t *testing.T) {
		h := newRuntime(t)
		if !h.TwoVMLimit {
			t.Skip("harness does not run the two-VM limit check")
		}
		a, b, c := clone(t, h), clone(t, h), clone(t, h)
		require.NoError(t, h.Runtime.Run(ctx, a, ports.RunOptions{NoGraphics: true}))
		require.NoError(t, h.Runtime.Run(ctx, b, ports.RunOptions{NoGraphics: true}))
		require.ErrorIs(t, h.Runtime.Run(ctx, c, ports.RunOptions{NoGraphics: true}), ports.ErrVMLimit)
		require.NoError(t, h.Runtime.Stop(ctx, a, 30*time.Second))
		require.NoError(t, h.Runtime.Run(ctx, c, ports.RunOptions{NoGraphics: true}), "a slot frees when a VM stops")
	})

	t.Run("ImagesListsClonedImage", func(t *testing.T) {
		h := newRuntime(t)
		clone(t, h)
		imgs, err := h.Runtime.Images(ctx)
		require.NoError(t, err)
		require.True(t, slices.ContainsFunc(imgs, func(i ports.ImageInfo) bool { return i.Reference == h.Image }))
	})
}

// HostFleetHarness configures RunHostFleet.
type HostFleetHarness struct {
	Fleet ports.HostFleet
	// Host is an online, approved, uncordoned host with two free VM slots.
	Host              string
	Pool              domain.PoolName
	Image, Generation string
	Await             Await
}

// RunHostFleet checks the ports.HostFleet contract (R-MAC-3/6, R-POOL-6):
// starting is idempotent, the two-VM limit holds, stopping keeps the VM,
// cordoned and unknown hosts refuse work.
func RunHostFleet(t *testing.T, newFleet func(t *testing.T) HostFleetHarness) {
	ctx := context.Background()
	req := func(h HostFleetHarness, vm string) ports.StartVMRequest {
		return ports.StartVMRequest{Pool: h.Pool, Generation: h.Generation, Image: h.Image, VMName: vm, CPU: 4, MemoryGiB: 8, DiskGiB: 80}
	}
	vmState := func(t *testing.T, h HostFleetHarness, vm string) (domain.VMState, bool) {
		hosts, err := h.Fleet.Hosts(ctx)
		require.NoError(t, err)
		for _, hs := range hosts {
			if hs.Serial != h.Host {
				continue
			}
			for _, v := range hs.VMs {
				if v.ID == h.Host+"/"+vm {
					return v.State, true
				}
			}
		}
		return "", false
	}
	cleanup := func(t *testing.T, h HostFleetHarness, vms ...string) {
		t.Cleanup(func() {
			for _, vm := range vms {
				_ = h.Fleet.StopVM(ctx, h.Host, vm, 30*time.Second, "porttest")
				_ = h.Fleet.DeleteVM(ctx, h.Host, vm)
			}
		})
	}

	t.Run("HostIsListedOnline", func(t *testing.T) {
		h := newFleet(t)
		hosts, err := h.Fleet.Hosts(ctx)
		require.NoError(t, err)
		i := slices.IndexFunc(hosts, func(s ports.HostState) bool { return s.Serial == h.Host })
		require.GreaterOrEqual(t, i, 0)
		require.True(t, hosts[i].Online)
	})

	t.Run("StartIsIdempotentAndTwoVMsMax", func(t *testing.T) {
		h := newFleet(t)
		a, b, c := uniq(t, "vm"), uniq(t, "vm"), uniq(t, "vm")
		cleanup(t, h, a, b, c)
		require.NoError(t, h.Fleet.StartVM(ctx, h.Host, req(h, a)))
		require.NoError(t, h.Fleet.StartVM(ctx, h.Host, req(h, a)), "starting a running VM is a no-op")
		require.NoError(t, h.Fleet.StartVM(ctx, h.Host, req(h, b)))
		require.ErrorIs(t, h.Fleet.StartVM(ctx, h.Host, req(h, c)), ports.ErrVMLimit)
	})

	t.Run("StopKeepsTheVM", func(t *testing.T) {
		h := newFleet(t)
		a := uniq(t, "vm")
		cleanup(t, h, a)
		require.NoError(t, h.Fleet.StartVM(ctx, h.Host, req(h, a)))
		h.Await(t, "VM running", func() bool {
			st, ok := vmState(t, h, a)
			return ok && st != domain.VMStopped
		})
		require.NoError(t, h.Fleet.StopVM(ctx, h.Host, a, 30*time.Second, "porttest"))
		h.Await(t, "VM stopped", func() bool {
			st, ok := vmState(t, h, a)
			require.True(t, ok, "a stopped VM keeps its disk and stays listed")
			return st == domain.VMStopped
		})
		require.NoError(t, h.Fleet.StartVM(ctx, h.Host, req(h, a)), "a stopped VM starts again")
	})

	t.Run("CordonedAndUnknownHostsRefuseWork", func(t *testing.T) {
		h := newFleet(t)
		a := uniq(t, "vm")
		cleanup(t, h, a)
		require.NoError(t, h.Fleet.SetCordon(ctx, h.Host, true))
		t.Cleanup(func() { _ = h.Fleet.SetCordon(ctx, h.Host, false) })
		require.Error(t, h.Fleet.StartVM(ctx, h.Host, req(h, a)))
		require.NoError(t, h.Fleet.SetCordon(ctx, h.Host, false))
		require.NoError(t, h.Fleet.StartVM(ctx, h.Host, req(h, a)))
		require.ErrorIs(t, h.Fleet.StartVM(ctx, "porttest-no-such-host", req(h, a)), ports.ErrNotFound)
	})
}
