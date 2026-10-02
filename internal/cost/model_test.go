// SPDX-License-Identifier: FSL-1.1-ALv2

package cost

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func testModel(t testing.TB) Model {
	rates, ok := DefaultRates("us-west-1")
	require.True(t, ok)
	m := Model{Rates: rates}
	m.SetInstancePrice("t4g.nano", false, 0.005)
	m.SetInstancePrice("m6id.large", true, 0.23165)
	m.SetInstancePrice("c7i.large", false, 0.1)
	return m
}

var now = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func at(day, hour, minute, second int) time.Time {
	return time.Date(2026, 10, day, hour, minute, second, 0, time.UTC)
}

// R-OBS-5 / R-DATA-7 / NFR-C1: one month of a small fleet, checked against hand
// arithmetic from the us-west-1 rates (see the comments for each number).
func TestSummarizeExample(t *testing.T) {
	m := testModel(t)
	u := Usage{
		Launches: []Launch{
			// 30 s → billed 60 s: 60/3600 × $0.005 = $0.0000833.
			{Pool: "linux", Type: "t4g.nano", Start: at(15, 10, 0, 0), End: at(15, 10, 0, 30)},
			// 2 h across midnight: $0.01 this month, $0.005 today; gp3 20 GiB with 1000 IOPS and
			// 125 MiB/s above the baseline; a public IPv4; 8 GiB initialized at 200 MiB/s.
			{Pool: "linux", Type: "t4g.nano", Start: at(14, 23, 0, 0), End: at(15, 1, 0, 0), PublicIPv4: true,
				Volumes: []Volume{{Type: "gp3", SizeGiB: 20, IOPS: 4000, ThroughputMiBps: 250, InitRateMiBps: 200, InitGiB: 8}}},
			// Still running for 1 h: $0.23165 (Windows licence included in the price).
			{Pool: "win", Type: "m6id.large", Windows: true, Start: at(15, 11, 0, 0)},
		},
		Images:     []Image{{Pool: "linux", ImageID: "ami-1", SnapshotGiB: 8}},
		FastLaunch: []FastLaunch{{Pool: "win", ImageID: "ami-w", Snapshots: 4, SnapshotGiB: 30, Since: at(10, 0, 0, 0)}},
		Transfer: []Transfer{
			{Pool: "linux", Path: PathInternetEgress, Bytes: 150 << 30, At: at(5, 0, 0, 0)}, // 50 GB above the free tier: $4.50
			{Pool: "linux", Path: PathInterAZ, Bytes: 10 << 30, At: at(15, 9, 0, 0)},        // 10 GB × $0.01 × 2 directions
		},
	}
	r := m.Summarize(u, now)

	assert.Equal(t, []PoolCost{
		{Pool: "linux", InstanceSeconds: 60 + 7200, Compute: 10083,
			EBS:          61336,     // 20×0.096×2/730 + 1000×0.006×2/730 + 125×0.048×2/730 + 8×0.0029
			PublicIPv4:   10000,     // 2 h × $0.005
			DataTransfer: 4_700_000, // $4.50 + $0.20
			Standing:     209753,    // 8 GiB × $0.055 × 348 h / 730
		},
		{Pool: "win", InstanceSeconds: 3600, Compute: 231650,
			Standing: 1193425, // 4 × 30 GiB × $0.055 × 132 h / 730
		},
	}, r.Pools)
	assert.Equal(t, Micros(6_416_247), r.MonthToDate)
	assert.Equal(t, Micros(576_527), r.Today)
	assert.Equal(t, Micros(7_040_000), r.StandingPerMonth, "8 GiB AMI ($0.44) + 120 GiB of Fast Launch snapshots ($6.60)")
	assert.Equal(t, map[string]Micros{CategoryAMIStorage: 440_000, CategoryFastLaunch: 6_600_000}, r.StandingByCategory)
	assert.Empty(t, r.Unpriced)
	assert.Contains(t, r.Assumptions, "2026-10-02")
	var sum Micros
	for _, l := range r.Lines {
		sum += l.Amount
	}
	assert.Equal(t, r.MonthToDate, sum, "lines itemise the month")
}

