// SPDX-License-Identifier: FSL-1.1-ALv2

package nfr

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/harness"
)

func s(sec ...float64) []time.Duration {
	out := make([]time.Duration, len(sec))
	for i, x := range sec {
		out[i] = time.Duration(x * float64(time.Second))
	}
	return out
}

func passes(rs []harness.NFRResult) []bool {
	out := make([]bool, len(rs))
	for i, r := range rs {
		out[i] = r.Pass
	}
	return out
}

// Guards §8's computed verdicts at their boundaries (PROMPT §10.3 "pass
// criteria … computed, not eyeballed").
func TestCalculators(t *testing.T) {
	// NFR-P1: [p50, max] per OS.
	require.Equal(t, []bool{true, true}, passes(ColdStart("linux", s(40, 60, 90))))
	require.Equal(t, []bool{true, false}, passes(ColdStart("linux", s(40, 50, 91))))
	require.Equal(t, []bool{false, true}, passes(ColdStart("macos", s(46, 46, 50))))
	require.Equal(t, []bool{true, true}, passes(ColdStart("windows", s(85, 120, 180))))
	require.Nil(t, ColdStart("linux", nil), "no samples: not measured")

	// NFR-P2: exactly 50 % passes; including the cold start it must be strictly faster.
	p2 := ColdBuild("linux", 600*time.Second, 100*time.Second, 1000*time.Second, 4000*time.Second)
	require.Equal(t, []bool{true, true, true}, passes(p2))
	require.InDelta(t, 4000.0/600, p2[2].Measured, 1e-9)
	require.Equal(t, []bool{false, false}, passes(ColdBuild("linux", 1000*time.Second, 400*time.Second, 1000*time.Second, 0)))
	require.Nil(t, ColdBuild("linux", 600*time.Second, 60*time.Second, 0, 4000*time.Second), "no worker-type baseline: not measured")

	// NFR-P3: hits, launches, wall.
	require.Equal(t, []bool{true, true, true}, passes(WarmRebuild("linux", 0.99, 0, 250*time.Second, 1000*time.Second)))
	require.Equal(t, []bool{false, false, false}, passes(WarmRebuild("linux", 0.989, 1, 251*time.Second, 1000*time.Second)))

	// NFR-P4.
	require.Equal(t, []bool{true, true}, passes(QueueAndOverhead("linux", time.Second, 100*time.Millisecond, time.Second)))
	require.Equal(t, []bool{false, false}, passes(QueueAndOverhead("linux", 1001*time.Millisecond, 101*time.Millisecond, time.Second)))

	// Ratios: T1 (≤ 10 %), T3 (≥ 90 %); an empty denominator never passes.
	require.True(t, Ratio("NFR-T1", "linux", 10, 100, 10, true, "").Pass)
	require.False(t, Ratio("NFR-T1", "linux", 11, 100, 10, true, "").Pass)
	require.True(t, Ratio("NFR-T3", "linux", 90, 100, 90, false, "").Pass)
	require.False(t, Ratio("NFR-T3", "linux", 0, 0, 90, false, "").Pass)

	// NFR-M2 worker samples cannot be supplied by a different component.
	require.True(t, WorkerRSS(64<<20, true).Pass)
	require.False(t, WorkerRSS(2<<30, true).Pass)
	missingWorker := WorkerRSS(0, false)
	require.False(t, missingWorker.Pass)
	require.Equal(t, "bb_worker", missingWorker.Subject)
	require.NotEmpty(t, missingWorker.Unqualified)

	// NFR-X1: 99 % of compile/link, all tests.
	require.Equal(t, []bool{true, true}, passes(Routing("x86_64-linux-gnu", 99, 100, 7, 7, true)))
	require.Equal(t, []bool{false, false}, passes(Routing("x86_64-linux-gnu", 98, 100, 6, 7, true)))
	require.Equal(t, []bool{false, false}, passes(Routing("missing logs", 0, 0, 0, 0, true)))
	// Build-only (wasm, BPF): the test clause is not applicable.
	require.Equal(t, []bool{true}, passes(Routing("wasm32-unknown-unknown", 2, 2, 0, 0, false)))

	require.True(t, Residue("zero scale", 0, 0, 0, 0, 0).Pass)
	require.False(t, Residue("zero scale", 0, 1, 0, 0, 0).Pass)
	require.True(t, Spend(299.99, 300).Pass)
	require.False(t, Spend(300.01, 300).Pass)
}

// Guards NFR-X2: deviations from the local baseline need a root cause; a
// test that passed after retries counts as passed (retries are reported).
func TestOutcomes(t *testing.T) {
	remote := map[string]string{"//a": "PASSED", "//b": "FAILED", "//c": "FLAKY", "//d": "TIMEOUT", "//remote-only": "PASSED"}
	local := map[string]string{"//a": "PASSED", "//b": "PASSED", "//c": "PASSED", "//d": "PASSED"}
	r, devs := Outcomes("x86_64-linux-musl", remote, local, map[string]string{"//d": "qemu-user timeout scaling"})
	require.False(t, r.Pass)
	require.InDelta(t, 2.0, r.Measured, 1e-9)
	require.Equal(t, []Deviation{
		{Label: "//b", Remote: "FAILED", Local: "PASSED"},
		{Label: "//d", Remote: "TIMEOUT", Local: "PASSED", RootCause: "qemu-user timeout scaling"},
	}, devs)
	r, _ = Outcomes("x86_64-linux-musl", remote, local, map[string]string{"//b": "toolchain bug", "//d": "qemu"})
	require.True(t, r.Pass)
	r, _ = Outcomes("missing outcomes", nil, nil, nil)
	require.False(t, r.Pass, "empty comparisons cannot prove outcome equivalence")
}

// Guards the report's NFR table: every §8 row appears once; a row passes only
// when measured and all measurements pass; skipped scenarios do not count.
func TestAggregate(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Catalog {
		require.False(t, seen[d.ID], d.ID)
		seen[d.ID] = true
	}
	require.Len(t, Catalog, 29, "P1–P4, M1–M3, T1–T9, C1–C4, R1–R4, X1–X5")

	results := []*harness.Result{
		{ID: "T1", Status: harness.StatusFail, NFRs: append(ColdStart("linux", s(40, 95)), Ratio("NFR-T1", "linux", 1, 100, 10, true, ""))},
		{ID: "T2", Status: harness.StatusPass, NFRs: WarmRebuild("linux", 1, 0, time.Second, time.Hour)},
		{ID: "T4", Status: harness.StatusSkip, NFRs: ColdStart("windows", s(500))},
	}
	rows := map[string]Row{}
	for _, r := range Aggregate(results) {
		rows[r.ID] = r
	}
	require.Equal(t, Fail, rows["NFR-P1"].Status)
	require.Len(t, rows["NFR-P1"].Measurements, 2, "the skipped Windows run is ignored")
	require.NotEqual(t, Pass, rows["NFR-P3"].Status, "Linux-only measurements cannot pass a Linux+Windows requirement")
	require.Equal(t, Pass, rows["NFR-T1"].Status)
	require.Equal(t, []string{"T1"}, rows["NFR-T1"].From)
	require.Equal(t, NotMeasured, rows["NFR-X5"].Status)
}
