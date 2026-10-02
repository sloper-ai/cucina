// SPDX-License-Identifier: FSL-1.1-ALv2

// Package nfr holds the catalogue of PROMPT §8's non-functional targets and
// the calculators that turn collector measurements into pass/fail
// NFRResults ("NFR numbers computed, not eyeballed"). Scenarios call the
// calculators; the report generator aggregates the results per NFR ID and
// lists rows no scenario measured as "not measured".
package nfr

import (
	"fmt"
	"sort"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/harness"
)

// Def is one NFR row.
type Def struct {
	ID     string `json:"id"`
	Target string `json:"target"`
	// Scenarios measure it (report cross-reference).
	Scenarios []string `json:"scenarios"`
	// ReportOnly rows pass when measured and reported (no numeric gate).
	ReportOnly bool `json:"reportOnly,omitempty"`
}

// Catalog lists every NFR of PROMPT §8 in table order.
var Catalog = []Def{
	{ID: "NFR-P1", Target: "Cold start: Linux p50 ≤ 60 s, max ≤ 90 s; Windows (Fast Launch) p50 ≤ 120 s, max ≤ 180 s; macOS VM p50 ≤ 45 s, max ≤ 90 s", Scenarios: []string{"T1", "T4", "T13", "canary-exec"}},
	{ID: "NFR-P2", Target: "Cold //absl/... build, pool max 4: excl. cold start ≤ 50 % of a local build on one worker-type instance; incl. cold start faster than it; speed-up vs the client VM reported", Scenarios: []string{"T1", "T4"}},
	{ID: "NFR-P3", Target: "Warm rebuild after clean --expunge: ≥ 99 % remote cache hits, 0 workers launched, wall ≤ 25 % of the cold build", Scenarios: []string{"T2", "T5"}},
	{ID: "NFR-P4", Target: "Queue time p95 ≤ 1 s while free slots exist; worker overhead (input root + output upload) p50 ≤ 100 ms with warm L1 (p95 reported)", Scenarios: []string{"T1", "T3", "T4"}},
	{ID: "NFR-M1", Target: "Control plane ≤ 2 GiB total RSS idle, ≤ 4 GiB under test load; no OOM kills", Scenarios: []string{"T0", "T1", "T7"}},
	{ID: "NFR-M2", Target: "Actuals reported; limits: cucina-controller ≤ 256 MiB, cucina-hostd ≤ 100 MiB, bb_worker ≤ 1 GiB at full concurrency", Scenarios: []string{"T1", "T13"}},
	{ID: "NFR-M3", Target: "Bazel client peak RSS with Build without the Bytes vs full downloads reported", Scenarios: []string{"T1", "T3"}, ReportOnly: true},
	{ID: "NFR-T1", Target: "Cold remote build with BwoB: client download bytes ≤ 10 % of total action output bytes", Scenarios: []string{"T1"}},
	{ID: "NFR-T2", Target: "Second fresh client uploads ≤ 1 % of the first client's bytes (toolchain uploads reported separately)", Scenarios: []string{"T21"}},
	{ID: "NFR-T3", Target: "L1: EC2 re-execution within one scale-out ≥ 90 % of input bytes from L1; after scale-to-zero from same-AZ L3; macOS after VM restart ≥ 90 % from L1", Scenarios: []string{"T3", "T6", "T13"}},
	{ID: "NFR-T4", Target: "zstd active client↔frontend and host↔control plane; ratio and CPU cost reported", Scenarios: []string{"T1", "T13"}},
	{ID: "NFR-T5", Target: "Cross-AZ bytes ≈ 0 in the test topology; data-transfer charges reported", Scenarios: []string{"T15"}},
	{ID: "NFR-T6", Target: "Second Mac VM on the same host fetches ≤ 10 % of the first VM's WAN bytes", Scenarios: []string{"T13"}},
	{ID: "NFR-T7", Target: "With the repo contents cache seeded, a fresh client's cold build downloads ≤ 50 MB of external-repository bytes", Scenarios: []string{"T21"}},
	{ID: "NFR-T8", Target: "Workers fetch ≤ 20 % of compile actions' input-root bytes (virtual build directories)", Scenarios: []string{"T1"}},
	{ID: "NFR-T9", Target: "Zero NAT-gateway bytes and zero same-AZ public-IP bytes; worker internet egress ≈ 0 apart from SSM/ECR", Scenarios: []string{"T15"}},
	{ID: "NFR-C1", Target: "At zero scale: zero worker instances, pool volumes, ENIs, EIPs, public IPs; no idle worker beyond idleTimeout + drain grace; pool standing cost = AMIs (+ Fast Launch)", Scenarios: []string{"T0", "T8", "T15"}},
	{ID: "NFR-C2", Target: "Itemised monthly standing cost for the test topology and a small production topology", Scenarios: []string{"T15"}, ReportOnly: true},
	{ID: "NFR-C3", Target: "Campaign spend within the $300 budget, itemised", Scenarios: []string{"T15"}},
	{ID: "NFR-C4", Target: "Cost-aware defaults: smallest fitting root volumes, gp3, no EIPs, public IPv4 only without another egress path (SHOULD: $/build benchmark)", Scenarios: []string{"T1", "T4"}},
	{ID: "NFR-R1", Target: "Every §10 build succeeds despite the injected failures", Scenarios: []string{"T9a", "T9b", "T9c", "T9d", "T9e"}},
	{ID: "NFR-R2", Target: "Control-plane pod restarts lose no cache contents", Scenarios: []string{"T9b", "T9c", "T11"}},
	{ID: "NFR-R3", Target: "Controller restarts leak no instances and terminate no busy worker", Scenarios: []string{"T9d"}},
	{ID: "NFR-R4", Target: "Orphaned workers power themselves off and terminate", Scenarios: []string{"T9e"}},
	{ID: "NFR-X1", Target: "≥ 99 % of compile/link actions on the selected compile pool; 100 % of test actions on the target's runner", Scenarios: []string{"T16", "T17"}},
	{ID: "NFR-X2", Target: "Test outcomes equal the local baseline wherever the target runs locally; every deviation has a root cause", Scenarios: []string{"T16", "T19"}},
	{ID: "NFR-X3", Target: "Rebuilding a configuration from a second client host gives ≥ 99 % cache hits", Scenarios: []string{"T18"}},
	{ID: "NFR-X4", Target: "Per-configuration and whole-matrix wall time reported; pools scale independently; no starvation", Scenarios: []string{"T16", "T7"}},
	{ID: "NFR-X5", Target: "Toolchain/SDK/CRT bytes uploaded once per toolchain version, not per configuration", Scenarios: []string{"T16"}},
}

