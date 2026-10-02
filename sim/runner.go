// SPDX-License-Identifier: FSL-1.1-ALv2

package sim

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
	"github.com/sloper-ai/cucina/invariants"
)

// Policy is an autoscaler policy version: the planner configuration plus an
// optional per-pool spec adjustment (for example another idle timeout).
type Policy struct {
	Name   string
	Config scaling.Config
	Spec   func(*scaling.Spec)
}

// DefaultPolicy is the shipped policy.
func DefaultPolicy() Policy { return Policy{Name: "default", Config: scaling.DefaultConfig()} }

// Options tune a run.
type Options struct {
	Policy Policy
	// Shadow, if set, plans with this policy on a clone of the state at every
	// step without acting and counts the differing decisions (R-TEST-7).
	Shadow *Policy
	// LogLines keeps the last n decision log lines in the result (0 = none).
	LogLines int
}

// Result is the outcome of one run.
type Result struct {
	Scenario   string
	Seed       uint64
	Policy     string
	Passed     bool
	Failures   []string    // unmet expectations
	Violations []Violation // invariant violations (first 50)
	Metrics    Metrics
	Log        []string
}

// Violation is an invariant violation at a simulated time offset.
type Violation struct {
	At time.Duration
	invariants.Violation
}

func (v Violation) String() string { return fmt.Sprintf("t=%s %s", v.At, v.Error()) }

// Metrics are the measured outcomes of a run.
type Metrics struct {
	Actions, Succeeded, Failed, Unfinished int
	WaitP50, WaitP95, WaitMax              time.Duration
	FailWaitMax                            time.Duration
	CostUSD, InstanceSeconds               float64
	Launches, MaxLive                      map[string]int
	Decisions, Stops                       map[string]int
	EndLive                                int
	MaxIdleWithEmptyQueue                  time.Duration
	ShadowDiffSteps                        int
	ShadowSamples                          []string
}

const sweepInterval = 5 * time.Minute

type action struct {
	pool     *poolRun
	queue    domain.QueueKey
	duration time.Duration
	arrival  time.Time
	op       string
	done     bool
	failed   bool
	started  time.Time
	failedAt time.Time
}

type poolRun struct {
	def     PoolDef
	spec    scaling.Spec
	st      *scaling.PoolState
	ledger  scaling.Ledger
	results []scaling.Result
	queues  []domain.QueueKey
	threads map[domain.QueueKey]int
	imageID string
	last    scaling.Decision
	shadow  *scaling.Planner
	vmSeq   int
}

type timed struct {
	at time.Time
	fn func()
}

type world struct {
	sc      *Scenario
	opt     Options
	ctx     context.Context
	start   time.Time
	clock   *fakes.Clock
	rnd     *fakes.Rand
	compute *fakes.Compute
	bq      *fakes.BuildQueue
	hosts   *fakes.HostFleet
	planner *scaling.Planner
	pools   []*poolRun
	byName  map[string]*poolRun
	bucket  scaling.TokenBucket
	cluster string

	actions   []*action
	pending   []*action // submitted, not yet done
	arrivals  []*action // sorted by arrival, not yet submitted
	byOp      map[string]*action
	timers    []timed
	faults    []Fault
	pendingMS bool // a mid-scale controller restart is armed

	startFailure map[string]time.Time // pool → launches before this never register
	imageMissing map[string]time.Time // pool → until
	noRegister   map[string]bool      // instance IDs that never register
	drainStart   map[string]time.Time // node → first AddDrain
	lastBusy     map[string]time.Time // node → last step observed busy (or registered)
	queueBusy    map[string]time.Time // pool → last step with queued work
	idleEmpty    map[string]time.Time // node → idle with empty queues since
	lastSweep    time.Time

	m          Metrics
	violations []Violation
	seenViol   map[string]bool
	log        []string
}

// Run runs a scenario with the default policy.
func Run(sc *Scenario) Result { return RunWith(sc, Options{Policy: DefaultPolicy()}) }

// RunWith runs a scenario with explicit options.
func RunWith(sc *Scenario, opt Options) Result {
	w, err := newWorld(sc, opt)
	if err != nil {
		return Result{Scenario: sc.Name, Seed: sc.Seed, Policy: opt.Policy.Name, Failures: []string{"setup: " + err.Error()}}
	}
	return w.run()
}

