// SPDX-License-Identifier: FSL-1.1-ALv2

package bep

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fixtures are real Bazel 9.2.0 build event streams of a two-target
// workspace (genrule + sh_test), cold and then warm from a disk cache, reduced
// to the events the collector reads and scrubbed of user, host and paths. The
// .bin files went through the trimmed schema (unknown fields dropped), the
// .json files are Bazel's own protojson; both must summarise identically,
// which proves the trimmed proto keeps Bazel's field numbers (ADR 1001).
//
// Guards: §10.4 client metrics (wall time, critical path, runner counts,
// NetworkMetrics, heap), NFR-P3/T22 ratios, "report tests that needed retries".
func TestSummaryOfBazel92Streams(t *testing.T) {
	for _, tc := range []struct {
		fixture         string
		wall, critical  time.Duration
		runners         map[string]int
		remoteHitRatio  float64
		local           int
		peakHeap        int64
		netSent, netRcv uint64
		processStats    string
	}{
		{
			fixture: "local-cold", wall: 9539 * time.Millisecond, critical: 633496418 * time.Nanosecond,
			runners: map[string]int{RunnerTotal: 8, RunnerInternal: 6, "darwin-sandbox": 3}, local: 3,
			peakHeap: 166081216, netSent: 2540544, netRcv: 71878656,
			processStats: "8 processes: 6 internal, 3 darwin-sandbox.",
		},
		{
			fixture: "disk-cache-warm", wall: 3907 * time.Millisecond, critical: 151647542 * time.Nanosecond,
			runners: map[string]int{RunnerTotal: 8, RunnerInternal: 6, RunnerDiskCacheHit: 2, "darwin-sandbox": 1}, local: 1,
			peakHeap: 166254184, netSent: 71153664, netRcv: 941626368,
			processStats: "8 processes: 2 disk cache hit, 6 internal, 1 darwin-sandbox.",
		},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			fromJSON, err := ReadFile(filepath.Join("testdata", tc.fixture+".json"))
			require.NoError(t, err)
			fromBinary, err := ReadFile(filepath.Join("testdata", tc.fixture+".bin"))
			require.NoError(t, err)
			require.Equal(t, fromJSON, fromBinary, "JSON and binary streams must summarise identically")

			s := fromJSON
			require.True(t, s.Success())
			require.Equal(t, "test", s.Command)
			require.Equal(t, "9.2.0", s.BuildToolVersion)
			require.Equal(t, tc.wall, s.WallTime)
			require.Equal(t, tc.critical, s.CriticalPath)
			require.Contains(t, s.CriticalPathLog, "Critical Path:")
			require.Equal(t, tc.processStats, s.ProcessStats)
			for name, n := range tc.runners {
				require.Equal(t, n, s.Runner(name), name)
			}
			require.Equal(t, 2, s.Spawns())
			require.Equal(t, tc.local, s.LocalExecutions())
			require.Zero(t, s.RemoteCacheHitRatio(), "a disk cache hit is not a remote cache hit")
			require.Equal(t, tc.peakHeap, s.PeakPostGCHeapBytes)
			require.Equal(t, tc.netSent, s.NetworkBytesSent)
			require.Equal(t, tc.netRcv, s.NetworkBytesReceived)
			require.Len(t, s.Tests, 1)
			require.Equal(t, Test{Label: "//:t", Status: "PASSED", Attempts: 1, Runs: 1, Duration: s.Tests[0].Duration, Strategy: s.Tests[0].Strategy}, s.Tests[0])
			require.Empty(t, s.RetriedTests())
			require.Empty(t, s.FailedTests())
		})
	}
}

// Guards NFR-P3 / T2 (≥ 99 % remote cache hits) and T22 (≥ 95 % of actions
// remote): the ratios exclude Bazel-internal actions.
func TestRemoteRatios(t *testing.T) {
	s := &Summary{Runners: []RunnerCount{
		{Name: RunnerTotal, Count: 1010}, {Name: RunnerInternal, Count: 10},
		{Name: RunnerRemoteCacheHit, Count: 990, ExecKind: "Remote"}, {Name: RunnerRemote, Count: 5, ExecKind: "Remote"},
		{Name: "linux-sandbox", Count: 5, ExecKind: "Local"},
	}}
	require.Equal(t, 1000, s.Spawns())
	require.InDelta(t, 0.99, s.RemoteCacheHitRatio(), 1e-9)
	require.InDelta(t, 0.995, s.RemoteRatio(), 1e-9)
	require.Equal(t, 5, s.LocalExecutions())

	retried := &Summary{Tests: []Test{{Label: "//a", Status: "FLAKY", Attempts: 2}, {Label: "//b", Status: "PASSED", Attempts: 1}, {Label: "//c", Status: "FAILED", Attempts: 3}}}
	require.Equal(t, []string{"//a", "//c"}, retried.RetriedTests())
	require.Equal(t, []string{"//c"}, retried.FailedTests())
}
