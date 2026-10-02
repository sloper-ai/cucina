// SPDX-License-Identifier: FSL-1.1-ALv2

// Package cost is Cucina's pure cost model (R-OBS-5, R-DATA-7, NFR-C1/C2). It turns
// recorded usage — instance launches with their volumes and public IPs, AMIs kept
// for rollback, Windows Fast Launch snapshot pools and per-path byte counters —
// into the data behind cucina.v1.CostSummary / GetCostResponse: spend today and
// month to date, a per-pool split, itemised lines and the standing cost per month
// that remains when every pool is at zero. It performs no I/O; the controller
// feeds it prices (ports.Compute.InstancePrices + Rates) and usage records.
package cost

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// Micros is an amount of US dollars in millionths (cucina.v1.Money.micros).
type Micros int64

// USD returns the amount in dollars.
func (m Micros) USD() float64 { return float64(m) / 1e6 }

func micros(usd float64) Micros { return Micros(math.Round(usd * 1e6)) }

// Line categories (cucina.v1.CostLine.category).
const (
	CategoryCompute      = "compute"
	CategoryEBS          = "ebs"
	CategoryPublicIPv4   = "public-ipv4"
	CategoryAMIStorage   = "ami-storage"
	CategoryFastLaunch   = "fast-launch"
	CategoryDataTransfer = "data-transfer"
)

// Path is a data-transfer path with its own price (PROMPT §6.14).
type Path string

const (
	PathInterAZ        Path = "inter-az"        // cross-AZ traffic (should be ≈ 0, NFR-T5)
	PathNAT            Path = "nat"             // through a NAT gateway (none by default)
	PathPublicIP       Path = "public-ip"       // same-region traffic via public IPv4
	PathInternetEgress Path = "internet-egress" // to the internet (first 100 GB/month free)
)

// MinimumBilledSeconds is EC2's per-launch minimum (per-second billing, one-minute minimum).
const MinimumBilledSeconds = 60

// bytesPerGB: AWS bills data transfer in binary gigabytes.
const bytesPerGB = 1 << 30

// InstanceKey identifies an on-demand instance price.
type InstanceKey struct {
	Type    string
	Windows bool // price includes the Windows licence
}

// Volume is an EBS volume that exists for the lifetime of its instance
// (DeleteOnTermination, R-POOL-1).
type Volume struct {
	Type            string // "gp3" (default when empty)
	SizeGiB         int
	IOPS            int
	ThroughputMiBps int
	// InitRateMiBps and InitGiB describe a per-launch Provisioned Rate for Volume
	// Initialization: InitGiB of snapshot data initialized at InitRateMiBps.
	InitRateMiBps int
	InitGiB       int
}

// Launch is one instance from RunInstances to termination.
type Launch struct {
	Pool    string
	Type    string
	Windows bool
	// SpotUSDPerHour is the spot price actually paid; 0 prices the launch on demand.
	SpotUSDPerHour float64
	Start          time.Time
	End            time.Time // zero while the instance runs
	Volumes        []Volume
	PublicIPv4     bool
}

// StandaloneVolume is a volume not tied to a launch (an orphan until swept).
type StandaloneVolume struct {
	Pool       string
	Volume     Volume
	Start, End time.Time // End zero while it exists
}

// Image is an AMI kept for a pool (the current one and one previous for rollback).
// Its snapshots are standing cost.
type Image struct {
	Pool    string
	ImageID string
	// SnapshotGiB is the stored snapshot size (the AMI's volume size is an upper bound).
	SnapshotGiB  float64
	Since, Until time.Time // registration window; zero Since = before the report window, zero Until = still registered
}

// FastLaunch is a Windows AMI's pool of pre-provisioned snapshots (resources tagged
// CreatedBy=EC2 Fast Launch): standing snapshot storage plus the prep instances EC2
// runs to (re)create snapshots.
type FastLaunch struct {
	Pool         string
	ImageID      string
	Snapshots    int
	SnapshotGiB  float64 // size of each pre-provisioned snapshot
	Since, Until time.Time
	Prep         []Launch
}

// Transfer is a byte count observed on one path, attributed to the time At.
type Transfer struct {
	Pool  string
	Path  Path
	Bytes int64
	At    time.Time
}

// Usage is everything the model prices.
type Usage struct {
	Launches   []Launch
	Volumes    []StandaloneVolume
	Images     []Image
	FastLaunch []FastLaunch
	Transfer   []Transfer
}

// Model prices Usage with Rates and on-demand instance prices.
type Model struct {
	Rates              Rates
	InstanceUSDPerHour map[InstanceKey]float64
}

