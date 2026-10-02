// SPDX-License-Identifier: FSL-1.1-ALv2

package spend

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/cost"
	"github.com/sloper-ai/cucina/test/e2e/collect/awsinv"
)

// Guards NFR-C3 (campaign spend itemised against the §12 budget): sampled
// lifecycles price through internal/cost with the dated instance prices,
// volumes billed for the instance's lifetime and EC2's 60 s minimum.
func TestPriceSampledLifecycles(t *testing.T) {
	t0 := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	lcs := []awsinv.Lifecycle{
		{ID: "i-1", Pool: "linux-x86-64", Type: "c8i.8xlarge", Platform: "linux", Launched: t0, Gone: t0.Add(time.Hour),
			Volumes: []awsinv.Volume{{ID: "vol-1", GiB: 73, IOPS: 3000, MiBps: 125}}},
		{ID: "i-2", Pool: "windows-x86-64", Type: "c7a.8xlarge", Platform: "windows", Launched: t0, Gone: t0.Add(30 * time.Second)},
	}
	b := Price(cost.Usage{Launches: Launches(lcs)}, t0.Add(2*time.Hour))
	require.Empty(t, b.Unpriced)
	linux := 1.86976
	ebs := 73 * 0.096 / 730
	windowsMin := 3.51984 * 60 / 3600
	require.InDelta(t, linux+ebs+windowsMin, b.TotalUSD, 1e-5)

	// The small-functional campaign must price every selected OS/type pair.
	// In particular, standard Windows AMIs use RunInstances:0002, NOT the
	// cheaper RunInstances:0002:box (Windows without licences) product.
	for _, tc := range []struct {
		name, typ, platform string
		hourly              float64
	}{
		{"small x86 Linux", "m7i.large", "linux", 0.1176},
		{"small x86 Windows licence included", "m7i.large", "windows", 0.2096},
		{"small arm Linux", "m7g.large", "linux", 0.0952},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := cost.Usage{Launches: Launches([]awsinv.Lifecycle{{ID: "i-small", Pool: "small", Type: tc.typ, Platform: tc.platform, Launched: t0, Gone: t0.Add(time.Hour)}})}
			bill := Price(usage, t0.Add(2*time.Hour))
			require.Empty(t, bill.Unpriced, "selected small-runner shape needs a verified rate")
			require.InDelta(t, tc.hourly, bill.TotalUSD, 1e-8)
		})
	}
	for _, key := range []cost.InstanceKey{{Type: "unlisted.large"}, {Type: "m7g.large", Windows: true}} {
		bill := Price(cost.Usage{Launches: []cost.Launch{{Type: key.Type, Windows: key.Windows, Start: t0, End: t0.Add(time.Hour)}}}, t0.Add(2*time.Hour))
		require.NotEmpty(t, bill.Unpriced, "unknown type/OS rates must remain incomplete, never fall back to a cheaper rate")
	}

	_, testMonth := Standing(TestTopology())
	_, prodMonth := Standing(SmallProductionTopology())
	require.Greater(t, testMonth, prodMonth, "the production starting point drops the two client VMs")
}
