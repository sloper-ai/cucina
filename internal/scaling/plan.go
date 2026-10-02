// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/invariants"
)

// stopRetry is how long a terminate/stop may stay unconfirmed before it is re-issued.
const stopRetry = 2 * time.Minute

// Plan computes the next decision for one pool. It is deterministic in
// (Config, Rand state, spec, obs, st) and mutates only st.
func (p *Planner) Plan(spec Spec, obs Observation, st *PoolState) Decision {
	now := obs.Now
	ec2 := spec.Provider != domain.ProviderTart
	prefix := TokenPrefix(spec.ClusterID, spec.Name)
	st.ensureEpoch(p.rnd, now)

	p.applyResults(spec, obs, st)
	if obs.ProviderKnown && !st.providerSeen {
		st.providerSeen = true
		if ec2 {
			st.restoreFromObservation(prefix, obs.Instances, now, p.cfg.ConsistencyGrace)
		}
	}
	st.expireGhosts(now)
	st.expireCooldowns(now)
	st.expireIntents(now, p.cfg.ConsistencyGrace)

	facts := gather(spec, obs)
	p.reconcileRecords(spec, obs, st, facts, prefix)
	qv := analyzeQueues(spec, obs)
	switch {
	case !obs.QueuesKnown:
		// Blind: the queues may have held work. Restart the timer (a gap in
		// observation only ever delays scale-in, never speeds it up).
		st.queueEmptySince = time.Time{}
	case qv.rawQueued > 0:
		st.queueEmptySince = time.Time{}
	case st.queueEmptySince.IsZero():
		st.queueEmptySince = now
	}

	pl := &planning{p: p, spec: spec, obs: obs, st: st, qv: qv, facts: facts, prefix: prefix, ec2: ec2}
	pl.run()
	st.commit()

	d := pl.d
	d.Pool, d.Now = spec.Name, now
	d.Ledger = st.ledger
	d.LedgerChanged = ec2 && st.ledgerChanged()
	for _, id := range st.sortedIDs() {
		if r := st.vms[id]; r.state != domain.VMTerminated {
			d.VMs = append(d.VMs, r.view(spec.Name))
		}
	}
	return d
}

// ------------------------------------------------------------ results

func resultErr(res Result, id string) error {
	if res.Err != nil {
		return res.Err
	}
	return res.PerVM[id]
}

func (p *Planner) applyResults(spec Spec, obs Observation, st *PoolState) {
	now := obs.Now
	for _, res := range obs.Results {
		a := res.Action
		switch a.Kind {
		case ActLaunch:
			p.applyLaunch(spec, res, st, now)
		case ActAddDrain:
			for _, id := range a.VMs {
				if r := st.vms[id]; r != nil {
					r.drainSent = false
					r.drainAcked = resultErr(res, id) == nil
				}
			}
		case ActUndrain, ActRemoveDrain:
			for _, id := range a.VMs {
				if r := st.vms[id]; r != nil {
					r.removeDrainSent = false
				}
				delete(st.drainCleanup, id)
			}
		case ActTerminate, ActStop:
			for _, id := range a.VMs {
				r := st.vms[id]
				if r == nil {
					continue
				}
				r.stopPending = false
				switch err := resultErr(res, id); {
				case err == nil:
					r.stopAcked = true
				case errors.Is(err, ports.ErrNotFound) || errors.Is(err, ports.ErrVMNotFound):
					r.stopAcked = true // already gone
				case errors.Is(err, ports.ErrNotOwned):
					// Never retry: the instance lacks our tags. Stop tracking it.
					r.toTerminated()
					st.violations = append(st.violations, invariants.Violation{
						Invariant: invariants.EveryResourceTagged, Pool: string(spec.Name), Subject: id,
						Detail: "terminate refused: instance not owned by this controller"})
				default:
					r.stopRetry = true // re-issue on the next decision
				}
			}
		}
	}
	st.commit()
}

