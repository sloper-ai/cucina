// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/cost"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Cost accounting (R-OBS-5, UC17, NFR-C1/C2): the loops record every EC2
// launch they observe (instance type, its DeleteOnTermination volumes, public
// IPv4, start and end), each pool's current and previous AMI (kept for
// rollback: standing snapshot storage) and its EC2 Fast Launch snapshots
// (standing). internal/cost prices the usage. The leader persists the usage
// (UsageStore, a ConfigMap) and a new leader resumes from it, so month-to-date
// figures survive leader changes and restarts.

// usage is the recorded usage, guarded by mu.
type usage struct {
	mu       sync.Mutex
	launches map[string]*cost.Launch // by instance ID
	images   map[domain.PoolName][]*cost.Image
	fast     map[string]*cost.FastLaunch // by image ID
	dirty    bool
}

func (u *usage) init() {
	if u.launches == nil {
		u.launches = map[string]*cost.Launch{}
		u.images = map[domain.PoolName][]*cost.Image{}
		u.fast = map[string]*cost.FastLaunch{}
	}
}

func (u *usage) observe(rt *PoolRuntime, instances []ports.Instance, now time.Time) {
	if rt.EC2 == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.init()
	for _, in := range instances {
		l, ok := u.launches[in.ID]
		if !ok {
			if in.State == ports.InstanceTerminated && in.LaunchTime.IsZero() {
				continue
			}
			l = &cost.Launch{Pool: string(in.Pool), Type: in.Type, Windows: rt.EC2.Windows, Start: in.LaunchTime, PublicIPv4: rt.EC2.AssociatePublicIP}
			if l.Start.IsZero() {
				l.Start = now
			}
			root := rt.EC2.RootVolume
			if root.SizeGiB == 0 {
				root.SizeGiB = rt.ImageSizeGiB // the AMI's root volume size
			}
			for _, v := range append([]ports.VolumeSpec{root}, rt.EC2.ExtraVolumes...) {
				if v.SizeGiB > 0 {
					l.Volumes = append(l.Volumes, cost.Volume{Type: v.Type, SizeGiB: v.SizeGiB, IOPS: v.IOPS, ThroughputMiBps: v.Throughput, InitRateMiBps: v.InitializationRate})
				}
			}
			u.launches[in.ID] = l
			u.dirty = true
		}
		if l.End.IsZero() && (in.State == ports.InstanceShuttingDown || in.State == ports.InstanceTerminated) {
			l.End = now
			u.dirty = true
		}
	}
	if rt.ImageRef != "" && rt.ImageErr == nil {
		u.image(rt.Spec.Name, rt.ImageRef, float64(rt.ImageSizeGiB), now)
	}
	// Forget launches that ended before the current month (never priced again).
	month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for id, l := range u.launches {
		if !l.End.IsZero() && l.End.Before(month) {
			delete(u.launches, id)
			u.dirty = true
		}
	}
}

// image records the pool's current AMI. The previous one stays registered for
// rollback (R-OPS-2) and keeps costing snapshot storage; older ones are assumed
// deregistered (Until = now).
func (u *usage) image(pool domain.PoolName, id string, gib float64, now time.Time) {
	imgs := u.images[pool]
	if n := len(imgs); n > 0 && imgs[n-1].ImageID == id {
		return
	}
	for _, img := range imgs {
		if img.ImageID == id {
			return // rolled back to a kept image
		}
	}
	imgs = append(imgs, &cost.Image{Pool: string(pool), ImageID: id, SnapshotGiB: gib, Since: now})
	for i := 0; i < len(imgs)-2; i++ {
		if imgs[i].Until.IsZero() {
			imgs[i].Until = now
		}
	}
	u.images[pool] = imgs
	u.dirty = true
}

// fastLaunch records the pre-provisioned snapshots of a Windows AMI (standing
// cost while Fast Launch is enabled).
func (u *usage) fastLaunch(pool domain.PoolName, image string, snapshots int, gib float64, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.init()
	fl := u.fast[image]
	switch {
	case fl == nil && snapshots > 0:
		u.fast[image] = &cost.FastLaunch{Pool: string(pool), ImageID: image, Snapshots: snapshots, SnapshotGiB: gib, Since: now}
		u.dirty = true
	case fl != nil && snapshots == 0 && fl.Until.IsZero():
		fl.Until = now
		u.dirty = true
	case fl != nil && snapshots > 0 && fl.Snapshots != snapshots:
		fl.Snapshots = snapshots
		u.dirty = true
	}
}

