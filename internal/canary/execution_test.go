// SPDX-License-Identifier: FSL-1.1-ALv2

package canary_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/canary"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/pools"
)

func executionFixture(t *testing.T, objects ...*v1alpha1.WorkerPool) (*canary.ExecutionScheduler, fakes.SystemClock, *prometheus.Registry) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	var objs []client.Object
	for _, wp := range objects {
		objs = append(objs, wp)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.WorkerPool{}).WithObjects(objs...).Build()
	clock := fakes.SystemClock{} // virtual time inside the test's synctest bubble
	reg := prometheus.NewPedanticRegistry()
	return &canary.ExecutionScheduler{
		Client: cl, Reader: cl, Namespace: "cucina", InstanceNames: []string{"main", "ci"},
		Clock: clock, Deployment: "release/1", Timeout: canary.ExecutionTimeout, Metrics: canary.NewMetrics(reg),
		Catalog: &pools.Catalog{Platforms: []pools.Platform{
			{Name: "linux", Provider: domain.ProviderEC2, Runners: []pools.Runner{{Name: "native", Properties: map[string]string{"OSFamily": "linux", "ISA": "x86-64"}}}},
			{Name: "windows", Provider: domain.ProviderEC2, Runners: []pools.Runner{{Name: "native", Properties: map[string]string{"OSFamily": "windows", "ISA": "x86-64"}}}},
			{Name: "macos", Provider: domain.ProviderTart, Runners: []pools.Runner{
				{Name: "generic", Generic: true, Properties: map[string]string{"OSFamily": "macos", "ISA": "arm-a64"}},
				{Name: "xcode", Properties: map[string]string{"OSFamily": "macos", "ISA": "arm-a64", "xcode-version": "27.0"}},
			}},
		}},
	}, clock, reg
}

func readyPool(name, platform string) *v1alpha1.WorkerPool {
	return &v1alpha1.WorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cucina", Generation: 1},
		Spec:       v1alpha1.WorkerPoolSpec{Platform: platform, Provider: "ec2", Capacity: v1alpha1.CapacitySpec{Max: 2}},
		Status: v1alpha1.WorkerPoolStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Ready"},
			{Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionFalse, ObservedGeneration: 1, Reason: "Healthy"},
		}},
	}
}

func successfulExecution(clock fakes.SystemClock, target canary.ExecutionTarget) canary.Result {
	worker, _ := json.Marshal(map[string]string{"pool": target.Pool, "node": "synthetic-node"})
	return canary.Result{Kind: canary.KindExec, Pool: target.Pool, Instance: target.Instance, Started: clock.Now(), Success: true, Worker: string(worker)}
}

func sample(t *testing.T, reg *prometheus.Registry, name, pool string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "pool" && label.GetValue() == pool {
					return metric.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("missing %s for pool %s", name, pool)
	return 0
}

// Guards: R-TEST-7 — one uncached execution per pool daily and after each deploy;
// a naturally zero pool remains eligible, while a busy pool is never drained.
func TestExecutionScheduleCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		linux, windows, mac := readyPool("linux", "linux"), readyPool("windows", "windows"), readyPool("mac", "macos")
		windows.Spec.InstanceNames = []string{"ci"}
		mac.Spec.Provider = "tart"
		mac.Status.Busy, mac.Status.Registered = 1, 1
		s, clock, reg := executionFixture(t, linux, windows, mac)
		var targets []canary.ExecutionTarget
		s.Probe = func(ctx context.Context, target canary.ExecutionTarget) canary.Result {
			require.NoError(t, ctx.Err())
			_, bounded := ctx.Deadline()
			require.True(t, bounded, "cold-start execution must have a deadline")
			targets = append(targets, target)
			return successfulExecution(clock, target)
		}
		require.NoError(t, s.Tick(t.Context()))
		require.Equal(t, []canary.ExecutionTarget{
			{Pool: "linux", Instance: "main", Platform: map[string]string{"OSFamily": "linux", "ISA": "x86-64"}},
			{Pool: "mac", Instance: "main", Platform: map[string]string{"OSFamily": "macos", "ISA": "arm-a64", "xcode-version": "27.0"}},
			{Pool: "windows", Instance: "ci", Platform: map[string]string{"OSFamily": "windows", "ISA": "x86-64"}},
		}, targets)
		for _, pool := range []string{"linux", "windows", "mac"} {
			require.InDelta(t, 1.0, sample(t, reg, canary.MetricUp, pool), 0)
		}
		// New process, same release: restore truthful results into a fresh registry,
		// but suppress another paid run and do not re-count the historical attempts.
		restarted := *s
		reg = prometheus.NewPedanticRegistry()
		restarted.Metrics = canary.NewMetrics(reg)
		<-time.After(23 * time.Hour)
		require.NoError(t, restarted.Tick(t.Context()))
		require.Len(t, targets, 3)
		require.InDelta(t, 1.0, sample(t, reg, canary.MetricUp, "linux"), 0)
		require.InDelta(t, float64(clock.Now().Add(-23*time.Hour).Unix()), sample(t, reg, canary.MetricLastSuccess, "linux"), 0)
		families, err := reg.Gather()
		require.NoError(t, err)
		for _, family := range families {
			require.NotEqual(t, canary.MetricRuns, family.GetName(), "restoring gauges must not fabricate a new run")
		}
		<-time.After(time.Hour)
		require.NoError(t, restarted.Tick(t.Context()))
		require.Len(t, targets, 6)
		restarted.Deployment = "release/2"
		require.NoError(t, restarted.Tick(t.Context()))
		require.Len(t, targets, 9)
		require.NoError(t, s.Client.Get(t.Context(), client.ObjectKeyFromObject(mac), mac))
		require.Equal(t, int32(1), mac.Status.Busy)
		require.False(t, mac.Spec.Paused)
		require.Equal(t, int32(2), mac.Spec.Capacity.Max)
		// Historical success must not make a newly paused pool look healthy today.
		mac.Spec.Paused = true
		require.NoError(t, s.Client.Update(t.Context(), mac))
		require.NoError(t, restarted.Tick(t.Context()))
		require.Len(t, targets, 9)
		require.Zero(t, sample(t, reg, canary.MetricUp, "mac"))
		problems, err := testutil.GatherAndLint(reg)
		require.NoError(t, err)
		require.Empty(t, problems)
	})
}

