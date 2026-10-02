// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// world is a minimal ground-truth model of one EC2 pool for the property
// tests: VMs with execution slots, a FIFO queue, launches deduplicated by
// token, drains, terminations and injectable faults. It plays scheduler,
// cloud and executor around the planner, exactly as the reconciler does.

var errAmbiguous = errors.New("connection reset")

type work struct{ left time.Duration }

type tvm struct {
	id, token, gen          string
	launchedAt, bootAt      time.Time
	terminated, drained     bool
	drainedAt, terminatedAt time.Time
	lost                    bool // worker gone, VM up (worker death)
	slots                   []*work
	lastBusy                time.Time // last observation instant with a busy slot (or registration)
	seenRegistered          bool
}

type world struct {
	now     time.Time
	spec    scaling.Spec
	key     domain.QueueKey
	planner *scaling.Planner
	st      *scaling.PoolState
	ledger  scaling.Ledger
	results []scaling.Result

	vms        []*tvm
	byToken    map[string]*tvm
	queue      []*work
	lastQueued time.Time // last observation instant with queued work
	boot       time.Duration
	visibleLag time.Duration // EC2 Describe lists a new instance only this long after launch
	maxLowered time.Time     // when max was last lowered

	// faults
	ice             bool
	throttle, ambig int // next launch calls fail with throttling / an ambiguous error
	blind           int // next observations without scheduler data
	stopFails       int // next terminate calls fail as a whole (throttled)

	// ground-truth bookkeeping
	failed, launches int
	violations       []string
	last             scaling.Decision
	lastObs          scaling.Observation
}

func newWorld(seed uint64, maxVMs, slots int, idle time.Duration) *world {
	props := map[string]string{"OSFamily": "linux", "ISA": "x86-64"}
	spec := scaling.Spec{
		PoolSpec: domain.PoolSpec{Name: "p", Provider: domain.ProviderEC2, SizeClass: 1, InstanceNames: []string{"main"},
			Runners: []domain.Runner{{Name: "native", Properties: props, Concurrency: slots}}, Max: maxVMs,
			IdleTimeout: idle, DrainTimeout: 30 * time.Minute, StartupTimeout: 5 * time.Minute, Generation: "g1",
			Rollout: domain.RolloutLazy},
		ClusterID: "c", InstanceTypes: []string{"c8i.2xlarge", "c7i.2xlarge"}, SubnetIDs: []string{"subnet-a"},
	}
	w := &world{now: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC), spec: spec,
		key:     domain.QueueKey{InstanceNamePrefix: "main", PlatformKey: domain.PropertiesKey(props), SizeClass: 1},
		planner: scaling.NewPlanner(scaling.Config{}, fakes.NewRand(seed)), byToken: map[string]*tvm{}, boot: 40 * time.Second}
	w.st = scaling.NewPoolState("p", scaling.Ledger{})
	return w
}

func (w *world) violate(format string, args ...any) {
	w.violations = append(w.violations, fmt.Sprintf("t=%s ", w.now.Format("15:04:05"))+fmt.Sprintf(format, args...))
}

func (w *world) registered(v *tvm) bool {
	return !v.terminated && !v.lost && !w.now.Before(v.bootAt)
}

func busySlots(v *tvm) int {
	n := 0
	for _, s := range v.slots {
		if s != nil {
			n++
		}
	}
	return n
}

// advance moves time by dt: work progresses, finished slots take queued work.
func (w *world) advance(dt time.Duration) {
	w.now = w.now.Add(dt)
	for _, v := range w.vms {
		for i, s := range v.slots {
			if s == nil {
				continue
			}
			s.left -= dt
			if s.left <= 0 {
				v.slots[i] = nil
			}
		}
	}
	w.dispatch()
}

func (w *world) dispatch() {
	for _, v := range w.vms {
		if !w.registered(v) || v.drained {
			continue
		}
		for i := range v.slots {
			if v.slots[i] == nil && len(w.queue) > 0 {
				v.slots[i], w.queue = w.queue[0], w.queue[1:]
			}
		}
	}
}

func (w *world) arrive(n int, d time.Duration) {
	for i := 0; i < n; i++ {
		w.queue = append(w.queue, &work{left: d})
	}
	w.dispatch()
}