// Lookup returns a catalogue row.
func Lookup(id string) (Def, bool) {
	for _, d := range Catalog {
		if d.ID == id {
			return d, true
		}
	}
	return Def{}, false
}

func pct(n, d float64) float64 {
	if d == 0 {
		return 0
	}
	return 100 * n / d
}

// Percentile is the nearest-rank percentile of samples (p in (0, 100]).
func Percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), samples...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(p/100*float64(len(s)) + 0.999999999)
	rank = max(1, min(rank, len(s)))
	return s[rank-1]
}

// ColdStartTargets are NFR-P1's per-OS limits.
var ColdStartTargets = map[string]struct{ P50, Max time.Duration }{
	"linux":   {60 * time.Second, 90 * time.Second},
	"windows": {120 * time.Second, 180 * time.Second},
	"macos":   {45 * time.Second, 90 * time.Second},
}

// ColdStart evaluates NFR-P1 for one OS from per-VM cold-start samples
// (Execute submitted with the pool at zero → first action executing).
func ColdStart(osName string, samples []time.Duration) []harness.NFRResult {
	t, ok := ColdStartTargets[osName]
	if !ok || len(samples) == 0 {
		return nil // not measured (the report lists the row as such)
	}
	p50, mx := Percentile(samples, 50), Percentile(samples, 100)
	detail := fmt.Sprintf("n=%d samples: %v", len(samples), roundAll(samples))
	return []harness.NFRResult{
		{ID: "NFR-P1", Subject: osName + " p50", Measured: p50.Seconds(), Unit: "s", Target: fmt.Sprintf("≤ %.0f s", t.P50.Seconds()), Pass: p50 <= t.P50, Detail: detail},
		{ID: "NFR-P1", Subject: osName + " max", Measured: mx.Seconds(), Unit: "s", Target: fmt.Sprintf("≤ %.0f s", t.Max.Seconds()), Pass: mx <= t.Max, Detail: detail},
	}
}

