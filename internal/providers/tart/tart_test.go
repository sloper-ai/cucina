// SPDX-License-Identifier: FSL-1.1-ALv2

package tart_test

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/hostd/sys"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/ports/porttest"
	"github.com/sloper-ai/cucina/internal/providers/tart"
	"github.com/sloper-ai/cucina/internal/providers/tart/faketart"
)

const testImage = "ghcr.io/cirruslabs/macos-golden-gate-base:latest"

// TestConformanceFakeTart runs the VMRuntime conformance suite (R-TEST-8b)
// against the real adapter driving the tart CLI emulator (integration tier).
func TestConformanceFakeTart(t *testing.T) {
	porttest.RunVMRuntime(t, func(t *testing.T) porttest.VMRuntimeHarness {
		ft := faketart.New()
		ft.AddImage(faketart.Image{Ref: testImage, SizeGB: 30})
		return porttest.VMRuntimeHarness{Runtime: tart.New(tart.Options{Exec: ft}), Image: testImage, TwoVMLimit: true}
	})
}

// prefixed runs every VM of the conformance suite under a cucina-test- name on
// a real host (acceptance: never touch other VMs).
type prefixed struct{ ports.VMRuntime }

const testPrefix = "cucina-test-"

func (p prefixed) List(ctx context.Context) ([]ports.TartVM, error) {
	vms, err := p.VMRuntime.List(ctx)
	out := vms[:0]
	for _, v := range vms {
		if strings.HasPrefix(v.Name, testPrefix) {
			v.Name = strings.TrimPrefix(v.Name, testPrefix)
			out = append(out, v)
		}
	}
	return out, err
}
func (p prefixed) Clone(ctx context.Context, image, name string, disk int) error {
	return p.VMRuntime.Clone(ctx, image, testPrefix+name, disk)
}
func (p prefixed) Run(ctx context.Context, name string, o ports.RunOptions) error {
	return p.VMRuntime.Run(ctx, testPrefix+name, o)
}
func (p prefixed) Stop(ctx context.Context, name string, d time.Duration) error {
	return p.VMRuntime.Stop(ctx, testPrefix+name, d)
}
func (p prefixed) Delete(ctx context.Context, name string) error {
	return p.VMRuntime.Delete(ctx, testPrefix+name)
}
func (p prefixed) IP(ctx context.Context, name string, d time.Duration) (netip.Addr, error) {
	return p.VMRuntime.IP(ctx, testPrefix+name, d)
}
func (p prefixed) GuestExec(ctx context.Context, name string, c ports.Command) (ports.ExecResult, error) {
	return p.VMRuntime.GuestExec(ctx, testPrefix+name, c)
}

// TestConformanceRealTart is the acceptance-tier run (T13) against the real tart
// on a developer Mac: set CUCINA_TART_ACCEPTANCE=1 and CUCINA_TART_IMAGE to a
// locally pulled image. It clones throwaway cucina-test-* VMs and deletes them.
func TestConformanceRealTart(t *testing.T) {
	img := os.Getenv("CUCINA_TART_IMAGE")
	if os.Getenv("CUCINA_TART_ACCEPTANCE") != "1" || img == "" {
		t.Skip("acceptance tier: set CUCINA_TART_ACCEPTANCE=1 and CUCINA_TART_IMAGE (a locally pulled Tart image)")
	}
	bin, err := exec.LookPath("tart")
	require.NoError(t, err)
	porttest.RunVMRuntime(t, func(t *testing.T) porttest.VMRuntimeHarness {
		rt := tart.New(tart.Options{Exec: &sys.Exec{}, Binary: bin, Env: os.Environ()})
		return porttest.VMRuntimeHarness{Runtime: prefixed{rt}, Image: img, BootWait: 3 * time.Minute,
			TwoVMLimit: os.Getenv("CUCINA_TART_TWO_VMS") == "1"}
	})
}

