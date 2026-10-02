// SPDX-License-Identifier: FSL-1.1-ALv2

package metrics_test

import (
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/metrics"
)

// contract mirrors docs/contracts.md §6 (controller fleet metrics): names and
// label names are API for dashboards, alerts, SLOs and e2e scenarios. The STS
// and certificate-expiry metrics of §6 are owned by internal/sts and
// internal/pki.
var contract = map[string][]string{
	"cucina_pool_desired":                    {"pool"},
	"cucina_pool_vms":                        {"pool", "state"},
	"cucina_pool_max":                        {"pool"},
	"cucina_vm_start_seconds":                {"phase", "pool"},
	"cucina_vm_stops_total":                  {"pool", "reason"},
	"cucina_ec2_api_errors_total":            {"code", "op"},
	"cucina_ec2_capacity_errors_total":       {"kind", "pool", "type"},
	"cucina_instance_seconds_total":          {"pool", "type"},
	"cucina_cost_usd_total":                  {"category", "pool"},
	"cucina_standing_cost_usd_per_month":     {"category"},
	"cucina_queue_queued":                    {"instance", "platform", "size_class"},
	"cucina_queue_oldest_seconds":            {"instance", "platform", "size_class"},
	"cucina_scale_decisions_total":           {"action", "pool"},
	"cucina_invariant_violations_total":      {"invariant"},
	"cucina_orphans":                         {"kind"},
	"cucina_idle_instances_with_empty_queue": {"pool"},
	"cucina_hosts":                           {"phase"},
	"cucina_host_heartbeat_age_seconds":      {"serial"},
}

// Guards R-OBS-1 / contracts §6 (exact metric names and labels) and R-TEST-8f
// (metric hygiene: testutil lint clean).
func TestMetricsMatchContractAndLintClean(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m, err := metrics.New(reg)
	require.NoError(t, err)

	// Touch every series once so that vectors are gathered.
	m.PoolDesired.WithLabelValues("linux").Set(1)
	m.SetPoolVMs("linux", map[string]int{metrics.StateBusy: 2})
	m.PoolMax.WithLabelValues("linux").Set(4)
	m.VMStartSeconds.WithLabelValues("linux", metrics.PhaseToRegistered).Observe(42)
	m.VMStops.WithLabelValues("linux", metrics.StopIdle).Inc()
	m.EC2APIErrors.WithLabelValues("RunInstances", "RequestLimitExceeded").Inc()
	m.EC2CapacityErrors.WithLabelValues("linux", "c8i.8xlarge", metrics.CapacityICE).Inc()
	m.InstanceSeconds.WithLabelValues("linux", "c8i.8xlarge").Add(60)
	m.CostUSD.WithLabelValues("linux", "compute").Add(0.5)
	m.StandingCostUSDPerMonth.WithLabelValues("ami").Set(3)
	m.QueueQueued.WithLabelValues("ISA=x86-64;OSFamily=linux", "1", "main").Set(7)
	m.QueueOldestSeconds.WithLabelValues("ISA=x86-64;OSFamily=linux", "1", "main").Set(3)
	m.ScaleDecisions.WithLabelValues("linux", "launch").Inc()
	m.InvariantViolated("InstancesNeverExceedMax")
	m.Orphans.WithLabelValues("volume").Set(0)
	m.IdleInstancesWithEmptyQueue.WithLabelValues("linux").Set(0)
	m.Hosts.WithLabelValues("Online").Set(1)
	m.HostHeartbeatAgeSeconds.WithLabelValues("C02ABC").Set(5)

	problems, err := testutil.GatherAndLint(reg)
	require.NoError(t, err)
	assert.Empty(t, problems)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	got := map[string][]string{}
	for _, mf := range mfs {
		var labels []string
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			labels = append(labels, lp.GetName())
		}
		sort.Strings(labels)
		got[mf.GetName()] = labels
	}
	assert.Equal(t, contract, got)

	// A second construction on the same registry shares the collectors.
	m2, err := metrics.New(reg)
	require.NoError(t, err)
	assert.InDelta(t, 1.0, testutil.ToFloat64(m2.InvariantViolations.WithLabelValues("InstancesNeverExceedMax")), 0)
}