func roundAll(ds []time.Duration) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Round(100 * time.Millisecond).String()
	}
	return out
}

// ColdBuild evaluates NFR-P2. remoteWall includes the cold start; coldStart
// is the first VM's cold start; localWorker is the same build run locally on
// one worker-type instance; localClient is the local build on the client VM
// (speed-up reported). Without a worker-type baseline NFR-P2 is not
// evaluated (nil): the report shows it as not measured.
func ColdBuild(osName string, remoteWall, coldStart, localWorker, localClient time.Duration) []harness.NFRResult {
	if localWorker <= 0 || remoteWall <= 0 {
		return nil
	}
	excl := remoteWall - coldStart
	out := []harness.NFRResult{
		{ID: "NFR-P2", Subject: osName + " excl. cold start", Measured: pct(excl.Seconds(), localWorker.Seconds()), Unit: "% of local worker-type build",
			Target: "≤ 50 %", Pass: excl*2 <= localWorker, Detail: fmt.Sprintf("remote %s − cold start %s vs local %s", remoteWall.Round(time.Second), coldStart.Round(time.Second), localWorker.Round(time.Second))},
		{ID: "NFR-P2", Subject: osName + " incl. cold start", Measured: remoteWall.Seconds(), Unit: "s",
			Target: fmt.Sprintf("< local worker-type build (%s)", localWorker.Round(time.Second)), Pass: remoteWall < localWorker},
	}
	if localClient > 0 {
		out = append(out, harness.NFRResult{ID: "NFR-P2", Subject: osName + " speed-up vs client VM", Measured: localClient.Seconds() / remoteWall.Seconds(), Unit: "×",
			Target: "reported", Pass: true})
	}
	return out
}

// WarmRebuild evaluates NFR-P3.
func WarmRebuild(osName string, hitRatio float64, workersLaunched int, warm, cold time.Duration) []harness.NFRResult {
	return []harness.NFRResult{
		{ID: "NFR-P3", Subject: osName + " cache hits", Measured: 100 * hitRatio, Unit: "%", Target: "≥ 99 %", Pass: hitRatio >= 0.99},
		{ID: "NFR-P3", Subject: osName + " workers launched", Measured: float64(workersLaunched), Target: "0", Pass: workersLaunched == 0},
		{ID: "NFR-P3", Subject: osName + " wall vs cold", Measured: pct(warm.Seconds(), cold.Seconds()), Unit: "% of cold build", Target: "≤ 25 %",
			Pass: cold > 0 && warm*4 <= cold, Detail: fmt.Sprintf("warm %s, cold %s", warm.Round(time.Second), cold.Round(time.Second))},
	}
}

// QueueAndOverhead evaluates NFR-P4 from the execution log (client view of
// the worker's ExecutedActionMetadata).
func QueueAndOverhead(subject string, queueP95, overheadP50, overheadP95 time.Duration) []harness.NFRResult {
	return []harness.NFRResult{
		{ID: "NFR-P4", Subject: subject + " queue p95", Measured: queueP95.Seconds(), Unit: "s", Target: "≤ 1 s", Pass: queueP95 <= time.Second},
		{ID: "NFR-P4", Subject: subject + " overhead p50", Measured: float64(overheadP50.Milliseconds()), Unit: "ms", Target: "≤ 100 ms",
			Pass: overheadP50 <= 100*time.Millisecond, Detail: fmt.Sprintf("p95 %s", overheadP95.Round(time.Millisecond))},
	}
}

const gib = 1 << 30