func newWorld(sc *Scenario, opt Options) (*world, error) {
	if opt.Policy.Name == "" {
		opt.Policy = DefaultPolicy()
	}
	start := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC) // a Monday morning
	rnd := fakes.NewRand(sc.Seed)
	clock := fakes.NewClock(start)
	w := &world{sc: sc, opt: opt, ctx: context.Background(), start: start, clock: clock, rnd: rnd,
		byName: map[string]*poolRun{}, byOp: map[string]*action{}, startFailure: map[string]time.Time{},
		imageMissing: map[string]time.Time{}, noRegister: map[string]bool{}, drainStart: map[string]time.Time{},
		lastBusy: map[string]time.Time{}, queueBusy: map[string]time.Time{}, idleEmpty: map[string]time.Time{},
		seenViol: map[string]bool{}, lastSweep: start}
	w.cluster = sc.Fleet.ClusterID
	if w.cluster == "" {
		w.cluster = "sim"
	}
	w.m.Launches, w.m.MaxLive, w.m.Decisions, w.m.Stops = map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}

	ccfg := fakes.DefaultComputeConfig()
	cd := sc.Fleet.Compute
	setD := func(dst *time.Duration, v Duration) {
		if v > 0 {
			*dst = v.D()
		}
	}
	setD(&ccfg.PendingMin, cd.PendingMin)
	setD(&ccfg.PendingMax, cd.PendingMax)
	setD(&ccfg.BootMin, cd.BootMin)
	setD(&ccfg.BootMax, cd.BootMax)
	setD(&ccfg.WindowsBootFast, cd.WindowsBootFast)
	setD(&ccfg.WindowsBootSlow, cd.WindowsBootSlow)
	setD(&ccfg.VisibleMax, cd.VisibleMax)
	setD(&ccfg.FastLaunchRefill, cd.FastLaunchRefill)
	ccfg.VCPUQuota = cd.VCPUQuota
	if cd.LaunchBurst > 0 {
		ccfg.Buckets["Launch"] = fakes.BucketSpec{Burst: cd.LaunchBurst, Rate: max(cd.LaunchRate, 0.1)}
	}
	ccfg.Prices = fakes.DefaultPrices()
	for typ, usd := range cd.Prices {
		ccfg.Prices[typ] = ports.InstancePrice{Type: typ, USDPerHour: usd, VCPU: 8, MemoryGiB: 16}
	}
	w.compute = fakes.NewCompute(clock, rnd.Child("compute"), ccfg)
	w.bq = fakes.NewBuildQueue(clock, rnd.Child("buildqueue"), fakes.DefaultBuildQueueConfig())
	hcfg := fakes.DefaultHostFleetConfig()
	setD(&hcfg.BootMin, sc.Fleet.HostFleet.BootMin)
	setD(&hcfg.BootMax, sc.Fleet.HostFleet.BootMax)
	w.hosts = fakes.NewHostFleet(clock, rnd.Child("hosts"), hcfg)
	for _, h := range sc.Fleet.Hosts {
		w.hosts.AddHost(ports.HostState{Serial: h.Serial, Name: h.Serial, Online: true, Approved: true, Slots: h.Slots, Labels: h.Labels})
	}

	cfg := opt.Policy.Config
	if sc.Fleet.QueueFailAfter > 0 {
		cfg.QueueFailAfter = sc.Fleet.QueueFailAfter.D()
	}
	w.planner = scaling.NewPlanner(cfg, rnd.Child("planner"))
	w.bucket = scaling.NewTokenBucket(5, 2, start)

	for _, def := range sc.Fleet.Pools {
		p, err := w.newPool(def, cfg)
		if err != nil {
			return nil, err
		}
		w.pools = append(w.pools, p)
		w.byName[def.Name] = p
	}
	w.compute.OnReady(func(in ports.Instance) {
		if p := w.byName[string(in.Pool)]; p != nil && !w.noRegister[in.ID] {
			w.bq.RegisterNode(in.Pool, in.ID, p.threads)
		}
	})
	w.compute.OnTerminated(func(in ports.Instance) { w.bq.RemoveNode(in.ID) })
	w.hosts.OnVMReady(func(_ string, vm domain.VM) {
		if p := w.byName[string(vm.Pool)]; p != nil && !w.noRegister[vm.ID] {
			w.bq.RegisterNode(vm.Pool, vm.ID, p.threads)
		}
	})
	w.hosts.OnVMStopped(func(_ string, vm domain.VM) { w.bq.RemoveNode(vm.ID) })

	if err := w.expandWorkload(); err != nil {
		return nil, err
	}
	w.faults = slices.Clone(sc.Faults)
	sort.SliceStable(w.faults, func(i, j int) bool { return w.faults[i].At < w.faults[j].At })
	return w, nil
}

