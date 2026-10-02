// SPDX-License-Identifier: FSL-1.1-ALv2

// Package metrics defines the controller's fleet metrics. Names and labels are
// API (docs/contracts.md §6): dashboards, PrometheusRules, SLOs and e2e
// scenarios query them, so change them only together with the contract.
//
// Everything is registered on one registry (controller-runtime's
// metrics.Registry in production) and served on the metrics listener next to
// the Prometheus HTTP service discovery endpoint (internal/httpsd). Three
// contract metrics are owned by the component that produces them and are
// registered on the same registry by the wiring (internal/controller):
// cucina_sts_exchanges_total and cucina_sts_token_ttl_seconds (internal/sts),
// cucina_cert_expiry_seconds (internal/pki ExpiryTracker).
package metrics

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Label values of cucina_pool_vms{state}.
const (
	StateLaunching  = "launching"
	StateRegistered = "registered"
	StateBusy       = "busy"
	StateIdle       = "idle"
	StateDraining   = "draining"
	StateStopped    = "stopped"
	StateFailed     = "failed"
)

// VMStates lists every cucina_pool_vms state (gauges are reset to 0 for absent states).
var VMStates = []string{StateLaunching, StateRegistered, StateBusy, StateIdle, StateDraining, StateStopped, StateFailed}

// Label values of cucina_vm_start_seconds{phase}.
const (
	PhaseToRunning     = "to_running"
	PhaseToRegistered  = "to_registered"
	PhaseToFirstAction = "to_first_action"
)

// Label values of cucina_vm_stops_total{reason}.
const (
	StopIdle           = "idle"
	StopDrain          = "drain"
	StopRollout        = "rollout"
	StopStartupTimeout = "startup-timeout"
	StopDeadman        = "deadman"
	StopMaintenance    = "maintenance"
)

// Label values of cucina_ec2_capacity_errors_total{kind}.
const (
	CapacityICE   = "ice"
	CapacityQuota = "quota"
)

// Metrics holds the controller's fleet metrics of the contract.
type Metrics struct {
	PoolDesired                 *prometheus.GaugeVec     // cucina_pool_desired{pool}
	PoolVMs                     *prometheus.GaugeVec     // cucina_pool_vms{pool,state}
	PoolMax                     *prometheus.GaugeVec     // cucina_pool_max{pool}
	VMStartSeconds              *prometheus.HistogramVec // cucina_vm_start_seconds{pool,phase}
	VMStops                     *prometheus.CounterVec   // cucina_vm_stops_total{pool,reason}
	EC2APIErrors                *prometheus.CounterVec   // cucina_ec2_api_errors_total{op,code}
	EC2CapacityErrors           *prometheus.CounterVec   // cucina_ec2_capacity_errors_total{pool,type,kind}
	InstanceSeconds             *prometheus.CounterVec   // cucina_instance_seconds_total{pool,type}
	CostUSD                     *prometheus.CounterVec   // cucina_cost_usd_total{pool,category}
	StandingCostUSDPerMonth     *prometheus.GaugeVec     // cucina_standing_cost_usd_per_month{category}
	QueueQueued                 *prometheus.GaugeVec     // cucina_queue_queued{platform,size_class,instance}
	QueueOldestSeconds          *prometheus.GaugeVec     // cucina_queue_oldest_seconds{platform,size_class,instance}
	ScaleDecisions              *prometheus.CounterVec   // cucina_scale_decisions_total{pool,action}
	InvariantViolations         *prometheus.CounterVec   // cucina_invariant_violations_total{invariant}
	Orphans                     *prometheus.GaugeVec     // cucina_orphans{kind}
	IdleInstancesWithEmptyQueue *prometheus.GaugeVec     // cucina_idle_instances_with_empty_queue{pool}
	Hosts                       *prometheus.GaugeVec     // cucina_hosts{phase}
	HostHeartbeatAgeSeconds     *prometheus.GaugeVec     // cucina_host_heartbeat_age_seconds{serial}
}

// StartBuckets cover Linux (~40 s), macOS VMs and the Windows slow path (> 4 min).
var StartBuckets = []float64{5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 240, 300, 600, 900}

func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
}

