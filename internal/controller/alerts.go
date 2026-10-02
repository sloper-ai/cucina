// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/mgmt"
	"github.com/sloper-ai/cucina/internal/reconcile"
)

// Alerts the controller derives from its own state for `cucinactl status` and
// the TUI (UC16). They complement the chart's PrometheusRules, which page;
// these explain what the controller currently sees.
const (
	AlertQueueNotDeclared     = "CucinaPoolQueueNotDeclared"
	AlertNoCapacity           = "CucinaPoolNoCapacity"
	AlertImageMissing         = "CucinaPoolImageMissing"
	AlertStartupFailures      = "CucinaPoolStartupFailures"
	AlertSchedulerUnreachable = "CucinaSchedulerUnreachable"
	AlertHostOffline          = "CucinaHostOffline"
	AlertOrphans              = "CucinaOrphanedResources"
	AlertCostLeak             = "CucinaIdleInstancesWithEmptyQueue"
	AlertInvariant            = "CucinaInvariantViolation"
	AlertCanary               = "CucinaCanaryFailing"
)

// costLeakGrace is added to the pool's idle timeout before idle VMs with empty
// queues are reported (drain and terminate take a few polls).
const costLeakGrace = 2 * time.Minute

// AlertInputs is the controller state alerts are derived from.
type AlertInputs struct {
	Now       time.Time
	Pools     []v1alpha1.WorkerPool
	Hosts     []v1alpha1.MacHost
	Snapshots map[domain.PoolName]reconcile.Snapshot
	// IdleTimeouts are the resolved pools' idle timeouts.
	IdleTimeouts map[domain.PoolName]time.Duration
	Orphans      int
	OrphansAt    time.Time
	// Violations counts invariant violations since this process started.
	Violations      int64
	ViolationsSince time.Time
	// Canary is the last in-process cache canary result (nil before the first).
	Canary *canary.Result
}

