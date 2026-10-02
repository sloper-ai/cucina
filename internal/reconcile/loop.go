// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
	"github.com/sloper-ai/cucina/invariants"
)

// poolLoop drives one pool: observe → Plan → persist ledger → execute → feed
// results back (docs/dev/scaling.md "executor contract").
type poolLoop struct {
	name    domain.PoolName
	f       *Fleet
	planner *scaling.Planner

	mu   sync.Mutex // guards rt, snap
	rt   *PoolRuntime
	snap Snapshot

	stepMu  sync.Mutex // one decision at a time; guards the fields below
	st      *scaling.PoolState
	results []scaling.Result
	starts  map[string]*startTrack
	acct    accounting
	// attempted holds launch tokens already sent to the provider (a later
	// launch with the same token is a retry that EC2 deduplicates).
	attempted map[string]time.Time
	// pendingTart are VMs started earlier in the current decision.
	pendingTart []string
	// idleEmpty tracks continuous idle-and-empty-queue evidence per non-floor VM,
	// including draining/stopping VMs whose provider call has not taken effect.
	idleEmpty map[string]time.Time
	// idleEmptySince is the oldest non-floor registered VM's idleEmpty time.
	idleEmptySince time.Time
	last           struct {
		scale time.Time
		err   string
	}

	cancel context.CancelFunc
	done   chan struct{}
}

type startTrack struct {
	launched                time.Time
	running, registered     bool
	firstAction             bool
	toRunning, toRegistered time.Duration
}

type accounting struct {
	at      time.Time
	day     int
	seconds float64
}

func newPoolLoop(f *Fleet, rt *PoolRuntime) *poolLoop {
	l := &poolLoop{
		name:      rt.Spec.Name,
		f:         f,
		planner:   scaling.NewPlanner(f.o.Scaling, f.o.Rand),
		rt:        rt,
		st:        scaling.NewPoolState(rt.Spec.Name, rt.Ledger),
		starts:    map[string]*startTrack{},
		attempted: map[string]time.Time{},
	}
	now := f.o.Clock.Now().UTC()
	l.acct = accounting{at: now, day: now.YearDay(), seconds: rt.InstanceSecondsToday}
	return l
}

func (l *poolLoop) setRuntime(rt *PoolRuntime) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rt = rt
}

func (l *poolLoop) runtime() *PoolRuntime {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rt
}

func (l *poolLoop) snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snap
}

func (l *poolLoop) start(parent context.Context) {
	if l.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	l.cancel, l.done = cancel, make(chan struct{})
	go func() {
		defer close(l.done)
		for {
			l.step(ctx)
			select {
			case <-ctx.Done():
				return
			case <-l.f.o.Clock.After(l.f.o.PollInterval):
			}
		}
	}()
}

func (l *poolLoop) stop() {
	if l.cancel == nil {
		return
	}
	l.cancel()
	<-l.done
	l.cancel = nil
}

// step runs one decision.
func (l *poolLoop) step(ctx context.Context) {
	l.stepMu.Lock()
	defer l.stepMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	rt := l.runtime()
	now := l.f.o.Clock.Now()
	obs := l.observe(ctx, rt, now)
	l.results = nil
	d := l.planner.Plan(rt.Spec, obs, l.st)
	if len(d.Violations) > 0 {
		invariants.Report(d.Violations...)
		for _, v := range d.Violations {
			l.f.o.Log.Error("invariant violation (action aborted)", "pool", l.name, "violation", v.Error())
		}
	}
	launchesAllowed := true
	if d.LedgerChanged && l.f.o.Ledgers != nil {
		if err := l.f.o.Ledgers.SaveLedger(ctx, l.name, d.Ledger); err != nil {
			// Write-ahead failed: launching now could reuse a token after a
			// restart, so skip every launch of this decision (ErrSkipped).
			launchesAllowed = false
			l.f.o.Log.Warn("persisting the launch ledger failed; skipping launches", "pool", l.name, "err", err)
		}
	}
	if l.f.o.Shadow {
		// Shadow mode (R-TEST-7): decide without acting.
		l.results = make([]scaling.Result, 0, len(d.Actions))
		for _, a := range d.Actions {
			l.results = append(l.results, scaling.Result{Action: a, Err: scaling.ErrSkipped, At: now})
		}
		if len(d.Actions) > 0 {
			l.f.o.Log.Info("shadow decision (not executed)", "pool", l.name, "desired", d.Desired, "actions", summarize(d.Actions), "hold", string(d.Hold))
		}
		l.account(now, rt, obs, d)
		l.publish(now, rt, d, obs)
		return
	}
	l.pendingTart = l.pendingTart[:0]
	for tok, at := range l.attempted {
		if now.Sub(at) > 2*time.Hour {
			delete(l.attempted, tok)
		}
	}
	l.results = l.execute(ctx, rt, d, obs, launchesAllowed)
	l.account(now, rt, obs, d)
	if obs.ProviderKnown {
		l.f.usage.observe(rt, obs.Instances, now)
	}
	l.publish(now, rt, d, obs)
}