func (u *usage) snapshot() cost.Usage {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out cost.Usage
	ids := make([]string, 0, len(u.launches))
	for id := range u.launches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		l := *u.launches[id]
		l.Volumes = append([]cost.Volume(nil), l.Volumes...)
		out.Launches = append(out.Launches, l)
	}
	for _, imgs := range u.images {
		for _, img := range imgs {
			out.Images = append(out.Images, *img)
		}
	}
	for _, fl := range u.fast {
		out.FastLaunch = append(out.FastLaunch, *fl)
	}
	return out
}

// persisted is the stored form (launches keyed by instance ID).
type persisted struct {
	Launches   map[string]cost.Launch `json:"launches"`
	Images     []cost.Image           `json:"images"`
	FastLaunch []cost.FastLaunch      `json:"fastLaunch"`
}

func (u *usage) export() (persisted, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p := persisted{Launches: map[string]cost.Launch{}}
	for id, l := range u.launches {
		p.Launches[id] = *l
	}
	for _, imgs := range u.images {
		for _, img := range imgs {
			p.Images = append(p.Images, *img)
		}
	}
	for _, fl := range u.fast {
		p.FastLaunch = append(p.FastLaunch, *fl)
	}
	dirty := u.dirty
	u.dirty = false
	return p, dirty
}

// restore merges stored usage; records already observed by this leader win.
func (u *usage) restore(p persisted) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.init()
	for id, l := range p.Launches {
		if _, ok := u.launches[id]; !ok {
			l := l
			u.launches[id] = &l
		}
	}
	for _, img := range p.Images {
		pool := domain.PoolName(img.Pool)
		known := false
		for _, x := range u.images[pool] {
			known = known || x.ImageID == img.ImageID
		}
		if !known {
			img := img
			u.images[pool] = append([]*cost.Image{&img}, u.images[pool]...)
		}
	}
	for _, fl := range p.FastLaunch {
		if _, ok := u.fast[fl.ImageID]; !ok {
			fl := fl
			u.fast[fl.ImageID] = &fl
		}
	}
}

func (u *usage) types() map[cost.InstanceKey]bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[cost.InstanceKey]bool{}
	for _, l := range u.launches {
		out[cost.InstanceKey{Type: l.Type, Windows: l.Windows}] = true
	}
	for _, fl := range u.fast {
		for _, p := range fl.Prep {
			out[cost.InstanceKey{Type: p.Type, Windows: p.Windows}] = true
		}
	}
	return out
}

// Usage returns the recorded usage (management API `GetCost`).
func (f *Fleet) Usage(context.Context) (cost.Usage, error) { return f.usage.snapshot(), nil }

// RecordFastLaunch records the Fast Launch snapshots of a pool's AMI.
func (f *Fleet) RecordFastLaunch(pool domain.PoolName, image string, snapshots, sizeGiB int) {
	f.usage.fastLaunch(pool, image, snapshots, float64(sizeGiB), f.o.Clock.Now())
}

// UsageStore persists the recorded usage across leader changes.
type UsageStore interface {
	LoadUsage(ctx context.Context) ([]byte, error) // nil, nil when nothing is stored
	SaveUsage(ctx context.Context, b []byte) error
}

// CostModel prices recorded usage with the region's rates and the on-demand
// prices of the instance types seen so far (Compute.InstancePrices, cached).
type CostModel struct {
	Rates   cost.Rates
	Compute ports.Compute
	Fleet   *Fleet
	Metrics *metrics.Metrics
	Clock   ports.Clock
	Log     *slog.Logger
	Store   UsageStore // optional
	// Every is the export period (default 1 min).
	Every time.Duration

	mu       sync.Mutex
	prices   map[cost.InstanceKey]float64
	exported map[[2]string]float64 // (pool, category) → month-to-date already added to the counter
	month    time.Month
	today    map[domain.PoolName]float64
	loaded   bool
}

// Model returns a pricing model with the prices known so far.
func (c *CostModel) Model() cost.Model {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cost.Model{Rates: c.Rates, InstanceUSDPerHour: maps.Clone(c.prices)}
}

// TodayUSD returns the pool's estimated cost since 00:00 UTC (WorkerPool status).
func (c *CostModel) TodayUSD(pool domain.PoolName) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.today[pool]
	return v, ok
}

func (c *CostModel) refreshPrices(ctx context.Context) {
	need := map[bool][]string{}
	c.mu.Lock()
	for k := range c.Fleet.usage.types() {
		if _, ok := c.prices[k]; !ok {
			need[k.Windows] = append(need[k.Windows], k.Type)
		}
	}
	c.mu.Unlock()
	for windows, types := range need {
		ps, err := c.Compute.InstancePrices(ctx, types, windows)
		if err != nil {
			c.Log.Warn("instance prices unavailable; launches stay unpriced for now", "err", err)
			continue
		}
		c.mu.Lock()
		if c.prices == nil {
			c.prices = map[cost.InstanceKey]float64{}
		}
		for t, p := range ps {
			c.prices[cost.InstanceKey{Type: t, Windows: windows}] = p.USDPerHour
		}
		c.mu.Unlock()
	}
}

