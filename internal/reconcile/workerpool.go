// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Requeue periods.
const (
	// poolResync re-resolves images (new AMI ⇒ new generation) and refreshes
	// the status even without snapshot changes.
	poolResync = 30 * time.Second
	// finalizeRequeue polls a retiring pool until its VMs are gone.
	finalizeRequeue = 2 * time.Second
)

// WorkerPoolReconciler is level-triggered and crash-only: every reconcile
// rebuilds the pool runtime from the resource, the catalog and the provider,
// hands it to the Fleet (which runs the autoscaler loop), and writes the status
// from the loop's latest snapshot. Leader-only (controller-runtime default).
type WorkerPoolReconciler struct {
	Client  client.Client
	Config  *config.Controller
	Builder *RuntimeBuilder
	Fleet   *Fleet
	Pools   *pools.Set
	Compute ports.Compute
	Clock   ports.Clock
	Events  *RecorderEvents
	Log     *slog.Logger
	// FastLaunch manages EC2 Fast Launch on Windows pools (nil disables it).
	FastLaunch *FastLaunchManager
	// Costs supplies status.estimatedCostTodayUSD (nil without cost accounting).
	Costs interface {
		TodayUSD(pool domain.PoolName) (float64, bool)
	}

	mu   sync.Mutex
	last map[domain.PoolName]*PoolRuntime
}

