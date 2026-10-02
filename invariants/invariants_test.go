// SPDX-License-Identifier: FSL-1.1-ALv2

package invariants_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/invariants"
)

// TestGuards is the one failure-path test per production guard (R-TEST-7):
// each row must report exactly the named invariant (or nothing when it holds).
func TestGuards(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	tags := map[string]string{domain.TagManagedBy: domain.ManagedByValue, domain.TagCluster: "c", domain.TagPool: "p",
		domain.TagGeneration: "g1", domain.TagLaunchToken: "t1", domain.TagRole: "worker"}
	with := func(over map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range tags {
			m[k] = v
		}
		for k, v := range over {
			if v == "" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	inst := func(id string, st ports.InstanceState, tg map[string]string) ports.Instance {
		return ports.Instance{ID: id, Pool: "p", State: st, Tags: tg}
	}
	busy := domain.VM{ID: "i-1", Threads: 4, Busy: 2, IdleSince: time.Time{}}
	idle := domain.VM{ID: "i-2", Threads: 4, IdleSince: now.Add(-10 * time.Minute)}
	for _, tc := range []struct {
		name string
		got  []invariants.Violation
		want invariants.Name // "" = holds
	}{
		{"launch within max", invariants.LaunchWithinMax("p", 4, 2, 1, 1), ""},
		{"launch beyond max", invariants.LaunchWithinMax("p", 4, 2, 1, 2), invariants.InstancesNeverExceedMax},
		{"stop idle VM", invariants.StopAllowed("p", idle, now, time.Time{}, 30*time.Minute), ""},
		{"stop busy VM, not draining", invariants.StopAllowed("p", busy, now, time.Time{}, 30*time.Minute), invariants.NeverTerminateBusyOrLeased},
		{"stop busy VM, drain young", invariants.StopAllowed("p", busy, now, now.Add(-time.Minute), 30*time.Minute), invariants.NeverTerminateBusyOrLeased},
		{"stop busy VM, drain timed out", invariants.StopAllowed("p", busy, now, now.Add(-31*time.Minute), 30*time.Minute), ""},
		{"idle scale-in allowed", invariants.IdleScaleInAllowed("p", idle, now, now.Add(-6*time.Minute), 5*time.Minute), ""},
		{"idle scale-in of busy VM", invariants.IdleScaleInAllowed("p", busy, now, now.Add(-6*time.Minute), 5*time.Minute), invariants.ScaleInOnlyAfterIdleTimeout},
		{"idle scale-in too early", invariants.IdleScaleInAllowed("p", idle, now, now.Add(-6*time.Minute), 15*time.Minute), invariants.ScaleInOnlyAfterIdleTimeout},
		{"idle scale-in, queue recently busy", invariants.IdleScaleInAllowed("p", idle, now, now.Add(-time.Minute), 5*time.Minute), invariants.ScaleInOnlyAfterIdleTimeout},
		{"fleet fully tagged", invariants.CheckInstances("c", []ports.Instance{inst("i-1", ports.InstanceRunning, tags)}), ""},
		{"untagged instance", invariants.CheckInstances("c", []ports.Instance{inst("i-1", ports.InstanceRunning, with(map[string]string{domain.TagRole: ""}))}), invariants.EveryResourceTagged},
		{"stopped instance", invariants.CheckInstances("c", []ports.Instance{inst("i-1", ports.InstanceStopped, tags)}), invariants.NoStoppedEC2Instances},
		{"duplicate token", invariants.CheckInstances("c", []ports.Instance{inst("i-1", ports.InstanceRunning, tags), inst("i-2", ports.InstanceTerminated, tags)}), invariants.NoDuplicateLaunchPerToken},
		{"untagged volume", invariants.CheckResourceTags("volume", "vol-1", with(map[string]string{domain.TagCluster: ""})), invariants.EveryResourceTagged},
		{"pool at max", invariants.CheckPoolMax("p", 4, 4), ""},
		{"pool over max", invariants.CheckPoolMax("p", 4, 5), invariants.InstancesNeverExceedMax},
		{"two VMs on a host", invariants.CheckHostVMs([]ports.HostState{{Serial: "H", RunningVMs: 2}}), ""},
		{"three VMs on a host", invariants.CheckHostVMs([]ports.HostState{{Serial: "H", VMs: []domain.VM{
			{State: domain.VMRegistered}, {State: domain.VMLaunching}, {State: domain.VMDraining}, {State: domain.VMStopped}}}}), invariants.AtMostTwoVMsPerHost},
		{"young orphan", invariants.CheckOrphans([]ports.Orphan{{Kind: ports.OrphanVolume, ID: "vol-1", Age: time.Minute}}, 10*time.Minute), ""},
		{"old orphan", invariants.CheckOrphans([]ports.Orphan{{Kind: ports.OrphanENI, ID: "eni-1", Age: time.Hour}}, 10*time.Minute), invariants.NoOrphanVolumesBeyondGrace},
	} {
		if tc.want == "" {
			require.Empty(t, tc.got, tc.name)
			continue
		}
		require.NotEmpty(t, tc.got, tc.name)
		for _, v := range tc.got {
			require.Equal(t, tc.want, v.Invariant, tc.name)
		}
	}
}

// TestReport guards the violation hook: every violation reaches the
// installed reporter (cucina_invariant_violations_total in production).
func TestReport(t *testing.T) {
	var got []invariants.Name
	invariants.SetReporter(invariants.ReporterFunc(func(v invariants.Violation) { got = append(got, v.Invariant) }))
	t.Cleanup(func() { invariants.SetReporter(nil) })
	invariants.Report(invariants.CheckPoolMax("p", 1, 2)...)
	require.Equal(t, []invariants.Name{invariants.InstancesNeverExceedMax}, got)
}
