// SPDX-License-Identifier: FSL-1.1-ALv2

package scenarios

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
)

// Guards PROMPT §10.3 and §8 coverage: every campaign scenario T0–T22 is
// registered (valid declarations, unique IDs), T15 runs last, every NFR row
// names registered scenarios, and every NFR a scenario claims exists.
func TestRegistryCoversTheCampaign(t *testing.T) {
	reg := harness.NewRegistry()
	Register(reg)
	ids := map[string]bool{}
	for _, id := range reg.IDs() {
		ids[id] = true
	}
	for i := 0; i <= 22; i++ {
		id := fmt.Sprintf("T%d", i)
		switch i {
		case 9:
			for _, sub := range "abcde" {
				require.True(t, ids[id+string(sub)], id+string(sub))
			}
		case 10:
			for _, sub := range "abcdefghi" {
				require.True(t, ids[id+string(sub)], id+string(sub))
			}
		default:
			require.True(t, ids[id], id)
		}
	}
	campaign := []string{}
	for _, id := range reg.IDs() {
		if s, _ := reg.Get(id); s.AllowedIn(harness.EnvAWS) {
			campaign = append(campaign, id)
		}
	}
	require.Equal(t, "T15", campaign[len(campaign)-1-countAfter(reg, "T15")], "teardown runs after every other AWS scenario but the read-only smoke checks")
	for _, d := range nfr.Catalog {
		for _, s := range d.Scenarios {
			require.True(t, ids[s], "%s names unknown scenario %s", d.ID, s)
		}
	}
	for _, id := range reg.IDs() {
		s, _ := reg.Get(id)
		for _, n := range s.NFRs {
			_, ok := nfr.Lookup(n)
			require.True(t, ok, "%s claims unknown %s", id, n)
		}
		if s.AllowedIn(harness.EnvKindCluster) {
			require.LessOrEqual(t, s.Timeout, 2*time.Minute, "%s: kind scenarios ≤ 2 min", id)
		}
	}
}

// countAfter counts AWS-allowed scenarios registered after id (the read-only
// smoke checks registered for kind/prod-smoke).
func countAfter(reg *harness.Registry, id string) int {
	n, seen := 0, false
	for _, x := range reg.IDs() {
		if x == id {
			seen = true
			continue
		}
		if s, _ := reg.Get(x); seen && s.AllowedIn(harness.EnvAWS) {
			n++
		}
	}
	return n
}

// Guards NFR-P1's definition: a VM start counts from the client's first
// Execute when it is the first launch of the scale-out, else from its launch.
func TestColdStartSamples(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	starts := []PoolStart{
		{VM: "a", Launched: t0.Add(2 * time.Second), ToFirstAction: 40 * time.Second},
		{VM: "b", Launched: t0.Add(30 * time.Second), ToFirstAction: 45 * time.Second},
		{VM: "old", Launched: t0.Add(-time.Hour), ToFirstAction: 50 * time.Second},
	}
	got := coldStarts(starts, t0, t0.Add(time.Hour), t0)
	require.Equal(t, []time.Duration{42 * time.Second, 45 * time.Second}, got)
	require.Equal(t, 12500*time.Millisecond, dur("12.5s"))
	require.Equal(t, 3*time.Second+5, dur(map[string]any{"seconds": 3.0, "nanos": 5.0}))
}

// Guards §10.2 "count actions with bazel aquery --output=summary" against
// Bazel 9.2's real output format.
func TestParseAquerySummary(t *testing.T) {
	out := "7 total actions.\n\nMnemonics:\n  SymlinkTree: 1\n  TestRunner: 1\n  Genrule: 5\n\nConfigurations:\n  darwin_arm64-fastbuild: 7\n\n" +
		"Execution Platforms:\n  @@platforms//host:host: 7\n"
	require.Equal(t, map[string]int{"total": 7, "SymlinkTree": 1, "TestRunner": 1, "Genrule": 5}, parseAquerySummary(out))
}