// Guards: R-TEST-7 — paused, offline, failed and ambiguously routed pools never
// receive a passing execution result or submit work just to satisfy the canary.
func TestExecutionScheduleUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*v1alpha1.WorkerPool)
		peer bool
	}{
		{name: "paused", edit: func(w *v1alpha1.WorkerPool) { w.Spec.Paused = true }},
		{name: "capacity disabled", edit: func(w *v1alpha1.WorkerPool) { w.Spec.Capacity.Max = 0 }},
		{name: "offline", edit: func(w *v1alpha1.WorkerPool) {
			w.Status.Conditions[0].Status = metav1.ConditionFalse
			w.Status.Conditions[0].Reason = "NoCapacity"
		}},
		{name: "failed", edit: func(w *v1alpha1.WorkerPool) {
			w.Status.Conditions[1].Status = metav1.ConditionTrue
			w.Status.Conditions[1].Reason = "StartupFailures"
		}},
		{name: "not observed", edit: func(w *v1alpha1.WorkerPool) { w.Generation++ }},
		{name: "shared queue", peer: true},
		{name: "malformed reservation", edit: func(w *v1alpha1.WorkerPool) {
			w.Annotations = map[string]string{canary.ExecutionStateAnnotation: "not-json"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				wp := readyPool("linux", "linux")
				if tc.edit != nil {
					tc.edit(wp)
				}
				objects := []*v1alpha1.WorkerPool{wp}
				if tc.peer {
					objects = append(objects, readyPool("peer", "linux"))
				}
				s, clock, reg := executionFixture(t, objects...)
				s.Metrics.Observe(successfulExecution(clock, canary.ExecutionTarget{Pool: wp.Name, Instance: "main"}))
				s.Probe = func(context.Context, canary.ExecutionTarget) canary.Result {
					t.Fatal("ineligible pool submitted an execution")
					return canary.Result{}
				}
				_ = s.Tick(t.Context()) // Corrupt durable state fails closed, rather than executing.
				require.Zero(t, sample(t, reg, canary.MetricUp, wp.Name))
				require.NoError(t, s.Client.Get(t.Context(), client.ObjectKeyFromObject(wp), wp))
				require.Equal(t, int32(0), wp.Status.Registered)
			})
		})
	}
	for _, fault := range []string{"corrupt earlier pool", "inventory unavailable", "deleted pool"} {
		t.Run(fault, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a, z := readyPool("a", "linux"), readyPool("z", "windows")
				s, clock, reg := executionFixture(t, a, z)
				s.Probe = func(_ context.Context, target canary.ExecutionTarget) canary.Result {
					return successfulExecution(clock, target)
				}
				require.NoError(t, s.Tick(t.Context()))
				lastSuccess := sample(t, reg, canary.MetricLastSuccess, "z")
				s.Metrics.Observe(canary.Result{Kind: canary.KindCache, Started: clock.Now(), Success: true})
				switch fault {
				case "corrupt earlier pool":
					require.NoError(t, s.Client.Get(t.Context(), client.ObjectKeyFromObject(a), a))
					a.Annotations[canary.ExecutionStateAnnotation] = "not-json"
					require.NoError(t, s.Client.Update(t.Context(), a))
					require.NoError(t, s.Client.Get(t.Context(), client.ObjectKeyFromObject(z), z))
					z.Spec.Paused = true
					require.NoError(t, s.Client.Update(t.Context(), z))
				case "inventory unavailable":
					s.Reader = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{
						List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
							return errors.New("synthetic inventory outage")
						},
					})
				case "deleted pool":
					require.NoError(t, s.Client.Delete(t.Context(), z))
				}
				s.Probe = func(context.Context, canary.ExecutionTarget) canary.Result {
					t.Fatal("failed refresh must not authorize another execution")
					return canary.Result{}
				}
				_ = s.Tick(t.Context())
				require.Zero(t, sample(t, reg, canary.MetricUp, "z"))
				require.InDelta(t, lastSuccess, sample(t, reg, canary.MetricLastSuccess, "z"), 0, "history is not current-health evidence")
				require.InDelta(t, 1.0, sample(t, reg, canary.MetricUp, ""), 0, "cache-canary health must not be masked")
			})
		})
	}
}

