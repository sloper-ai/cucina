// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/cost"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Cost accounting (R-OBS-5, UC17): the loops record every EC2 launch they
// observe (instance, type, volumes, public IPv4, start and end) and every
// pool's current image; internal/cost prices that usage. The recorder lives on
// the leader and starts from the instances still visible after a restart, so
// month-to-date figures are a lower bound across leader changes (documented).

// usage is the recorded usage, guarded by mu.
type usage struct {
	mu       sync.Mutex
	launches map[string]*cost.Launch // by instance ID
	images   map[domain.PoolName]*cost.Image
}

func (u *usage) observe(rt *PoolRuntime, instances []ports.Instance, now time.Time) {
	if rt.EC2 == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.launches == nil {
		u.launches = map[string]*cost.Launch{}
		u.images = map[domain.PoolName]*cost.Image{}
	}
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
			for _, v := range append([]ports.VolumeSpec{rt.EC2.RootVolume}, rt.EC2.ExtraVolumes...) {
				if v.SizeGiB > 0 {
					l.Volumes = append(l.Volumes, cost.Volume{Type: v.Type, SizeGiB: v.SizeGiB, IOPS: v.IOPS, ThroughputMiBps: v.Throughput, InitRateMiBps: v.InitializationRate})
				}
			}
			u.launches[in.ID] = l
		}
		if l.End.IsZero() && (in.State == ports.InstanceShuttingDown || in.State == ports.InstanceTerminated) {
			l.End = now
		}
	}
	if rt.ImageRef != "" && rt.ImageErr == nil {
		img := u.images[rt.Spec.Name]
		if img == nil || img.ImageID != rt.ImageRef {
			if img != nil {
				img.Until = now
			}
			u.images[rt.Spec.Name] = &cost.Image{Pool: string(rt.Spec.Name), ImageID: rt.ImageRef, SnapshotGiB: float64(rt.ImageSizeGiB), Since: now}
		}
	}
	// Forget launches that ended before the current month (never priced again).
	month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for id, l := range u.launches {
		if !l.End.IsZero() && l.End.Before(month) {
			delete(u.launches, id)
		}
	}
}

func (u *usage) snapshot() cost.Usage {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out cost.Usage
	for _, l := range u.launches {
		c := *l
		c.Volumes = append([]cost.Volume(nil), l.Volumes...)
		out.Launches = append(out.Launches, c)
	}
	for _, img := range u.images {
		out.Images = append(out.Images, *img)
	}
	return out
}

func (u *usage) types() map[cost.InstanceKey]bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[cost.InstanceKey]bool{}
	for _, l := range u.launches {
		out[cost.InstanceKey{Type: l.Type, Windows: l.Windows}] = true
	}
	return out
}

// Usage returns the recorded usage (management API `GetCost`).
func (f *Fleet) Usage(context.Context) (cost.Usage, error) { return f.usage.snapshot(), nil }

// CostModel prices recorded usage with the region's rates and the on-demand
// prices of the instance types seen so far (Compute.InstancePrices, cached).
type CostModel struct {
	Rates   cost.Rates
	Compute ports.Compute
	Fleet   *Fleet
	Metrics *metrics.Metrics
	Clock   ports.Clock
	Log     *slog.Logger
	// Every is the export period (default 1 min).
	Every time.Duration

	mu       sync.Mutex
	prices   map[cost.InstanceKey]float64
	exported map[[2]string]float64 // (pool, category) → month-to-date already added to the counter
	month    time.Month
}

// Model returns a pricing model with the prices known so far.
func (c *CostModel) Model() cost.Model {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cost.Model{Rates: c.Rates, InstanceUSDPerHour: maps.Clone(c.prices)}
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
// increase since the last export) and cucina_standing_cost_usd_per_month{category}.
func (c *CostModel) export(now time.Time) {
	r := c.Model().Summarize(c.Fleet.usage.snapshot(), now)
	c.mu.Lock()
	defer c.mu.Unlock()
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
		select {
		case <-ctx.Done():
			return nil
		case <-c.Clock.After(every):
		}
		c.Export(ctx)
	}
}

// Export refreshes missing prices and updates the cost metrics now.
func (c *CostModel) Export(ctx context.Context) {
	c.refreshPrices(ctx)
	if c.Metrics != nil {
		c.export(c.Clock.Now())
	}
}
