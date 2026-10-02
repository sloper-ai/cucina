// SPDX-License-Identifier: FSL-1.1-ALv2

package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultBudgetUSD is the campaign budget (§12, §13).
const DefaultBudgetUSD = 300

// LedgerEntry records one scenario's reserved and measured spend.
type LedgerEntry struct {
	ID          string    `json:"id"`
	At          time.Time `json:"at"`
	EstimateUSD float64   `json:"estimateUSD"`
	MeasuredUSD float64   `json:"measuredUSD"`
}

// Ledger is the persisted spend state of a campaign run. It lives in the
// run's results directory so separate scenario invocations share it.
type Ledger struct {
	BudgetUSD float64 `json:"budgetUSD"`
	// ActualUSD is the spend so far measured from AWS (instance-seconds ×
	// price + EBS + transfer of everything tagged with the run, including the
	// standing environment and image builds), refreshed by the cost
	// collector. Zero until the first refresh.
	ActualUSD float64 `json:"actualUSD"`
	// ReserveUSD is held back for what must still happen regardless of
	// scenarios: the standing environment until teardown and teardown itself.
	ReserveUSD float64       `json:"reserveUSD"`
	Entries    []LedgerEntry `json:"entries"`
	UpdatedAt  time.Time     `json:"updatedAt"`
}

// Governor is the budget governor (§12): it admits a scenario only if the
// projected campaign spend stays within the budget. Non-essential scenarios
// are skipped when it would not; essential ones are held for the user's
// approval (Safety.AllowOverBudget), never run silently over budget.
type Governor struct {
	mu   sync.Mutex
	path string
	L    Ledger
}

// Decision is the governor's verdict for one scenario.
type Decision struct {
	Admit        bool    `json:"admit"`
	ProjectedUSD float64 `json:"projectedUSD"`
	Reason       string  `json:"reason,omitempty"`
}

// OpenGovernor loads (or initialises) the ledger at path.
func OpenGovernor(path string, budgetUSD, reserveUSD float64) (*Governor, error) {
	g := &Governor{path: path, L: Ledger{BudgetUSD: budgetUSD, ReserveUSD: reserveUSD}}
	if path == "" {
		return g, nil
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return g, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &g.L); err != nil {
		return nil, fmt.Errorf("budget ledger %s: %w", path, err)
	}
	if budgetUSD > 0 {
		g.L.BudgetUSD = budgetUSD
	}
	if reserveUSD > 0 {
		g.L.ReserveUSD = reserveUSD
	}
	return g, nil
}

// SpentUSD is the best current estimate of spend so far: the AWS-measured
// actual if available and larger, else the sum of scenario measurements.
func (g *Governor) SpentUSD() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spent()
}

func (g *Governor) spent() float64 {
	var sum float64
	for _, e := range g.L.Entries {
		sum += e.MeasuredUSD
	}
	if g.L.ActualUSD > sum {
		return g.L.ActualUSD
	}
	return sum
}

// Decide projects spent + reserve + the scenario's estimate against the
// budget.
func (g *Governor) Decide(s *Scenario, safety Safety) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	budget := g.L.BudgetUSD
	if safety.MaxSpendUSD > 0 && safety.MaxSpendUSD < budget {
		budget = safety.MaxSpendUSD
	}
	projected := g.spent() + g.L.ReserveUSD + s.EstimateUSD
	d := Decision{ProjectedUSD: projected, Admit: true}
	if projected <= budget || s.EstimateUSD == 0 {
		return d
	}
	if !s.Essential {
		d.Admit = false
		d.Reason = fmt.Sprintf("budget governor: projected spend $%.2f exceeds the $%.2f budget; non-essential scenario skipped", projected, budget)
		return d
	}
	if safety.AllowOverBudget {
		d.Reason = fmt.Sprintf("budget governor: projected spend $%.2f exceeds the $%.2f budget; admitted with the user's approval (safety.allowOverBudget)", projected, budget)
		return d
	}
	d.Admit = false
	d.Reason = fmt.Sprintf("budget governor: projected spend $%.2f exceeds the $%.2f budget; essential scenario held — ask the user (§12) and set safety.allowOverBudget to proceed", projected, budget)
	return d
}

// Record stores a scenario's estimate and measured spend and persists.
func (g *Governor) Record(id string, at time.Time, estimate, measured float64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.L.Entries = append(g.L.Entries, LedgerEntry{ID: id, At: at, EstimateUSD: estimate, MeasuredUSD: measured})
	g.L.UpdatedAt = at
	return g.save()
}

// Refresh sets the AWS-measured actual spend and persists.
func (g *Governor) Refresh(actualUSD float64, at time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.L.ActualUSD = actualUSD
	g.L.UpdatedAt = at
	return g.save()
}

func (g *Governor) save() error {
	if g.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(g.L, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, g.path)
}
