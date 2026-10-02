// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// LedgerStore persists a pool's launch ledger. The loop calls it before
// executing a decision's launches (write-ahead, R-SCALE-5).
type LedgerStore interface {
	SaveLedger(ctx context.Context, pool domain.PoolName, l scaling.Ledger) error
}

// EventSink records Kubernetes events about a pool; they form the VM lifecycle
// timeline the campaign report uses (§10.4).
type EventSink interface {
	PoolEvent(pool domain.PoolName, warning bool, reason, message string)
}

// FleetOptions configure the autoscaler runtime.
type FleetOptions struct {
	ClusterID    string
	PollInterval time.Duration
	Scaling      scaling.Config
	Clock        ports.Clock
	Rand         ports.Rand
	Log          *slog.Logger
	Metrics      *metrics.Metrics

	Compute    ports.Compute    // nil without AWS
	BuildQueue ports.BuildQueue // required
	HostFleet  ports.HostFleet  // nil without the host stream

	Ledgers LedgerStore
	Events  EventSink
	// UserData renders the public, non-secret EC2 boot data of a pool generation
	// (internal/workeragent/bootdata). Nil means no user data.
	UserData func(pool domain.PoolName, generation string) ([]byte, error)
	// Bucket is the shared RunInstances token bucket (R-SCALE-6).
	Bucket *TokenBucket
	// Notify is called (never blocking) when a pool's snapshot changed.
	Notify func(pool domain.PoolName)
	// SD receives every fresh instance observation (Prometheus HTTP SD cache).
	SD interface{ Publish([]ports.Instance) }

	// SweepInterval is the orphan sweep period (EC2); 0 disables the periodic sweep.
	SweepInterval time.Duration
	// OrphanGrace protects volumes/ENIs of launches in progress from the sweep.
	OrphanGrace time.Duration
	// TartStopTimeout bounds a graceful `tart stop`.
	TartStopTimeout time.Duration
	// Shadow decides without acting (config autoscaler.shadow, R-TEST-7).
	Shadow bool
}

// Fleet runs one autoscaler loop per WorkerPool on the leader (R-SCALE-5:
// leader election across 2 replicas). It is a manager.Runnable.
type Fleet struct {
	o      FleetOptions
	shared *sharedObs
	hist   history
	usage  usage

	mu     sync.Mutex
	loops  map[domain.PoolName]*poolLoop
	runCtx context.Context // set while Start runs

	sweepMu sync.Mutex
}

// NewFleet returns an idle fleet; pools are added by the WorkerPool reconciler.
func NewFleet(o FleetOptions) *Fleet {
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.OrphanGrace <= 0 {
		o.OrphanGrace = 10 * time.Minute
	}
	if o.TartStopTimeout <= 0 {
		o.TartStopTimeout = 2 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	f := &Fleet{o: o, loops: map[domain.PoolName]*poolLoop{}}
	f.shared = newSharedObs(o, o.PollInterval*9/10)
	return f
}

// Upsert installs or replaces a pool's runtime. A new pool starts a loop (on
// the leader); an existing loop picks the runtime up at its next decision.
func (f *Fleet) Upsert(rt *PoolRuntime) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.loops[rt.Spec.Name]; ok {
		l.setRuntime(rt)
		return
	}
	l := newPoolLoop(f, rt)
	f.loops[rt.Spec.Name] = l
	if f.runCtx != nil {
		l.start(f.runCtx)
	}
}

// Remove stops and forgets a pool's loop (after its finalizer completed or the
// resource is gone).
func (f *Fleet) Remove(name domain.PoolName) {
	f.mu.Lock()
	l, ok := f.loops[name]
	delete(f.loops, name)
	f.mu.Unlock()
	if ok {
		l.stop()
		if f.o.Metrics != nil {
			f.o.Metrics.ForgetPool(string(name))
		}
	}
}

// Has reports whether a loop exists for the pool.
func (f *Fleet) Has(name domain.PoolName) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.loops[name]
	return ok
}

// Snapshot returns the pool's latest snapshot.
func (f *Fleet) Snapshot(name domain.PoolName) (Snapshot, bool) {
	f.mu.Lock()
	l, ok := f.loops[name]
	f.mu.Unlock()
	if !ok {
		return Snapshot{}, false
	}
	return l.snapshot(), true
}

// Snapshots returns every pool's latest snapshot by name.
func (f *Fleet) Snapshots() map[domain.PoolName]Snapshot {
	out := map[domain.PoolName]Snapshot{}
	for _, l := range f.sortedLoops() {
		out[l.name] = l.snapshot()
	}
	return out
}

func (f *Fleet) sortedLoops() []*poolLoop {
	f.mu.Lock()
	defer f.mu.Unlock()
	ls := make([]*poolLoop, 0, len(f.loops))
	for _, l := range f.loops {
		ls = append(ls, l)
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].name < ls[j].name })
	return ls
}

