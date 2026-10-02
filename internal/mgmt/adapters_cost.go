// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/cost"
)

// CostAdapter implements CostSource with the cost model (internal/cost, agent ec2).
// Usage returns the usage the controller recorded (launches, volumes, images, Fast
// Launch resources, transfer counters); it must answer from memory.
type CostAdapter struct {
	Model cost.Model
	Usage func(ctx context.Context) (cost.Usage, error)
}

var _ CostSource = (*CostAdapter)(nil)

// Cost implements CostSource. The model reports fixed UTC windows (today and the
// month to date); totals are cluster-wide, a pool filter narrows pools and lines.
func (a *CostAdapter) Cost(ctx context.Context, q CostQuery) (CostReport, error) {
	u, err := a.Usage(ctx)
	if err != nil {
		return CostReport{}, err
	}
	r := a.Model.Summarize(u, time.Now())
	out := CostReport{
		Today: Micros(r.Today), MonthToDate: Micros(r.MonthToDate), StandingPerMonth: Micros(r.StandingPerMonth),
		Assumptions: r.Assumptions,
	}
	var notes []string
	if len(r.Unpriced) > 0 {
		notes = append(notes, "unpriced (counted as $0): "+strings.Join(r.Unpriced, ", "))
	}
	if q.Pool != "" {
		notes = append(notes, "totals are cluster-wide")
	}
	if !q.Since.IsZero() {
		notes = append(notes, "amounts cover today and the month to date (UTC)")
	}
	if len(notes) > 0 {
		out.Assumptions = strings.TrimPrefix(out.Assumptions+"; "+strings.Join(notes, "; "), "; ")
	}
	for _, p := range r.Pools {
		if q.Pool != "" && p.Pool != q.Pool {
			continue
		}
		out.Pools = append(out.Pools, PoolCost{
			Pool: p.Pool, InstanceSeconds: p.InstanceSeconds, Compute: Micros(p.Compute), EBS: Micros(p.EBS),
			DataTransfer: Micros(p.DataTransfer), Standing: Micros(p.Standing),
		})
	}
	for _, l := range r.Lines {
		if q.Pool != "" && l.Pool != q.Pool {
			continue
		}
		out.Lines = append(out.Lines, CostLine{
			Pool: l.Pool, Category: l.Category, Detail: l.Detail, Quantity: l.Quantity, Unit: l.Unit, Amount: Micros(l.Amount),
		})
	}
	return out, nil
}