func (l *poolLoop) observe(ctx context.Context, rt *PoolRuntime, now time.Time) scaling.Observation {
	o := scaling.Observation{
		Now:                  now,
		ImageErr:             rt.ImageErr,
		Results:              l.results,
		InstanceSecondsToday: l.acct.seconds,
	}
	all, err := l.f.shared.getQueues(ctx)
	if err == nil {
		o.QueuesKnown = true
		declared := map[domain.QueueKey]bool{}
		for _, q := range all {
			declared[q.Key] = true
			if slices.Contains(rt.Queues, q.Key) {
				o.Queues = append(o.Queues, q)
			}
		}
		o.QueuedShare = l.f.queuedShare(l.name, rt, all)
		workersOK := true
		for _, q := range rt.Queues {
			if !declared[q] {
				continue
			}
			ws, err := l.f.o.BuildQueue.ListWorkers(ctx, q)
			switch {
			case errors.Is(err, ports.ErrQueueUnknown):
			case err != nil:
				workersOK = false
			default:
				for _, w := range ws {
					if w.ID[domain.LabelPool] == string(l.name) {
						o.Workers = append(o.Workers, w)
					}
				}
			}
			ds, err := l.f.o.BuildQueue.ListDrains(ctx, q)
			switch {
			case errors.Is(err, ports.ErrQueueUnknown):
			case err != nil:
				workersOK = false
			default:
				o.Drains = append(o.Drains, ds...)
			}
		}
		o.WorkersKnown = workersOK
	}

	switch rt.Spec.Provider {
	case domain.ProviderEC2:
		if ins, err := l.f.shared.getInstances(ctx); err == nil {
			o.ProviderKnown = true
			for _, in := range ins {
				if in.Pool == l.name {
					o.Instances = append(o.Instances, in)
				}
			}
		}
		if l.f.o.Bucket != nil {
			o.LaunchBudget = scaling.Budget{Limited: true, Launches: l.f.o.Bucket.Available(now)}
		}
	case domain.ProviderTart:
		if hosts, err := l.f.shared.getHosts(ctx); err == nil {
			o.ProviderKnown = true
			o.Hosts = hosts
			if rt.Tart != nil {
				o.TartFreeSlots = len(Place(placementRequest(rt, 2*len(hosts)), placementHosts(l.name, rt, hosts)))
			}
		}
	}
	return o
}

// execute runs the decision's actions in order and returns one Result per
// action (ErrSkipped for anything not run).
func (l *poolLoop) execute(ctx context.Context, rt *PoolRuntime, d scaling.Decision, obs scaling.Observation, launchesAllowed bool) []scaling.Result {
	results := make([]scaling.Result, 0, len(d.Actions))
	m := l.f.o.Metrics
	launched := 0
	for _, a := range d.Actions {
		res := scaling.Result{Action: a}
		switch {
		case ctx.Err() != nil:
			res.Err = scaling.ErrSkipped
		case a.Kind == scaling.ActLaunch:
			if !launchesAllowed {
				res.Err = scaling.ErrSkipped
				break
			}
			_, retry := l.attempted[a.Launch.Token]
			res = l.launch(ctx, rt, a, d, obs, launched)
			if !errors.Is(res.Err, scaling.ErrSkipped) {
				l.attempted[a.Launch.Token] = l.f.o.Clock.Now()
				if !retry {
					launched++
				}
			}
		case a.Kind == scaling.ActAddDrain:
			res.PerVM = l.drains(ctx, rt, a.VMs, true)
		case a.Kind == scaling.ActUndrain, a.Kind == scaling.ActRemoveDrain:
			res.PerVM = l.drains(ctx, rt, a.VMs, false)
		case a.Kind == scaling.ActTerminate:
			res.PerVM, res.Err = l.terminate(ctx, a.VMs)
		case a.Kind == scaling.ActStop:
			res.PerVM, res.Err = l.stopVMs(ctx, obs.Hosts, a)
		case a.Kind == scaling.ActFailQueues:
			res.Err = l.failQueues(ctx, a)
		default:
			res.Err = fmt.Errorf("%w: unknown action %q", scaling.ErrSkipped, a.Kind)
		}
		res.At = l.f.o.Clock.Now()
		results = append(results, res)

		if res.Err != nil && !errors.Is(res.Err, scaling.ErrSkipped) {
			l.last.err = fmt.Sprintf("%s: %v", a.Kind, res.Err)
		}
		if res.Err == nil && m != nil {
			m.ScaleDecisions.WithLabelValues(string(l.name), string(a.Kind)).Inc()
		}
		if res.Err == nil {
			l.afterAction(rt, a, res)
		}
	}
	return results
}