// StepAll runs one decision for every pool, in name order, from fresh
// observations. Production loops step concurrently on their own tickers; tests
// and the scenario harness use StepAll for deterministic progress.
func (f *Fleet) StepAll(ctx context.Context) {
	f.shared.invalidate()
	for _, l := range f.sortedLoops() {
		l.step(ctx)
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: only the
// leader scales (R-SCALE-5).
func (f *Fleet) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable: one goroutine per pool plus the orphan sweep.
func (f *Fleet) Start(ctx context.Context) error {
	f.mu.Lock()
	f.runCtx = ctx
	for _, l := range f.loops {
		l.start(ctx)
	}
	f.mu.Unlock()
	f.o.Log.Info("autoscaler started", "pollInterval", f.o.PollInterval.String())

	if f.o.Compute != nil && f.o.SweepInterval > 0 {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-f.o.Clock.After(f.o.SweepInterval):
				}
				if _, err := f.SweepOrphans(ctx); err != nil && ctx.Err() == nil {
					f.o.Log.Warn("orphan sweep failed", "err", err)
				}
			}
		}()
	}
	<-ctx.Done()
	f.mu.Lock()
	f.runCtx = nil
	loops := make([]*poolLoop, 0, len(f.loops))
	for _, l := range f.loops {
		loops = append(loops, l)
	}
	f.mu.Unlock()
	for _, l := range loops {
		l.stop()
	}
	return nil
}

// SweepOrphans lists pool-tagged volumes/ENIs that are not attached to a live
// instance (R-POOL-2), exports cucina_orphans{kind}, and deletes those older
// than the grace period. It also backs `cucinactl pools gc`.
func (f *Fleet) SweepOrphans(ctx context.Context) (int, error) {
	if f.o.Compute == nil {
		return 0, errors.New("no EC2 provider configured")
	}
	f.sweepMu.Lock()
	defer f.sweepMu.Unlock()
	orphans, err := f.o.Compute.ListOrphans(ctx, f.o.ClusterID)
	if err != nil {
		return 0, err
	}
	counts := map[ports.OrphanKind]int{ports.OrphanVolume: 0, ports.OrphanENI: 0}
	var old []ports.Orphan
	for _, o := range orphans {
		counts[o.Kind]++
		if o.Age >= f.o.OrphanGrace {
			old = append(old, o)
		}
	}
	if f.o.Metrics != nil {
		for k, n := range counts {
			f.o.Metrics.Orphans.WithLabelValues(string(k)).Set(float64(n))
		}
	}
	if len(old) == 0 {
		return 0, nil
	}
	perID, err := f.o.Compute.DeleteOrphans(ctx, f.o.ClusterID, old)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, o := range old {
		if e := perID[o.ID]; e != nil {
			f.o.Log.Warn("deleting orphan failed", "kind", o.Kind, "id", o.ID, "pool", o.Pool, "err", e)
			continue
		}
		deleted++
		f.o.Log.Info("deleted orphan", "kind", o.Kind, "id", o.ID, "pool", o.Pool, "age", o.Age.String())
		if f.o.Events != nil && o.Pool != "" {
			f.o.Events.PoolEvent(o.Pool, true, "OrphanDeleted", string(o.Kind)+" "+o.ID+" was not attached to a live instance and was deleted")
		}
	}
	return deleted, nil
}

// errorCode maps port sentinels to a short code for logs. The EC2 adapter
// itself exports cucina_ec2_api_errors_total{op,code} with the real AWS codes
// (its OnAPIError hook), so the executor does not count them again.
func errorCode(err error) string {
	switch {
	case errors.Is(err, ports.ErrInsufficientCapacity):
		return "InsufficientInstanceCapacity"
	case errors.Is(err, ports.ErrQuotaExceeded):
		return "QuotaExceeded"
	case errors.Is(err, ports.ErrThrottled):
		return "Throttled"
	case errors.Is(err, ports.ErrImageNotFound):
		return "ImageNotFound"
	case errors.Is(err, ports.ErrNotOwned):
		return "NotOwned"
	case errors.Is(err, ports.ErrNotFound):
		return "NotFound"
	case errors.Is(err, ports.ErrInvalid):
		return "InvalidRequest"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "Timeout"
	default:
		return "Other"
	}
}

// eligibleElsewhere reports whether a pool other than self can currently serve
// queue q (its last decision was eligible). fail-queues must never fail work
// another pool will pick up (shared macOS generic queues).
func (f *Fleet) eligibleElsewhere(self domain.PoolName, q domain.QueueKey) bool {
	for _, l := range f.sortedLoops() {
		if l.name == self {
			continue
		}
		rt := l.runtime()
		served := false
		for _, k := range rt.Queues {
			if k == q {
				served = true
				break
			}
		}
		if served && l.snapshot().Status.Eligible && !rt.Spec.Deleting {
			return true
		}
	}
	return false
}