// SetInstancePrice records the on-demand hourly price of an instance type.
func (m *Model) SetInstancePrice(typ string, windows bool, usdPerHour float64) {
	if m.InstanceUSDPerHour == nil {
		m.InstanceUSDPerHour = map[InstanceKey]float64{}
	}
	m.InstanceUSDPerHour[InstanceKey{Type: typ, Windows: windows}] = usdPerHour
}

// PoolCost is one pool's month-to-date cost (cucina.v1.PoolCost; PublicIPv4 has no
// field there and is reported in its own line category).
type PoolCost struct {
	Pool            string
	InstanceSeconds int64 // billed seconds, 60 s minimum per launch included
	Compute         Micros
	EBS             Micros
	PublicIPv4      Micros
	DataTransfer    Micros
	Standing        Micros // ami-storage + fast-launch
}

// Line is one itemised month-to-date cost (cucina.v1.CostLine).
type Line struct {
	Pool     string
	Category string
	Detail   string
	Quantity float64
	Unit     string
	Amount   Micros
}

// Report is the cost summary at one instant (cucina.v1.CostSummary + GetCostResponse).
type Report struct {
	Today       Micros // since 00:00 UTC
	MonthToDate Micros // since the 1st, 00:00 UTC (AWS billing months are UTC)
	// StandingPerMonth projects the cost with every pool at zero: AMI snapshots and
	// Fast Launch pre-provisioned snapshots that exist now (NFR-C1).
	StandingPerMonth   Micros
	StandingByCategory map[string]Micros // per month; keys ami-storage, fast-launch
	Pools              []PoolCost
	Lines              []Line
	// Unpriced lists instance types and volume types with no known price (counted as 0).
	Unpriced    []string
	Assumptions string
}

type window struct{ from, to time.Time }

// overlap is the length of [a, b) ∩ [w.from, w.to).
func (w window) overlap(a, b time.Time) time.Duration {
	if a.Before(w.from) {
		a = w.from
	}
	if b.After(w.to) {
		b = w.to
	}
	if !b.After(a) {
		return 0
	}
	return b.Sub(a)
}

func (w window) contains(t time.Time) bool { return !t.Before(w.from) && !t.After(w.to) }

type lineKey struct{ pool, category, detail, unit string }

type amounts struct {
	quantity     float64 // month to date
	month, today float64 // USD
}

type ledger struct {
	lines           map[lineKey]*amounts
	instanceSeconds map[string]float64
	unpriced        map[string]bool
}

func (l *ledger) add(k lineKey, quantity, month, today float64) {
	a := l.lines[k]
	if a == nil {
		a = &amounts{}
		l.lines[k] = a
	}
	a.quantity += quantity
	a.month += month
	a.today += today
}

// Summarize prices u as of now.
func (m Model) Summarize(u Usage, now time.Time) Report {
	now = now.UTC()
	month := window{time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), now}
	day := window{time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), now}
	l := &ledger{lines: map[lineKey]*amounts{}, instanceSeconds: map[string]float64{}, unpriced: map[string]bool{}}

	for _, x := range u.Launches {
		m.launch(l, x, CategoryCompute, month, day, now)
	}
	for _, v := range u.Volumes {
		end := clampEnd(v.Start, v.End, now)
		m.volume(l, v.Pool, CategoryEBS, v.Volume, month.overlap(v.Start, end), day.overlap(v.Start, end))
	}
	standing := map[string]float64{}
	for _, img := range u.Images {
		m.snapshots(l, img.Pool, CategoryAMIStorage, "ami "+img.ImageID, img.SnapshotGiB, img.Since, img.Until, month, day, now)
		if img.Until.IsZero() || img.Until.After(now) {
			standing[CategoryAMIStorage] += img.SnapshotGiB * m.Rates.SnapshotGBMonth
		}
	}
	for _, fl := range u.FastLaunch {
		gib := float64(fl.Snapshots) * fl.SnapshotGiB
		m.snapshots(l, fl.Pool, CategoryFastLaunch, "snapshots "+fl.ImageID, gib, fl.Since, fl.Until, month, day, now)
		if fl.Until.IsZero() || fl.Until.After(now) {
			standing[CategoryFastLaunch] += gib * m.Rates.SnapshotGBMonth
		}
		for _, p := range fl.Prep {
			m.launch(l, p, CategoryFastLaunch, month, day, now)
		}
	}
	m.transfer(l, u.Transfer, month, day)

	r := Report{StandingByCategory: map[string]Micros{}, Assumptions: m.assumptions()}
	for _, k := range slices.Sorted(maps.Keys(standing)) {
		r.StandingByCategory[k] = micros(standing[k])
		r.StandingPerMonth += r.StandingByCategory[k]
	}
	pools := map[string]*PoolCost{}
	keys := slices.SortedFunc(maps.Keys(l.lines), func(a, b lineKey) int {
		return strings.Compare(a.pool+"\x00"+a.category+"\x00"+a.detail+"\x00"+a.unit, b.pool+"\x00"+b.category+"\x00"+b.detail+"\x00"+b.unit)
	})
	for _, k := range keys {
		a := l.lines[k]
		amt := micros(a.month)
		r.MonthToDate += amt
		r.Today += micros(a.today)
		r.Lines = append(r.Lines, Line{Pool: k.pool, Category: k.category, Detail: k.detail, Quantity: a.quantity, Unit: k.unit, Amount: amt})
		pc := pools[k.pool]
		if pc == nil {
			pc = &PoolCost{Pool: k.pool, InstanceSeconds: int64(math.Round(l.instanceSeconds[k.pool]))}
			pools[k.pool] = pc
		}
		switch k.category {
		case CategoryCompute:
			pc.Compute += amt
		case CategoryEBS:
			pc.EBS += amt
		case CategoryPublicIPv4:
			pc.PublicIPv4 += amt
		case CategoryDataTransfer:
			pc.DataTransfer += amt
		case CategoryAMIStorage, CategoryFastLaunch:
			pc.Standing += amt
		}
	}
	for _, p := range slices.Sorted(maps.Keys(pools)) {
		r.Pools = append(r.Pools, *pools[p])
	}
	r.Unpriced = slices.Sorted(maps.Keys(l.unpriced))
	return r
}

