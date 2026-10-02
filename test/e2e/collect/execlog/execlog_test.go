// SPDX-License-Identifier: FSL-1.1-ALv2

package execlog

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fixtures are real Bazel 9.2.0 compact execution logs
// (--execution_log_compact_file) of a genrule + sh_test workspace: cold
// (sandboxed local execution) and warm from a disk cache. Guards §10.4
// "per-spawn … input bytes, from --execution_log_compact_file; write a small
// parser" and NFR-X3's premise that action keys are stable across runs.
func TestReadCompactExecutionLog(t *testing.T) {
	cold, err := ReadFile(filepath.Join("testdata", "local-cold.zst"))
	require.NoError(t, err)
	warm, err := ReadFile(filepath.Join("testdata", "disk-cache-warm.zst"))
	require.NoError(t, err)

	require.Equal(t, "SHA-256", cold.HashFunction)
	require.Len(t, cold.Spawns, 3)
	gen := cold.Spawns[0]
	require.Equal(t, "Genrule", gen.Mnemonic)
	require.Equal(t, "//:gen", gen.TargetLabel)
	require.Equal(t, "darwin-sandbox", gen.Runner)
	require.False(t, gen.CacheHit)
	require.Equal(t, Digest{Hash: "52a8f3bec5916e47e37ac583d0064dc880a7ba06cf74aead5011e9d66a92368a", SizeBytes: 145}, gen.ActionDigest)
	require.Equal(t, int64(6), gen.OutputBytes, `"hello\n"`)
	require.Equal(t, int64(1), gen.OutputFiles)
	require.Positive(t, gen.InputBytes)
	require.Equal(t, time.Date(2026, 10, 2, 8, 46, 28, 750_000_000, time.UTC), gen.Start)
	require.Equal(t, 181*time.Millisecond, gen.Timings.Execution)

	s := cold.Summarize()
	require.Equal(t, map[string]int{"darwin-sandbox": 3}, s.ByRunner)
	require.Equal(t, 3, s.LocalExecutions)
	require.Equal(t, int64(491), s.OutputBytes)

	w := warm.Summarize()
	require.Equal(t, map[string]int{"darwin-sandbox": 1, "disk cache hit": 2}, w.ByRunner)
	require.Equal(t, 2, w.OtherCacheHits)
	require.Zero(t, w.RemoteCacheHits, "a disk cache hit is not a remote cache hit")
	require.Equal(t, gen.ActionDigest, warm.Spawns[0].ActionDigest, "the same action has the same key in both runs")
}

// Guards NFR-P4 (queue time; worker overhead = input root setup + output
// upload, p50/p95 over remote executions) and NFR-X1 (≥ 99 % of compile/link
// actions on the selected pool, 100 % of tests on the target's runner).
func TestRemoteTimingsAndRouting(t *testing.T) {
	linux := map[string]string{"OSFamily": "linux", "ISA": "x86-64"}
	win := map[string]string{"OSFamily": "windows", "ISA": "x86-64"}
	remote := func(mn string, props map[string]string, queue, setup, outs time.Duration) Spawn {
		return Spawn{Mnemonic: mn, Runner: "remote", Platform: props, OutputBytes: 10,
			Timings: Timings{Queue: queue, Setup: setup, ProcessOutputs: outs}}
	}
	l := &Log{Spawns: []Spawn{
		remote("CppCompile", linux, 10*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond),
		remote("CppCompile", linux, 20*time.Millisecond, 40*time.Millisecond, 20*time.Millisecond),
		remote("CppLink", win, 2*time.Second, 50*time.Millisecond, 50*time.Millisecond), // misrouted
		remote("TestRunner", win, 30*time.Millisecond, 60*time.Millisecond, 40*time.Millisecond),
		{Mnemonic: "CppCompile", Runner: "remote cache hit", CacheHit: true, Platform: win},
		{Mnemonic: "Genrule", Runner: "linux-sandbox", OutputBytes: 5},
	}}

	s := l.Summarize()
	require.Equal(t, 4, s.RemoteExecutions)
	require.Equal(t, 1, s.RemoteCacheHits)
	require.Equal(t, 1, s.LocalExecutions)
	require.Equal(t, int64(45), s.OutputBytes)
	require.Equal(t, Distribution{N: 4, P50: 20 * time.Millisecond, P95: 2 * time.Second, Max: 2 * time.Second}, s.Queue)
	require.Equal(t, Distribution{N: 4, P50: 60 * time.Millisecond, P95: 100 * time.Millisecond, Max: 100 * time.Millisecond}, s.WorkerOverhead)
	require.InDelta(t, 5.0/6, s.RemoteRatio(), 1e-9)

	r := l.RouteCheck(linux, win)
	require.Equal(t, 3, r.CompileLinkTotal)
	require.Equal(t, 2, r.CompileLinkOnPool)
	require.InDelta(t, 200.0/3, r.CompileLinkPercent, 1e-9)
	require.Equal(t, 1, r.TestTotal)
	require.InDelta(t, 100.0, r.TestPercent, 1e-9)
	require.Equal(t, map[string]int{"ISA=x86-64;OSFamily=windows": 1}, r.Misrouted)
}