// DeriveAlerts turns controller state into alerts, sorted by severity and name.
func DeriveAlerts(in AlertInputs) []mgmt.Alert {
	var out []mgmt.Alert
	add := func(name, severity, summary string, since time.Time, labels map[string]string) {
		out = append(out, mgmt.Alert{Name: name, Severity: severity, Summary: summary, Since: since, Labels: labels})
	}
	schedulerDown, schedulerDownSince := false, time.Time{}
	for _, wp := range in.Pools {
		lbl := map[string]string{"pool": wp.Name}
		cond := func(t string) (string, string, string, time.Time) {
			c := meta.FindStatusCondition(wp.Status.Conditions, t)
			if c == nil {
				return "", "", "", time.Time{}
			}
			return string(c.Status), c.Reason, c.Message, c.LastTransitionTime.Time
		}
		if s, reason, _, since := cond(v1alpha1.ConditionQueueDeclared); s == "Unknown" && reason == "SchedulerUnreachable" {
			if !schedulerDown || since.Before(schedulerDownSince) {
				schedulerDownSince = since
			}
			schedulerDown = true
		} else if s == "False" {
			add(AlertQueueNotDeclared, "warning", fmt.Sprintf("pool %s: the scheduler has no predeclared queue; its work is failed fast (add it to values.pools)", wp.Name), since, lbl)
		}
		if s, reason, msg, since := cond(v1alpha1.ConditionImageResolved); s == "False" {
			add(AlertImageMissing, "critical", fmt.Sprintf("pool %s: image missing (%s): %s", wp.Name, reason, msg), since, lbl)
		}
		if s, reason, msg, since := cond(v1alpha1.ConditionCapacity); s == "False" && reason != v1alpha1.ReasonPaused && reason != v1alpha1.ReasonImageMissing {
			sev := "warning"
			if reason == v1alpha1.ReasonNoCapacity {
				sev = "critical"
			}
			add(AlertNoCapacity, sev, fmt.Sprintf("pool %s: %s", wp.Name, msg), since, lbl)
		}
		if s, reason, msg, since := cond(v1alpha1.ConditionDegraded); s == "True" && reason == v1alpha1.ReasonStartupFailures {
			add(AlertStartupFailures, "warning", fmt.Sprintf("pool %s: %s", wp.Name, msg), since, lbl)
		}
		snap, ok := in.Snapshots[domain.PoolName(wp.Name)]
		if ok && !snap.IdleEmptySince.IsZero() {
			limit := in.IdleTimeouts[domain.PoolName(wp.Name)] + costLeakGrace
			if age := in.Now.Sub(snap.IdleEmptySince); limit > costLeakGrace && age > limit {
				add(AlertCostLeak, "warning", fmt.Sprintf("pool %s: %d idle VM(s) with empty queues for %s (idle timeout %s): standing cost",
					wp.Name, snap.Counts["idle"], age.Round(time.Second), limit-costLeakGrace), snap.IdleEmptySince, lbl)
			}
		}
	}
	if schedulerDown {
		add(AlertSchedulerUnreachable, "critical", "the scheduler's BuildQueueState API does not answer: no scaling decisions", schedulerDownSince, nil)
	}
	for _, h := range in.Hosts {
		if h.Status.Phase != v1alpha1.MacHostOffline {
			continue
		}
		since := time.Time{}
		if h.Status.LastHeartbeat != nil {
			since = h.Status.LastHeartbeat.Time
		}
		add(AlertHostOffline, "warning", fmt.Sprintf("Mac host %s is offline: its VMs are unavailable", h.Spec.Serial), since, map[string]string{"serial": h.Spec.Serial})
	}
	if in.Orphans > 0 {
		add(AlertOrphans, "warning", fmt.Sprintf("%d orphaned pool volume(s)/ENI(s) found by the last sweep", in.Orphans), in.OrphansAt, nil)
	}
	if in.Violations > 0 {
		add(AlertInvariant, "critical", fmt.Sprintf("%d invariant violation(s) since the controller started (see its logs)", in.Violations), in.ViolationsSince, nil)
	}
	if in.Canary != nil && !in.Canary.Success {
		add(AlertCanary, "warning", "the cache canary failed: "+in.Canary.Error, in.Canary.Started, nil)
	}
	rank := map[string]int{"critical": 0, "warning": 1, "info": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Severity] != rank[out[j].Severity] {
			return rank[out[i].Severity] < rank[out[j].Severity]
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// violationLog counts invariant violations for the alerts.
var violationLog struct {
	mu    sync.Mutex
	n     int64
	since time.Time
}

func noteViolation(now time.Time) {
	violationLog.mu.Lock()
	defer violationLog.mu.Unlock()
	if violationLog.n == 0 {
		violationLog.since = now
	}
	violationLog.n++
}

// alertSource implements mgmt.AlertSource from the controller's state.
type alertSource struct {
	d     *Deps
	fleet *reconcile.Fleet
}

func (a *alertSource) Alerts(ctx context.Context) ([]mgmt.Alert, error) {
	ns := client.InNamespace(a.d.Config.Namespace)
	var pools v1alpha1.WorkerPoolList
	if err := a.d.Client.List(ctx, &pools, ns); err != nil {
		return nil, err
	}
	var hosts v1alpha1.MacHostList
	if err := a.d.Client.List(ctx, &hosts, ns); err != nil {
		return nil, err
	}
	in := AlertInputs{Now: a.d.Clock.Now(), Pools: pools.Items, Hosts: hosts.Items, Snapshots: a.fleet.Snapshots(), IdleTimeouts: map[domain.PoolName]time.Duration{}}
	for _, r := range a.d.Pools.List() {
		in.IdleTimeouts[r.Spec.Name] = r.Spec.IdleTimeout
	}
	in.Orphans, in.OrphansAt = a.fleet.Orphans()
	violationLog.mu.Lock()
	in.Violations, in.ViolationsSince = violationLog.n, violationLog.since
	violationLog.mu.Unlock()
	if c, ok := Shared[*canaryLoop](a.d, sharedCanary); ok {
		in.Canary = c.last()
	}
	return DeriveAlerts(in), nil
}