// The price of an unknown instance type is reported instead of silently guessed.
func TestSummarizeReportsUnpricedTypes(t *testing.T) {
	r := testModel(t).Summarize(Usage{Launches: []Launch{{Pool: "p", Type: "z9.huge", Start: at(15, 0, 0, 0)}}}, now)
	assert.Equal(t, []string{"z9.huge"}, r.Unpriced)
	assert.Equal(t, int64(12*3600), r.Pools[0].InstanceSeconds)
}

// ---------------------------------------------------------------- properties

var types = []string{"t4g.nano", "c7i.large", "m6id.large"}

func genLaunch(t *rapid.T) Launch {
	start := now.Add(-time.Duration(rapid.Int64Range(0, int64(40*24*time.Hour)).Draw(t, "ago")))
	l := Launch{
		Pool:       rapid.SampledFrom([]string{"a", "b"}).Draw(t, "pool"),
		Type:       rapid.SampledFrom(types).Draw(t, "type"),
		Start:      start,
		PublicIPv4: rapid.Bool().Draw(t, "ipv4"),
	}
	l.Windows = l.Type == "m6id.large"
	if rapid.Bool().Draw(t, "ended") {
		l.End = start.Add(time.Duration(rapid.Int64Range(0, int64(48*time.Hour)).Draw(t, "dur")))
	}
	if rapid.Bool().Draw(t, "volume") {
		l.Volumes = []Volume{{Type: "gp3", SizeGiB: rapid.IntRange(1, 500).Draw(t, "size"),
			IOPS: rapid.IntRange(0, 16000).Draw(t, "iops"), ThroughputMiBps: rapid.IntRange(0, 1000).Draw(t, "tp"),
			InitRateMiBps: rapid.SampledFrom([]int{0, 100, 300}).Draw(t, "init"), InitGiB: rapid.IntRange(0, 50).Draw(t, "initGiB")}}
	}
	return l
}

func genUsage(t *rapid.T) Usage {
	u := Usage{Launches: rapid.SliceOfN(rapid.Custom(genLaunch), 0, 6).Draw(t, "launches")}
	for i := range rapid.IntRange(0, 3).Draw(t, "images") {
		u.Images = append(u.Images, Image{Pool: "a", ImageID: "ami", SnapshotGiB: float64(rapid.IntRange(1, 60).Draw(t, "imgGiB")),
			Since: now.Add(-time.Duration(rapid.IntRange(0, 40*24).Draw(t, "imgAge")) * time.Hour)})
		_ = i
	}
	for range rapid.IntRange(0, 4).Draw(t, "transfers") {
		u.Transfer = append(u.Transfer, Transfer{Pool: rapid.SampledFrom([]string{"a", "b"}).Draw(t, "tpool"),
			Path:  rapid.SampledFrom([]Path{PathInternetEgress, PathInterAZ, PathNAT, PathPublicIP}).Draw(t, "path"),
			Bytes: rapid.Int64Range(0, 300<<30).Draw(t, "bytes"),
			At:    now.Add(-time.Duration(rapid.Int64Range(0, int64(40*24*time.Hour)).Draw(t, "tAgo")))})
	}
	return u
}

// R-OBS-5: more usage never costs less — adding any record or extending a launch
// never lowers today's, the month's or any category's total.
func TestPropertyMonotone(t *testing.T) {
	m := testModel(t)
	rapid.Check(t, func(t *rapid.T) {
		u := genUsage(t)
		before := m.Summarize(u, now)
		more := u
		more.Launches = append(append([]Launch{}, u.Launches...), genLaunch(t))
		if len(u.Launches) > 0 && rapid.Bool().Draw(t, "extend") {
			i := rapid.IntRange(0, len(u.Launches)-1).Draw(t, "which")
			more.Launches = append([]Launch{}, u.Launches...)
			if !more.Launches[i].End.IsZero() {
				more.Launches[i].End = more.Launches[i].End.Add(time.Duration(rapid.Int64Range(0, int64(time.Hour)).Draw(t, "more")))
			}
		}
		more.Transfer = append(append([]Transfer{}, u.Transfer...), Transfer{Pool: "b", Path: PathInternetEgress,
			Bytes: rapid.Int64Range(0, 200<<30).Draw(t, "egress"), At: now.Add(-time.Hour)})
		after := m.Summarize(more, now)
		if after.MonthToDate < before.MonthToDate || after.Today < before.Today {
			t.Fatalf("cost decreased: month %d → %d, today %d → %d", before.MonthToDate, after.MonthToDate, before.Today, after.Today)
		}
		bc, ac := byCategory(before), byCategory(after)
		for c, v := range bc {
			if ac[c] < v {
				t.Fatalf("category %s decreased: %d → %d", c, v, ac[c])
			}
		}
	})
}