func (w *world) newPool(def PoolDef, cfg scaling.Config) (*poolRun, error) {
	p := &poolRun{def: def, threads: map[domain.QueueKey]int{}}
	names := def.InstanceNames
	if len(names) == 0 {
		names = []string{"main"}
	}
	sizeClass := def.SizeClass
	if sizeClass == 0 {
		sizeClass = 1
	}
	gen := def.Generation
	if gen == "" {
		gen = "g1"
	}
	rollout := domain.RolloutPolicy(def.Rollout)
	if rollout == "" {
		rollout = domain.RolloutLazy
	}
	drain := def.DrainTimeout.D()
	if drain <= 0 {
		drain = 30 * time.Minute
	}
	spec := scaling.Spec{PoolSpec: domain.PoolSpec{Name: domain.PoolName(def.Name), Provider: domain.Provider(def.Provider),
		Platform: def.Platform, SizeClass: sizeClass, InstanceNames: names, MinRunning: def.MinRunning, Max: def.Max,
		IdleTimeout: def.IdleTimeout.D(), DrainTimeout: drain, StartupTimeout: def.StartupTimeout.D(), Generation: gen,
		Rollout: rollout, Paused: def.Paused},
		ClusterID: w.cluster, VCPUs: def.VCPUs, InstanceTypes: def.InstanceTypes, SubnetIDs: def.Subnets,
		DailyInstanceHourCap: def.DailyInstanceHourCap}
	if len(spec.SubnetIDs) == 0 && spec.Provider == domain.ProviderEC2 {
		spec.SubnetIDs = []string{"subnet-a"}
	}
	for _, r := range def.Runners {
		spec.Runners = append(spec.Runners, domain.Runner{Name: r.Name, Properties: maps.Clone(r.Properties), Concurrency: r.Concurrency})
	}
	for _, f := range def.Floors {
		fw, err := scaling.ParseFloorWindow(f.Name, f.Days, f.Start, f.End, f.MinRunning)
		if err != nil {
			return nil, err
		}
		spec.Floors = append(spec.Floors, fw)
	}
	if w.opt.Policy.Spec != nil {
		w.opt.Policy.Spec(&spec)
	}
	if err := scaling.ValidateSpec(spec, cfg); err != nil {
		return nil, fmt.Errorf("pool %s: %w", def.Name, err)
	}
	p.spec = spec
	p.queues = scaling.PoolQueues(spec.PoolSpec)
	for _, q := range p.queues {
		for _, r := range spec.Runners {
			if domain.PropertiesKey(r.Properties) == q.PlatformKey {
				p.threads[q] = r.Concurrency
			}
		}
	}
	if !def.Undeclared {
		w.bq.Declare(p.queues...)
	}
	p.st = scaling.NewPoolState(spec.Name, p.ledger)
	if w.opt.Shadow != nil {
		p.shadow = scaling.NewPlanner(w.opt.Shadow.Config, w.rnd.Child("shadow-"+def.Name))
	}
	if spec.Provider == domain.ProviderEC2 {
		p.imageID = def.Image.ID
		if p.imageID == "" {
			p.imageID = "ami-" + def.Name
		}
		w.compute.AddImage(ports.Image{ID: p.imageID, Name: def.Name, Platform: def.Image.Platform, Generation: gen, Version: gen, CreatedAt: w.start})
		if fl := def.FastLaunch; fl != nil && fl.Ready {
			w.compute.PreProvisionFastLaunch(p.imageID, fl.TargetCount)
		}
	}
	return p, nil
}

// ------------------------------------------------------------- workload

func (p *poolRun) queueFor(runner string) domain.QueueKey {
	for _, r := range p.spec.Runners {
		if runner == "" || r.Name == runner {
			return domain.QueueKey{InstanceNamePrefix: p.spec.InstanceNames[0], PlatformKey: domain.PropertiesKey(r.Properties), SizeClass: p.spec.SizeClass}
		}
	}
	return p.queues[0]
}

func (w *world) expandWorkload() error {
	add := func(p *poolRun, runner string, at, d time.Duration) {
		if at > w.sc.Duration.D() {
			return
		}
		w.arrivals = append(w.arrivals, &action{pool: p, queue: p.queueFor(runner), duration: d, arrival: w.start.Add(at)})
	}
	for _, a := range w.sc.Workload.Arrivals {
		n := max(a.Count, 1)
		for i := 0; i < n; i++ {
			add(w.byName[a.Pool], a.Runner, a.At.D()+time.Duration(i)*a.Every.D(), a.Duration.D())
		}
	}
	gr := w.rnd.Child("workload")
	for _, g := range w.sc.Workload.Generators {
		p := w.byName[g.Pool]
		mean := float64(time.Minute) / g.RatePerMin
		for t := g.From.D(); ; {
			t += time.Duration(-math.Log(1-gr.Float64()) * mean)
			if t >= g.Until.D() {
				break
			}
			for i := 0; i < max(g.Burst, 1); i++ {
				add(p, g.Runner, t, gr.Duration(g.MinDuration.D(), g.MaxDuration.D()))
			}
		}
	}
	if w.sc.Workload.Trace != "" {
		path := w.sc.Workload.Trace
		if !strings.HasPrefix(path, "/") && w.sc.dir != "" {
			path = w.sc.dir + "/" + path
		}
		recs, err := LoadTrace(path)
		if err != nil {
			return err
		}
		for _, r := range recs {
			var p *poolRun
			for _, c := range w.pools {
				if c.def.Platform == r.Platform || c.def.Name == r.Platform {
					p = c
					break
				}
			}
			if p == nil {
				return fmt.Errorf("trace platform %q matches no pool", r.Platform)
			}
			add(p, r.Runner, time.Duration(r.At*float64(time.Second)), time.Duration(r.Duration*float64(time.Second)))
		}
	}
	sort.SliceStable(w.arrivals, func(i, j int) bool { return w.arrivals[i].arrival.Before(w.arrivals[j].arrival) })
	return nil
}

