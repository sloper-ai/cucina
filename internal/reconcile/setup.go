// SPDX-License-Identifier: FSL-1.1-ALv2

// Package reconcile holds the WorkerPool and MacHost reconcilers and the
// autoscaler runtime (Fleet): one loop per pool that observes the scheduler
// and the provider, asks internal/scaling for a decision, executes it through
// the ports (Compute, HostFleet, BuildQueue), and feeds the results back.
package reconcile

import (
	"errors"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/cost"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// Options wire the reconcilers.
type Options struct {
	Config  *config.Controller
	Catalog *pools.Catalog
	Pools   *pools.Set

	Compute    ports.Compute
	BuildQueue ports.BuildQueue
	HostFleet  ports.HostFleet

	Clock   ports.Clock
	Rand    ports.Rand
	Metrics *metrics.Metrics
	Log     *slog.Logger
	// SD receives fresh instance observations (HTTP SD cache); optional.
	SD interface{ Publish([]ports.Instance) }
	// UserData renders EC2 boot data; optional.
	UserData func(pool domain.PoolName, generation string) ([]byte, error)
}

// ScalingConfig maps the controller configuration onto the planner's.
func ScalingConfig(c *config.Controller) scaling.Config {
	a := c.Autoscaler
	return scaling.Config{
		PollInterval:   c.Scheduler.PollInterval.Duration,
		QueueFailAfter: c.Scheduler.QueueFailAfter.Duration,
		BackoffMin:     a.ICEBackoffMin.Duration,
		BackoffMax:     a.ICEBackoffMax.Duration,
		Deadman: scaling.Deadman{
			IdleLimit:        a.DeadmanIdleLimit.Duration,
			UnreachableLimit: a.DeadmanUnreachableLimit.Duration,
			MaxUptime:        a.DeadmanMaxUptime.Duration,
		},
	}
}

// Components are the reconcilers and the runtime built by New.
type Components struct {
	Fleet      *Fleet
	WorkerPool *WorkerPoolReconciler
	MacHost    *MacHostReconciler
	// Notifications carries Fleet snapshot changes to the WorkerPool controller.
	Notifications chan event.TypedGenericEvent[*v1alpha1.WorkerPool]
	// Cost prices the recorded usage (nil without AWS or with cost disabled).
	Cost *CostModel
}

// New builds the Fleet and the reconcilers without a manager (tests drive
// Reconcile and Fleet.StepAll directly).
func New(o Options, ledgers LedgerStore, events *RecorderEvents) (*Components, error) {
	if o.BuildQueue == nil {
		return nil, errNoBuildQueue
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	c := &Components{Notifications: make(chan event.TypedGenericEvent[*v1alpha1.WorkerPool], 256)}
	ns := o.Config.Namespace
	notify := func(p domain.PoolName) {
		ev := event.TypedGenericEvent[*v1alpha1.WorkerPool]{Object: &v1alpha1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Name: string(p), Namespace: ns}}}
		select {
		case c.Notifications <- ev:
		default: // the periodic resync catches up
		}
	}
	var sweep time.Duration
	if o.Config.AWS != nil {
		sweep = o.Config.AWS.SweepInterval.Duration
	}
	c.Fleet = NewFleet(FleetOptions{
		ClusterID:     o.Config.ClusterID,
		PollInterval:  o.Config.Scheduler.PollInterval.Duration,
		Scaling:       ScalingConfig(o.Config),
		Clock:         o.Clock,
		Rand:          o.Rand,
		Log:           o.Log.With("component", "autoscaler"),
		Metrics:       o.Metrics,
		Compute:       o.Compute,
		BuildQueue:    o.BuildQueue,
		HostFleet:     o.HostFleet,
		Ledgers:       ledgers,
		Events:        events,
		UserData:      o.UserData,
		Bucket:        NewTokenBucket(o.Config.Autoscaler.RunInstancesBurst, o.Config.Autoscaler.RunInstancesRefill),
		Notify:        notify,
		SD:            o.SD,
		SweepInterval: sweep,
		Shadow:        o.Config.Autoscaler.Shadow,
	})
	var fl *FastLaunchManager
	c.WorkerPool = &WorkerPoolReconciler{
		Config:  o.Config,
		Builder: &RuntimeBuilder{Config: o.Config, Catalog: o.Catalog, Compute: o.Compute, Clock: o.Clock, Scaling: ScalingConfig(o.Config)},
		Fleet:   c.Fleet,
		Pools:   o.Pools,
		Compute: o.Compute,
		Clock:   o.Clock,
		Events:  events,
		Log:     o.Log.With("controller", "workerpool"),
	}
	if o.Compute != nil {
		fl = &FastLaunchManager{Compute: o.Compute, Clock: o.Clock, Log: o.Log.With("component", "fastlaunch")}
		c.WorkerPool.FastLaunch = fl
	}
	if o.Compute != nil && o.Config.AWS != nil && o.Config.Observability.CostEnabled {
		rates, ok := cost.DefaultRates(o.Config.AWS.Region)
		if !ok {
			o.Log.Warn("no built-in AWS rates for the region: EBS, snapshots and transfer are priced at $0", "region", o.Config.AWS.Region)
		}
		c.Cost = &CostModel{Rates: rates, Compute: o.Compute, Fleet: c.Fleet, Metrics: o.Metrics, Clock: o.Clock, Log: o.Log.With("component", "cost")}
	}
	c.MacHost = &MacHostReconciler{
		HostFleet: o.HostFleet,
		Config:    o.Config,
		Clock:     o.Clock,
		Metrics:   o.Metrics,
		Log:       o.Log.With("controller", "machost"),
	}
	return c, nil
}

// UseClient injects the Kubernetes client into the reconcilers.
func (c *Components) UseClient(cl client.Client) {
	c.WorkerPool.Client = cl
	c.MacHost.Client = cl
	if c.WorkerPool.FastLaunch != nil {
		c.WorkerPool.FastLaunch.Client = cl
	}
	c.MacHost.PoolImages = c.WorkerPool.TartImagesFor
}

// Setup builds the components and registers them with the manager: the Fleet
// as a leader-only runnable, and the WorkerPool and MacHost controllers
// (leader-only by default).
func Setup(mgr manager.Manager, o Options) (*Components, error) {
	if o.Config == nil || o.Catalog == nil || o.Pools == nil {
		return nil, errors.New("reconcile: config, catalog and pool set are required")
	}
	events := &RecorderEvents{Recorder: mgr.GetEventRecorder("cucina-controller"), Namespace: o.Config.Namespace}
	cl := mgr.GetClient()
	c, err := New(o, AnnotationLedgers{Client: cl, Namespace: o.Config.Namespace}, events)
	if err != nil {
		return nil, err
	}
	c.UseClient(cl)
	if err := mgr.Add(c.Fleet); err != nil {
		return nil, err
	}
	if c.Cost != nil {
		if err := mgr.Add(c.Cost); err != nil {
			return nil, err
		}
	}
	err = builder.ControllerManagedBy(mgr).
		Named("workerpool").
		For(&v1alpha1.WorkerPool{}).
		WatchesRawSource(source.Channel(c.Notifications, &handler.TypedEnqueueRequestForObject[*v1alpha1.WorkerPool]{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(c.WorkerPool)
	if err != nil {
		return nil, err
	}
	err = builder.ControllerManagedBy(mgr).
		Named("machost").
		For(&v1alpha1.MacHost{}).
		Complete(c.MacHost)
	if err != nil {
		return nil, err
	}
	return c, nil
}