// ControlPlaneMemory evaluates NFR-M1.
func ControlPlaneMemory(idleBytes, loadBytes float64, oomKills int) []harness.NFRResult {
	var out []harness.NFRResult
	if idleBytes > 0 {
		out = append(out, harness.NFRResult{ID: "NFR-M1", Subject: "idle RSS", Measured: idleBytes / gib, Unit: "GiB", Target: "≤ 2 GiB", Pass: idleBytes <= 2*gib})
	}
	if loadBytes > 0 {
		out = append(out, harness.NFRResult{ID: "NFR-M1", Subject: "load RSS", Measured: loadBytes / gib, Unit: "GiB", Target: "≤ 4 GiB", Pass: loadBytes <= 4*gib})
	}
	return append(out, harness.NFRResult{ID: "NFR-M1", Subject: "OOM kills", Measured: float64(oomKills), Target: "0", Pass: oomKills == 0})
}

// ComponentLimit evaluates one NFR-M2 limit (component peak RSS in bytes).
func ComponentLimit(component string, peakBytes, limitBytes float64) harness.NFRResult {
	return harness.NFRResult{ID: "NFR-M2", Subject: component, Measured: peakBytes / (1 << 20), Unit: "MiB",
		Target: fmt.Sprintf("≤ %.0f MiB", limitBytes/(1<<20)), Pass: peakBytes > 0 && peakBytes <= limitBytes}
}

// ClientPeakRSS reports NFR-M3.
func ClientPeakRSS(osName string, bwobBytes, fullBytes int64) harness.NFRResult {
	return harness.NFRResult{ID: "NFR-M3", Subject: osName, Measured: float64(bwobBytes) / (1 << 20), Unit: "MiB (BwoB)", Target: "reported",
		Pass: bwobBytes > 0, Detail: fmt.Sprintf("full downloads: %.0f MiB", float64(fullBytes)/(1<<20))}
}

// Ratio evaluates "measured ≤ / ≥ limit %" style NFRs (T1, T2, T3, T6, T8).
func Ratio(id, subject string, num, den, limitPct float64, atMost bool, detail string) harness.NFRResult {
	m := pct(num, den)
	pass := den > 0 && ((atMost && m <= limitPct) || (!atMost && m >= limitPct))
	op := "≥"
	if atMost {
		op = "≤"
	}
	return harness.NFRResult{ID: id, Subject: subject, Measured: m, Unit: "%", Target: fmt.Sprintf("%s %g %%", op, limitPct), Pass: pass,
		Detail: fmt.Sprintf("%s (%.0f / %.0f bytes)", detail, num, den)}
}

// AtMostBytes evaluates absolute byte limits (T7: ≤ 50 MB; T5/T9: ≈ 0).
func AtMostBytes(id, subject string, bytes, limit float64, detail string) harness.NFRResult {
	return harness.NFRResult{ID: id, Subject: subject, Measured: bytes / 1e6, Unit: "MB", Target: fmt.Sprintf("≤ %g MB", limit/1e6), Pass: bytes <= limit, Detail: detail}
}

// Residue evaluates NFR-C1's zero-scale condition.
func Residue(subject string, instances, volumes, enis, eips, publicIPs int) harness.NFRResult {
	n := instances + volumes + enis + eips + publicIPs
	return harness.NFRResult{ID: "NFR-C1", Subject: subject, Measured: float64(n), Unit: "resources", Target: "0", Pass: n == 0,
		Detail: fmt.Sprintf("%d instances, %d volumes, %d ENIs, %d EIPs, %d public IPs", instances, volumes, enis, eips, publicIPs)}
}

// Spend evaluates NFR-C3.
func Spend(spentUSD, budgetUSD float64) harness.NFRResult {
	return harness.NFRResult{ID: "NFR-C3", Subject: "campaign", Measured: spentUSD, Unit: "USD", Target: fmt.Sprintf("≤ $%.0f", budgetUSD), Pass: spentUSD <= budgetUSD}
}

// Routing evaluates NFR-X1 for one configuration.
func Routing(config string, compileOnPool, compileTotal, testsOnRunner, testsTotal int) []harness.NFRResult {
	return []harness.NFRResult{
		{ID: "NFR-X1", Subject: config + " compile/link on pool", Measured: pct(float64(compileOnPool), float64(compileTotal)), Unit: "%", Target: "≥ 99 %",
			Pass: compileTotal == 0 || 100*compileOnPool >= 99*compileTotal, Detail: fmt.Sprintf("%d/%d", compileOnPool, compileTotal)},
		{ID: "NFR-X1", Subject: config + " tests on target runner", Measured: pct(float64(testsOnRunner), float64(testsTotal)), Unit: "%", Target: "100 %",
			Pass: testsOnRunner == testsTotal, Detail: fmt.Sprintf("%d/%d", testsOnRunner, testsTotal)},
	}
}

