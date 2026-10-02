// SPDX-License-Identifier: FSL-1.1-ALv2

package controller_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/internal/controller"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/reconcile"
)

func poolWith(name string, conds ...metav1.Condition) v1alpha1.WorkerPool {
	return v1alpha1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: v1alpha1.WorkerPoolStatus{Conditions: conds}}
}

func cond(typ, status, reason string) metav1.Condition {
	return metav1.Condition{Type: typ, Status: metav1.ConditionStatus(status), Reason: reason, Message: reason}
}

// Guards UC16 as shown by `cucinactl status`: each condition the controller
// sees becomes exactly one alert — queue not declared, no capacity, missing
// image, startup failures, scheduler unreachable (once), host offline,
// orphans, idle VMs with empty queues beyond the idle timeout (cost leak),
// invariant violations and a failing canary — and a healthy fleet has none.
func TestDeriveAlerts(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	offline := v1alpha1.MacHost{Spec: v1alpha1.MacHostSpec{Serial: "C02OFF1"}, Status: v1alpha1.MacHostStatus{Phase: v1alpha1.MacHostOffline}}
	cases := []struct {
		name string
		in   controller.AlertInputs
		want []string
	}{
		{"healthy", controller.AlertInputs{Pools: []v1alpha1.WorkerPool{poolWith("linux",
			cond(v1alpha1.ConditionQueueDeclared, "True", "Declared"), cond(v1alpha1.ConditionCapacity, "True", "Available"))}}, nil},
		{"pool problems", controller.AlertInputs{Pools: []v1alpha1.WorkerPool{
			poolWith("a", cond(v1alpha1.ConditionQueueDeclared, "False", v1alpha1.ReasonQueueNotDeclared)),
			poolWith("b", cond(v1alpha1.ConditionCapacity, "False", v1alpha1.ReasonNoCapacity), cond(v1alpha1.ConditionImageResolved, "False", v1alpha1.ReasonImageMissing)),
			poolWith("c", cond(v1alpha1.ConditionDegraded, "True", v1alpha1.ReasonStartupFailures), cond(v1alpha1.ConditionCapacity, "False", v1alpha1.ReasonPaused)),
		}}, []string{controller.AlertImageMissing, controller.AlertNoCapacity, controller.AlertQueueNotDeclared, controller.AlertStartupFailures}},
		{"scheduler down is reported once", controller.AlertInputs{Pools: []v1alpha1.WorkerPool{
			poolWith("a", cond(v1alpha1.ConditionQueueDeclared, "Unknown", "SchedulerUnreachable")),
			poolWith("b", cond(v1alpha1.ConditionQueueDeclared, "Unknown", "SchedulerUnreachable")),
		}}, []string{controller.AlertSchedulerUnreachable}},
		{"fleet problems", controller.AlertInputs{Hosts: []v1alpha1.MacHost{offline}, Orphans: 3, Violations: 1,
			Canary: &canary.Result{Kind: canary.KindCache, Success: false, Error: "cas-read: mismatch"}, CanaryFailures: 2},
			[]string{controller.AlertInvariant, controller.AlertCanary, controller.AlertHostOffline, controller.AlertOrphans}}, // severity, then name
		// Found by the kind smoke test: the leader's first run right after `helm install`
		// fails while the STS Pods start, and one failed run is no alert yet.
		{"one failed canary run is not an alert", controller.AlertInputs{
			Canary: &canary.Result{Kind: canary.KindCache, Success: false, Error: "token: connection refused"}, CanaryFailures: 1}, nil},
		{"cost leak only after the idle timeout", controller.AlertInputs{
			Pools: []v1alpha1.WorkerPool{poolWith("linux"), poolWith("windows")},
			Snapshots: map[domain.PoolName]reconcile.Snapshot{
				"linux":   {IdleEmptySince: now.Add(-10 * time.Minute), Counts: map[string]int{"idle": 2}},
				"windows": {IdleEmptySince: now.Add(-11 * time.Minute), Counts: map[string]int{"idle": 1}},
			},
			IdleTimeouts: map[domain.PoolName]time.Duration{"linux": 5 * time.Minute, "windows": 10 * time.Minute},
		}, []string{controller.AlertCostLeak}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Now = now
			var got []string
			for _, a := range controller.DeriveAlerts(tc.in) {
				got = append(got, a.Name)
				assert.NotEmpty(t, a.Summary)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