// Reconcile implements reconcile.Reconciler.
func (r *WorkerPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	name := domain.PoolName(req.Name)
	var wp v1alpha1.WorkerPool
	if err := r.Client.Get(ctx, req.NamespacedName, &wp); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if r.Events != nil {
		r.Events.Track(&wp)
	}
	deleting := !wp.DeletionTimestamp.IsZero()
	if !deleting && !controllerutil.ContainsFinalizer(&wp, v1alpha1.Finalizer) {
		controllerutil.AddFinalizer(&wp, v1alpha1.Finalizer)
		if err := r.Client.Update(ctx, &wp); err != nil {
			return ctrl.Result{}, err
		}
	}
	if deleting && !controllerutil.ContainsFinalizer(&wp, v1alpha1.Finalizer) {
		r.forget(name)
		return ctrl.Result{}, nil
	}

	hosts, err := r.macHosts(ctx, &wp)
	if err != nil {
		return ctrl.Result{}, err
	}
	prevGen := wp.Status.ImageGeneration
	if last := r.lastRuntime(name); last != nil && last.Spec.Generation != "" {
		prevGen = last.Spec.Generation
	}
	rt, resolveErr := r.Builder.Build(ctx, &wp, hosts, prevGen)
	if resolveErr != nil {
		r.Log.Warn("worker pool does not resolve", "pool", name, "err", resolveErr)
		if last := r.lastRuntime(name); last != nil || deleting {
			// Keep retiring/serving with the last good runtime; a deleted pool that
			// never resolved still gets its VMs drained and stopped.
			rt = r.Builder.RetireRuntime(&wp, last)
			if !deleting {
				rt.Spec.Deleting = false
			}
		}
	}
	if rt != nil {
		r.remember(name, rt)
		if rt.Resolved != nil && !deleting {
			r.Pools.Put(rt.Resolved)
		}
		r.Fleet.Upsert(rt)
	}

	snap, haveSnap := r.Fleet.Snapshot(name)
	now := r.Clock.Now()
	status := PoolStatus(&wp, rt, resolveErr, snap, haveSnap, now)
	if r.Costs != nil {
		if v, ok := r.Costs.TodayUSD(name); ok {
			status.EstimatedCostTodayUSD = fmt.Sprintf("%.2f", v)
		}
	}
	if !equality.Semantic.DeepEqual(status, wp.Status) {
		before := wp.DeepCopy()
		wp.Status = status
		patch, err := statusPatch(before, &wp, workerPoolStatusRequired...)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Client.Status().Patch(ctx, &wp, patch); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		r.conditionEvents(before, &wp)
	}

	if r.FastLaunch != nil && rt != nil && rt.EC2 != nil && rt.EC2.Windows {
		done, err := r.FastLaunch.Sync(ctx, &wp, rt, snap, deleting)
		if err != nil {
			r.Log.Warn("fast launch sync failed", "pool", name, "err", err)
		}
		if deleting && !done {
			return ctrl.Result{RequeueAfter: finalizeRequeue}, nil
		}
	}

	if deleting {
		finished, err := r.finished(ctx, name, rt, snap, haveSnap)
		if err != nil || !finished {
			return ctrl.Result{RequeueAfter: finalizeRequeue}, err
		}
		before := wp.DeepCopy()
		controllerutil.RemoveFinalizer(&wp, v1alpha1.Finalizer)
		if err := r.Client.Patch(ctx, &wp, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		r.Log.Info("worker pool retired: every VM stopped, finalizer removed", "pool", name)
		r.forget(name)
		return ctrl.Result{}, nil
	}
	requeue := poolResync
	if f := ReadFloorOverride(&wp); f != nil {
		// A temporary floor expires by itself: drop the annotation once it is
		// due and come back exactly then (level-triggered, R-SCALE-7).
		if left := f.ExpiresAt.Sub(now); left <= 0 {
			if err := patchAnnotation(ctx, r.Client, wp.Namespace, wp.Name, AnnFloorOverride, ""); err != nil {
				return ctrl.Result{}, err
			}
		} else if left < requeue {
			requeue = left
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// finished reports whether a retiring pool has no VM left (R-OPS-3): the loop
// observed the provider while retiring and is empty, and (EC2) a tag-filtered
// Describe confirms no instance of the pool is pending or running.
func (r *WorkerPoolReconciler) finished(ctx context.Context, name domain.PoolName, rt *PoolRuntime, snap Snapshot, haveSnap bool) (bool, error) {
	if !haveSnap || !snap.Deleting || !snap.Observed || !snap.Status.Empty {
		return false, nil
	}
	if rt != nil && rt.Spec.Provider == domain.ProviderEC2 && r.Compute != nil {
		left, err := r.Compute.Describe(ctx, ports.InstanceFilter{Cluster: r.Config.ClusterID, Pool: name,
			States: []ports.InstanceState{ports.InstancePending, ports.InstanceRunning, ports.InstanceStopping, ports.InstanceStopped}})
		if err != nil {
			return false, fmt.Errorf("verifying that pool %s has no instances left: %w", name, err)
		}
		if len(left) > 0 {
			return false, nil
		}
	}
	return true, nil
}

func (r *WorkerPoolReconciler) macHosts(ctx context.Context, wp *v1alpha1.WorkerPool) ([]v1alpha1.MacHost, error) {
	if wp.Spec.Provider != string(domain.ProviderTart) {
		return nil, nil
	}
	var list v1alpha1.MacHostList
	if err := r.Client.List(ctx, &list, client.InNamespace(wp.Namespace)); err != nil {
		return nil, err
	}
	if list.Items == nil {
		return []v1alpha1.MacHost{}, nil // known and empty: no host is admitted
	}
	return list.Items, nil
}

func (r *WorkerPoolReconciler) remember(name domain.PoolName, rt *PoolRuntime) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[domain.PoolName]*PoolRuntime{}
	}
	r.last[name] = rt
}

func (r *WorkerPoolReconciler) lastRuntime(name domain.PoolName) *PoolRuntime {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[name]
}

func (r *WorkerPoolReconciler) forget(name domain.PoolName) {
	r.Fleet.Remove(name)
	r.Pools.Delete(name)
	r.mu.Lock()
	delete(r.last, name)
	r.mu.Unlock()
}

// TartImagesFor returns the images of the Tart pools whose host selector
// matches labels (hosts pre-pull them, R-MAC-5).
func (r *WorkerPoolReconciler) TartImagesFor(labels map[string]string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rt := range r.last {
		if rt.Tart != nil && rt.Tart.Image != "" && !rt.Spec.Deleting && labelsMatch(rt.Tart.HostSelector, labels) {
			out = append(out, rt.Tart.Image)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// conditionEvents emits an event for every condition that changed status.
func (r *WorkerPoolReconciler) conditionEvents(before, after *v1alpha1.WorkerPool) {
	if r.Events == nil {
		return
	}
	for _, c := range after.Status.Conditions {
		var prev string
		for _, p := range before.Status.Conditions {
			if p.Type == c.Type {
				prev = string(p.Status)
			}
		}
		if prev == string(c.Status) {
			continue
		}
		bad := (c.Type == v1alpha1.ConditionDegraded) == (c.Status == "True")
		if c.Status == "Unknown" {
			continue
		}
		r.Events.PoolEvent(domain.PoolName(after.Name), bad, c.Reason, fmt.Sprintf("%s=%s: %s", c.Type, c.Status, c.Message))
	}
}

// Requests maps a pool name to a reconcile request (Fleet notifications).
func Requests(namespace string, pool domain.PoolName) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: string(pool)}}
}

var errNoBuildQueue = errors.New("no BuildQueue adapter: the autoscaler cannot run")