func (l *poolLoop) afterAction(rt *PoolRuntime, a scaling.Action, res scaling.Result) {
	m, ev := l.f.o.Metrics, l.f.o.Events
	now := res.At
	switch a.Kind {
	case scaling.ActLaunch:
		l.last.scale = now
		id := res.VM
		detail := ""
		if res.Instance != nil {
			id = res.Instance.ID
			detail = fmt.Sprintf(" (%s in %s)", res.Instance.Type, res.Instance.SubnetID)
		}
		if id != "" {
			l.starts[id] = &startTrack{launched: now}
			l.f.hist.event(HistoryEvent{Time: now, Pool: l.name, Type: "launch", Subject: id, Message: string(a.Reason) + detail})
			l.f.hist.start(StartRecord{Pool: l.name, VM: id, Launched: now, Path: string(rt.Spec.Provider)})
		}
		if ev != nil {
			ev.PoolEvent(l.name, false, "Launched", fmt.Sprintf("launched %s%s for %s, generation %s", id, detail, a.Reason, a.Launch.Generation))
		}
	case scaling.ActTerminate, scaling.ActStop:
		l.last.scale = now
		for _, vm := range a.VMs {
			if res.PerVM[vm] != nil {
				continue
			}
			if m != nil {
				m.VMStops.WithLabelValues(string(l.name), string(a.Reason)).Inc()
			}
			l.f.hist.event(HistoryEvent{Time: now, Pool: l.name, Type: string(a.Kind), Subject: vm, Message: string(a.Reason)})
			delete(l.starts, vm)
			if ev != nil {
				verb := "terminated"
				if a.Kind == scaling.ActStop {
					verb = "stopped"
				}
				ev.PoolEvent(l.name, false, "VMStopped", fmt.Sprintf("%s %s (%s)", verb, vm, a.Reason))
			}
		}
	case scaling.ActFailQueues:
		l.f.hist.event(HistoryEvent{Time: now, Pool: l.name, Type: "fail", Message: a.Message})
		if ev != nil {
			ev.PoolEvent(l.name, true, "QueuedWorkFailed", a.Message)
		}
	case scaling.ActAddDrain:
		for _, vm := range a.VMs {
			l.f.hist.event(HistoryEvent{Time: now, Pool: l.name, Type: "drain", Subject: vm, Message: string(a.Reason)})
		}
		if ev != nil && len(a.VMs) > 0 {
			ev.PoolEvent(l.name, false, "Draining", fmt.Sprintf("draining %s (%s)", strings.Join(a.VMs, ", "), a.Reason))
		}
	}
}

// launch starts one VM for an ActLaunch.
func (l *poolLoop) launch(ctx context.Context, rt *PoolRuntime, a scaling.Action, d scaling.Decision, obs scaling.Observation, launchedThisStep int) scaling.Result {
	res := scaling.Result{Action: a}
	in := a.Launch
	if in == nil || in.Token == "" {
		res.Err = fmt.Errorf("%w: launch without a token", scaling.ErrSkipped)
		return res
	}
	// R-TEST-7: re-check InstancesNeverExceedMax right before the call. A retry
	// (token already sent) is part of the decision's counted capacity.
	if _, retry := l.attempted[in.Token]; !retry {
		if vs := invariants.LaunchWithinMax(string(l.name), rt.Spec.Max, d.Demand.Capacity, 0, launchedThisStep+1); vs != nil {
			invariants.Report(vs...)
			l.f.o.Log.Error("invariant violation (launch aborted)", "pool", l.name, "violation", vs[0].Error())
			res.Err = fmt.Errorf("%w: %s", scaling.ErrSkipped, vs[0].Error())
			return res
		}
	}
	switch rt.Spec.Provider {
	case domain.ProviderEC2:
		return l.launchEC2(ctx, rt, a, res)
	case domain.ProviderTart:
		return l.launchTart(ctx, rt, a, obs, res)
	}
	res.Err = fmt.Errorf("%w: provider %q", scaling.ErrSkipped, rt.Spec.Provider)
	return res
}

