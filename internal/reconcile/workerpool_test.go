// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/controller"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/reconcile"
	"github.com/sloper-ai/cucina/internal/scaling"
)

func condition(t *testing.T, wp *v1alpha1.WorkerPool, typ string) (status, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(wp.Status.Conditions, typ)
	if c == nil {
		return "", ""
	}
	return string(c.Status), c.Reason
}

// Guards R-OPS-3 (uninstall: the finalizer drains and terminates every
// controller-created instance, volumes go with them, never a busy worker before
// its drain timeout), R-SCALE-5 (the launch ledger is written ahead of
// launches) and the WorkerPool status/conditions contract: a pool resolves,
// reports Ready, scales out for queued work, and `cucina-controller
// uninstall-prep` returns only once every instance is gone and no orphan is left.
func TestWorkerPoolLifecycleAndUninstall(t *testing.T) {
	c := apiServer(t)
	ns := namespace(t, c, "lifecycle")
	h := newHarness(t, 11, kubeOpt{c: c, ns: ns})
	ctx := context.Background()

	wp := linuxPool("linux", 4)
	wp.Namespace, wp.Generation = ns, 0
	require.NoError(t, c.Create(ctx, wp))
	rt := h.declare(wp, true)
	native := rt.Queues[0]

	h.reconcilePool("linux")
	var got v1alpha1.WorkerPool
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(wp), &got))
	assert.True(t, controllerutil.ContainsFinalizer(&got, v1alpha1.Finalizer), "finalizer added")

	h.step()
	h.reconcilePool("linux")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(wp), &got))
	for _, typ := range []string{v1alpha1.ConditionQueueDeclared, v1alpha1.ConditionImageResolved, v1alpha1.ConditionCapacity, v1alpha1.ConditionReady} {
		s, reason := condition(t, &got, typ)
		assert.Equal(t, "True", s, "%s (%s)", typ, reason)
	}
	s, _ := condition(t, &got, v1alpha1.ConditionDegraded)
	assert.Equal(t, "False", s)
	assert.Equal(t, "v1", got.Status.ImageGeneration)
	assert.Equal(t, testAMI, got.Status.ResolvedImage)

	// Work arrives: 40 actions of 10 minutes keep both VMs busy during the uninstall.
	for range 40 {
		_, err := h.bq.Submit(native, 10*time.Minute, "build")
		require.NoError(t, err)
	}
	for range 90 {
		h.step()
	}
	h.reconcilePool("linux")
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(wp), &got))
	assert.Equal(t, int32(2), got.Status.Registered)
	assert.Equal(t, int32(2), got.Status.Busy)
	var ledger scaling.Ledger
	require.NoError(t, json.Unmarshal([]byte(got.Annotations[reconcile.AnnLedger]), &ledger), "ledger annotation written ahead of launches")
	assert.GreaterOrEqual(t, ledger.Next, uint64(2))

	// helm uninstall → pre-delete hook.
	done := make(chan error, 1)
	go func() {
		done <- controller.UninstallPrep(ctx, c, h.clock, ns, 30*time.Minute, h.compute, h.cfg, slog.New(slog.DiscardHandler))
	}()
	var hookErr error
	finished := false
	for i := 0; i < 40*60 && !finished; i++ {
		h.step()
		h.reconcilePool("linux")
		select {
		case hookErr = <-done:
			finished = true
		default:
		}
	}
	require.True(t, finished, "uninstall-prep did not return")
	require.NoError(t, hookErr)
	h.noViolations()

	err := c.Get(ctx, client.ObjectKeyFromObject(wp), &got)
	assert.True(t, apierrors.IsNotFound(err), "pool removed after its finalizer, got %v", err)
	assert.Empty(t, h.alive("linux"), "no instance left running")
	for _, op := range h.bq.Ops() {
		assert.Equal(t, fakes.StageCompleted, op.Stage, op.Name)
		assert.False(t, op.Failed, "busy workers were not terminated before their actions finished: %s %s", op.Name, op.Message)
	}
	orphans, err := h.compute.ListOrphans(ctx, h.cfg.ClusterID)
	require.NoError(t, err)
	assert.Empty(t, orphans, "volumes and ENIs went with their instances")
}

// Guards R-RE-2 and R-SCALE-4: a pool that cannot obtain capacity never
// launches, says why in its conditions, and fails its queued work fast with
// FAILED_PRECONDITION and a clear message instead of letting clients hang.
func TestPoolConditionsFailFast(t *testing.T) {
	c := apiServer(t)
	ns := namespace(t, c, "fail-fast")
	ctx := context.Background()
	cases := []struct {
		name          string
		mutate        func(*v1alpha1.WorkerPool)
		declareQueues bool
		condition     string
		reason        string
		failsQueued   bool
	}{
		{"queue not declared", nil, false, v1alpha1.ConditionQueueDeclared, v1alpha1.ReasonQueueNotDeclared, false},
		{"max zero", func(p *v1alpha1.WorkerPool) { p.Spec.Capacity.Max = 0 }, true, v1alpha1.ConditionReady, "MaxZero", true},
		{"image missing", func(p *v1alpha1.WorkerPool) {
			p.Spec.Image = v1alpha1.ImageSpec{AMISelector: map[string]string{"cucina:image": "missing"}}
		}, true, v1alpha1.ConditionImageResolved, v1alpha1.ReasonImageMissing, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, uint64(20+i), kubeOpt{c: c, ns: ns})
			name := []string{"undeclared", "maxzero", "noimage"}[i]
			wp := linuxPool(name, 4)
			wp.Namespace, wp.Generation = ns, 0
			if tc.mutate != nil {
				tc.mutate(wp)
			}
			require.NoError(t, c.Create(ctx, wp))
			ref := linuxPool(name, 4) // resolvable twin to learn the queues
			rt := h.declare(ref, tc.declareQueues)
			var op string
			if tc.declareQueues {
				var err error
				op, err = h.bq.Submit(rt.Queues[0], time.Minute, "build")
				require.NoError(t, err)
			}
			for range 5 {
				h.reconcilePool(name)
				h.step()
			}
			h.reconcilePool(name)
			var got v1alpha1.WorkerPool
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(wp), &got))
			s, reason := condition(t, &got, tc.condition)
			assert.Equal(t, "False", s, tc.condition)
			assert.Equal(t, tc.reason, reason)
			ready, _ := condition(t, &got, v1alpha1.ConditionReady)
			assert.Equal(t, "False", ready)
			assert.Empty(t, h.compute.All(), "no launch")
			if tc.failsQueued {
				for _, o := range h.bq.Ops() {
					if o.Name != op {
						continue
					}
					assert.True(t, o.Failed, "queued work failed fast")
					assert.Equal(t, scaling.StatusFailedPrecondition, o.Code)
					assert.NotEmpty(t, o.Message)
				}
			}
			h.noViolations()
		})
	}
}
