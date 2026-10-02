// SPDX-License-Identifier: FSL-1.1-ALv2

package bbconfig_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/bbconfig"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/pools"
)

// Contract (docs/contracts.md §4): the WorkerSettings the controller hands out
// for every platform of the shipped catalog (internal/pools) render for a
// machine of that OS, worker and runner alike.
func TestShippedCatalogRenders(t *testing.T) {
	cat, err := pools.LoadCatalog("../../platforms/pools.json")
	require.NoError(t, err)
	cfg := &config.Controller{}
	cfg.Endpoints.WorkerScheduler = "workers.cucina.example.com:8983"
	cfg.Endpoints.WorkerStorage = "workers.cucina.example.com:8981"
	cfg.Endpoints.ServerName = "workers.cucina.example.com"
	cfg.Worker.MaximumMessageSizeBytes = 16 << 20
	cfg.Worker.WANCompressionForHosts = true
	machines := map[string]func() bbconfig.Machine{
		"linux":   linuxNVMeMachine,
		"windows": windowsMachine,
		"macos":   macMachine,
	}
	for _, p := range cat.Platforms {
		t.Run(p.Name, func(t *testing.T) {
			wp := &v1alpha1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: v1alpha1.WorkerPoolSpec{
				Platform: p.Name, Provider: string(p.Provider), SizeClass: "default", Capacity: v1alpha1.CapacitySpec{Max: 4},
			}}
			r, err := pools.Resolve(cat, wp, pools.Env{InstanceNames: []string{"main"}, VCPUsPerVM: 8, Generation: "v1"})
			require.NoError(t, err)
			settings := r.WorkerSettings(cfg, "node-1")
			machine, ok := machines[p.OS]
			require.True(t, ok, "no test machine for OS %q", p.OS)
			_, err = bbconfig.RenderWorker(settings, machine())
			require.NoError(t, err)
			_, err = bbconfig.RenderRunner(settings, machine())
			require.NoError(t, err)
		})
	}
}