// export updates cucina_cost_usd_total{pool,category} (by the month-to-date
// increase since the last export), cucina_standing_cost_usd_per_month{category}
// and the per-pool cost of the day.
func (c *CostModel) export(now time.Time) {
	m := c.Model()
	u := c.Fleet.usage.snapshot()
	r := m.Summarize(u, now)
	today := map[domain.PoolName]float64{}
	for _, l := range u.Launches {
		today[domain.PoolName(l.Pool)] = 0
	}
	for pool := range today {
		var pu cost.Usage
		for _, l := range u.Launches {
			if l.Pool == string(pool) {
				pu.Launches = append(pu.Launches, l)
			}
		}
		for _, img := range u.Images {
			if img.Pool == string(pool) {
				pu.Images = append(pu.Images, img)
			}
		}
		for _, fl := range u.FastLaunch {
			if fl.Pool == string(pool) {
				pu.FastLaunch = append(pu.FastLaunch, fl)
			}
		}
		today[pool] = m.Summarize(pu, now).Today.USD()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.today = today
	if c.Metrics == nil {
		return
	}
	if c.exported == nil || now.UTC().Month() != c.month {
		c.exported, c.month = map[[2]string]float64{}, now.UTC().Month()
	}
	totals := map[[2]string]float64{}
	for _, l := range r.Lines {
		totals[[2]string{l.Pool, l.Category}] += l.Amount.USD()
	}
	for k, total := range totals {
		if d := total - c.exported[k]; d > 0 {
			c.Metrics.CostUSD.WithLabelValues(k[0], k[1]).Add(d)
			c.exported[k] = total
		}
	}
	for cat, v := range r.StandingByCategory {
		c.Metrics.StandingCostUSDPerMonth.WithLabelValues(cat).Set(v.USD())
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable (the usage lives on the leader).
func (c *CostModel) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (c *CostModel) Start(ctx context.Context) error {
	every := c.Every
	if every <= 0 {
		every = time.Minute
	}
	for {
		c.Export(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-c.Clock.After(every):
		}
	}
}

// Export loads the stored usage once, refreshes missing prices, updates the
// cost metrics and persists the usage when it changed.
func (c *CostModel) Export(ctx context.Context) {
	if c.Store != nil && !c.isLoaded() {
		if err := c.load(ctx); err != nil {
			c.Log.Warn("loading the stored cost usage failed; month-to-date figures restart from now", "err", err)
		}
	}
	c.refreshPrices(ctx)
	c.export(c.Clock.Now())
	if c.Store != nil {
		if err := c.save(ctx); err != nil {
			c.Log.Warn("persisting the cost usage failed", "err", err)
		}
	}
}

func (c *CostModel) isLoaded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaded
}

// maxStoredUsage keeps the ConfigMap well below the 1 MiB object limit.
const maxStoredUsage = 900 << 10

func (c *CostModel) load(ctx context.Context) error {
	b, err := c.Store.LoadUsage(ctx)
	if err != nil {
		return err
	}
	if len(b) > 0 {
		var p persisted
		if err := json.Unmarshal(b, &p); err != nil {
			c.Log.Warn("stored cost usage is unreadable; starting over", "err", err)
		} else {
			c.Fleet.usage.restore(p)
			c.Log.Info("resumed cost usage", "launches", len(p.Launches), "images", len(p.Images))
		}
	}
	c.mu.Lock()
	c.loaded = true
	c.mu.Unlock()
	return nil
}

func (c *CostModel) save(ctx context.Context) error {
	p, dirty := c.Fleet.usage.export()
	if !dirty {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(b) > maxStoredUsage {
		// Drop the oldest finished launches (their cost stays in the exported
		// counters; only a later leader's month-to-date would miss them).
		ended := make([]string, 0, len(p.Launches))
		for id, l := range p.Launches {
			if !l.End.IsZero() {
				ended = append(ended, id)
			}
		}
		sort.Slice(ended, func(i, j int) bool { return p.Launches[ended[i]].End.Before(p.Launches[ended[j]].End) })
		for _, id := range ended {
			if len(b) <= maxStoredUsage {
				break
			}
			delete(p.Launches, id)
			if b, err = json.Marshal(p); err != nil {
				return err
			}
		}
		c.Log.Warn("stored cost usage truncated to the most recent launches", "launches", len(p.Launches))
	}
	if err := c.Store.SaveUsage(ctx, b); err != nil {
		c.Fleet.usage.markDirty()
		return err
	}
	return nil
}

func (u *usage) markDirty() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.dirty = true
}