func (w *world) submitDue(now time.Time) {
	for len(w.arrivals) > 0 && !w.arrivals[0].arrival.After(now) {
		a := w.arrivals[0]
		w.arrivals = w.arrivals[1:]
		w.actions = append(w.actions, a)
		w.submit(a, now)
		if !a.done {
			w.pending = append(w.pending, a)
		}
	}
}

func (w *world) submit(a *action, now time.Time) {
	name, err := w.bq.Submit(a.queue, a.duration, a.pool.def.Name)
	if err != nil {
		a.done, a.failed, a.failedAt = true, true, now
		return
	}
	a.op = name
	w.byOp[name] = a
}

// track follows each action's current operation; UNAVAILABLE (scheduler
// restart) is retried by the client like Bazel does, anything else is final.
func (w *world) track(now time.Time) {
	defer func() { w.pending = slices.DeleteFunc(w.pending, func(a *action) bool { return a.done }) }()
	for _, a := range w.pending {
		if a.done || a.op == "" {
			continue
		}
		op, ok := w.bq.Op(a.op)
		if !ok || op.Stage != fakes.StageCompleted {
			continue
		}
		switch {
		case !op.Failed:
			a.done, a.started = true, op.FirstStarted
		case op.Code == 14:
			delete(w.byOp, a.op)
			w.submit(a, now)
		default:
			a.done, a.failed, a.failedAt = true, true, op.CompletedAt
		}
	}
}

// --------------------------------------------------------------- the loop

func (w *world) run() Result {
	step := w.sc.Step.D()
	if step <= 0 {
		step = time.Second
	}
	drain := w.sc.Drain.D()
	if drain <= 0 {
		drain = 30 * time.Minute
	}
	end := w.start.Add(w.sc.Duration.D() + drain)
	for now := w.start; !now.After(end); now = now.Add(step) {
		w.clock.Set(now)
		w.runTimers(now)
		w.applyFaults(now)
		w.submitDue(now)
		w.compute.Tick()
		w.hosts.Tick()
		w.bq.Tick()
		w.track(now)
		w.observeGroundTruth(now)
		if w.pendingMS && w.launchedLastStep() {
			w.restartController(true)
		}
		for _, p := range w.pools {
			w.stepPool(p, now)
		}
		w.sweepOrphans(now)
		w.checkInvariants(now)
	}
	return w.finish(end)
}

func (w *world) runTimers(now time.Time) {
	sort.SliceStable(w.timers, func(i, j int) bool { return w.timers[i].at.Before(w.timers[j].at) })
	for len(w.timers) > 0 && !w.timers[0].at.After(now) {
		t := w.timers[0]
		w.timers = w.timers[1:]
		t.fn()
	}
}

func (w *world) after(d time.Duration, fn func()) {
	w.timers = append(w.timers, timed{at: w.clock.Now().Add(d), fn: fn})
}

func (w *world) launchedLastStep() bool {
	for _, p := range w.pools {
		for _, r := range p.results {
			if r.Action.Kind == scaling.ActLaunch && r.Err == nil {
				return true
			}
		}
	}
	return false
}

func (w *world) restartController(midScale bool) {
	w.pendingMS = false
	w.logf("controller restart (mid-scale=%t)", midScale)
	for _, p := range w.pools {
		// The new leader starts from the persisted ledger; in-memory state and
		// the results of the last decision are lost.
		p.st = scaling.NewPoolState(p.spec.Name, p.ledger)
		p.results = nil
		p.last = scaling.Decision{}
	}
	w.planner = scaling.NewPlanner(w.planner.Config(), w.rnd.Child(fmt.Sprintf("planner-%d", w.clock.Now().Unix())))
}

// ---------------------------------------------------------------- a pool step