func clampEnd(start, end, now time.Time) time.Time {
	if end.IsZero() || end.After(now) {
		end = now
	}
	if end.Before(start) {
		end = start
	}
	return end
}

// launch prices one instance lifetime: instance-seconds with the 60 s minimum
// (attributed to the window holding the launch), its volumes and public IPv4.
func (m Model) launch(l *ledger, x Launch, category string, month, day window, now time.Time) {
	end := clampEnd(x.Start, x.End, now)
	topUp := max(0, MinimumBilledSeconds*time.Second-end.Sub(x.Start))
	billed := func(w window) time.Duration {
		d := w.overlap(x.Start, end)
		if w.contains(x.Start) {
			d += topUp
		}
		return d
	}
	ms, ds := billed(month), billed(day)
	price, ok := x.SpotUSDPerHour, x.SpotUSDPerHour > 0
	detail := x.Type
	if ok {
		detail += " spot"
	} else {
		price, ok = m.InstanceUSDPerHour[InstanceKey{Type: x.Type, Windows: x.Windows}]
	}
	if x.Windows {
		detail += " windows"
	}
	if !ok {
		l.unpriced[x.Type] = true
	}
	if category == CategoryFastLaunch {
		detail = "prep " + detail
	} else {
		l.instanceSeconds[x.Pool] += ms.Seconds()
	}
	l.add(lineKey{x.Pool, category, detail, "instance-seconds"}, ms.Seconds(), ms.Hours()*price, ds.Hours()*price)

	volCategory := CategoryEBS
	if category == CategoryFastLaunch {
		volCategory = CategoryFastLaunch
	}
	mo, do := month.overlap(x.Start, end), day.overlap(x.Start, end)
	for _, v := range x.Volumes {
		m.volume(l, x.Pool, volCategory, v, mo, do)
		if v.InitRateMiBps > 0 && v.InitGiB > 0 {
			rate := m.Rates.InitRateLowGB
			if v.InitRateMiBps > m.Rates.InitRateTierMiBps {
				rate = m.Rates.InitRateHighGB
			}
			usd := float64(v.InitGiB) * rate
			l.add(lineKey{x.Pool, volCategory, "volume initialization", "GB"}, inWindow(month, x.Start, float64(v.InitGiB)),
				inWindow(month, x.Start, usd), inWindow(day, x.Start, usd))
		}
	}
	if x.PublicIPv4 {
		l.add(lineKey{x.Pool, CategoryPublicIPv4, "in-use address", "IP-hours"}, mo.Hours(),
			mo.Hours()*m.Rates.PublicIPv4Hour, do.Hours()*m.Rates.PublicIPv4Hour)
	}
}

func inWindow(w window, t time.Time, v float64) float64 {
	if w.contains(t) {
		return v
	}
	return 0
}