// New creates the metrics and registers them on reg. Registering twice on the
// same registry returns collectors bound to the first registration.
func New(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		PoolDesired: gauge("cucina_pool_desired", "VMs the autoscaler wants for the pool (last decision).", "pool"),
		PoolVMs:     gauge("cucina_pool_vms", "VMs of the pool by lifecycle state.", "pool", "state"),
		PoolMax:     gauge("cucina_pool_max", "Configured maximum VMs of the pool.", "pool"),
		VMStartSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cucina_vm_start_seconds",
			Help:    "VM start latency from the launch API call to running, registered and first action.",
			Buckets: StartBuckets,
		}, []string{"pool", "phase"}),
		VMStops:                     counter("cucina_vm_stops_total", "VMs stopped or terminated by the controller, by reason.", "pool", "reason"),
		EC2APIErrors:                counter("cucina_ec2_api_errors_total", "Failed EC2 API calls by operation and error code.", "op", "code"),
		EC2CapacityErrors:           counter("cucina_ec2_capacity_errors_total", "Launch attempts refused for capacity (ice) or quota reasons.", "pool", "type", "kind"),
		InstanceSeconds:             counter("cucina_instance_seconds_total", "Instance-seconds consumed by pool and instance type.", "pool", "type"),
		CostUSD:                     counter("cucina_cost_usd_total", "Estimated spend in US dollars by pool and category.", "pool", "category"),
		StandingCostUSDPerMonth:     gauge("cucina_standing_cost_usd_per_month", "Current standing cost (AMI snapshots, Fast Launch) in US dollars per month.", "category"),
		QueueQueued:                 gauge("cucina_queue_queued", "Queued operations per scheduler size class queue.", "platform", "size_class", "instance"),
		QueueOldestSeconds:          gauge("cucina_queue_oldest_seconds", "Age of the oldest queued operation per size class queue.", "platform", "size_class", "instance"),
		ScaleDecisions:              counter("cucina_scale_decisions_total", "Autoscaler decisions executed, by action.", "pool", "action"),
		InvariantViolations:         counter("cucina_invariant_violations_total", "Production invariant violations (R-TEST-7); any increase is a bug.", "invariant"),
		Orphans:                     gauge("cucina_orphans", "Orphaned pool-tagged resources found by the last sweep.", "kind"),
		IdleInstancesWithEmptyQueue: gauge("cucina_idle_instances_with_empty_queue", "Live idle worker VMs above the effective floor with continuously empty pool queues beyond idleTimeout plus 2 minutes of drain/termination grace.", "pool"),
		Hosts:                       gauge("cucina_hosts", "Mac hosts by phase.", "phase"),
		HostHeartbeatAgeSeconds:     gauge("cucina_host_heartbeat_age_seconds", "Seconds since the last heartbeat of a Mac host.", "serial"),
	}
	var errs []error
	reg1 := func(c prometheus.Collector, set func(prometheus.Collector)) {
		if err := reg.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				set(are.ExistingCollector)
				return
			}
			errs = append(errs, err)
		}
	}
	reg1(m.PoolDesired, func(c prometheus.Collector) { m.PoolDesired = c.(*prometheus.GaugeVec) })
	reg1(m.PoolVMs, func(c prometheus.Collector) { m.PoolVMs = c.(*prometheus.GaugeVec) })
	reg1(m.PoolMax, func(c prometheus.Collector) { m.PoolMax = c.(*prometheus.GaugeVec) })
	reg1(m.VMStartSeconds, func(c prometheus.Collector) { m.VMStartSeconds = c.(*prometheus.HistogramVec) })
	reg1(m.VMStops, func(c prometheus.Collector) { m.VMStops = c.(*prometheus.CounterVec) })
	reg1(m.EC2APIErrors, func(c prometheus.Collector) { m.EC2APIErrors = c.(*prometheus.CounterVec) })
	reg1(m.EC2CapacityErrors, func(c prometheus.Collector) { m.EC2CapacityErrors = c.(*prometheus.CounterVec) })
	reg1(m.InstanceSeconds, func(c prometheus.Collector) { m.InstanceSeconds = c.(*prometheus.CounterVec) })
	reg1(m.CostUSD, func(c prometheus.Collector) { m.CostUSD = c.(*prometheus.CounterVec) })
	reg1(m.StandingCostUSDPerMonth, func(c prometheus.Collector) { m.StandingCostUSDPerMonth = c.(*prometheus.GaugeVec) })
	reg1(m.QueueQueued, func(c prometheus.Collector) { m.QueueQueued = c.(*prometheus.GaugeVec) })
	reg1(m.QueueOldestSeconds, func(c prometheus.Collector) { m.QueueOldestSeconds = c.(*prometheus.GaugeVec) })
	reg1(m.ScaleDecisions, func(c prometheus.Collector) { m.ScaleDecisions = c.(*prometheus.CounterVec) })
	reg1(m.InvariantViolations, func(c prometheus.Collector) { m.InvariantViolations = c.(*prometheus.CounterVec) })
	reg1(m.Orphans, func(c prometheus.Collector) { m.Orphans = c.(*prometheus.GaugeVec) })
	reg1(m.IdleInstancesWithEmptyQueue, func(c prometheus.Collector) { m.IdleInstancesWithEmptyQueue = c.(*prometheus.GaugeVec) })
	reg1(m.Hosts, func(c prometheus.Collector) { m.Hosts = c.(*prometheus.GaugeVec) })
	reg1(m.HostHeartbeatAgeSeconds, func(c prometheus.Collector) { m.HostHeartbeatAgeSeconds = c.(*prometheus.GaugeVec) })
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return m, nil
}

// SetPoolVMs sets cucina_pool_vms for every state (absent states are 0, so a
// state that empties is visible as 0 rather than a stale value).
func (m *Metrics) SetPoolVMs(pool string, byState map[string]int) {
	for _, s := range VMStates {
		m.PoolVMs.WithLabelValues(pool, s).Set(float64(byState[s]))
	}
}

// ForgetPool removes the per-pool gauge series of a deleted pool. Counters are
// kept (they are cumulative and rate() handles their disappearance poorly).
func (m *Metrics) ForgetPool(pool string) {
	l := prometheus.Labels{"pool": pool}
	m.PoolDesired.DeletePartialMatch(l)
	m.PoolVMs.DeletePartialMatch(l)
	m.PoolMax.DeletePartialMatch(l)
	m.IdleInstancesWithEmptyQueue.DeletePartialMatch(l)
}

// InvariantViolated increments cucina_invariant_violations_total{invariant}; it
// is the hook the shared invariants package reports through (R-TEST-7).
func (m *Metrics) InvariantViolated(invariant string) {
	m.InvariantViolations.WithLabelValues(invariant).Inc()
}