func (w *world) stepPool(p *poolRun, now time.Time) {
	obs := w.observe(p, now)
	var shadowState *scaling.PoolState
	if p.shadow != nil {
		shadowState = p.st.Clone()
	}
	d := w.planner.Plan(p.spec, obs, p.st)
	if p.shadow != nil {
		spec := p.spec
		if w.opt.Shadow.Spec != nil {
			w.opt.Shadow.Spec(&spec)
		}
		c := p.shadow.Plan(spec, obs, shadowState)
		if diff := scaling.DiffDecisions(d, c); !diff.Empty() {
			w.m.ShadowDiffSteps++
			if len(w.m.ShadowSamples) < 20 {
				w.m.ShadowSamples = append(w.m.ShadowSamples, fmt.Sprintf("t=%s %s: %s", now.Sub(w.start), p.def.Name, diff))
			}
		}
	}
	p.last = d
	for _, v := range d.Violations {
		w.violate(now, v)
	}
	if d.LedgerChanged {
		p.ledger = d.Ledger // write-ahead persistence before any launch
	}
	if len(d.Actions) > 0 && w.opt.LogLines > 0 {
		var parts []string
		for _, a := range d.Actions {
			parts = append(parts, fmt.Sprintf("%s/%s%v", a.Kind, a.Reason, a.VMs))
		}
		w.logf("%s desired=%d cap=%d vms=%d hold=%s ledger=%d..%d %s", p.def.Name, d.Desired, d.Demand.Capacity, len(d.VMs), d.Hold,
			d.Ledger.Committed, d.Ledger.Next, strings.Join(parts, " "))
	}
	p.results = w.execute(p, d, obs, now)
}

func (w *world) observe(p *poolRun, now time.Time) scaling.Observation {
	ctx := w.ctx
	obs := scaling.Observation{Now: now, Results: p.results}
	p.results = nil
	if until, ok := w.imageMissing[p.def.Name]; ok && now.Before(until) {
		obs.ImageErr = fmt.Errorf("image of %s: %w", p.def.Name, ports.ErrImageNotFound)
	}
	if qs, err := w.bq.ListPlatformQueues(ctx); err == nil {
		obs.QueuesKnown = true
		present := map[domain.QueueKey]bool{}
		for _, q := range qs {
			if slices.Contains(p.queues, q.Key) {
				obs.Queues = append(obs.Queues, q)
				present[q.Key] = true
			}
		}
		obs.QueuedShare = w.share(p, qs)
		ok := true
		for _, q := range p.queues {
			if !present[q] {
				continue
			}
			ws, err := w.bq.ListWorkers(ctx, q)
			if err != nil {
				ok = false
				continue
			}
			for _, wk := range ws {
				if wk.ID[domain.LabelPool] == p.def.Name {
					obs.Workers = append(obs.Workers, wk)
				}
			}
			ds, err := w.bq.ListDrains(ctx, q)
			if err != nil {
				ok = false
				continue
			}
			obs.Drains = append(obs.Drains, ds...)
		}
		obs.WorkersKnown = ok
	}
	switch p.spec.Provider {
	case domain.ProviderEC2:
		ins, err := w.compute.Describe(ctx, ports.InstanceFilter{Cluster: w.cluster, Pool: p.spec.Name, States: []ports.InstanceState{
			ports.InstancePending, ports.InstanceRunning, ports.InstanceShuttingDown, ports.InstanceTerminated, ports.InstanceStopping, ports.InstanceStopped}})
		if err == nil {
			obs.ProviderKnown, obs.Instances = true, ins
		}
		obs.LaunchBudget = w.bucket.Budget(now)
		if p.spec.DailyInstanceHourCap > 0 {
			secs, _ := w.compute.InstanceSeconds()
			obs.InstanceSecondsToday = secs[p.spec.Name]
		}
	case domain.ProviderTart:
		if hs, err := w.hosts.Hosts(ctx); err == nil {
			obs.ProviderKnown, obs.Hosts = true, hs
			obs.TartFreeSlots = len(w.placements(p, hs, 1<<10, false))
		}
	}
	return obs
}

// share computes the pool's share of queues served by several pools.
func (w *world) share(p *poolRun, qs []domain.QueueObservation) map[domain.QueueKey]int {
	var in []scaling.ShareInput
	shared := false
	for _, o := range w.pools {
		in = append(in, scaling.ShareInput{Pool: o.spec.Name, Queues: o.queues, Eligible: o.last.Status.Eligible, Headroom: o.last.ThreadHeadroom})
		if o != p {
			for _, q := range o.queues {
				if slices.Contains(p.queues, q) {
					shared = true
				}
			}
		}
	}
	if !shared {
		return nil
	}
	return scaling.AttributeShared(qs, in)[p.spec.Name]
}

type placement struct{ host, vm string }

