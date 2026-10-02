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

	_, testMonth := Standing(TestTopology())
	_, prodMonth := Standing(SmallProductionTopology())
	require.Greater(t, testMonth, prodMonth, "the production starting point drops the two client VMs")
}
