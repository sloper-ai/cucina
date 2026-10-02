// SPDX-License-Identifier: FSL-1.1-ALv2

// Package spend prices what the e2e collectors measured — instance lifecycles
// and their volumes from the AWS sampler, AMI and Fast Launch snapshots, data
// transfer — with Cucina's own cost model (internal/cost), so the campaign
// report, the budget governor and `cucinactl cost` share one model
// (NFR-C2, NFR-C3, §12). internal/cost takes instance prices from the caller;
// InstancePrices is the dated Price List API snapshot for the campaign's
// instance types in us-west-1.
package spend

import (
	"time"

	"github.com/sloper-ai/cucina/internal/cost"
	"github.com/sloper-ai/cucina/test/e2e/collect/awsinv"
)

// PriceSource documents the instance price snapshot.
const PriceSource = "AWS Price List API, us-west-1, on-demand, shared tenancy, queried 2026-10-02 (Windows: licence included)"

// InstancePrices is the on-demand $/h of every instance type the campaign
// may run (k3s node, clients, the three pools and their fallbacks).
func InstancePrices() map[cost.InstanceKey]float64 {
	return map[cost.InstanceKey]float64{
		{Type: "c8i.8xlarge"}: 1.86976, {Type: "c7i.8xlarge"}: 1.7808, {Type: "c7a.8xlarge"}: 2.04784,
		{Type: "m6id.8xlarge"}: 2.2344, {Type: "c8g.8xlarge"}: 1.58592, {Type: "c7g.8xlarge"}: 1.4416,
		{Type: "c7gd.8xlarge"}: 1.8144, {Type: "m8i.2xlarge"}: 0.49392, {Type: "m7i.xlarge"}: 0.2352,
		{Type: "c7a.8xlarge", Windows: true}: 3.51984, {Type: "c8i.8xlarge", Windows: true}: 3.34176,
		{Type: "c7i.8xlarge", Windows: true}: 3.2528, {Type: "m7i.xlarge", Windows: true}: 0.4192,
	}
}

// Model returns the us-west-1 cost model with the instance price snapshot.
func Model() cost.Model {
	rates, _ := cost.DefaultRates("us-west-1")
	return cost.Model{Rates: rates, InstanceUSDPerHour: InstancePrices()}
}

// Launches converts sampled lifecycles into priced launches.
func Launches(lcs []awsinv.Lifecycle) []cost.Launch {
	var out []cost.Launch
	for _, l := range lcs {
		start := l.Launched
		if start.IsZero() {
			start = l.Running
		}
		end := l.Gone
		if end.IsZero() {
			end = l.LastSeen
		}
		cl := cost.Launch{Pool: l.Pool, Type: l.Type, Windows: l.Platform == "windows", Start: start, End: end, PublicIPv4: l.PublicIP}
		for _, v := range l.Volumes {
			cl.Volumes = append(cl.Volumes, cost.Volume{Type: "gp3", SizeGiB: int(v.GiB), IOPS: int(v.IOPS), ThroughputMiBps: int(v.MiBps)})
		}
		out = append(out, cl)
	}
	return out
}

// Bill is the priced usage of one scenario (or of the whole campaign).
type Bill struct {
	TotalUSD float64         `json:"totalUSD"`
	Lines    []cost.Line     `json:"lines"`
	Pools    []cost.PoolCost `json:"pools"`
	Unpriced []string        `json:"unpriced,omitempty"`
}

// Price prices usage as of `at`. All usage must start within the month of
// `at` (true for a campaign that runs within one week).
func Price(u cost.Usage, at time.Time) Bill {
	r := Model().Summarize(u, at)
	return Bill{TotalUSD: r.MonthToDate.USD(), Lines: r.Lines, Pools: r.Pools, Unpriced: r.Unpriced}
}

// StandingLine is one row of a monthly standing-cost table (NFR-C2).
type StandingLine struct {
	Item        string  `json:"item"`
	Quantity    float64 `json:"quantity"`
	Unit        string  `json:"unit"`
	USDPerMonth float64 `json:"usdPerMonth"`
}

// Topology is what is always on (NFR-C2).
type Topology struct {
	Name     string        `json:"name"`
	AlwaysOn []cost.Launch `json:"alwaysOn"` // Start/End ignored
	// Volumes that persist independently of instances (k3s data volume).
	Volumes    []cost.Volume `json:"volumes"`
	PublicIPv4 int           `json:"publicIpv4"`
	// Pool standing cost: AMI snapshots (current + previous) and Windows Fast
	// Launch pre-provisioned snapshots.
	Images     []cost.Image      `json:"images"`
	FastLaunch []cost.FastLaunch `json:"fastLaunch"`
}