// Deviation is one test whose remote outcome differs from the local baseline.
type Deviation struct {
	Label     string `json:"label"`
	Remote    string `json:"remote"`
	Local     string `json:"local"`
	RootCause string `json:"rootCause,omitempty"`
}

// Outcomes evaluates NFR-X2: deviations between remote and local test
// outcomes (labels present in both), each needing a recorded root cause.
func Outcomes(config string, remote, local map[string]string, rootCauses map[string]string) (harness.NFRResult, []Deviation) {
	var devs []Deviation
	labels := make([]string, 0, len(remote))
	for l := range remote {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	unexplained := 0
	for _, l := range labels {
		lo, ok := local[l]
		if !ok || normalize(lo) == normalize(remote[l]) {
			continue
		}
		d := Deviation{Label: l, Remote: remote[l], Local: lo, RootCause: rootCauses[l]}
		if d.RootCause == "" {
			unexplained++
		}
		devs = append(devs, d)
	}
	return harness.NFRResult{ID: "NFR-X2", Subject: config, Measured: float64(len(devs)), Unit: "deviations", Target: "0 unexplained",
		Pass: unexplained == 0, Detail: fmt.Sprintf("%d deviations, %d without root cause", len(devs), unexplained)}, devs
}

// normalize treats FLAKY (passed after retries) as PASSED for the outcome
// comparison; retries are reported separately.
func normalize(s string) string {
	if s == "FLAKY" {
		return "PASSED"
	}
	return s
}

// CacheHits evaluates hit-ratio rows (NFR-X3, T11/T9c warm builds).
func CacheHits(id, subject string, hitRatio float64) harness.NFRResult {
	return harness.NFRResult{ID: id, Subject: subject, Measured: 100 * hitRatio, Unit: "%", Target: "≥ 99 %", Pass: hitRatio >= 0.99}
}

// Count evaluates "exactly zero" counters (R1 failed builds, R3 leaked or
// busy-terminated instances, X5 duplicate toolchain uploads).
func Count(id, subject string, n int, detail string) harness.NFRResult {
	return harness.NFRResult{ID: id, Subject: subject, Measured: float64(n), Target: "0", Pass: n == 0, Detail: detail}
}

// Reported records a report-only measurement.
func Reported(id, subject string, v float64, unit, detail string) harness.NFRResult {
	return harness.NFRResult{ID: id, Subject: subject, Measured: v, Unit: unit, Target: "reported", Pass: true, Detail: detail}
}

// Status is an aggregated NFR verdict.
type Status string

const (
	Pass        Status = "PASS"
	Fail        Status = "FAIL"
	NotMeasured Status = "NOT MEASURED"
)

// Row is one line of the report's NFR table.
type Row struct {
	Def
	Status       Status              `json:"status"`
	Measurements []harness.NFRResult `json:"measurements"`
	From         []string            `json:"from"` // scenario IDs
}

// Aggregate builds the NFR table from scenario results: a row passes when it
// has measurements and all pass; report-only rows pass when measured.
func Aggregate(results []*harness.Result) []Row {
	by := map[string]*Row{}
	for _, d := range Catalog {
		by[d.ID] = &Row{Def: d, Status: NotMeasured}
	}
	for _, r := range results {
		if r.Status == harness.StatusSkip {
			continue
		}
		for _, m := range r.NFRs {
			row, ok := by[m.ID]
			if !ok {
				continue
			}
			row.Measurements = append(row.Measurements, m)
			if !contains(row.From, r.ID) {
				row.From = append(row.From, r.ID)
			}
		}
	}
	var out []Row
	for _, d := range Catalog {
		row := by[d.ID]
		if len(row.Measurements) > 0 {
			row.Status = Pass
			for _, m := range row.Measurements {
				if !m.Pass {
					row.Status = Fail
				}
			}
		}
		out = append(out, *row)
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
