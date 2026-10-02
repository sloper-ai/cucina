// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling

import (
	"slices"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// vmRecord is the planner's memory of one VM (EC2 instance or Tart VM).
type vmRecord struct {
	id           string
	state        domain.VMState
	generation   string
	host         string
	instanceType string
	token        string

	launchedAt   time.Time // instance launch / VM start
	registeredAt time.Time // zero until a runner thread was seen
	lostSince    time.Time // registered VM whose threads vanished
	idleSince    time.Time // zero while busy
	firstSeen    time.Time
	lastListed   time.Time // last provider sighting
	listed       bool      // ever listed by the provider

	threads, busy, drainedThreads int
	ownDrain                      bool      // a {pool,node} drain is registered (observed)
	ownDrainCreated               time.Time // earliest Created of the own drains

	drainReason Reason // set while draining
	drainStart  time.Time
	drainSent   bool // AddDrain emitted, result pending
	drainAcked  bool // AddDrain succeeded or own drain observed
	operator    bool // drained by someone else (not rescuable, never undrained by us)

	stopReason  Reason
	stopSent    time.Time // last terminate/stop emission (zero: not sent)
	stopPending bool      // result pending
	stopRetry   bool      // the stop call failed: re-issue it
	stopAcked   bool      // terminate/stop call succeeded
	ec2Stopped  bool      // EC2 instance in stopped/stopping (never allowed)

	removeDrainSent bool
	// staleBefore: own drains created before this instant belong to an earlier
	// run of the VM (Tart restart) or were undrained; they are removed, never adopted.
	staleBefore time.Time
}

// intent is a launch whose outcome is not known yet.
type intent struct {
	token      string
	seq        uint64
	generation string
	issuedAt   time.Time // first emission
	pending    bool      // emitted, result not yet received
	retryAfter time.Time // ambiguous failure: re-emit with the same token after this
	types      []string
	subnets    []string
}

// provStatus classifies what the provider says about a VM.
type provStatus int

const (
	provUnknown     provStatus = iota // not listed / provider not known this round
	provAlive                         // EC2 pending/running; Tart powered on
	provStopping                      // EC2 shutting-down; Tart stopping
	provStopped                       // Tart stopped (disk kept)
	provEC2Stopped                    // EC2 stopped/stopping: forbidden, terminate it
	provGone                          // EC2 terminated; Tart VM deleted
	provUnavailable                   // Tart host offline / unknown
)

// vmFacts is what one observation says about one node.
type vmFacts struct {
	prov         provStatus
	inst         *ports.Instance
	tart         *domain.VM
	host         string
	hostCordoned bool

	threads, busy, drained int
	ownDrain               bool
	ownDrainCreated        time.Time
}

func ec2Status(s ports.InstanceState) provStatus {
	switch s {
	case ports.InstancePending, ports.InstanceRunning:
		return provAlive
	case ports.InstanceShuttingDown:
		return provStopping
	case ports.InstanceTerminated:
		return provGone
	case ports.InstanceStopping, ports.InstanceStopped:
		return provEC2Stopped
	}
	return provAlive
}

// tartStatus maps the VM state hostd reports (HostState.VMs[].State).
func tartStatus(s domain.VMState) provStatus {
	switch s {
	case domain.VMStopped, domain.VMFailed:
		return provStopped
	case domain.VMStopping:
		return provStopping
	case domain.VMTerminated:
		return provGone
	case domain.VMUnavailable:
		return provUnavailable
	}
	return provAlive
}

// isOwnDrain reports whether a drain pattern is exactly {pool: <pool>, node: <node>}.
func isOwnDrain(p ports.WorkerID, pool domain.PoolName) (string, bool) {
	if len(p) != 2 || p[domain.LabelPool] != string(pool) {
		return "", false
	}
	node, ok := p[domain.LabelNode]
	return node, ok && node != ""
}

// gather collects per-node facts from one observation.
func gather(spec Spec, obs Observation) map[string]*vmFacts {
	facts := map[string]*vmFacts{}
	get := func(id string) *vmFacts {
		f := facts[id]
		if f == nil {
			f = &vmFacts{prov: provUnknown}
			facts[id] = f
		}
		return f
	}
	if obs.WorkersKnown {
		for _, w := range obs.Workers {
			if w.ID[domain.LabelPool] != string(spec.Name) || w.ID[domain.LabelNode] == "" {
				continue
			}
			f := get(w.ID[domain.LabelNode])
			f.threads++
			if w.Executing {
				f.busy++
			}
			if w.Drained {
				f.drained++
			}
		}
		for _, d := range obs.Drains {
			node, ok := isOwnDrain(d.Pattern, spec.Name)
			if !ok {
				continue
			}
			f := get(node)
			if !f.ownDrain || d.Created.Before(f.ownDrainCreated) {
				f.ownDrainCreated = d.Created
			}
			f.ownDrain = true
		}
	}
	if !obs.ProviderKnown {
		return facts
	}
	if spec.Provider == domain.ProviderTart {
		for i := range obs.Hosts {
			h := &obs.Hosts[i]
			for j := range h.VMs {
				vm := &h.VMs[j]
				if vm.Pool != spec.Name {
					continue
				}
				f := get(vm.ID)
				f.tart, f.host, f.hostCordoned = vm, h.Serial, h.Cordoned
				f.prov = tartStatus(vm.State)
				if !h.Online {
					f.prov = provUnavailable
				}
			}
		}
		return facts
	}
	for i := range obs.Instances {
		in := &obs.Instances[i]
		if in.Pool != spec.Name {
			continue
		}
		f := get(in.ID)
		f.inst = in
		f.prov = ec2Status(in.State)
	}
	return facts
}

// tartHostOf returns the host part of a "<host>/<vm>" node ID.
func tartHostOf(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return id[:i]
	}
	return ""
}

