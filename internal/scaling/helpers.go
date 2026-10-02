// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
)

// ---------------------------------------------------------- shared queues

// ShareInput describes one pool competing for queues that several pools
// serve (for example the macOS "generic" runner, identical in every macOS pool).
type ShareInput struct {
	Pool domain.PoolName
	// Queues the pool serves.
	Queues []domain.QueueKey
	// Eligible and Headroom come from the pool's previous decision
	// (Decision.Status.Eligible, Decision.ThreadHeadroom).
	Eligible bool
	Headroom map[string]int
}

// AttributeShared splits the queued operations of queues served by more than
// one pool (contracts §3). Pools are considered in lexical name order; each
// eligible pool takes up to its thread headroom for the queue's platform and
// the last eligible pool takes the remainder. With no eligible pool the first
// pool takes everything (it then fails the work fast). The result maps each
// pool to its share per shared queue (Observation.QueuedShare); queues served
// by a single pool are absent (that pool takes all their work).
func AttributeShared(queues []domain.QueueObservation, pools []ShareInput) map[domain.PoolName]map[domain.QueueKey]int {
	ps := slices.Clone(pools)
	slices.SortFunc(ps, func(a, b ShareInput) int { return compareStrings(string(a.Pool), string(b.Pool)) })
	servers := map[domain.QueueKey][]int{}
	for i, p := range ps {
		for _, q := range p.Queues {
			if !slices.Contains(servers[q], i) {
				servers[q] = append(servers[q], i)
			}
		}
	}
	left := make([]map[string]int, len(ps))
	for i, p := range ps {
		left[i] = maps.Clone(p.Headroom)
		if left[i] == nil {
			left[i] = map[string]int{}
		}
	}
	out := map[domain.PoolName]map[domain.QueueKey]int{}
	set := func(i int, q domain.QueueKey, n int) {
		m := out[ps[i].Pool]
		if m == nil {
			m = map[domain.QueueKey]int{}
			out[ps[i].Pool] = m
		}
		m[q] += n
	}
	qs := slices.Clone(queues)
	slices.SortFunc(qs, func(a, b domain.QueueObservation) int { return queueLess(a.Key, b.Key) })
	for _, q := range qs {
		srv := servers[q.Key]
		if len(srv) < 2 {
			continue
		}
		var eligible []int
		for _, i := range srv {
			set(i, q.Key, 0)
			if ps[i].Eligible {
				eligible = append(eligible, i)
			}
		}
		remaining := max(q.Queued, 0)
		if len(eligible) == 0 {
			set(srv[0], q.Key, remaining)
			continue
		}
		for k, i := range eligible {
			take := remaining
			if k < len(eligible)-1 {
				take = min(remaining, max(left[i][q.Key.PlatformKey], 0))
			}
			left[i][q.Key.PlatformKey] -= take
			set(i, q.Key, take)
			remaining -= take
		}
	}
	return out
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ----------------------------------------------------------- validation

// ValidateSpec fails fast on a pool the autoscaler cannot run safely
// (R-TEST-7), including dead-man consistency (R-POOL-7): the controller's idle
// policy must be strictly tighter than the worker's own dead-man idle limit,
// and a VM recycled before its maximum uptime must be able to finish its drain
// before the dead-man switch fires.
func ValidateSpec(spec Spec, cfg Config) error {
	cfg = cfg.withDefaults()
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if spec.Name == "" {
		add("pool name is empty")
	}
	if spec.Provider != domain.ProviderEC2 && spec.Provider != domain.ProviderTart {
		add("provider %q is neither ec2 nor tart", spec.Provider)
	}
	if spec.Max < 0 || spec.MinRunning < 0 || spec.MinRunning > spec.Max {
		add("capacity must satisfy 0 <= minRunning (%d) <= max (%d)", spec.MinRunning, spec.Max)
	}
	if len(spec.Runners) == 0 {
		add("pool has no runners")
	}
	for _, r := range spec.Runners {
		if r.Concurrency <= 0 {
			add("runner %q has concurrency %d (must be > 0)", r.Name, r.Concurrency)
		}
	}
	if spec.Generation == "" {
		add("generation is empty")
	}
	if spec.Provider == domain.ProviderEC2 && len(spec.InstanceTypes) == 0 {
		add("ec2 pool has no instance types")
	}
	if spec.IdleTimeout <= 0 || spec.StartupTimeout <= 0 || spec.DrainTimeout <= 0 {
		add("idleTimeout (%s), startupTimeout (%s) and drainTimeout (%s) must be > 0", spec.IdleTimeout, spec.StartupTimeout, spec.DrainTimeout)
	}
	// Scale-in of an idle VM takes idleTimeout, then drain + confirm + terminate (two polls) plus slack.
	margin := 2*cfg.PollInterval + time.Minute
	if spec.IdleTimeout+margin >= cfg.Deadman.IdleLimit {
		add("idleTimeout %s + %s margin must be below the worker dead-man idle limit %s (R-POOL-7)", spec.IdleTimeout, margin, cfg.Deadman.IdleLimit)
	}
	if cfg.RecycleMargin < spec.DrainTimeout+margin {
		add("recycle margin %s must cover drainTimeout %s + %s so recycled VMs finish before the dead-man max uptime", cfg.RecycleMargin, spec.DrainTimeout, margin)
	}
	if cfg.Deadman.MaxUptime <= cfg.RecycleMargin+spec.StartupTimeout {
		add("dead-man max uptime %s leaves no useful life after startup (%s) and recycle margin (%s)", cfg.Deadman.MaxUptime, spec.StartupTimeout, cfg.RecycleMargin)
	}
	for _, w := range spec.Floors {
		if w.MinRunning > spec.Max {
			add("floor window %q minRunning %d exceeds max %d", w.Name, w.MinRunning, spec.Max)
		}
	}
	if spec.DailyInstanceHourCap < 0 {
		add("dailyInstanceHourCap %g < 0", spec.DailyInstanceHourCap)
	}
	return errors.Join(errs...)
}

// ------------------------------------------------------------ shadow mode

// DecisionDiff summarizes how a candidate policy's decision differs from the
// current one for the same observation (shadow mode, R-TEST-7).
type DecisionDiff struct {
	Desired    [2]int
	Launches   [2]int
	Drains     [2]int
	Stops      [2]int
	FailQueues [2]bool
	Hold       [2]Reason
}

// Empty reports identical decisions (as far as the summary goes).
func (d DecisionDiff) Empty() bool {
	return d.Desired[0] == d.Desired[1] && d.Launches[0] == d.Launches[1] && d.Drains[0] == d.Drains[1] &&
		d.Stops[0] == d.Stops[1] && d.FailQueues[0] == d.FailQueues[1] && d.Hold[0] == d.Hold[1]
}

func (d DecisionDiff) String() string {
	return fmt.Sprintf("desired %d→%d launches %d→%d drains %d→%d stops %d→%d fail-queues %t→%t hold %q→%q",
		d.Desired[0], d.Desired[1], d.Launches[0], d.Launches[1], d.Drains[0], d.Drains[1],
		d.Stops[0], d.Stops[1], d.FailQueues[0], d.FailQueues[1], d.Hold[0], d.Hold[1])
}

// DiffDecisions compares the current decision with a shadow candidate. Run the
// candidate on st.Clone() taken before the current Plan, so both see the same
// state, and discard the clone (the candidate never acts).
func DiffDecisions(current, candidate Decision) DecisionDiff {
	var d DecisionDiff
	for i, dec := range []Decision{current, candidate} {
		d.Desired[i], d.Hold[i] = dec.Desired, dec.Hold
		for _, a := range dec.Actions {
			switch a.Kind {
			case ActLaunch:
				d.Launches[i]++
			case ActAddDrain:
				d.Drains[i] += len(a.VMs)
			case ActTerminate, ActStop:
				d.Stops[i] += len(a.VMs)
			case ActFailQueues:
				d.FailQueues[i] = true
			}
		}
	}
	return d
}

// Clone returns a deep copy (for shadow-mode planning).
func (s *PoolState) Clone() *PoolState {
	c := *s
	c.vms = make(map[string]*vmRecord, len(s.vms))
	for id, r := range s.vms {
		rc := *r
		c.vms[id] = &rc
	}
	c.intents = make(map[string]*intent, len(s.intents))
	for tok, in := range s.intents {
		ic := *in
		ic.types, ic.subnets = slices.Clone(in.types), slices.Clone(in.subnets)
		c.intents[tok] = &ic
	}
	c.reuse = slices.Clone(s.reuse)
	c.ghosts = maps.Clone(s.ghosts)
	c.unseen = maps.Clone(s.unseen)
	c.typeCooldown = maps.Clone(s.typeCooldown)
	c.subnetCooldown = maps.Clone(s.subnetCooldown)
	c.drainCleanup = maps.Clone(s.drainCleanup)
	c.violations = slices.Clone(s.violations)
	return &c
}

// ------------------------------------------------------------ token bucket

// TokenBucket is a pure token bucket for EC2 API hygiene (R-SCALE-6, e.g.
// RunInstances: burst 5, refill 2/s). It holds no lock: share one per account
// and region across pool loops behind a mutex.
type TokenBucket struct {
	Burst  float64
	Rate   float64 // tokens per second
	tokens float64
	last   time.Time
}

// NewTokenBucket returns a full bucket.
func NewTokenBucket(burst int, ratePerSecond float64, now time.Time) TokenBucket {
	return TokenBucket{Burst: float64(burst), Rate: ratePerSecond, tokens: float64(burst), last: now}
}

func (b *TokenBucket) refill(now time.Time) {
	if now.After(b.last) {
		b.tokens = min(b.Burst, b.tokens+now.Sub(b.last).Seconds()*b.Rate)
		b.last = now
	}
}

// Available returns the whole tokens available at now.
func (b *TokenBucket) Available(now time.Time) int {
	b.refill(now)
	return int(b.tokens)
}

// Take removes up to n tokens and returns how many were taken.
func (b *TokenBucket) Take(n int, now time.Time) int {
	b.refill(now)
	k := min(n, int(b.tokens))
	if k > 0 {
		b.tokens -= float64(k)
	}
	return max(k, 0)
}

// Budget returns the launch budget for an observation.
func (b *TokenBucket) Budget(now time.Time) Budget {
	return Budget{Limited: true, Launches: b.Available(now)}
}