func (w *world) observe() scaling.Observation {
	obs := scaling.Observation{Now: w.now, ProviderKnown: true, Results: w.results}
	w.results = nil
	// Ground truth at this poll instant (whether or not the planner can see it).
	if len(w.queue) > 0 {
		w.lastQueued = w.now
	}
	for _, v := range w.vms {
		if w.registered(v) && (busySlots(v) > 0 || !v.seenRegistered) {
			v.lastBusy, v.seenRegistered = w.now, true
		}
	}
	for _, v := range w.vms {
		if w.now.Sub(v.launchedAt) < w.visibleLag {
			continue // eventual consistency: not listed yet
		}
		st := ports.InstanceRunning
		if v.terminated {
			st = ports.InstanceShuttingDown
			if w.now.Sub(v.terminatedAt) > 20*time.Second {
				st = ports.InstanceTerminated
			}
		}
		obs.Instances = append(obs.Instances, ports.Instance{ID: v.id, Pool: "p", Generation: v.gen, State: st, Type: "c8i.2xlarge",
			LaunchTime: v.launchedAt, Tags: map[string]string{domain.TagLaunchToken: v.token, domain.TagCluster: "c"}})
	}
	if w.blind > 0 {
		w.blind--
		return obs
	}
	obs.QueuesKnown, obs.WorkersKnown = true, true
	q := domain.QueueObservation{Key: w.key, Queued: len(w.queue)}
	for _, v := range w.vms {
		if v.drained && (!v.terminated || w.now.Sub(v.terminatedAt) <= 20*time.Second) {
			obs.Drains = append(obs.Drains, ports.Drain{Queue: w.key, Pattern: ports.WorkerID{"pool": "p", "node": v.id}, Created: v.drainedAt})
		}
		if !w.registered(v) {
			continue
		}
		for i, s := range v.slots {
			obs.Workers = append(obs.Workers, ports.Worker{ID: ports.WorkerID{"pool": "p", "node": v.id, "thread": fmt.Sprint(i)},
				Queue: w.key, Executing: s != nil, Drained: v.drained})
			q.Workers++
			if s != nil {
				q.Executing++
			} else {
				q.Idle++
			}
		}
	}
	obs.Queues = []domain.QueueObservation{q}
	return obs
}

// step runs one controller poll: observe, plan, persist, execute, check.
func (w *world) step() scaling.Decision {
	obs := w.observe()
	w.lastObs = obs
	queuedNow := len(w.queue)
	d := w.planner.Plan(w.spec, obs, w.st)
	w.last = d
	for _, v := range d.Violations {
		w.violate("planner guard: %s", v.Error())
	}
	if d.LedgerChanged {
		w.ledger = d.Ledger
	}
	// Liveness of scale-out (R-TEST-6): desired ≥ 1 whenever work is queued and max > 0.
	if obs.QueuesKnown && queuedNow > 0 && w.spec.Max > 0 && !w.spec.Paused && d.Desired < 1 {
		w.violate("queue %d > 0 and max %d > 0 but desired %d", queuedNow, w.spec.Max, d.Desired)
	}
	for _, a := range d.Actions {
		w.results = append(w.results, w.execute(a))
	}
	// InstancesNeverExceedMax: active (not draining, not terminated) VMs never
	// exceed max. The only exemption: a VM launched before max was lowered that
	// the planner cannot see yet (not listed) — it must be retired once listed.
	active := 0
	for _, v := range w.vms {
		if v.terminated || v.drained {
			continue
		}
		if v.launchedAt.Before(w.maxLowered) && w.now.Sub(v.launchedAt) < w.visibleLag {
			continue
		}
		active++
	}
	if active > w.spec.Max {
		w.violate("%d active VMs exceed max %d", active, w.spec.Max)
	}
	return d
}

func (w *world) vm(id string) *tvm {
	for _, v := range w.vms {
		if v.id == id {
			return v
		}
	}
	return nil
}