// Standing prices a topology for one 730-hour month.
func Standing(t Topology) ([]StandingLine, float64) {
	m := Model()
	var lines []StandingLine
	var total float64
	add := func(item string, q float64, unit string, usd float64) {
		lines = append(lines, StandingLine{Item: item, Quantity: q, Unit: unit, USDPerMonth: usd})
		total += usd
	}
	for _, l := range t.AlwaysOn {
		p := m.InstanceUSDPerHour[cost.InstanceKey{Type: l.Type, Windows: l.Windows}]
		name := l.Type
		if l.Windows {
			name += " (Windows)"
		}
		add(name+" "+l.Pool, cost.HoursPerMonth, "hours", p*cost.HoursPerMonth)
		for _, v := range l.Volumes {
			add(name+" volume", float64(v.SizeGiB), "GiB", volumeMonth(m.Rates, v))
		}
	}
	for _, v := range t.Volumes {
		add("persistent volume", float64(v.SizeGiB), "GiB", volumeMonth(m.Rates, v))
	}
	if t.PublicIPv4 > 0 {
		add("public IPv4 addresses", float64(t.PublicIPv4), "addresses", float64(t.PublicIPv4)*m.Rates.PublicIPv4Hour*cost.HoursPerMonth)
	}
	for _, img := range t.Images {
		add("AMI snapshots "+img.Pool, img.SnapshotGiB, "GiB", img.SnapshotGiB*m.Rates.SnapshotGBMonth)
	}
	for _, fl := range t.FastLaunch {
		gib := float64(fl.Snapshots) * fl.SnapshotGiB
		add("Fast Launch snapshots "+fl.Pool, gib, "GiB", gib*m.Rates.SnapshotGBMonth)
	}
	return lines, total
}

func volumeMonth(r cost.Rates, v cost.Volume) float64 {
	typ := v.Type
	if typ == "" {
		typ = "gp3"
	}
	usd := float64(v.SizeGiB) * r.VolumeGBMonth[typ]
	if typ == "gp3" {
		usd += float64(max(0, v.IOPS-r.GP3FreeIOPS)) * r.GP3IOPSMonth
		usd += float64(max(0, v.ThroughputMiBps-r.GP3FreeThroughput)) * r.GP3ThroughputMonth
	}
	return usd
}

// TestTopology is the campaign's standing topology (§10.1) for NFR-C2 (a):
// the k3s node with its root and data volumes, the two clients, the Elastic
// IP, plus the pools' AMIs and Windows Fast Launch snapshots (sizes are the
// image defaults; the report overrides them with measured values).
func TestTopology() Topology {
	return Topology{
		Name: "test topology (§10.1)",
		AlwaysOn: []cost.Launch{
			{Pool: "k3s node", Type: "m8i.2xlarge", Volumes: []cost.Volume{{SizeGiB: 30}}},
			{Pool: "linux-client", Type: "m7i.xlarge", Volumes: []cost.Volume{{SizeGiB: 100}}},
			{Pool: "windows-client", Type: "m7i.xlarge", Windows: true, Volumes: []cost.Volume{{SizeGiB: 100}}},
		},
		Volumes:    []cost.Volume{{SizeGiB: 300, IOPS: 6000, ThroughputMiBps: 500}},
		PublicIPv4: 1,
		Images: []cost.Image{
			{Pool: "linux-x86-64", SnapshotGiB: 2 * 30}, {Pool: "linux-aarch64", SnapshotGiB: 2 * 30}, {Pool: "windows-x86-64", SnapshotGiB: 2 * 80},
		},
		FastLaunch: []cost.FastLaunch{{Pool: "windows-x86-64", Snapshots: 5, SnapshotGiB: 80}},
	}
}

// SmallProductionTopology is the starting point for NFR-C2 (b): one
// always-on control-plane node (the test topology's m8i.2xlarge until the
// NFR-M1/M2 measurements justify a smaller one), a 500 GiB gp3 storage
// volume for the sharded CAS, one public IPv4 endpoint, and the same pool
// images and Fast Launch snapshots. The report generator revises it from the
// measured control-plane memory.
func SmallProductionTopology() Topology {
	t := Topology{
		Name: "recommended small production topology",
		AlwaysOn: []cost.Launch{
			{Pool: "control plane", Type: "m8i.2xlarge", Volumes: []cost.Volume{{SizeGiB: 30}}},
		},
		Volumes:    []cost.Volume{{SizeGiB: 500, IOPS: 6000, ThroughputMiBps: 500}},
		PublicIPv4: 1,
	}
	tt := TestTopology()
	t.Images, t.FastLaunch = tt.Images, tt.FastLaunch
	return t
}