func (l *poolLoop) launchEC2(ctx context.Context, rt *PoolRuntime, a scaling.Action, res scaling.Result) scaling.Result {
	o := l.f.o
	in := a.Launch
	if o.Compute == nil || rt.EC2 == nil {
		res.Err = fmt.Errorf("%w: no EC2 provider", scaling.ErrSkipped)
		return res
	}
	if in.Generation != rt.Spec.Generation {
		// The image changed after the decision; the tag would lie about the AMI.
		res.Err = fmt.Errorf("%w: generation %s superseded by %s", scaling.ErrSkipped, in.Generation, rt.Spec.Generation)
		return res
	}
	if o.Bucket != nil && !o.Bucket.Take(o.Clock.Now()) {
		res.Err = fmt.Errorf("%w: RunInstances budget exhausted", scaling.ErrSkipped)
		return res
	}
	var userData []byte
	if o.UserData != nil {
		b, err := o.UserData(l.name, in.Generation)
		if err != nil {
			res.Err = fmt.Errorf("%w: rendering boot data: %w", scaling.ErrSkipped, err)
			return res
		}
		userData = b
	}
	tags := maps.Clone(rt.EC2.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	for k, v := range map[string]string{
		domain.TagManagedBy:    domain.ManagedByValue,
		domain.TagCluster:      o.ClusterID,
		domain.TagPool:         string(l.name),
		domain.TagGeneration:   in.Generation,
		domain.TagImageVersion: rt.ImageVersion,
		domain.TagLaunchToken:  in.Token,
		domain.TagRole:         "worker",
	} {
		tags[k] = v
	}
	req := ports.LaunchRequest{
		Pool:              l.name,
		Generation:        in.Generation,
		Token:             in.Token,
		ImageID:           rt.EC2.ImageID,
		InstanceTypes:     in.InstanceTypes,
		SubnetIDs:         in.SubnetIDs,
		CapacityType:      rt.EC2.CapacityType,
		FallbackOnDemand:  rt.EC2.FallbackOnDemand,
		SecurityGroupIDs:  rt.EC2.SecurityGroupIDs,
		InstanceProfile:   rt.EC2.InstanceProfile,
		AssociatePublicIP: rt.EC2.AssociatePublicIP,
		RootVolume:        rt.EC2.RootVolume,
		ExtraVolumes:      rt.EC2.ExtraVolumes,
		UserData:          userData,
		Tags:              tags,
	}
	inst, err := o.Compute.Launch(ctx, req)
	if err != nil {
		res.Err = err
		l.f.o.Log.Info("launch failed", "pool", l.name, "code", errorCode(err), "err", err)
		if o.Metrics != nil {
			kind := ""
			switch {
			case errors.Is(err, ports.ErrInsufficientCapacity):
				kind = metrics.CapacityICE
			case errors.Is(err, ports.ErrQuotaExceeded):
				kind = metrics.CapacityQuota
			}
			if kind != "" && len(in.InstanceTypes) > 0 {
				o.Metrics.EC2CapacityErrors.WithLabelValues(string(l.name), in.InstanceTypes[0], kind).Inc()
			}
		}
		return res
	}
	res.Instance = &inst
	return res
}

func (l *poolLoop) launchTart(ctx context.Context, rt *PoolRuntime, a scaling.Action, obs scaling.Observation, res scaling.Result) scaling.Result {
	o := l.f.o
	if o.HostFleet == nil || rt.Tart == nil {
		res.Err = fmt.Errorf("%w: no host stream", scaling.ErrSkipped)
		return res
	}
	hosts := obs.Hosts
	// Account for VMs started earlier in this step (placement is per decision).
	ps := Place(placementRequest(rt, 1), l.placementHostsWithPending(rt, hosts))
	if len(ps) == 0 {
		res.Err = fmt.Errorf("%w: no eligible host slot", scaling.ErrSkipped)
		return res
	}
	p := ps[0]
	vm := p.VM
	if vm == "" {
		vm = l.newVMName(p.Serial, hosts)
	}
	node := TartNode(p.Serial, vm)
	// A restarted VM may still carry its {pool,node} drain from the last idle
	// shutdown (drains persist across worker disappearance, R-SCALE-3 step 4).
	l.drains(ctx, rt, []string{node}, false)
	err := o.HostFleet.StartVM(ctx, p.Serial, ports.StartVMRequest{
		Pool:       l.name,
		Generation: a.Launch.Generation,
		Image:      rt.Tart.Image,
		VMName:     vm,
		CPU:        rt.Tart.CPU,
		MemoryGiB:  rt.Tart.MemoryGiB,
		DiskGiB:    rt.Tart.DiskGiB,
		MaxAge:     rt.Tart.MaxAge,
	})
	if err != nil {
		res.Err = err
		return res
	}
	res.VM = node
	l.pendingTart = append(l.pendingTart, node)
	return res
}

// drains adds or removes the {pool, node} drain on every queue of the pool.
func (l *poolLoop) drains(ctx context.Context, rt *PoolRuntime, nodes []string, add bool) map[string]error {
	per := map[string]error{}
	for _, node := range nodes {
		pattern := ports.WorkerID{domain.LabelPool: string(l.name), domain.LabelNode: node}
		for _, q := range rt.Queues {
			var err error
			if add {
				err = l.f.o.BuildQueue.AddDrain(ctx, q, pattern)
			} else {
				err = l.f.o.BuildQueue.RemoveDrain(ctx, q, pattern)
			}
			if err != nil && !errors.Is(err, ports.ErrQueueUnknown) && !errors.Is(err, ports.ErrNotFound) {
				per[node] = err
			}
		}
	}
	return per
}

func (l *poolLoop) terminate(ctx context.Context, ids []string) (map[string]error, error) {
	if l.f.o.Compute == nil {
		return nil, fmt.Errorf("%w: no EC2 provider", scaling.ErrSkipped)
	}
	return l.f.o.Compute.Terminate(ctx, l.f.o.ClusterID, ids)
}

func (l *poolLoop) stopVMs(ctx context.Context, hosts []ports.HostState, a scaling.Action) (map[string]error, error) {
	if l.f.o.HostFleet == nil {
		return nil, fmt.Errorf("%w: no host stream", scaling.ErrSkipped)
	}
	per := map[string]error{}
	for _, node := range a.VMs {
		serial, vm, ok := SplitTartNode(node, hosts)
		if !ok {
			per[node] = fmt.Errorf("unknown host for VM %s", node)
			continue
		}
		if err := l.f.o.HostFleet.StopVM(ctx, serial, vm, l.f.o.TartStopTimeout, string(a.Reason)); err != nil {
			per[node] = err
		}
	}
	return per, nil
}

func (l *poolLoop) failQueues(ctx context.Context, a scaling.Action) error {
	var errs []error
	for _, q := range a.Queues {
		if l.f.eligibleElsewhere(l.name, q) {
			continue // another pool will serve this shared queue
		}
		q := q
		if err := l.f.o.BuildQueue.KillOperations(ctx, ports.KillFilter{QueueWithoutWorkers: &q}, a.Code, a.Message); err != nil && !errors.Is(err, ports.ErrQueueUnknown) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// account updates instance-seconds (EC2) and the VM start-latency histograms.
func (l *poolLoop) account(now time.Time, rt *PoolRuntime, obs scaling.Observation, d scaling.Decision) {
	m := l.f.o.Metrics
	utc := now.UTC()
	if utc.YearDay() != l.acct.day {
		l.acct.day, l.acct.seconds = utc.YearDay(), 0
	}
	dt := now.Sub(l.acct.at).Seconds()
	l.acct.at = now
	if dt > 0 && dt <= 3*l.f.o.PollInterval.Seconds()+1 && obs.ProviderKnown {
		for _, in := range obs.Instances {
			if in.State != ports.InstancePending && in.State != ports.InstanceRunning {
				continue
			}
			l.acct.seconds += dt
			if m != nil {
				m.InstanceSeconds.WithLabelValues(string(l.name), in.Type).Add(dt)
			}
		}
	}
	if m == nil {
		return
	}
	running := map[string]bool{}
	for _, in := range obs.Instances {
		if in.State == ports.InstanceRunning {
			running[in.ID] = true
		}
	}
	for _, vm := range d.VMs {
		t, ok := l.starts[vm.ID]
		if !ok {
			continue
		}
		rec := StartRecord{Pool: l.name, VM: vm.ID, Launched: t.launched, Path: string(rt.Spec.Provider), ToRunning: t.toRunning, ToRegistered: t.toRegistered}
		if !t.running && (running[vm.ID] || (rt.Spec.Provider == domain.ProviderTart && vm.State != domain.VMLaunching) || !vm.RegisteredAt.IsZero()) {
			t.running = true
			t.toRunning = now.Sub(t.launched)
			rec.ToRunning = t.toRunning
			l.f.hist.start(rec)
			m.VMStartSeconds.WithLabelValues(string(l.name), metrics.PhaseToRunning).Observe(t.toRunning.Seconds())
		}
		if !t.registered && !vm.RegisteredAt.IsZero() {
			t.registered = true
			t.toRegistered = max(0, vm.RegisteredAt.Sub(t.launched))
			rec.ToRegistered = t.toRegistered
			l.f.hist.start(rec)
			l.f.hist.event(HistoryEvent{Time: vm.RegisteredAt, Pool: l.name, Type: "register", Subject: vm.ID})
			m.VMStartSeconds.WithLabelValues(string(l.name), metrics.PhaseToRegistered).Observe(t.toRegistered.Seconds())
			if l.f.o.Events != nil {
				l.f.o.Events.PoolEvent(l.name, false, "Registered", fmt.Sprintf("%s registered with the scheduler after %s", vm.ID, vm.RegisteredAt.Sub(t.launched).Round(time.Second)))
			}
		}
		if !t.firstAction && vm.Busy > 0 {
			t.firstAction = true
			rec.ToFirstAction = now.Sub(t.launched)
			l.f.hist.start(rec)
			m.VMStartSeconds.WithLabelValues(string(l.name), metrics.PhaseToFirstAction).Observe(rec.ToFirstAction.Seconds())
			delete(l.starts, vm.ID)
		}
	}
	// Forget launches that never showed up (failed, vanished).
	for id, t := range l.starts {
		if now.Sub(t.launched) > rt.Spec.StartupTimeout+10*time.Minute {
			delete(l.starts, id)
		}
	}
}

// publish exports metrics and the snapshot, and notifies the reconciler when
// the status-relevant part changed.
func (l *poolLoop) publish(now time.Time, rt *PoolRuntime, d scaling.Decision, obs scaling.Observation) {
	counts := map[string]int{}
	for _, vm := range d.VMs {
		switch vm.State {
		case domain.VMLaunching:
			counts[metrics.StateLaunching]++
		case domain.VMRegistered:
			counts[metrics.StateRegistered]++
			if vm.Busy > 0 {
				counts[metrics.StateBusy]++
			} else {
				counts[metrics.StateIdle]++
			}
		case domain.VMDraining:
			counts[metrics.StateDraining]++
		case domain.VMStopped:
			counts[metrics.StateStopped]++
		case domain.VMFailed:
			counts[metrics.StateFailed]++
		}
	}
	idleLeaks := l.idleCostLeaks(now, rt, d, obs)
	if m := l.f.o.Metrics; m != nil {
		p := string(l.name)
		m.PoolDesired.WithLabelValues(p).Set(float64(d.Desired))
		m.PoolMax.WithLabelValues(p).Set(float64(rt.Spec.Max))
		m.SetPoolVMs(p, counts)
		m.IdleInstancesWithEmptyQueue.WithLabelValues(p).Set(float64(idleLeaks))
	}
	snap := Snapshot{
		At:                   now,
		Desired:              d.Desired,
		Demand:               d.Demand,
		Hold:                 d.Hold,
		Counts:               counts,
		VMs:                  d.VMs,
		Status:               d.Status,
		Headroom:             d.ThreadHeadroom,
		Deleting:             rt.Spec.Deleting,
		Observed:             obs.ProviderKnown,
		QueuesKnown:          obs.QueuesKnown,
		LastScale:            l.last.scale,
		LastError:            l.last.err,
		IdleEmptySince:       l.idleEmptySince,
		InstanceSecondsToday: l.acct.seconds,
		Generation:           rt.Spec.Generation,
	}
	l.mu.Lock()
	prev := l.snap
	snap.Observed = snap.Observed || prev.Observed
	l.snap = snap
	l.mu.Unlock()
	if l.f.o.Notify != nil && statusChanged(prev, snap) {
		l.f.o.Notify(l.name)
	}
}

// idleDrainGrace allows idle drain/termination calls and provider observations
// to converge. It is not drainTimeout: busy workers never count as idle leaks.
const idleDrainGrace = 2 * time.Minute

func (l *poolLoop) idleCostLeaks(now time.Time, rt *PoolRuntime, d scaling.Decision, obs scaling.Observation) int {
	previous := l.idleEmpty
	l.idleEmpty = nil
	l.idleEmptySince = time.Time{}
	if !queuesEmpty(obs) || !obs.WorkersKnown || !obs.ProviderKnown {
		return 0 // Unknown observations are not evidence of continuous idleness.
	}

	// A requested stop is not a completed stop. Keep counting a stuck drain or
	// terminate while the provider still reports the worker running, but never
	// count stale scheduler registrations after it has shut down.
	running := map[string]bool{}
	for _, in := range obs.Instances {
		if in.State == ports.InstanceRunning {
			running[in.ID] = true
		}
	}
	for _, host := range obs.Hosts {
		if !host.Online {
			continue
		}
		for _, vm := range host.VMs {
			switch vm.State {
			case domain.VMLaunching, domain.VMRegistered, domain.VMDraining:
				running[vm.ID] = true
			}
		}
	}

	capacity := 0
	var idle []domain.VM
	l.idleEmpty = map[string]time.Time{}
	for _, vm := range d.VMs {
		if vm.State == domain.VMLaunching || vm.State == domain.VMRegistered {
			capacity++
		}
		switch vm.State {
		case domain.VMRegistered, domain.VMDraining, domain.VMStopping:
		default:
			continue
		}
		if !running[vm.ID] || vm.Threads == 0 || vm.Busy > 0 {
			continue
		}
		idle = append(idle, vm)
	}

	// The floor protects active capacity, not a stuck retiring worker. Busy and
	// launching VMs already contribute to it; exempt the youngest remaining
	// registered idle VMs, leaving the oldest eligible for scale-in.
	excess := max(0, capacity-d.Demand.Floor)
	slices.SortStableFunc(idle, func(a, b domain.VM) int {
		return a.IdleSince.Compare(b.IdleSince)
	})
	leaks := 0
	for _, vm := range idle {
		if vm.State == domain.VMRegistered {
			if excess == 0 {
				continue
			}
			excess--
		}
		// Do not carry protected floor time into a later normal scale-in.
		since := previous[vm.ID]
		if since.IsZero() {
			since = now
		}
		l.idleEmpty[vm.ID] = since
		if vm.State == domain.VMRegistered && (l.idleEmptySince.IsZero() || since.Before(l.idleEmptySince)) {
			l.idleEmptySince = since
		}
		if now.Sub(since) > rt.Spec.IdleTimeout+idleDrainGrace {
			leaks++
		}
	}
	return leaks
}

func summarize(as []scaling.Action) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		n := len(a.VMs)
		switch a.Kind {
		case scaling.ActLaunch:
			n = 1
		case scaling.ActFailQueues:
			n = len(a.Queues)
		}
		parts = append(parts, fmt.Sprintf("%s/%s×%d", a.Kind, a.Reason, n))
	}
	return strings.Join(parts, ",")
}

func queuesEmpty(obs scaling.Observation) bool {
	if !obs.QueuesKnown {
		return false
	}
	for _, q := range obs.Queues {
		if q.Queued > 0 {
			return false
		}
	}
	return true
}

// statusChanged compares the parts of two snapshots that the WorkerPool status shows.
func statusChanged(a, b Snapshot) bool {
	return a.Desired != b.Desired || !maps.Equal(a.Counts, b.Counts) || a.Status != b.Status || a.QueuesKnown != b.QueuesKnown ||
		a.Deleting != b.Deleting || a.Observed != b.Observed || !a.LastScale.Equal(b.LastScale) ||
		a.Generation != b.Generation || a.LastError != b.LastError
}

func itoa(v uint32) string { return strconv.FormatUint(uint64(v), 10) }