func (p *Planner) applyLaunch(spec Spec, res Result, st *PoolState, now time.Time) {
	a := res.Action
	if a.Launch == nil {
		return
	}
	in := st.intents[a.Launch.Token]
	seq := a.Launch.Seq
	consume := func() { delete(st.intents, a.Launch.Token) }
	switch err := res.Err; {
	case err == nil:
		consume()
		id := res.VM
		if res.Instance != nil {
			id = res.Instance.ID
		}
		if id == "" {
			return
		}
		r := st.vms[id]
		if r == nil {
			r = &vmRecord{id: id, state: domain.VMLaunching, firstSeen: now}
			st.vms[id] = r
		}
		r.generation, r.token = a.Launch.Generation, a.Launch.Token
		r.launchedAt = res.At
		if r.launchedAt.IsZero() {
			r.launchedAt = now
		}
		if res.Instance != nil {
			if !res.Instance.LaunchTime.IsZero() {
				r.launchedAt = res.Instance.LaunchTime
			}
			r.instanceType = res.Instance.Type
			if s := ec2Status(res.Instance.State); s == provGone || s == provStopping {
				// The token was used before and its instance is gone (stale token):
				// the seq is consumed, nothing launched.
				r.toTerminated()
				return
			}
			until := now.Add(p.cfg.CapacityCooldown)
			coolBefore(st.typeCooldown, a.Launch.InstanceTypes, res.Instance.Type, until)
			coolBefore(st.subnetCooldown, a.Launch.SubnetIDs, res.Instance.SubnetID, until)
			if !r.listed {
				st.unseen[seq] = now.Add(p.cfg.ConsistencyGrace)
			}
		}
		if res.VM != "" {
			r.host = tartHostOf(res.VM)
			r.staleBefore = r.launchedAt
		}
		if r.state == domain.VMStopped || r.state == domain.VMTerminated {
			r.resetRun()
			r.state = domain.VMLaunching
		}
		st.capacity.failures, st.capacity.throttle, st.capacity.until = 0, 0, time.Time{}
	case errors.Is(err, ErrSkipped):
		consume()
		st.release(seq)
	case errors.Is(err, ports.ErrThrottled):
		// Not processed by EC2: the token is unused and can be reused.
		consume()
		st.release(seq)
		st.capacity.throttle++
		st.capacity.until = now.Add(Backoff(st.capacity.throttle, p.cfg.ThrottleBackoffMin, p.cfg.ThrottleBackoffMax, p.rnd))
		st.capacity.hold = ReasonThrottled
	case errors.Is(err, ports.ErrInsufficientCapacity), errors.Is(err, ports.ErrQuotaExceeded),
		errors.Is(err, ports.ErrVMLimit), errors.Is(err, ports.ErrDiskFull):
		consume()
		p.capacityFailure(st, ReasonCapacity, now, false)
	case errors.Is(err, ports.ErrImageNotFound):
		consume()
		p.capacityFailure(st, ReasonImageMissing, now, false)
	case errors.Is(err, ports.ErrInvalid):
		consume()
		p.capacityFailure(st, ReasonLaunchError, now, true)
	default:
		if spec.Provider == domain.ProviderTart {
			// StartVM is idempotent per VM name, not per token: count it as failed
			// (a VM that did start shows up in the host report and is adopted).
			consume()
			p.capacityFailure(st, ReasonLaunchError, now, false)
			return
		}
		// Ambiguous (timeout, 5xx, connection reset): the instance may exist.
		// Keep the token and re-issue it after a short backoff; EC2 deduplicates.
		if in == nil {
			in = &intent{token: a.Launch.Token, seq: seq, generation: a.Launch.Generation, issuedAt: now,
				types: a.Launch.InstanceTypes, subnets: a.Launch.SubnetIDs}
			st.intents[a.Launch.Token] = in
		}
		in.pending = false
		st.capacity.throttle++
		in.retryAfter = now.Add(Backoff(st.capacity.throttle, p.cfg.ThrottleBackoffMin, p.cfg.ThrottleBackoffMax, p.rnd))
	}
}

func (p *Planner) capacityFailure(st *PoolState, reason Reason, now time.Time, permanent bool) {
	st.capacity.failures++
	if st.capacity.since.IsZero() {
		st.capacity.since = now
	}
	st.capacity.reason, st.capacity.hold = reason, reason
	delay := Backoff(st.capacity.failures, p.cfg.BackoffMin, p.cfg.BackoffMax, p.rnd)
	if permanent {
		delay = p.cfg.BackoffMax
	}
	st.capacity.until = now.Add(delay)
}

// expireIntents forgets ambiguous launches whose instance never appeared.
func (s *PoolState) expireIntents(now time.Time, grace time.Duration) {
	for tok, in := range s.intents {
		if !in.pending && !in.retryAfter.IsZero() && now.Sub(in.retryAfter) >= grace {
			delete(s.intents, tok)
		}
	}
}

// ------------------------------------------------------------ records

func (s *PoolState) sortedIDs() []string {
	ids := make([]string, 0, len(s.vms))
	for id := range s.vms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// resolveToken records that an instance with this token exists.
func (s *PoolState) resolveToken(tok, prefix string) {
	if tok == "" {
		return
	}
	delete(s.intents, tok)
	if p, e, seq, ok := ParseToken(tok); ok && p == prefix && e == s.ledger.Epoch {
		delete(s.ghosts, seq)
		delete(s.unseen, seq)
		s.reuse = slices.DeleteFunc(s.reuse, func(x uint64) bool { return x == seq })
	}
}

func (p *Planner) reconcileRecords(spec Spec, obs Observation, st *PoolState, facts map[string]*vmFacts, prefix string) {
	now := obs.Now
	ids := make([]string, 0, len(facts))
	for id := range facts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := facts[id]
		if f.inst != nil {
			st.resolveToken(f.inst.Tags[domain.TagLaunchToken], prefix)
		}
		if st.vms[id] != nil {
			continue
		}
		var r *vmRecord
		switch {
		case f.inst != nil:
			if f.prov == provGone {
				continue
			}
			r = &vmRecord{id: id, state: domain.VMLaunching, launchedAt: f.inst.LaunchTime, generation: f.inst.Generation,
				token: f.inst.Tags[domain.TagLaunchToken], instanceType: f.inst.Type}
		case f.tart != nil:
			if f.prov == provGone {
				continue
			}
			r = &vmRecord{id: id, state: domain.VMLaunching, launchedAt: f.tart.LaunchedAt, generation: f.tart.Generation, host: f.host}
			if f.prov == provStopped {
				r.state = domain.VMStopped
			}
		default:
			// Known only to the scheduler (threads or a drain): a VM the provider
			// does not list yet. Capacity accounting rests on the provider listing
			// and on launch results; until Describe catches up, the launch is
			// covered by its in-flight intent or ledger ghost. Drains are handled
			// by drain cleanup.
			continue
		}
		if r.launchedAt.IsZero() {
			r.launchedAt = now
		}
		if spec.Provider == domain.ProviderTart && r.state != domain.VMStopped {
			r.staleBefore = r.launchedAt
		}
		r.firstSeen = now
		st.vms[id] = r
	}
	for _, id := range st.sortedIDs() {
		r := st.vms[id]
		before := r.state
		p.advance(r, facts[id], spec, obs)
		if before == domain.VMLaunching && r.state == domain.VMRegistered {
			st.startupStreak = 0 // a VM registered: provisioning works again
		}
	}
}