// placements returns up to n VM starts for a Tart pool: hosts with free slots,
// one VM per host before a second (R-POOL-6), restarting a stopped VM of the
// pool before cloning a new one (warm L1). New VM names are only allocated
// when assign is set (a count must not consume names).
func (w *world) placements(p *poolRun, hosts []ports.HostState, n int, assign bool) []placement {
	perHost := p.def.VMsPerHost
	if perHost <= 0 || perHost > 2 {
		perHost = 2
	}
	type cand struct {
		h       ports.HostState
		running int
		stopped []string
	}
	var cs []*cand
	for _, h := range hosts {
		if !h.Online || !h.Approved || h.Cordoned {
			continue
		}
		c := &cand{h: h, running: h.RunningVMs}
		for _, vm := range h.VMs {
			if vm.Pool == p.spec.Name && vm.State == domain.VMStopped {
				c.stopped = append(c.stopped, strings.TrimPrefix(vm.ID, h.Serial+"/"))
			}
		}
		cs = append(cs, c)
	}
	var out []placement
	for len(out) < n {
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].running != cs[j].running {
				return cs[i].running < cs[j].running
			}
			return cs[i].h.Serial < cs[j].h.Serial
		})
		var best *cand
		for _, c := range cs {
			if c.running < min(perHost, max(c.h.Slots, 1)) {
				best = c
				break
			}
		}
		if best == nil {
			break
		}
		name := ""
		switch {
		case len(best.stopped) > 0:
			name, best.stopped = best.stopped[0], best.stopped[1:]
		case assign:
			p.vmSeq++
			name = fmt.Sprintf("%s-%d", p.def.Name, p.vmSeq)
		}
		best.running++
		out = append(out, placement{host: best.h.Serial, vm: name})
	}
	return out
}

// ---------------------------------------------------------------- executor

func (w *world) execute(p *poolRun, d scaling.Decision, obs scaling.Observation, now time.Time) []scaling.Result {
	ctx := w.ctx
	results := make([]scaling.Result, 0, len(d.Actions))
	var tart []placement
	if p.spec.Provider == domain.ProviderTart {
		n := 0
		for _, a := range d.Actions {
			if a.Kind == scaling.ActLaunch {
				n++
			}
		}
		tart = w.placements(p, obs.Hosts, n, true)
	}
	vmsByID := map[string]domain.VM{}
	for _, vm := range d.VMs {
		vmsByID[vm.ID] = vm
	}
	for _, a := range d.Actions {
		res := scaling.Result{Action: a, At: now}
		switch a.Kind {
		case scaling.ActLaunch:
			if p.spec.Provider == domain.ProviderTart {
				if len(tart) == 0 {
					res.Err = scaling.ErrSkipped
					break
				}
				pl := tart[0]
				tart = tart[1:]
				res.Err = w.hosts.StartVM(ctx, pl.host, ports.StartVMRequest{Pool: p.spec.Name, Generation: a.Launch.Generation,
					Image: "ghcr.io/sloper-ai/cucina-worker-macos:" + a.Launch.Generation, VMName: pl.vm, CPU: 4, MemoryGiB: 8, DiskGiB: 100})
				if res.Err == nil {
					res.VM = pl.host + "/" + pl.vm
					w.m.Launches[p.def.Name]++
					if until, ok := w.startFailure[p.def.Name]; ok && now.Before(until) {
						w.noRegister[res.VM] = true
					}
				}
				break
			}
			if w.bucket.Take(1, now) == 0 {
				res.Err = scaling.ErrSkipped
				break
			}
			before := len(w.compute.LaunchesPerToken())
			in, err := w.compute.Launch(ctx, w.launchRequest(p, a))
			res.Err = err
			if err == nil {
				res.Instance = &in
				if len(w.compute.LaunchesPerToken()) > before {
					w.m.Launches[p.def.Name]++
					if until, ok := w.startFailure[p.def.Name]; ok && now.Before(until) {
						w.noRegister[in.ID] = true
					}
				}
			}
		case scaling.ActAddDrain, scaling.ActUndrain, scaling.ActRemoveDrain:
			res.PerVM = map[string]error{}
			for _, id := range a.VMs {
				pattern := ports.WorkerID{domain.LabelPool: p.def.Name, domain.LabelNode: id}
				for _, q := range p.queues {
					var err error
					if a.Kind == scaling.ActAddDrain {
						err = w.bq.AddDrain(ctx, q, pattern)
					} else {
						err = w.bq.RemoveDrain(ctx, q, pattern)
					}
					if err != nil && !errors.Is(err, ports.ErrQueueUnknown) && res.PerVM[id] == nil {
						res.PerVM[id] = err
					}
				}
				if a.Kind == scaling.ActAddDrain {
					if _, ok := w.drainStart[id]; !ok && res.PerVM[id] == nil {
						w.drainStart[id] = now
					}
					if a.Reason == scaling.StopIdle {
						w.checkIdleDrain(p, id, now)
					}
				} else if res.PerVM[id] == nil {
					delete(w.drainStart, id)
				}
			}
		case scaling.ActTerminate, scaling.ActStop:
			busy := w.bq.Executing()
			for _, id := range a.VMs {
				if busy[id] > 0 {
					ds, ok := w.drainStart[id]
					if !ok || now.Sub(ds) < p.spec.DrainTimeout {
						w.violate(now, invariants.Violation{Invariant: invariants.NeverTerminateBusyOrLeased, Pool: p.def.Name, Subject: id,
							Detail: fmt.Sprintf("%s with %d executing (ground truth), reason %s", a.Kind, busy[id], a.Reason)})
					}
				}
			}
			if a.Kind == scaling.ActTerminate {
				res.PerVM, res.Err = w.compute.Terminate(ctx, w.cluster, a.VMs)
			} else {
				res.PerVM = map[string]error{}
				for _, id := range a.VMs {
					host, vm, _ := strings.Cut(id, "/")
					if err := w.hosts.StopVM(ctx, host, vm, 2*time.Minute, string(a.Reason)); err != nil {
						res.PerVM[id] = err
					}
				}
			}
			if res.Err == nil {
				for _, id := range a.VMs {
					if res.PerVM[id] == nil {
						w.m.Stops[string(a.Reason)]++
						delete(w.drainStart, id)
					}
				}
			}
		case scaling.ActFailQueues:
			for _, q := range a.Queues {
				q := q
				if err := w.bq.KillOperations(ctx, ports.KillFilter{QueueWithoutWorkers: &q}, a.Code, a.Message); err != nil && res.Err == nil {
					res.Err = err
				}
			}
		}
		if res.Err == nil {
			w.m.Decisions[string(a.Kind)]++
		}
		results = append(results, res)
	}
	return results
}