// volume prices storage and gp3 performance above the free baseline while the
// volume exists (mo/do: its lifetime within the month/day windows).
func (m Model) volume(l *ledger, pool, category string, v Volume, mo, do time.Duration) {
	typ := v.Type
	if typ == "" {
		typ = "gp3"
	}
	perMonth := func(d time.Duration) float64 { return d.Hours() / HoursPerMonth }
	rate, ok := m.Rates.VolumeGBMonth[typ]
	if !ok {
		l.unpriced["volume "+typ] = true
	}
	size := float64(v.SizeGiB)
	l.add(lineKey{pool, category, typ + " storage", "GB-month"}, size*perMonth(mo), size*perMonth(mo)*rate, size*perMonth(do)*rate)
	if typ != "gp3" {
		return
	}
	if extra := float64(v.IOPS - m.Rates.GP3FreeIOPS); v.IOPS > 0 && extra > 0 {
		l.add(lineKey{pool, category, "gp3 iops", "IOPS-month"}, extra*perMonth(mo),
			extra*perMonth(mo)*m.Rates.GP3IOPSMonth, extra*perMonth(do)*m.Rates.GP3IOPSMonth)
	}
	if extra := float64(v.ThroughputMiBps - m.Rates.GP3FreeThroughput); v.ThroughputMiBps > 0 && extra > 0 {
		l.add(lineKey{pool, category, "gp3 throughput", "MiBps-month"}, extra*perMonth(mo),
			extra*perMonth(mo)*m.Rates.GP3ThroughputMonth, extra*perMonth(do)*m.Rates.GP3ThroughputMonth)
	}
}

// snapshots accrues snapshot storage (GB-month) over [since, until) ∩ window.
func (m Model) snapshots(l *ledger, pool, category, detail string, gib float64, since, until time.Time, month, day window, now time.Time) {
	if since.IsZero() {
		since = month.from
	}
	end := clampEnd(since, until, now)
	mo, do := month.overlap(since, end), day.overlap(since, end)
	l.add(lineKey{pool, category, detail, "GB-month"}, gib*mo.Hours()/HoursPerMonth,
		gib*mo.Hours()/HoursPerMonth*m.Rates.SnapshotGBMonth, gib*do.Hours()/HoursPerMonth*m.Rates.SnapshotGBMonth)
}

// transfer prices byte counters. Internet egress shares the account-wide monthly
// free tier pro rata among pools.
func (m Model) transfer(l *ledger, ts []Transfer, month, day window) {
	type split struct{ month, today, beforeToday float64 } // GB
	egress := map[string]*split{}
	var total split
	for _, t := range ts {
		if !month.contains(t.At) || t.Bytes <= 0 {
			continue
		}
		gb := float64(t.Bytes) / bytesPerGB
		inDay := day.contains(t.At)
		var rate float64
		switch t.Path {
		case PathInternetEgress:
			s := egress[t.Pool]
			if s == nil {
				s = &split{}
				egress[t.Pool] = s
			}
			s.month += gb
			total.month += gb
			if inDay {
				s.today += gb
				total.today += gb
			} else {
				total.beforeToday += gb
			}
			continue
		case PathInterAZ:
			rate = 2 * m.Rates.InterAZGB
		case PathPublicIP:
			rate = 2 * m.Rates.PublicIPGB
		case PathNAT:
			rate = m.Rates.NATGB
		default:
			l.unpriced["transfer "+string(t.Path)] = true
		}
		today := 0.0
		if inDay {
			today = gb * rate
		}
		l.add(lineKey{t.Pool, CategoryDataTransfer, string(t.Path), "GB"}, gb, gb*rate, today)
	}
	free := m.Rates.InternetFreeGBMonth
	billMonth := max(0, total.month-free)
	billToday := billMonth - max(0, total.beforeToday-free)
	for _, pool := range slices.Sorted(maps.Keys(egress)) {
		s := egress[pool]
		var mUSD, tUSD float64
		if total.month > 0 {
			mUSD = billMonth * s.month / total.month * m.Rates.InternetEgressGB
		}
		if total.today > 0 {
			tUSD = billToday * s.today / total.today * m.Rates.InternetEgressGB
		}
		l.add(lineKey{pool, CategoryDataTransfer, string(PathInternetEgress), "GB"}, s.month, mUSD, tUSD)
	}
}

func (m Model) assumptions() string {
	return fmt.Sprintf("rates %s as of %s; instances at AWS Price List on-demand prices (spot at on-demand unless the paid spot price is recorded); "+
		"%d s minimum per launch; GB-month = %d h; data transfer GB = 2^30 bytes; inter-AZ and public-IP traffic charged in both directions; "+
		"internet egress free tier %.0f GB/month shared pro rata; standing = AMI snapshots + Fast Launch snapshots and prep instances",
		m.Rates.Region, m.Rates.AsOf, MinimumBilledSeconds, HoursPerMonth, m.Rates.InternetFreeGBMonth)
}