// ------------------------------------------------------------ queues

type queueView struct {
	declared  bool
	missing   []string
	present   []domain.QueueObservation // the pool's queues, sorted
	queued    int                       // attributed to this pool
	rawQueued int
	executing int
	perRunner map[string]int // D_r
	starving  []domain.QueueKey
	runnerOf  map[string]domain.Runner // platform key → runner
}

func queueLess(a, b domain.QueueKey) int {
	if c := strings.Compare(a.InstanceNamePrefix, b.InstanceNamePrefix); c != 0 {
		return c
	}
	if c := strings.Compare(a.PlatformKey, b.PlatformKey); c != 0 {
		return c
	}
	switch {
	case a.SizeClass < b.SizeClass:
		return -1
	case a.SizeClass > b.SizeClass:
		return 1
	}
	return 0
}

// PoolQueues returns the queue keys a pool expects the scheduler to have:
// instance names × runners × size class (empty when InstanceNames is empty).
func PoolQueues(spec domain.PoolSpec) []domain.QueueKey {
	var keys []domain.QueueKey
	for _, n := range spec.InstanceNames {
		for _, r := range spec.Runners {
			keys = append(keys, domain.QueueKey{InstanceNamePrefix: n, PlatformKey: domain.PropertiesKey(r.Properties), SizeClass: spec.SizeClass})
		}
	}
	slices.SortFunc(keys, queueLess)
	return keys
}

func analyzeQueues(spec Spec, obs Observation) queueView {
	qv := queueView{perRunner: map[string]int{}, runnerOf: map[string]domain.Runner{}}
	for _, r := range spec.Runners {
		qv.runnerOf[domain.PropertiesKey(r.Properties)] = r
		qv.perRunner[r.Name] = 0
	}
	present := map[domain.QueueKey]domain.QueueObservation{}
	for _, q := range obs.Queues {
		if q.Key.SizeClass != spec.SizeClass {
			continue
		}
		if _, ok := qv.runnerOf[q.Key.PlatformKey]; !ok {
			continue
		}
		if len(spec.InstanceNames) > 0 && !slices.Contains(spec.InstanceNames, q.Key.InstanceNamePrefix) {
			continue
		}
		present[q.Key] = q
	}
	qv.declared = true
	if exp := PoolQueues(spec.PoolSpec); len(exp) > 0 {
		for _, k := range exp {
			if _, ok := present[k]; !ok {
				qv.declared = false
				qv.missing = append(qv.missing, fmt.Sprintf("%s/%s/%d", k.InstanceNamePrefix, k.PlatformKey, k.SizeClass))
			}
		}
	} else {
		for _, r := range spec.Runners {
			key := domain.PropertiesKey(r.Properties)
			found := false
			for k := range present {
				if k.PlatformKey == key {
					found = true
					break
				}
			}
			if !found {
				qv.declared = false
				qv.missing = append(qv.missing, fmt.Sprintf("*/%s/%d", key, spec.SizeClass))
			}
		}
	}
	for _, q := range present {
		qv.present = append(qv.present, q)
	}
	slices.SortFunc(qv.present, func(a, b domain.QueueObservation) int { return queueLess(a.Key, b.Key) })
	for _, q := range qv.present {
		r := qv.runnerOf[q.Key.PlatformKey]
		queued := q.Queued
		if obs.QueuedShare != nil {
			if v, ok := obs.QueuedShare[q.Key]; ok {
				queued = max(v, 0)
			}
		}
		qv.queued += queued
		qv.rawQueued += q.Queued
		qv.perRunner[r.Name] += queued
		if !obs.WorkersKnown {
			qv.perRunner[r.Name] += q.Executing
			qv.executing += q.Executing
		}
		if queued > 0 && q.Workers == 0 {
			qv.starving = append(qv.starving, q.Key)
		}
	}
	if obs.WorkersKnown {
		for _, w := range obs.Workers {
			if !w.Executing || w.ID[domain.LabelPool] != string(spec.Name) {
				continue
			}
			if r, ok := qv.runnerOf[w.Queue.PlatformKey]; ok {
				qv.perRunner[r.Name]++
				qv.executing++
			}
		}
	}
	return qv
}