func (w *world) execute(a scaling.Action) scaling.Result {
	res := scaling.Result{Action: a, At: w.now}
	switch a.Kind {
	case scaling.ActLaunch:
		// Like EC2 (and fakes.Compute): throttling first, then a known client
		// token returns its instance, and only new launches need capacity.
		if w.throttle > 0 {
			w.throttle--
			res.Err = fmt.Errorf("RequestLimitExceeded: %w", ports.ErrThrottled)
			return res
		}
		v, ok := w.byToken[a.Launch.Token]
		if !ok && w.ice {
			res.Err = fmt.Errorf("ICE: %w", ports.ErrInsufficientCapacity)
			return res
		}
		if !ok {
			v = &tvm{id: fmt.Sprintf("i-%04d", len(w.vms)), token: a.Launch.Token, gen: a.Launch.Generation, launchedAt: w.now,
				bootAt: w.now.Add(w.boot), slots: make([]*work, w.spec.Runners[0].Concurrency)}
			w.vms = append(w.vms, v)
			w.byToken[a.Launch.Token] = v
			w.launches++
		}
		if w.ambig > 0 {
			w.ambig--
			res.Err = errAmbiguous // the instance exists, the caller does not know
			return res
		}
		in := ports.Instance{ID: v.id, Pool: "p", Generation: v.gen, State: ports.InstancePending, Type: "c8i.2xlarge", LaunchTime: v.launchedAt,
			Tags: map[string]string{domain.TagLaunchToken: v.token}}
		if v.terminated {
			in.State = ports.InstanceTerminated
		}
		res.Instance = &in
	case scaling.ActAddDrain:
		for _, id := range a.VMs {
			v := w.vm(id)
			if v == nil {
				continue
			}
			if a.Reason == scaling.StopIdle {
				// ScaleInOnlyAfterIdleTimeout against ground truth at poll instants.
				if w.now.Sub(v.lastBusy) < w.spec.IdleTimeout || w.now.Sub(w.lastQueued) < w.spec.IdleTimeout {
					w.violate("idle drain of %s: last busy %s ago, queue non-empty %s ago", id, w.now.Sub(v.lastBusy), w.now.Sub(w.lastQueued))
				}
			}
			if !v.drained {
				v.drained, v.drainedAt = true, w.now
			}
		}
	case scaling.ActUndrain, scaling.ActRemoveDrain:
		for _, id := range a.VMs {
			if v := w.vm(id); v != nil {
				v.drained = false
			}
		}
		w.dispatch()
	case scaling.ActTerminate:
		if w.stopFails > 0 {
			w.stopFails--
			res.Err = fmt.Errorf("RequestLimitExceeded: %w", ports.ErrThrottled)
			return res
		}
		for _, id := range a.VMs {
			v := w.vm(id)
			if v == nil || v.terminated {
				continue
			}
			// NeverTerminateBusyOrLeased against ground truth.
			if busySlots(v) > 0 && (!v.drained || w.now.Sub(v.drainedAt) < w.spec.DrainTimeout) {
				w.violate("terminated %s with %d busy slots (drained %t)", id, busySlots(v), v.drained)
			}
			for i, s := range v.slots {
				if s != nil {
					w.queue = append([]*work{s}, w.queue...) // the scheduler requeues cut-off work
					v.slots[i] = nil
				}
			}
			v.terminated, v.terminatedAt = true, w.now
		}
	case scaling.ActFailQueues:
		if len(w.vmsServing()) == 0 {
			w.failed += len(w.queue)
			w.queue = nil
		}
	}
	return res
}

func (w *world) vmsServing() []*tvm {
	var out []*tvm
	for _, v := range w.vms {
		if w.registered(v) {
			out = append(out, v)
		}
	}
	return out
}

// setMax changes the pool's max live (UC11).
func (w *world) setMax(n int) {
	if n < w.spec.Max {
		w.maxLowered = w.now
	}
	w.spec.Max = n
}

// restart simulates a controller restart: in-memory state and the results of
// the last decision are lost; the persisted ledger survives.
func (w *world) restart(seed uint64) {
	w.st = scaling.NewPoolState("p", w.ledger)
	w.results = nil
	w.planner = scaling.NewPlanner(scaling.Config{}, fakes.NewRand(seed))
}

// busy reports whether any work is queued or executing.
func (w *world) busy() bool {
	if len(w.queue) > 0 {
		return true
	}
	for _, v := range w.vms {
		if busySlots(v) > 0 {
			return true
		}
	}
	return false
}

// settle polls every dt until all work is done (at most limit), then for
// idleTimeout plus a few minutes of drain/terminate polls.
func (w *world) settle(dt, limit time.Duration) {
	for spent := time.Duration(0); w.busy() && spent < limit; spent += dt {
		w.step()
		w.advance(dt)
	}
	for spent := time.Duration(0); spent < w.spec.IdleTimeout+3*time.Minute; spent += dt {
		w.step()
		w.advance(dt)
	}
}

func (w *world) live() int {
	n := 0
	for _, v := range w.vms {
		if !v.terminated {
			n++
		}
	}
	return n
}