// alive reports whether the record occupies a VM (anything but terminated/stopped).
func (r *vmRecord) alive() bool {
	return r.state != domain.VMTerminated && r.state != domain.VMStopped
}

// active reports whether the record counts as capacity: launching, or
// registered and not draining.
func (r *vmRecord) active() bool {
	return r.state == domain.VMLaunching || r.state == domain.VMRegistered
}

func (r *vmRecord) resetRun() {
	r.registeredAt, r.lostSince, r.idleSince = time.Time{}, time.Time{}, time.Time{}
	r.drainReason, r.drainStart, r.drainSent, r.drainAcked, r.operator = "", time.Time{}, false, false, false
	r.stopReason, r.stopSent, r.stopPending, r.stopAcked, r.stopRetry = "", time.Time{}, false, false, false
	r.threads, r.busy, r.drainedThreads = 0, 0, 0
}

func (r *vmRecord) toTerminated() {
	if r.stopReason == "" {
		r.stopReason = StopExternal
	}
	r.state = domain.VMTerminated
	r.threads, r.busy, r.drainedThreads = 0, 0, 0
}

// advance applies one observation to a record: the per-VM state machine.
// Decisions (drain, stop) are taken later by the planner; this only follows
// what the provider and the scheduler report.
func (p *Planner) advance(r *vmRecord, f *vmFacts, spec Spec, obs Observation) {
	now := obs.Now
	if f == nil {
		f = &vmFacts{prov: provUnknown}
	}
	if obs.WorkersKnown {
		r.threads, r.busy, r.drainedThreads = f.threads, f.busy, f.drained
		r.ownDrain, r.ownDrainCreated = f.ownDrain, f.ownDrainCreated
	} else {
		// Blind: the VM may have worked meanwhile. Restart its idle timer.
		r.idleSince = time.Time{}
	}
	ps := f.prov
	if obs.ProviderKnown {
		switch {
		case f.inst != nil || f.tart != nil:
			r.lastListed, r.listed = now, true
			if f.inst != nil {
				r.instanceType = f.inst.Type
				if r.generation == "" {
					r.generation = f.inst.Generation
				}
			}
			if f.tart != nil {
				r.host = f.host
				if r.generation == "" {
					r.generation = f.tart.Generation
				}
			}
		case spec.Provider == domain.ProviderTart:
			// Not reported by any host: deleted if its host answered, else unavailable.
			ps = provGone
			if !hostReported(obs.Hosts, tartHostOf(r.id)) {
				ps = provUnavailable
			}
		default:
			// EC2 Describe includes terminated instances for about an hour; a
			// launch not visible after the grace period is gone.
			if now.Sub(r.launchedAt) >= p.cfg.ConsistencyGrace && now.Sub(r.lastListed) >= p.cfg.ConsistencyGrace {
				ps = provGone
			}
		}
	}

	switch r.state {
	case domain.VMTerminated:
		return
	case domain.VMStopped:
		switch ps {
		case provAlive:
			// A stopped Tart VM runs again: a new run, its old drain is stale.
			r.resetRun()
			r.state = domain.VMLaunching
			r.launchedAt = now
			if f.tart != nil && !f.tart.LaunchedAt.IsZero() {
				r.launchedAt = f.tart.LaunchedAt
			}
			r.staleBefore = r.launchedAt
			if f.tart != nil && f.tart.Generation != "" {
				r.generation = f.tart.Generation
			}
		case provGone:
			r.toTerminated()
			return
		default:
			return
		}
	}

	switch ps {
	case provGone:
		r.toTerminated()
		return
	case provStopping:
		if r.state != domain.VMStopping {
			r.state = domain.VMStopping
			if r.stopReason == "" {
				r.stopReason = StopExternal
			}
		}
		return
	case provStopped:
		r.state = domain.VMStopped
		if r.stopReason == "" {
			r.stopReason = StopExternal
		}
		r.threads, r.busy, r.drainedThreads = 0, 0, 0
		r.stopSent, r.stopPending = time.Time{}, false
		return
	case provEC2Stopped:
		r.ec2Stopped = true
		if r.state != domain.VMStopping {
			r.state = domain.VMFailed
			if r.stopReason == "" {
				r.stopReason = StopExternal
			}
		}
		return
	case provUnavailable:
		if r.state != domain.VMStopping {
			r.state = domain.VMUnavailable
		}
		return
	}

	// provAlive or provUnknown: follow the scheduler.
	if r.state == domain.VMUnavailable {
		r.state = domain.VMLaunching
		if !r.registeredAt.IsZero() {
			r.state = domain.VMRegistered
		}
	}
	if r.state == domain.VMStopping {
		return
	}
	if !obs.WorkersKnown {
		return
	}
	if r.threads > 0 {
		if r.registeredAt.IsZero() {
			r.registeredAt = now
		}
		r.lostSince = time.Time{}
		if r.state == domain.VMLaunching {
			r.state = domain.VMRegistered
		}
	} else if (r.state == domain.VMRegistered || r.state == domain.VMDraining) && r.lostSince.IsZero() {
		r.lostSince = now
	}

	if r.busy > 0 {
		r.idleSince = time.Time{}
	} else if r.idleSince.IsZero() && r.threads > 0 {
		r.idleSince = now
	}

	if r.state == domain.VMRegistered || r.state == domain.VMLaunching {
		switch {
		case r.ownDrain && !r.drainIsStale() && !r.removeDrainSent && r.drainReason == "":
			// A drain of ours we don't remember (controller restart): adopt it.
			r.state = domain.VMDraining
			r.drainReason = StopIdle
			if r.generation != "" && r.generation != spec.Generation {
				r.drainReason = StopRollout
			}
			r.drainStart, r.drainAcked = now, true
		case !r.ownDrain && r.threads > 0 && r.drainedThreads == r.threads && r.drainReason == "":
			// Drained by an operator (pattern {node}): not capacity, stopped once idle.
			r.state = domain.VMDraining
			r.drainReason, r.operator = StopDrain, true
			r.drainStart, r.drainAcked = now, true
		}
	}
	if r.state == domain.VMDraining && r.operator && r.threads > 0 && r.drainedThreads < r.threads {
		// The operator undrained it.
		r.state = domain.VMRegistered
		r.drainReason, r.operator, r.drainStart, r.drainAcked = "", false, time.Time{}, false
	}

	switch r.state {
	case domain.VMLaunching:
		if spec.StartupTimeout > 0 && now.Sub(r.launchedAt) >= spec.StartupTimeout {
			r.state = domain.VMFailed
			r.stopReason = StopStartupTimeout
		}
	case domain.VMRegistered:
		if spec.StartupTimeout > 0 && !r.lostSince.IsZero() && now.Sub(r.lostSince) >= spec.StartupTimeout {
			r.state = domain.VMFailed
			r.stopReason = StopStartupTimeout
		}
	}
}

func hostReported(hosts []ports.HostState, serial string) bool {
	return slices.ContainsFunc(hosts, func(h ports.HostState) bool { return h.Serial == serial && h.Online })
}

// drainIsStale reports an own drain that predates the VM's current run.
func (r *vmRecord) drainIsStale() bool {
	return r.ownDrain && !r.staleBefore.IsZero() && r.ownDrainCreated.Before(r.staleBefore)
}

// view converts a record into the shared domain.VM.
func (r *vmRecord) view(pool domain.PoolName) domain.VM {
	return domain.VM{
		ID:           r.id,
		Pool:         pool,
		Generation:   r.generation,
		State:        r.state,
		LaunchedAt:   r.launchedAt,
		RegisteredAt: r.registeredAt,
		IdleSince:    r.idleSince,
		Threads:      r.threads,
		Busy:         r.busy,
		Drained:      r.state == domain.VMDraining || (r.threads > 0 && r.drainedThreads == r.threads),
		InstanceType: r.instanceType,
		Host:         r.host,
	}
}