func ceilDiv(a, b int) int {
	if a <= 0 || b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// DesiredVMs is the scale-out policy of contracts §3 before clamping:
// max(ceil(Σ_r D_r / N), max_r ceil(D_r / S_r)).
func DesiredVMs(spec Spec, perRunner map[string]int) (byTotal, byRunner int) {
	n := spec.VCPUs
	if n <= 0 {
		for _, r := range spec.Runners {
			n = max(n, r.Concurrency)
		}
	}
	total := 0
	for _, r := range spec.Runners {
		dr := perRunner[r.Name]
		total += dr
		if r.Concurrency > 0 {
			byRunner = max(byRunner, ceilDiv(dr, r.Concurrency))
		}
	}
	return ceilDiv(total, max(n, 1)), byRunner
}

// ------------------------------------------------------------ planning

type planning struct {
	p      *Planner
	spec   Spec
	obs    Observation
	st     *PoolState
	qv     queueView
	facts  map[string]*vmFacts
	prefix string
	ec2    bool

	d        Decision
	floor    int
	maxVMs   int
	stops    map[Reason][]string
	drains   map[Reason][]string
	undrains []string
	cleanups []string
	launches []Action
	fresh    int // launches with a newly allocated token (retries are already in flight)
}

func (pl *planning) now() time.Time { return pl.obs.Now }

func (pl *planning) records() []*vmRecord {
	ids := pl.st.sortedIDs()
	out := make([]*vmRecord, 0, len(ids))
	for _, id := range ids {
		out = append(out, pl.st.vms[id])
	}
	return out
}

func (pl *planning) counts() (active, inflight int) {
	for _, r := range pl.st.vms {
		if r.active() {
			active++
		}
	}
	return active, len(pl.st.intents) + len(pl.st.ghosts)
}

func (pl *planning) run() {
	spec, now := pl.spec, pl.now()
	pl.stops, pl.drains = map[Reason][]string{}, map[Reason][]string{}

	pl.maxVMs = max(spec.Max, 0)
	if !spec.Paused && !spec.Deleting {
		pl.floor = FloorAt(spec.MinRunning, spec.Floors, now)
	}
	if spec.Deleting {
		pl.maxVMs = 0
	}
	pl.floor = min(pl.floor, pl.maxVMs)
	byTotal, byRunner := DesiredVMs(spec, pl.qv.perRunner)
	desired := min(max(byTotal, byRunner, pl.floor), pl.maxVMs)
	pl.d.Desired = desired
	pl.d.Demand = Demand{PerRunner: pl.qv.perRunner, Queued: pl.qv.queued, Executing: pl.qv.executing,
		ByTotal: byTotal, ByRunner: byRunner, Floor: pl.floor, Max: pl.maxVMs}

	pl.retire()
	pl.scaleOut(desired, max(byTotal, byRunner))
	pl.scaleIn(desired)
	pl.progressDrains()
	pl.cleanupDrains()
	pl.failFast()
	pl.emit()
	pl.status()
}

// retire drains VMs that must go regardless of demand: failed, deleting,
// rollout, host maintenance, dead-man recycle, max reduction.
func (pl *planning) retire() {
	spec, now, cfg := pl.spec, pl.now(), pl.p.cfg
	newFailures := 0
	defer func() {
		if newFailures > 0 {
			// A VM that never registers is a provisioning failure (broken image,
			// network): back off, probe with one VM at a time until a VM registers,
			// and let QueueFailAfter fail waiting work.
			pl.st.startupStreak += newFailures
			pl.p.capacityFailure(pl.st, ReasonStartupFailures, now, false)
		}
	}()
	for _, r := range pl.records() {
		switch {
		case r.state == domain.VMFailed && r.ec2Stopped && r.stopSent.IsZero():
			pl.stop(r, StopExternal) // a stopped EC2 instance never runs work: terminate now
			pl.st.violations = append(pl.st.violations, invariants.Violation{Invariant: invariants.NoStoppedEC2Instances,
				Pool: string(spec.Name), Subject: r.id, Detail: "instance found stopped; terminating"})
		case r.state == domain.VMFailed:
			if r.drainReason == "" {
				r.drainReason, r.drainStart = r.stopReason, now
				if r.stopReason == StopStartupTimeout {
					newFailures++
				}
			}
		case !r.active():
		case spec.Deleting:
			pl.drain(r, StopRetire)
		case r.generation != "" && r.generation != spec.Generation && spec.Rollout == domain.RolloutEager:
			pl.drain(r, StopRollout)
		case r.generation != "" && r.generation != spec.Generation && pl.ec2 &&
			r.state == domain.VMRegistered && r.busy == 0 && r.threads > 0 && pl.qv.rawQueued == 0 && pl.obs.QueuesKnown:
			pl.drain(r, StopRollout) // lazy: replaced at its first idle moment
		case spec.Provider == domain.ProviderTart && pl.facts[r.id] != nil && pl.facts[r.id].hostCordoned:
			pl.drain(r, StopMaintenance)
		case r.state == domain.VMRegistered && now.Sub(r.launchedAt) >= cfg.Deadman.MaxUptime-cfg.RecycleMargin:
			pl.drain(r, StopDeadman)
		}
	}
	// Max reduction: retire the excess VMs, launching ones first, then idle, then
	// the newest. In-flight launches are not counted: once listed they are
	// launching VMs and rank first, so no working VM makes room for them.
	active, _ := pl.counts()
	excess := active - pl.maxVMs
	if excess <= 0 {
		return
	}
	var cands []*vmRecord
	for _, r := range pl.records() {
		if r.active() {
			cands = append(cands, r)
		}
	}
	slices.SortStableFunc(cands, func(a, b *vmRecord) int {
		ka, kb := retireRank(a), retireRank(b)
		if ka != kb {
			return ka - kb
		}
		return b.launchedAt.Compare(a.launchedAt)
	})
	for _, r := range cands[:min(excess, len(cands))] {
		pl.drain(r, StopRetire)
	}
}

func retireRank(r *vmRecord) int {
	switch {
	case r.state == domain.VMLaunching:
		return 0
	case r.busy == 0:
		return 1
	}
	return 2
}

func (pl *planning) drain(r *vmRecord, reason Reason) {
	if r.state == domain.VMDraining && r.drainReason != "" {
		return
	}
	r.state = domain.VMDraining
	r.drainReason, r.drainStart = reason, pl.now()
	r.drainSent, r.drainAcked, r.operator = true, false, false
	pl.drains[reason] = append(pl.drains[reason], r.id)
}

func (pl *planning) stop(r *vmRecord, reason Reason) {
	r.state = domain.VMStopping
	r.stopReason = reason
	r.stopSent, r.stopPending, r.stopAcked = pl.now(), true, false
	pl.stops[reason] = append(pl.stops[reason], r.id)
}

// launchHold returns why no launch may happen now ("" if launches are allowed).
func (pl *planning) launchHold() Reason {
	spec, obs, st, now := pl.spec, pl.obs, pl.st, pl.now()
	switch {
	case spec.Deleting:
		return ReasonDeleting
	case pl.maxVMs == 0:
		return ReasonMaxZero
	case spec.Paused:
		return ReasonPaused
	case obs.QueuesKnown && !pl.qv.declared:
		return ReasonQueueNotDeclared
	case obs.ImageErr != nil:
		return ReasonImageMissing
	case spec.DailyInstanceHourCap > 0 && obs.InstanceSecondsToday >= spec.DailyInstanceHourCap*3600:
		return ReasonDailyCap
	case !obs.QueuesKnown || !st.providerSeen:
		return ReasonIncomplete
	case now.Before(st.capacity.until):
		if st.capacity.hold == "" {
			return ReasonCapacity
		}
		return st.capacity.hold
	}
	return ""
}

func (pl *planning) scaleOut(desired, rawDesired int) {
	spec, st, now := pl.spec, pl.st, pl.now()
	active, inflight := pl.counts()
	capacity := active + inflight
	pl.d.Demand.Capacity = capacity
	need := desired - capacity
	if rawDesired > pl.maxVMs && capacity >= pl.maxVMs && pl.maxVMs > 0 {
		pl.d.Hold = ReasonAtMax
	}
	// Ambiguous launches (kept in capacity) that are due for a retry with the same token.
	var due []*intent
	for _, tok := range sortedKeys(st.intents) {
		if in := st.intents[tok]; !in.pending && !in.retryAfter.IsZero() && !now.Before(in.retryAfter) {
			due = append(due, in)
		}
	}
	retries := min(len(due), need+len(due))
	if need <= 0 && retries <= 0 {
		return
	}
	// Rescue VMs draining for idleness before paying for a cold start.
	if need > 0 && pl.obs.QueuesKnown && !spec.Deleting {
		for _, r := range pl.records() {
			if need == 0 {
				break
			}
			if r.state != domain.VMDraining || r.drainReason != StopIdle || r.operator || r.generation != spec.Generation ||
				!r.stopSent.IsZero() || r.threads == 0 {
				continue
			}
			if f := pl.facts[r.id]; f != nil && f.hostCordoned {
				continue
			}
			r.state = domain.VMRegistered
			r.drainReason, r.drainStart, r.drainSent, r.drainAcked = "", time.Time{}, false, false
			r.idleSince = now
			r.removeDrainSent = true
			r.staleBefore = now
			pl.undrains = append(pl.undrains, r.id)
			need--
		}
	}
	if hold := pl.launchHold(); hold != "" {
		if need > 0 && pl.d.Hold == "" {
			pl.d.Hold = hold
		}
		return
	}
	budget := -1
	if pl.ec2 && pl.obs.LaunchBudget.Limited {
		budget = max(pl.obs.LaunchBudget.Launches, 0)
	}
	if !pl.ec2 {
		budget = max(pl.obs.TartFreeSlots, 0)
	}
	take := func() bool {
		if budget == 0 {
			if pl.ec2 {
				pl.d.Hold = ReasonAPIBudget
			} else {
				pl.d.Hold = ReasonNoHostSlots
			}
			return false
		}
		if budget > 0 {
			budget--
		}
		return true
	}
	for _, in := range due[:max(retries, 0)] {
		if !take() {
			return
		}
		in.pending, in.retryAfter = true, time.Time{}
		st.ledger.At = now // a retry is a launch attempt: it may create the instance now
		pl.launches = append(pl.launches, Action{Kind: ActLaunch, Reason: ReasonDemand, Launch: &LaunchIntent{
			Token: in.token, Seq: in.seq, Generation: in.generation, InstanceTypes: in.types, SubnetIDs: in.subnets}})
	}
	if st.startupStreak > 0 {
		// Circuit breaker: while launched VMs keep failing to register, only one
		// probe VM may be starting at a time, so a broken image cannot relaunch
		// the whole pool every startupTimeout (cost runaway).
		starting := len(st.intents) + len(st.ghosts)
		for _, r := range st.vms {
			if r.state == domain.VMLaunching {
				starting++
			}
		}
		if allowed := max(1-starting, 0); need > allowed {
			need = allowed
			pl.d.Hold = ReasonStartupFailures
		}
	}
	types := RotateTypes(spec.InstanceTypes, st.typeCooldown, now)
	subnets := RotateTypes(spec.SubnetIDs, st.subnetCooldown, now)
	reason := ReasonDemand
	if rawDesired < desired {
		reason = ReasonFloor
	}
	for ; need > 0; need-- {
		if !take() {
			return
		}
		tok, seq := st.allocToken(pl.prefix, now)
		in := &intent{token: tok, seq: seq, generation: spec.Generation, issuedAt: now, pending: true,
			types: slices.Clone(types), subnets: slices.Clone(subnets)}
		st.intents[tok] = in
		pl.fresh++
		pl.launches = append(pl.launches, Action{Kind: ActLaunch, Reason: reason, Launch: &LaunchIntent{
			Token: tok, Seq: seq, Generation: spec.Generation, InstanceTypes: in.types, SubnetIDs: in.subnets}})
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// scaleIn drains VMs whose idle timer expired while all pool queues were empty.
func (pl *planning) scaleIn(desired int) {
	spec, st, now := pl.spec, pl.st, pl.now()
	if !pl.obs.QueuesKnown || !pl.obs.WorkersKnown || pl.qv.rawQueued > 0 || st.queueEmptySince.IsZero() ||
		spec.IdleTimeout <= 0 || len(pl.launches) > 0 || len(pl.undrains) > 0 {
		return
	}
	if now.Sub(st.queueEmptySince) < spec.IdleTimeout {
		return
	}
	active := 0
	var cands []*vmRecord
	for _, r := range pl.records() {
		if !r.active() {
			continue
		}
		active++
		if r.state == domain.VMRegistered && r.busy == 0 && r.threads > 0 && !r.idleSince.IsZero() &&
			now.Sub(r.idleSince) >= spec.IdleTimeout {
			cands = append(cands, r)
		}
	}
	allowed := active - max(pl.floor, desired)
	if allowed <= 0 || len(cands) == 0 {
		return
	}
	slices.SortStableFunc(cands, func(a, b *vmRecord) int {
		oa, ob := a.generation != spec.Generation, b.generation != spec.Generation
		if oa != ob {
			if oa {
				return -1
			}
			return 1
		}
		return a.idleSince.Compare(b.idleSince)
	})
	for _, r := range cands[:min(allowed, len(cands))] {
		if vs := invariants.IdleScaleInAllowed(string(spec.Name), r.view(spec.Name), now, st.queueEmptySince, spec.IdleTimeout); vs != nil {
			st.violations = append(st.violations, vs...)
			continue
		}
		pl.drain(r, StopIdle)
	}
}

// progressDrains stops drained VMs once confirmed idle (or the drain timeout
// expired), re-issues lost drains and stops, and handles operator drains.
func (pl *planning) progressDrains() {
	spec, now := pl.spec, pl.now()
	for _, r := range pl.records() {
		switch r.state {
		case domain.VMDraining, domain.VMFailed:
		case domain.VMStopping:
			// Re-issue only our own stops that failed or did not take effect; a VM
			// shutting down on its own (dead-man, spot, operator) is left alone.
			if !r.stopSent.IsZero() && !r.stopPending && (r.stopRetry || now.Sub(r.stopSent) >= stopRetry) {
				pl.stops[r.stopReason] = append(pl.stops[r.stopReason], r.id)
				r.stopSent, r.stopPending, r.stopRetry = now, true, false
			}
			continue
		default:
			continue
		}
		if slices.Contains(pl.drains[r.drainReason], r.id) {
			continue // drained in this decision: confirm on a later observation
		}
		if r.ec2Stopped {
			continue
		}
		expired := spec.DrainTimeout > 0 && !r.drainStart.IsZero() && now.Sub(r.drainStart) >= spec.DrainTimeout
		if r.operator {
			// Drained by an operator: stop it once it was idle for idleTimeout.
			idleLong := r.busy == 0 && (r.threads == 0 || (!r.idleSince.IsZero() && now.Sub(r.idleSince) >= spec.IdleTimeout))
			if pl.obs.WorkersKnown && idleLong && spec.IdleTimeout > 0 {
				pl.stop(r, StopDrain)
			}
			continue
		}
		if !r.drainAcked && !r.drainSent {
			r.drainSent = true
			if r.drainReason == "" {
				r.drainReason = StopRetire
			}
			pl.drains[r.drainReason] = append(pl.drains[r.drainReason], r.id)
			continue
		}
		confirmedIdle := pl.obs.WorkersKnown && r.drainAcked && r.busy == 0 && (r.threads == 0 || r.drainedThreads == r.threads)
		if confirmedIdle || expired {
			reason := r.drainReason
			if r.state == domain.VMFailed && r.stopReason != "" {
				reason = r.stopReason
			}
			if vs := invariants.StopAllowed(string(spec.Name), r.view(spec.Name), now, r.drainStart, spec.DrainTimeout); vs != nil {
				pl.st.violations = append(pl.st.violations, vs...)
				continue
			}
			pl.stop(r, reason)
		}
	}
}

// cleanupDrains removes {pool,node} drains of VMs that are gone (EC2: right
// after termination, instance IDs never return) or restarted (Tart), and of
// nodes nobody knows any more.
func (pl *planning) cleanupDrains() {
	if !pl.obs.WorkersKnown {
		return
	}
	spec, st, now := pl.spec, pl.st, pl.now()
	for _, id := range sortedKeys(pl.facts) {
		f := pl.facts[id]
		if !f.ownDrain {
			continue
		}
		r := st.vms[id]
		if r != nil && r.removeDrainSent {
			continue
		}
		switch {
		case r == nil:
			// Unknown node: remove after the consistency grace (it may be a fresh launch we drained).
			if f.prov == provUnavailable || (spec.Provider == domain.ProviderTart && f.prov == provStopped) {
				continue
			}
			if st.drainCleanup == nil {
				st.drainCleanup = map[string]time.Time{}
			}
			first, ok := st.drainCleanup[id]
			if !ok {
				st.drainCleanup[id] = now
				continue
			}
			if now.Sub(first) < pl.p.cfg.ConsistencyGrace {
				continue
			}
			st.drainCleanup[id] = now.Add(pl.p.cfg.ConsistencyGrace) // re-issue after another grace if it persists
		case r.state == domain.VMTerminated:
		case r.state == domain.VMStopping && pl.ec2 && r.stopAcked && r.threads == 0:
		case r.drainIsStale() && r.active():
		default:
			continue
		}
		if r != nil {
			r.removeDrainSent = true
		}
		pl.cleanups = append(pl.cleanups, id)
	}
	// Forget terminated records with nothing left to clean up.
	for _, id := range st.sortedIDs() {
		r := st.vms[id]
		if r.state == domain.VMTerminated && !r.ownDrain && !r.removeDrainSent && !slices.Contains(pl.cleanups, id) {
			delete(st.vms, id)
		}
	}
}

// failFast fails queued work that cannot be served: immediately when the pool
// cannot launch at all, after QueueFailAfter when capacity errors persist
// with work waiting and no VM serving it (R-RE-2, R-SCALE-4).
func (pl *planning) failFast() {
	spec, st, now, obs := pl.spec, pl.st, pl.now(), pl.obs
	if !obs.QueuesKnown {
		return
	}
	registered := 0
	for _, r := range st.vms {
		if r.state == domain.VMRegistered {
			registered++
		}
	}
	waiting := pl.qv.queued > 0 && registered == 0
	if waiting && !st.capacity.since.IsZero() {
		if st.noCapacitySince.IsZero() {
			st.noCapacitySince = now
		}
	} else {
		st.noCapacitySince = time.Time{}
	}
	if registered > 0 {
		st.capacity.since = time.Time{}
	}
	var reason Reason
	var msg string
	switch hold := pl.launchHold(); hold {
	case ReasonDeleting:
		reason, msg = hold, "pool is being deleted"
	case ReasonMaxZero:
		reason, msg = hold, "pool max is 0 (disabled)"
	case ReasonPaused:
		reason, msg = hold, "pool is paused (cordoned)"
	case ReasonQueueNotDeclared:
		reason, msg = hold, "platform queues not declared in the scheduler (add the pool to values.pools and helm upgrade): "+strings.Join(pl.qv.missing, ", ")
	case ReasonImageMissing:
		reason, msg = hold, "pool image cannot be resolved: "+errString(obs.ImageErr)
	case ReasonDailyCap:
		reason, msg = hold, fmt.Sprintf("pool reached its daily instance-hour cap (%g h)", spec.DailyInstanceHourCap)
	}
	if reason == "" && !st.noCapacitySince.IsZero() && now.Sub(st.noCapacitySince) >= pl.p.cfg.QueueFailAfter {
		reason = ReasonNoCapacity
		msg = fmt.Sprintf("no capacity for %s (%s)", now.Sub(st.noCapacitySince).Round(time.Second), st.capacity.reason)
	}
	if reason == "" || len(pl.qv.starving) == 0 {
		return
	}
	pl.d.Status.CapacityFailure = reason
	pl.d.Actions = append(pl.d.Actions, Action{Kind: ActFailQueues, Reason: reason, Queues: slices.Clone(pl.qv.starving),
		Code: StatusFailedPrecondition, Message: fmt.Sprintf("cucina: pool %s cannot run this action: %s", spec.Name, msg)})
	st.lastFailQueues = now
}

func errString(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

// emit orders the actions: cleanups, rescues, stops, drains, launches; then
// fail-queues (already appended) last. Within a kind, one action per reason.
func (pl *planning) emit() {
	var acts []Action
	if len(pl.cleanups) > 0 {
		acts = append(acts, Action{Kind: ActRemoveDrain, Reason: ReasonCleanup, VMs: pl.cleanups})
	}
	if len(pl.undrains) > 0 {
		acts = append(acts, Action{Kind: ActUndrain, Reason: ReasonRescue, VMs: pl.undrains})
	}
	stopKind := ActTerminate
	if !pl.ec2 {
		stopKind = ActStop
	}
	for _, reason := range sortedReasons(pl.stops) {
		ids := pl.checkStops(pl.stops[reason])
		if len(ids) > 0 {
			acts = append(acts, Action{Kind: stopKind, Reason: reason, VMs: ids})
		}
	}
	for _, reason := range sortedReasons(pl.drains) {
		acts = append(acts, Action{Kind: ActAddDrain, Reason: reason, VMs: pl.drains[reason]})
	}
	if pl.fresh > 0 {
		// Only launches with fresh tokens add capacity (retries of ambiguous
		// launches are already counted in flight); pl.counts() includes them.
		active, inflight := pl.counts()
		if vs := invariants.LaunchWithinMax(string(pl.spec.Name), pl.maxVMs, active, inflight-pl.fresh, pl.fresh); vs != nil {
			pl.st.violations = append(pl.st.violations, vs...)
			fresh := pl.launches[len(pl.launches)-pl.fresh:]
			for _, a := range fresh {
				delete(pl.st.intents, a.Launch.Token)
				pl.st.release(a.Launch.Seq)
			}
			pl.launches = pl.launches[:len(pl.launches)-pl.fresh]
		}
	}
	acts = append(acts, pl.launches...)
	pl.d.Actions = append(acts, pl.d.Actions...)
	pl.d.Violations = append(pl.d.Violations, pl.st.violations...)
	pl.st.violations = nil
}

// checkStops drops (and reports) any stop that would hit a busy VM whose drain
// timeout has not expired (defence in depth; the planner never proposes one).
func (pl *planning) checkStops(ids []string) []string {
	out := ids[:0:0]
	for _, id := range ids {
		r := pl.st.vms[id]
		if r == nil {
			continue
		}
		if vs := invariants.StopAllowed(string(pl.spec.Name), r.view(pl.spec.Name), pl.now(), r.drainStart, pl.spec.DrainTimeout); vs != nil {
			pl.st.violations = append(pl.st.violations, vs...)
			r.state, r.stopSent, r.stopPending = domain.VMDraining, time.Time{}, false
			continue
		}
		out = append(out, id)
	}
	return out
}

func sortedReasons(m map[Reason][]string) []Reason {
	rs := make([]Reason, 0, len(m))
	for r, ids := range m {
		if len(ids) > 0 {
			rs = append(rs, r)
		}
	}
	slices.Sort(rs)
	return rs
}

func (pl *planning) status() {
	spec, st, obs := pl.spec, pl.st, pl.obs
	s := &pl.d.Status
	s.QueueDeclared = !obs.QueuesKnown || pl.qv.declared
	s.ImageResolved = obs.ImageErr == nil
	hold := pl.launchHold()
	s.Eligible = hold == ""
	if s.CapacityFailure == "" {
		switch {
		case !st.capacity.since.IsZero():
			s.CapacityFailure = st.capacity.reason
		case obs.ImageErr != nil:
			s.CapacityFailure = ReasonImageMissing
		}
	}
	s.CapacityFailingSince = st.capacity.since
	s.CapacityAvailable = s.CapacityFailure == "" || s.CapacityFailure == ReasonThrottled
	empty := len(st.intents) == 0 && len(st.ghosts) == 0
	for _, r := range st.vms {
		if r.alive() {
			empty = false
		}
		if r.alive() && r.generation != "" && r.generation != spec.Generation {
			s.OldGenerationVMs++
		}
	}
	s.Empty = empty
	// Thread headroom per runner platform: free threads now plus what the pool may still launch.
	pl.d.ThreadHeadroom = map[string]int{}
	active, inflight := pl.counts()
	room := 0
	if s.Eligible {
		room = max(pl.maxVMs-active-inflight, 0)
	}
	free := map[string]int{}
	if obs.WorkersKnown {
		for _, w := range obs.Workers {
			if w.ID[domain.LabelPool] == string(spec.Name) && !w.Executing && !w.Drained {
				free[w.Queue.PlatformKey]++
			}
		}
	}
	for _, r := range spec.Runners {
		k := domain.PropertiesKey(r.Properties)
		pl.d.ThreadHeadroom[k] = free[k] + room*r.Concurrency
	}
}