// queuedShare attributes queued work of queues served by several pools (the
// macOS generic runner) so that each pool only scales for its share.
func (f *Fleet) queuedShare(self domain.PoolName, rt *PoolRuntime, all []domain.QueueObservation) map[domain.QueueKey]int {
	var inputs []scaling.ShareInput
	shared := false
	for _, l := range f.sortedLoops() {
		lrt := l.runtime()
		if lrt.Spec.Deleting && l.name != self {
			continue
		}
		snap := l.snapshot()
		inputs = append(inputs, scaling.ShareInput{Pool: l.name, Queues: lrt.Queues, Eligible: snap.Status.Eligible, Headroom: snap.Headroom})
		if l.name != self {
			for _, q := range lrt.Queues {
				if slices.Contains(rt.Queues, q) {
					shared = true
				}
			}
		}
	}
	if !shared {
		return nil
	}
	return scaling.AttributeShared(all, inputs)[self]
}

// sharedObs coalesces the cluster-wide reads that every pool needs, so N pools
// cost one ListPlatformQueues, one Describe and one Hosts call per interval.
type sharedObs struct {
	o   FleetOptions
	ttl time.Duration

	mu        sync.Mutex
	queues    cached[[]domain.QueueObservation]
	instances cached[[]ports.Instance]
	hosts     cached[[]ports.HostState]
}

type cached[T any] struct {
	v   T
	err error
	at  time.Time
	ok  bool
}

func newSharedObs(o FleetOptions, ttl time.Duration) *sharedObs { return &sharedObs{o: o, ttl: ttl} }

func (s *sharedObs) invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queues.ok, s.instances.ok, s.hosts.ok = false, false, false
}

func fresh[T any](c *cached[T], now time.Time, ttl time.Duration) bool {
	return c.ok && now.Sub(c.at) < ttl
}

func (s *sharedObs) getQueues(ctx context.Context) ([]domain.QueueObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.o.Clock.Now()
	if !fresh(&s.queues, now, s.ttl) {
		v, err := s.o.BuildQueue.ListPlatformQueues(ctx)
		logTransition(s.o, "ListPlatformQueues", s.queues.err, err)
		s.queues = cached[[]domain.QueueObservation]{v: v, err: err, at: now, ok: true}
		if err == nil && s.o.Metrics != nil {
			exportQueues(s.o.Metrics, v)
		}
	}
	return s.queues.v, s.queues.err
}

// describeStates includes terminated instances: the planner needs to see a
// launch that ended so it can account for it (R-SCALE-5).
var describeStates = []ports.InstanceState{ports.InstancePending, ports.InstanceRunning, ports.InstanceShuttingDown, ports.InstanceTerminated, ports.InstanceStopping, ports.InstanceStopped}

func (s *sharedObs) getInstances(ctx context.Context) ([]ports.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.o.Compute == nil {
		return nil, errors.New("no EC2 provider configured")
	}
	now := s.o.Clock.Now()
	if !fresh(&s.instances, now, s.ttl) {
		v, err := s.o.Compute.Describe(ctx, ports.InstanceFilter{Cluster: s.o.ClusterID, States: describeStates})
		logTransition(s.o, "DescribeInstances", s.instances.err, err)
		s.instances = cached[[]ports.Instance]{v: v, err: err, at: now, ok: true}
		if err == nil && s.o.SD != nil {
			s.o.SD.Publish(v)
		}
	}
	return s.instances.v, s.instances.err
}

func (s *sharedObs) getHosts(ctx context.Context) ([]ports.HostState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.o.HostFleet == nil {
		return nil, errors.New("no host stream configured")
	}
	now := s.o.Clock.Now()
	if !fresh(&s.hosts, now, s.ttl) {
		v, err := s.o.HostFleet.Hosts(ctx)
		logTransition(s.o, "Hosts", s.hosts.err, err)
		s.hosts = cached[[]ports.HostState]{v: v, err: err, at: now, ok: true}
	}
	return s.hosts.v, s.hosts.err
}

// logTransition logs when an observation source starts or stops failing (not
// on every poll).
func logTransition(o FleetOptions, source string, before, after error) {
	switch {
	case before == nil && after != nil:
		o.Log.Warn("observation source failing; decisions only take actions that are safe without it", "source", source, "code", errorCode(after), "err", after)
	case before != nil && after == nil:
		o.Log.Info("observation source recovered", "source", source)
	}
}

func exportQueues(m *metrics.Metrics, qs []domain.QueueObservation) {
	m.QueueQueued.Reset()
	for _, q := range qs {
		m.QueueQueued.WithLabelValues(q.Key.PlatformKey, itoa(q.Key.SizeClass), q.Key.InstanceNamePrefix).Set(float64(q.Queued))
	}
}