func (w *world) launchRequest(p *poolRun, a scaling.Action) ports.LaunchRequest {
	return ports.LaunchRequest{
		Pool: p.spec.Name, Generation: a.Launch.Generation, Token: a.Launch.Token, ImageID: p.imageID,
		InstanceTypes: a.Launch.InstanceTypes, SubnetIDs: a.Launch.SubnetIDs, CapacityType: ports.OnDemand,
		RootVolume: ports.VolumeSpec{SizeGiB: 30, Type: "gp3"},
		Tags: map[string]string{
			domain.TagManagedBy: domain.ManagedByValue, domain.TagCluster: w.cluster, domain.TagPool: p.def.Name,
			domain.TagGeneration: a.Launch.Generation, domain.TagImageVersion: a.Launch.Generation,
			domain.TagLaunchToken: a.Launch.Token, domain.TagRole: "worker",
			"cucina:env": "sim",
		},
	}
}

// ------------------------------------------------------------ invariants

func (w *world) violate(now time.Time, v invariants.Violation) {
	key := string(v.Invariant) + "|" + v.Pool + "|" + v.Subject
	if w.seenViol[key] {
		return
	}
	w.seenViol[key] = true
	if len(w.violations) < 50 {
		w.violations = append(w.violations, Violation{At: now.Sub(w.start), Violation: v})
	}
	w.logf("VIOLATION %s", v.Error())
}

// observeGroundTruth records what a poll at this instant can see: busy nodes
// and pools with queued work (the operational meaning of "idle").
func (w *world) observeGroundTruth(now time.Time) {
	busy := w.bq.Executing()
	nodes := w.bq.Nodes()
	queued := map[string]bool{}
	if qs, err := w.bq.ListPlatformQueuesTruth(); err == nil {
		for _, q := range qs {
			if q.Queued == 0 {
				continue
			}
			for _, p := range w.pools {
				if slices.Contains(p.queues, q.Key) {
					queued[p.def.Name] = true
				}
			}
		}
	}
	for _, p := range w.pools {
		if queued[p.def.Name] {
			w.queueBusy[p.def.Name] = now
		}
	}
	for _, n := range nodes {
		if _, ok := w.lastBusy[n]; !ok || busy[n] > 0 {
			w.lastBusy[n] = now
		}
	}
	// NFR-C1: no worker idles with an empty queue longer than idleTimeout + drain grace.
	poolOf := w.nodePools()
	for _, n := range nodes {
		pool := poolOf[n]
		if pool == nil || busy[n] > 0 || queued[pool.def.Name] {
			delete(w.idleEmpty, n)
			continue
		}
		since, ok := w.idleEmpty[n]
		if !ok {
			w.idleEmpty[n] = now
			continue
		}
		w.m.MaxIdleWithEmptyQueue = max(w.m.MaxIdleWithEmptyQueue, now.Sub(since))
	}
	for n := range w.idleEmpty {
		if !slices.Contains(nodes, n) {
			delete(w.idleEmpty, n)
		}
	}
}

