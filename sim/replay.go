// SPDX-License-Identifier: FSL-1.1-ALv2

package sim

import (
	"fmt"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/scaling"
)

// ParsePolicy parses "default" or "<name>:<key>=<value>,…" into a policy
// derived from the default one. Keys: idle, startup, drain (pool timers),
// queueFailAfter, backoffMin, backoffMax, cooldown, grace (planner config).
func ParsePolicy(s string) (Policy, error) {
	p := DefaultPolicy()
	if s == "" || s == "default" {
		return p, nil
	}
	name, kvs, _ := strings.Cut(s, ":")
	p.Name = name
	var specMods []func(*scaling.Spec)
	for _, kv := range strings.Split(kvs, ",") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return p, fmt.Errorf("policy %q: %q is not key=value", s, kv)
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return p, fmt.Errorf("policy %q: %s: %w", s, k, err)
		}
		switch k {
		case "idle":
			specMods = append(specMods, func(sp *scaling.Spec) { sp.IdleTimeout = d })
		case "startup":
			specMods = append(specMods, func(sp *scaling.Spec) { sp.StartupTimeout = d })
		case "drain":
			specMods = append(specMods, func(sp *scaling.Spec) { sp.DrainTimeout = d })
		case "queueFailAfter":
			p.Config.QueueFailAfter = d
		case "backoffMin":
			p.Config.BackoffMin = d
		case "backoffMax":
			p.Config.BackoffMax = d
		case "cooldown":
			p.Config.CapacityCooldown = d
		case "grace":
			p.Config.ConsistencyGrace = d
		default:
			return p, fmt.Errorf("policy %q: unknown key %q", s, k)
		}
	}
	if len(specMods) > 0 {
		p.Spec = func(sp *scaling.Spec) {
			for _, m := range specMods {
				m(sp)
			}
		}
	}
	return p, nil
}

// ReplayRow is one policy's outcome on a trace: cost against wait.
type ReplayRow struct {
	Policy                    string
	CostUSD, InstanceSeconds  float64
	WaitP50, WaitP95, WaitMax time.Duration
	Launches, Failed          int
	Violations                int
}

func (r ReplayRow) String() string {
	return fmt.Sprintf("%-20s cost=$%7.2f instance-h=%7.2f wait p50=%-8s p95=%-8s max=%-8s launches=%-4d failed=%d violations=%d",
		r.Policy, r.CostUSD, r.InstanceSeconds/3600, r.WaitP50.Round(time.Second), r.WaitP95.Round(time.Second),
		r.WaitMax.Round(time.Second), r.Launches, r.Failed, r.Violations)
}

// Replay runs one scenario (typically a fleet plus a production queue trace)
// under several policies and reports cost against queue wait (R-TEST-8c).
func Replay(sc *Scenario, policies []Policy) []ReplayRow {
	var rows []ReplayRow
	for _, p := range policies {
		res := RunWith(sc, Options{Policy: p})
		row := ReplayRow{Policy: p.Name, CostUSD: res.Metrics.CostUSD, InstanceSeconds: res.Metrics.InstanceSeconds,
			WaitP50: res.Metrics.WaitP50, WaitP95: res.Metrics.WaitP95, WaitMax: res.Metrics.WaitMax,
			Failed: res.Metrics.Failed, Violations: len(res.Violations)}
		for _, n := range res.Metrics.Launches {
			row.Launches += n
		}
		rows = append(rows, row)
	}
	return rows
}

// Shadow runs a scenario with the current policy acting and the candidate
// deciding on a copy of the state at every step without acting (shadow mode,
// R-TEST-7). Metrics.ShadowDiffSteps counts the decisions that differed and
// Metrics.ShadowSamples shows the first ones.
func Shadow(sc *Scenario, current, candidate Policy) Result {
	return RunWith(sc, Options{Policy: current, Shadow: &candidate})
}