// listExec answers `tart list` with a fixed output.
type listExec struct{ out string }

func (l listExec) Run(context.Context, ports.Command) (ports.ExecResult, error) {
	return ports.ExecResult{Stdout: []byte(l.out)}, nil
}
func (listExec) Start(context.Context, ports.Command) (ports.Process, error) {
	return nil, errors.New("unsupported")
}

// TestParseList guards the adapter against tart's JSON shapes
// (Sources/tart/Commands/List.swift, 2.40.1): decimal GB, a null Disk while a
// running VM holds an ASIF disk open, the State string (and the legacy Running flag).
func TestParseList(t *testing.T) {
	rt := tart.New(tart.Options{Exec: listExec{out: `[
  {"Source":"local","Name":"cucina-vm-a","Disk":120,"Size":31,"Accessed":"2026-10-02T08:00:00Z","Running":true,"State":"running"},
  {"Source":"local","Name":"cucina-vm-b","Disk":null,"Size":12,"Accessed":"2026-10-02T08:00:00Z","Running":false,"State":"suspended"},
  {"Source":"local","Name":"old","Disk":50,"Size":0,"Accessed":"2026-10-01T08:00:00Z","Running":true}
]`}})
	vms, err := rt.List(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ports.TartVM{
		{Name: "cucina-vm-a", State: ports.TartRunning, DiskGiB: 120, SizeOnDiskBytes: 31_000_000_000},
		{Name: "cucina-vm-b", State: ports.TartSuspended, SizeOnDiskBytes: 12_000_000_000},
		{Name: "old", State: ports.TartRunning, DiskGiB: 50},
	}, vms)
	_, err = tart.New(tart.Options{Exec: listExec{out: "not json"}}).List(context.Background())
	require.Error(t, err)
}

// TestErrorMapping guards the stderr → sentinel mapping the VM manager relies
// on (tart 2.40.1 messages, Sources/tart/VMStorageHelper.swift).
func TestErrorMapping(t *testing.T) {
	ft := faketart.New()
	rt := tart.New(tart.Options{Exec: ft})
	ctx := context.Background()
	for _, tc := range []struct {
		sub, stderr string
		want        error
	}{
		{"clone", `Error: VM "x" already exists, use --overwrite to replace it`, ports.ErrVMExists},
		{"clone", "Error: No space left on device", ports.ErrDiskFull},
		{"stop", `the specified VM "x" does not exist`, ports.ErrVMNotFound},
	} {
		ft.FailNext(tc.sub, 1, tc.stderr)
		var err error
		switch tc.sub {
		case "clone":
			err = rt.Clone(ctx, testImage, "x", 0)
		case "stop":
			err = rt.Stop(ctx, "x", time.Second)
		}
		require.True(t, errors.Is(err, tc.want), "%s: %v", tc.stderr, err)
	}
	ft.AddImage(faketart.Image{Ref: testImage})
	require.NoError(t, rt.Clone(ctx, testImage, "vm", 0))
	ft.FailNext("run", 1, "Error: The number of VMs exceeds the system limit")
	require.ErrorIs(t, rt.Run(ctx, "vm", ports.RunOptions{}), ports.ErrVMLimit, "a VZ refusal surfaces as ErrVMLimit")
	require.NoError(t, rt.Run(ctx, "vm", ports.RunOptions{}))
	ft.SetAgentDown("vm", true)
	_, err := rt.GuestExec(ctx, "vm", ports.Command{Path: "/usr/bin/true"})
	require.ErrorIs(t, err, ports.ErrGuestAgent)
	ft.SetAgentDown("vm", false)
	res, err := rt.GuestExec(ctx, "vm", ports.Command{Path: "/bin/cat", Args: []string{"/nope"}})
	require.NoError(t, err, "a guest command's own failure is a result, not an adapter error")
	require.Equal(t, 1, res.ExitCode)
	ft.PowerOff()
}