// nodePools maps every known node (instance ID or "<host>/<vm>") to its pool.
func (w *world) nodePools() map[string]*poolRun {
	out := map[string]*poolRun{}
	for _, in := range w.compute.All() {
		out[in.ID] = w.byName[string(in.Pool)]
	}
	hs, _ := w.hosts.Hosts(w.ctx)
	for _, h := range hs {
		for _, vm := range h.VMs {
			out[vm.ID] = w.byName[string(vm.Pool)]
		}
	}
	return out
}

// checkIdleDrain verifies an idle scale-in against the observable ground truth.
func (w *world) checkIdleDrain(p *poolRun, node string, now time.Time) {
	idle := p.spec.IdleTimeout
	if lb, ok := w.lastBusy[node]; ok && now.Sub(lb) < idle {
		w.violate(now, invariants.Violation{Invariant: invariants.ScaleInOnlyAfterIdleTimeout, Pool: p.def.Name, Subject: node,
			Detail: fmt.Sprintf("last busy %s ago < idleTimeout %s", now.Sub(lb), idle)})
	}
	if qb, ok := w.queueBusy[p.def.Name]; ok && now.Sub(qb) < idle {
		w.violate(now, invariants.Violation{Invariant: invariants.ScaleInOnlyAfterIdleTimeout, Pool: p.def.Name, Subject: node,
			Detail: fmt.Sprintf("pool queues non-empty %s ago < idleTimeout %s", now.Sub(qb), idle)})
	}
}

func (w *world) checkInvariants(now time.Time) {
	all := w.compute.All()
	for _, v := range invariants.CheckInstances(w.cluster, all) {
		w.violate(now, v)
	}
	for tok, n := range w.compute.LaunchesPerToken() {
		if n > 1 {
			w.violate(now, invariants.Violation{Invariant: invariants.NoDuplicateLaunchPerToken, Subject: tok, Detail: fmt.Sprintf("%d instances", n)})
		}
	}
	if now.Sub(w.start)%time.Minute == 0 {
		for _, r := range w.compute.Resources() {
			if !r.Deleted {
				for _, v := range invariants.CheckResourceTags(string(r.Kind), r.ID, r.Tags) {
					w.violate(now, v)
				}
			}
		}
	}
	hosts, _ := w.hosts.Hosts(w.ctx)
	for _, v := range invariants.CheckHostVMs(hosts) {
		w.violate(now, v)
	}
	if orphans, err := w.compute.ListOrphans(w.ctx, w.cluster); err == nil {
		for _, v := range invariants.CheckOrphans(orphans, 2*sweepInterval+time.Minute) {
			w.violate(now, v)
		}
	}
	drained := w.drainedNodes()
	live, total := map[string]int{}, map[string]int{}
	count := func(pool, node string) {
		total[pool]++
		if !drained[node] {
			live[pool]++
		}
	}
	for _, in := range all {
		if in.State == ports.InstancePending || in.State == ports.InstanceRunning {
			count(string(in.Pool), in.ID)
		}
	}
	for _, h := range hosts {
		for _, vm := range h.VMs {
			if vm.State == domain.VMLaunching || vm.State == domain.VMRegistered || vm.State == domain.VMUnavailable {
				count(string(vm.Pool), vm.ID)
			}
		}
	}
	for _, p := range w.pools {
		w.m.MaxLive[p.def.Name] = max(w.m.MaxLive[p.def.Name], total[p.def.Name])
		for _, v := range invariants.CheckPoolMax(p.def.Name, p.spec.Max, live[p.def.Name]) {
			w.violate(now, v)
		}
	}
}

// drainedNodes returns the nodes with a {pool,node} drain: retiring VMs.
func (w *world) drainedNodes() map[string]bool {
	out := map[string]bool{}
	for _, p := range w.pools {
		for _, q := range p.queues {
			ds, err := w.bq.ListDrainsTruth(q)
			if err != nil {
				continue
			}
			for _, d := range ds {
				out[d.Pattern[domain.LabelNode]] = true
			}
		}
	}
	return out
}

func (w *world) sweepOrphans(now time.Time) {
	if now.Sub(w.lastSweep) < sweepInterval {
		return
	}
	w.lastSweep = now
	if orphans, err := w.compute.ListOrphans(w.ctx, w.cluster); err == nil && len(orphans) > 0 {
		_, _ = w.compute.DeleteOrphans(w.ctx, w.cluster, orphans)
	}
}

func (w *world) logf(format string, args ...any) {
	if w.opt.LogLines <= 0 {
		return
	}
	w.log = append(w.log, fmt.Sprintf("t=%s ", w.clock.Now().Sub(w.start))+fmt.Sprintf(format, args...))
	if len(w.log) > w.opt.LogLines {
		w.log = w.log[len(w.log)-w.opt.LogLines:]
	}
}
