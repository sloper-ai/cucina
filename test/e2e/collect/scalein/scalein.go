// SPDX-License-Identifier: FSL-1.1-ALv2

// Package scalein evaluates per-worker scale-in from live daemon/provider
// observations (T8, NFR-C1). A disappearing CLI row, an API error, and a
// terminate request are not proof that an EC2 instance terminated.
package scalein

import (
	"fmt"
	"sort"
	"time"
)

// Policy is the resolved pool policy. DrainGrace is the short grace for an
// already idle worker, not the timeout for forcibly draining a busy worker.
type Policy struct {
	IdleTimeout time.Duration `json:"idleTimeout"`
	DrainGrace  time.Duration `json:"drainGrace"`
}

// Worker is one daemon worker-summary row. IdleFor is absent except while
// the controller can positively report continuous idleness.
type Worker struct {
	Node    string         `json:"node"`
	Pool    string         `json:"pool"`
	State   string         `json:"state"`
	Busy    uint32         `json:"busy"`
	IdleFor *time.Duration `json:"idleFor,omitempty"`
	Drained bool           `json:"drained"`
}

// Queue reports aggregate pending work for every observed runner of a pool.
// A pool omitted from this map is unknown, not an empty queue.
type Queue struct {
	Queued uint64 `json:"queued"`
}

// Instance is an explicitly described, campaign-tagged provider instance.
// Include terminal states; absence alone never means termination.
type Instance struct {
	Node     string    `json:"node"`
	Pool     string    `json:"pool"`
	State    string    `json:"state"`
	Launched time.Time `json:"launched,omitempty"`
}

// Drain is an acknowledged drain event from the management timeline.
// Its time records successful drain application, not provider termination.
type Drain struct {
	Node         string    `json:"node"`
	Pool         string    `json:"pool"`
	At           time.Time `json:"at"`
	Acknowledged bool      `json:"acknowledged"` // new success-only drain-acknowledged contract
}

// Sample brackets the source calls with the observer's clock. Every source
// must succeed; Errors makes coverage incomplete instead of inventing zero.
type Sample struct {
	Started   time.Time        `json:"started"`
	Finished  time.Time        `json:"finished"`
	Workers   []Worker         `json:"workers"`
	Queues    map[string]Queue `json:"queues"`
	Instances []Instance       `json:"instances"`
	Drains    []Drain          `json:"drains,omitempty"`
	Errors    []string         `json:"errors,omitempty"`
}

// Node is the reportable evidence for one cohort member. Idle start is an
// interval because the duration was computed between request and response.
type Node struct {
	ID           string        `json:"id"`
	Pool         string        `json:"pool"`
	IdleEarliest time.Time     `json:"idleEarliest,omitempty"`
	IdleLatest   time.Time     `json:"idleLatest,omitempty"`
	DrainedAt    time.Time     `json:"drainedAt,omitempty"`
	TerminatedBy time.Time     `json:"terminatedBy,omitempty"`
	ElapsedUpper time.Duration `json:"elapsedUpper"`
	Limit        time.Duration `json:"limit"`
}

// Result is a complete verdict, never a vacuous pass over an empty cohort.
// Violations take precedence over Unavailable reasons.
type Result struct {
	Pass        bool     `json:"pass"`
	Nodes       []Node   `json:"nodes"`
	Violations  []string `json:"violations,omitempty"`
	Unavailable []string `json:"unavailable,omitempty"`
}