func byCategory(r Report) map[string]Micros {
	out := map[string]Micros{}
	for _, l := range r.Lines {
		out[l.Category] += l.Amount
	}
	return out
}

// R-OBS-5: every launch is billed at least 60 s, however short it was.
func TestPropertySixtySecondMinimum(t *testing.T) {
	m := testModel(t)
	rapid.Check(t, func(t *rapid.T) {
		typ := rapid.SampledFrom(types).Draw(t, "type")
		windows := typ == "m6id.large"
		start := now.Add(-time.Duration(rapid.Int64Range(int64(time.Minute), int64(10*24*time.Hour)).Draw(t, "ago")))
		d := time.Duration(rapid.Int64Range(0, int64(2*time.Minute)).Draw(t, "dur"))
		r := m.Summarize(Usage{Launches: []Launch{{Pool: "p", Type: typ, Windows: windows, Start: start, End: start.Add(d)}}}, now)
		price := m.InstanceUSDPerHour[InstanceKey{typ, windows}]
		elapsed := min(d, now.Sub(start)) // a launch still running is priced up to now
		want := micros(max(elapsed, time.Minute).Hours() * price)
		if got := r.Pools[0].Compute; got != want {
			t.Fatalf("%s for %v: billed %d µ$, want %d µ$", typ, d, got, want)
		}
		if got := r.Pools[0].InstanceSeconds; got < MinimumBilledSeconds {
			t.Fatalf("billed %d s < 60 s", got)
		}
	})
}

// NFR-C1: with zero instances the only cost is standing cost — AMI snapshots and
// Fast Launch snapshots — and the month's spend is exactly their accrual.
func TestPropertyZeroInstancesOnlyStandingCost(t *testing.T) {
	m := testModel(t)
	rapid.Check(t, func(t *rapid.T) {
		var u Usage
		for range rapid.IntRange(0, 3).Draw(t, "images") {
			u.Images = append(u.Images, Image{Pool: "a", ImageID: "ami", SnapshotGiB: float64(rapid.IntRange(1, 60).Draw(t, "gib"))})
		}
		for range rapid.IntRange(0, 2).Draw(t, "fl") {
			u.FastLaunch = append(u.FastLaunch, FastLaunch{Pool: "w", ImageID: "ami-w", Snapshots: rapid.IntRange(1, 10).Draw(t, "n"),
				SnapshotGiB: float64(rapid.IntRange(30, 60).Draw(t, "flGiB"))})
		}
		r := m.Summarize(u, now)
		var standing Micros
		for _, l := range r.Lines {
			if l.Category != CategoryAMIStorage && l.Category != CategoryFastLaunch {
				t.Fatalf("non-standing line %+v with zero instances", l)
			}
			standing += l.Amount
		}
		if r.MonthToDate != standing {
			t.Fatalf("month %d ≠ standing %d", r.MonthToDate, standing)
		}
		var perMonth float64
		for _, img := range u.Images {
			perMonth += img.SnapshotGiB * m.Rates.SnapshotGBMonth
		}
		for _, fl := range u.FastLaunch {
			perMonth += float64(fl.Snapshots) * fl.SnapshotGiB * m.Rates.SnapshotGBMonth
		}
		if d := r.StandingPerMonth - micros(perMonth); d < -2 || d > 2 {
			t.Fatalf("standing per month %d, want %d", r.StandingPerMonth, micros(perMonth))
		}
		for _, p := range r.Pools {
			if p.Compute != 0 || p.EBS != 0 || p.PublicIPv4 != 0 || p.DataTransfer != 0 || p.InstanceSeconds != 0 {
				t.Fatalf("pool %+v has non-standing cost", p)
			}
		}
	})
}