// Guards: R-TEST-7 — write ahead of work, no overlap on leadership transfer,
// canceled attempts are not reported as success or retried into another cold start.
func TestExecutionScheduleLeaderHandoff(t *testing.T) {
	t.Run("lost leadership", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, clock, reg := executionFixture(t, readyPool("linux", "linux"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			s.Probe = func(_ context.Context, target canary.ExecutionTarget) canary.Result {
				calls++
				wp := &v1alpha1.WorkerPool{}
				require.NoError(t, s.Reader.Get(t.Context(), client.ObjectKey{Namespace: "cucina", Name: "linux"}, wp))
				require.NotEmpty(t, wp.Annotations[canary.ExecutionStateAnnotation], "reservation precedes submission")
				successor := *s
				successor.Deployment = "release/2"
				successor.Probe = func(context.Context, canary.ExecutionTarget) canary.Result {
					t.Fatal("new leader overlaps a reserved execution")
					return canary.Result{}
				}
				require.NoError(t, successor.Tick(t.Context()))
				cancel()
				return successfulExecution(clock, target)
			}
			require.ErrorIs(t, s.Tick(ctx), context.Canceled)
			require.Equal(t, 1, calls)
			require.Zero(t, sample(t, reg, canary.MetricUp, "linux"))
			// Allow the probe deadline, abandoned-operation retention and action to elapse.
			<-time.After(canary.ExecutionTimeout + 2*time.Minute)
			require.NoError(t, s.Tick(t.Context()))
			require.Equal(t, 1, calls, "incomplete run is a failure, not immediate retry permission")
			require.Zero(t, sample(t, reg, canary.MetricUp, "linux"))
		})
	})
	for _, latency := range []time.Duration{3 * time.Minute, 16 * time.Minute} {
		t.Run("reservation response delayed "+latency.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, clock, _ := executionFixture(t, readyPool("linux", "linux"))
				started := clock.Now()
				reservation := true
				s.Client = interceptor.NewClient(s.Client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						err := cl.Patch(ctx, obj, patch, opts...)
						if reservation {
							reservation = false
							// The API stored the reservation promptly, but the caller
							// receives its response much later (including ambiguity).
							<-time.After(latency)
						}
						return err
					},
				})
				calls := 0
				s.Probe = func(ctx context.Context, target canary.ExecutionTarget) canary.Result {
					calls++
					deadline, bounded := ctx.Deadline()
					require.True(t, bounded)
					require.Equal(t, started.Add(canary.ExecutionTimeout), deadline, "reservation latency consumes the execution budget")
					require.Less(t, latency, canary.ExecutionTimeout, "expired reservations cannot submit work")
					return successfulExecution(clock, target)
				}
				_ = s.Tick(t.Context()) // A timed-out reservation response may fail closed.
				if latency >= canary.ExecutionTimeout {
					require.Zero(t, calls)
				} else {
					require.Equal(t, 1, calls)
				}
			})
		})
	}
}