// Evaluate covers every live instance observed in samples. Callers retain
// the pre-workload samples through final scale-in and separately prove zero
// remaining instances, volumes, interfaces and IP addresses.
func Evaluate(policies map[string]Policy, samples []Sample) Result {
	type emptyPeriod struct {
		empty                      bool
		lastBusy, earliest, latest time.Time
	}
	var result Result
	nodes := map[string]*Node{}
	historicalTerminals := map[string]bool{}
	queues := map[string]*emptyPeriod{}
	missing, violations := map[string]bool{}, map[string]bool{}
	var previous time.Time
	for sampleIndex, s := range samples {
		if s.Started.IsZero() || s.Finished.Before(s.Started) || (!previous.IsZero() && s.Started.Before(previous)) {
			missing["invalid or out-of-order observation times"] = true
			continue
		}
		previous = s.Started
		if len(s.Errors) > 0 {
			for _, e := range s.Errors {
				missing["source coverage: "+e] = true
			}
			continue
		}
		// Terminal instances from earlier scenarios are not this cohort. Once
		// observed live, a node remains required even after it disappears.
		for _, in := range s.Instances {
			if in.Node == "" || in.Pool == "" {
				missing["instance without node/pool identity"] = true
				continue
			}
			n := nodes[in.Node]
			if n == nil {
				if in.State == "terminated" && historicalTerminals[in.Node] {
					continue
				}
				if in.State == "terminated" && sampleIndex == 0 && !in.Launched.IsZero() && in.Launched.Before(s.Started) {
					historicalTerminals[in.Node] = true
					continue
				}
				p, ok := policies[in.Pool]
				if !ok || p.IdleTimeout <= 0 || p.DrainGrace < 0 {
					missing[in.Pool+": unresolved idle policy"] = true
				}
				n = &Node{ID: in.Node, Pool: in.Pool, Limit: p.IdleTimeout + p.DrainGrace}
				nodes[in.Node] = n
			} else if n.Pool != in.Pool {
				violations[in.Node+": provider pool identity changed"] = true
			}
		}
		for pool, q := range s.Queues {
			p := queues[pool]
			if p == nil {
				p = &emptyPeriod{}
				queues[pool] = p
			}
			if q.Queued > 0 {
				p.empty = false
				p.lastBusy = s.Started
				for _, n := range nodes {
					if n.Pool == pool && n.TerminatedBy.IsZero() {
						n.IdleEarliest = time.Time{}
						n.IdleLatest = time.Time{}
						n.DrainedAt = time.Time{}
					}
				}
			} else if !p.empty {
				p.empty = true
				p.earliest = p.lastBusy
				p.latest = s.Finished
			}
		}
		for _, n := range nodes {
			if n.TerminatedBy.IsZero() {
				if _, ok := s.Queues[n.Pool]; !ok {
					missing[n.Pool+": missing queue observation"] = true
				}
			}
		}
		for _, w := range s.Workers {
			n := nodes[w.Node]
			if n == nil {
				missing[w.Node+": daemon worker has no observed tagged provider membership"] = true
				continue
			}
			if w.Pool != n.Pool {
				violations[w.Node+": daemon/provider pool identity differs"] = true
				continue
			}
			if !n.TerminatedBy.IsZero() {
				continue
			}
			if w.Busy > 0 || w.State == "busy" {
				n.IdleEarliest = time.Time{}
				n.IdleLatest = time.Time{}
				n.DrainedAt = time.Time{}
				continue
			}
			q := queues[n.Pool]
			if w.State == "idle" && w.IdleFor != nil && q != nil && q.empty {
				if *w.IdleFor < 0 {
					missing[w.Node+": negative idle duration"] = true
					continue
				}
				lo, hi := s.Started.Add(-*w.IdleFor), s.Finished.Add(-*w.IdleFor)
				if q.earliest.After(lo) {
					lo = q.earliest
				}
				if q.latest.After(hi) {
					hi = q.latest
				}
				if !n.IdleLatest.IsZero() && lo.After(n.IdleLatest) {
					// A new idle episode may occur wholly between samples.
					n.DrainedAt = time.Time{}
				}
				n.IdleEarliest, n.IdleLatest = lo, hi
			}
			if w.Drained && !n.IdleEarliest.IsZero() && n.DrainedAt.IsZero() {
				n.DrainedAt = s.Finished
			}
		}
		for _, d := range s.Drains {
			if !d.Acknowledged {
				continue
			}
			n := nodes[d.Node]
			if n == nil || d.Pool != n.Pool || n.IdleEarliest.IsZero() {
				continue
			}
			if !d.At.Before(n.IdleEarliest) && !d.At.After(s.Finished) && (n.DrainedAt.IsZero() || d.At.Before(n.DrainedAt)) {
				n.DrainedAt = d.At
			}
		}
		for _, in := range s.Instances {
			n := nodes[in.Node]
			if n == nil {
				continue
			}
			switch in.State {
			case "terminated":
				if n.TerminatedBy.IsZero() {
					n.TerminatedBy = s.Finished
				}
			case "pending", "running", "shutting-down", "stopping", "stopped":
				if !n.TerminatedBy.IsZero() {
					violations[in.Node+": provider returned live after termination"] = true
				}
				if !n.IdleLatest.IsZero() && s.Started.After(n.IdleLatest.Add(n.Limit)) {
					violations[in.Node+": observed live beyond idle timeout plus drain grace"] = true
				}
			default:
				missing[in.Node+": unknown provider state"] = true
			}
		}
	}
	if len(nodes) == 0 {
		missing["no live worker cohort was observed"] = true
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := nodes[id]
		if n.IdleEarliest.IsZero() {
			missing[id+": no valid idle episode"] = true
		}
		if n.DrainedAt.IsZero() {
			missing[id+": no acknowledged drain"] = true
		}
		if n.TerminatedBy.IsZero() {
			missing[id+": no provider-confirmed termination"] = true
		}
		if !n.IdleEarliest.IsZero() && !n.TerminatedBy.IsZero() {
			n.ElapsedUpper = n.TerminatedBy.Sub(n.IdleEarliest)
			if n.ElapsedUpper < 0 || (!n.DrainedAt.IsZero() && n.DrainedAt.After(n.TerminatedBy)) {
				violations[id+": contradictory lifecycle timestamps"] = true
			}
			if n.ElapsedUpper > n.Limit {
				missing[fmt.Sprintf("%s: sampling upper bound %s does not prove deadline %s", id, n.ElapsedUpper, n.Limit)] = true
			}
		}
		result.Nodes = append(result.Nodes, *n)
	}
	for m := range missing {
		result.Unavailable = append(result.Unavailable, m)
	}
	sort.Strings(result.Unavailable)
	for v := range violations {
		result.Violations = append(result.Violations, v)
	}
	sort.Strings(result.Violations)
	result.Pass = len(nodes) > 0 && len(missing) == 0 && len(violations) == 0
	return result
}
