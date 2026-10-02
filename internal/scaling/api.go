// SPDX-License-Identifier: FSL-1.1-ALv2

// Package scaling is Cucina's autoscaler core (R-SCALE-1..8): one pure,
// deterministic decision function per pool plus the per-VM lifecycle state
// machine (launching → registered → draining → stopping → terminated, failed,
// and the Tart states stopped/unavailable).
//
// The package does no I/O, starts no goroutines, never reads the wall clock and
// draws randomness only from the injected ports.Rand (backoff jitter, ledger
// epochs), so every decision is reproducible from (Config, seed, inputs)
// (R-TEST-8c). The caller (internal/reconcile) drives one loop per pool:
//
//	st := scaling.NewPoolState(pool, persistedLedger)     // once per pool and leader term
//	for every poll (1–2 s, R-SCALE-1) {
//	    obs := observe()                                   // scheduler + provider + results of the last actions
//	    d := planner.Plan(spec, obs, st)                   // pure
//	    if d.LedgerChanged { persist(d.Ledger) }           // write-ahead, before any launch
//	    results := execute(d.Actions)                      // one Result per Action, ErrSkipped if not run
//	    // results go into the next obs.Results
//	}
//
// See docs/dev/scaling.md for the policy, the state machine and the restart
// (state reconstruction) design.
package scaling

import (
	"errors"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/invariants"
)

// ----------------------------------------------------------------- inputs

// Config is the controller-wide autoscaler tuning (config.Autoscaler and
// config.Scheduler). Zero fields take the defaults of DefaultConfig.
type Config struct {
	// PollInterval is the observation period (R-SCALE-1: 1–2 s).
	PollInterval time.Duration
	// QueueFailAfter: a pool with queued work, no workers and no way to obtain
	// capacity for this long has its queued work failed (R-RE-2, R-SCALE-4).
	QueueFailAfter time.Duration
	// BackoffMin/BackoffMax bound the jittered exponential backoff after capacity
	// errors (ICE, quota, missing image, permanent request errors).
	BackoffMin, BackoffMax time.Duration
	// ThrottleBackoffMin/Max bound the backoff after API throttling and ambiguous
	// (retryable) launch errors.
	ThrottleBackoffMin, ThrottleBackoffMax time.Duration
	// CapacityCooldown is how long an instance type that just failed with ICE is
	// moved to the end of the preference list (instance-type rotation).
	CapacityCooldown time.Duration
	// ConsistencyGrace is how long a launch whose instance is not (yet) visible
	// in Describe still counts as pending capacity (EC2 eventual consistency).
	ConsistencyGrace time.Duration
	// Deadman are the worker-side dead-man limits (R-POOL-7). The controller's
	// idle policy must be strictly tighter (see ValidateSpec).
	Deadman Deadman
	// RecycleMargin: VMs older than Deadman.MaxUptime-RecycleMargin are drained
	// so that the dead-man switch never kills a busy VM.
	RecycleMargin time.Duration
}

// Deadman are the limits after which a worker powers itself off (R-POOL-7).
type Deadman struct {
	IdleLimit        time.Duration // default 30m
	UnreachableLimit time.Duration // default 10m
	MaxUptime        time.Duration // default 12h
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		PollInterval:       time.Second,
		QueueFailAfter:     10 * time.Minute,
		BackoffMin:         10 * time.Second,
		BackoffMax:         5 * time.Minute,
		ThrottleBackoffMin: time.Second,
		ThrottleBackoffMax: 30 * time.Second,
		CapacityCooldown:   10 * time.Minute,
		ConsistencyGrace:   2 * time.Minute,
		Deadman:            Deadman{IdleLimit: 30 * time.Minute, UnreachableLimit: 10 * time.Minute, MaxUptime: 12 * time.Hour},
		RecycleMargin:      45 * time.Minute,
	}
}

// Spec is the resolved pool (domain.PoolSpec) plus the parameters only the
// autoscaler needs. internal/pools resolves it from the WorkerPool resource,
// the platform catalog and the defaults.
type Spec struct {
	domain.PoolSpec

	// ClusterID is the controller installation ID (tag cucina:cluster); it is
	// part of every launch token.
	ClusterID string
	// VCPUs is N in the scale-out policy: the CPUs one VM offers, shared by all
	// runners. 0 means the largest runner concurrency.
	VCPUs int
	// InstanceTypes and SubnetIDs are the ordered EC2 preference lists; the
	// planner rotates instance types that just failed with ICE to the end.
	InstanceTypes []string
	SubnetIDs     []string
	// Floors raise minRunning during recurring windows (R-SCALE-7, opt-in standing cost).
	Floors []FloorWindow
	// DailyInstanceHourCap stops new launches once the pool consumed this many
	// instance-hours since 00:00 UTC (R-SCALE-7). 0 = no cap.
	DailyInstanceHourCap float64
	// Deleting retires the pool (WorkerPool finalizer, R-OPS-3): launch nothing,
	// drain every VM, stop each when idle (or when its drain timeout expired) and
	// fail queued work that no other pool can serve. Decision.Status.Empty
	// reports when the finalizer may be removed.
	Deleting bool
}

// FloorWindow raises the pool floor during a recurring UTC window (R-SCALE-7).
// Use ParseFloorWindow to build one from the WorkerPool fields.
type FloorWindow struct {
	Name string
	// Days are the weekdays on which the window starts.
	Days []time.Weekday
	// Start and End are offsets from 00:00 UTC. End <= Start means the window
	// crosses midnight and ends on the following day.
	Start, End time.Duration
	MinRunning int
}

// Budget is the EC2 API budget available to this decision (R-SCALE-6): the
// number of RunInstances calls the shared token bucket allows right now.
type Budget struct {
	// Limited is false when no bucket applies (Tart pools, tests).
	Limited bool
	// Launches is the number of launch calls allowed now when Limited.
	Launches int
}

// Observation is everything that happened since the previous Plan for one
// pool. Fields marked "Known" say whether the corresponding source answered
// this round; when a source is unknown the planner keeps the previous view
// and only takes actions that are safe without it.
type Observation struct {
	Now time.Time

	// Queues are the scheduler's size class queues of this pool's runners
	// (ListPlatformQueues filtered to the pool's instance names × runner
	// platforms × size class). A pool queue missing from the list is not
	// declared (ADR 0002).
	Queues      []domain.QueueObservation
	QueuesKnown bool
	// QueuedShare overrides QueueObservation.Queued for queues shared with other
	// pools (computed by AttributeShared). nil means all queued work is this pool's.
	QueuedShare map[domain.QueueKey]int

	// Workers are the runner threads whose worker ID has pool=<this pool>
	// (ListWorkers over the pool's queues). Threads are grouped into VMs by the
	// "node" label (R-RE-4).
	Workers      []ports.Worker
	WorkersKnown bool
	// Drains are the drains registered on the pool's queues (ListDrains). The
	// planner manages only drains whose pattern is exactly {pool, node}; any
	// other pattern (operator drains use {node}) is left alone.
	Drains []ports.Drain

	// Instances are this pool's EC2 instances from a tag-filtered Describe
	// (cluster + pool tags) including shutting-down and terminated ones.
	Instances []ports.Instance
	// Hosts are the Mac hosts (HostFleet.Hosts); the planner uses the VMs whose
	// Pool is this pool and the hosts' online/cordon state.
	Hosts []ports.HostState
	// ProviderKnown is true when Describe (EC2) or Hosts (Tart) answered.
	ProviderKnown bool
	// TartFreeSlots is the number of VMs of this pool the placement helper could
	// start right now on eligible hosts (R-POOL-6). Ignored for EC2 pools.
	TartFreeSlots int

	// ImageErr is the result of resolving the pool's image (nil = resolved).
	// errors.Is(ImageErr, ports.ErrImageNotFound) fails queued work fast.
	ImageErr error
	// LaunchBudget is the shared EC2 API budget (R-SCALE-6).
	LaunchBudget Budget
	// InstanceSecondsToday is the pool's consumption since 00:00 UTC (cost
	// accounting), used for DailyInstanceHourCap.
	InstanceSecondsToday float64

	// Results are the outcomes of the actions of previous decisions, one per
	// action. Actions the executor did not run must be reported with ErrSkipped.
	Results []Result
}

// Result is the outcome of executing one Action.
type Result struct {
	Action Action
	// Err is nil on success, ErrSkipped when the executor did not run the
	// action, otherwise the provider error (wrapped ports sentinels).
	Err error
	// Instance is the launched instance (EC2 launch success).
	Instance *ports.Instance
	// VM is the node ID of the VM a Tart launch started ("<host>/<vm>").
	VM string
	// PerVM holds per-node errors of batched calls (terminate, stop, drains);
	// nodes missing from the map succeeded.
	PerVM map[string]error
	// At is when the call completed.
	At time.Time
}

// ErrSkipped reports an action the executor did not run (for example because
// the shared API budget was exhausted, or the leader lost its lease).
var ErrSkipped = errors.New("scaling: action not executed")

// ---------------------------------------------------------------- outputs

// ActionKind is what the executor must do. Values are the `action` label of
// cucina_scale_decisions_total.
type ActionKind string

const (
	// ActLaunch starts one VM: EC2 Compute.Launch with Launch.Token as the
	// idempotency token (and the cucina:launch-token tag); Tart HostFleet.StartVM
	// on a host chosen by placement (preferring a stopped VM of the pool).
	ActLaunch ActionKind = "launch"
	// ActAddDrain registers the drain {pool, node} on every queue of the pool.
	ActAddDrain ActionKind = "drain"
	// ActUndrain removes the drain of a draining VM because demand returned
	// before it stopped (cheaper than a cold start).
	ActUndrain ActionKind = "undrain"
	// ActRemoveDrain removes a drain after the VM is gone (EC2: right after
	// termination) or when a stopped Tart VM starts again (R-SCALE-3 step 4).
	ActRemoveDrain ActionKind = "remove-drain"
	// ActTerminate terminates EC2 instances (batched; never stop).
	ActTerminate ActionKind = "terminate"
	// ActStop gracefully stops Tart VMs (tart stop --timeout), keeping the disk.
	ActStop ActionKind = "stop"
	// ActFailQueues fails the queued work of queues without workers
	// (KillOperations{size_class_queue_without_workers}, R-RE-2).
	ActFailQueues ActionKind = "fail-queues"
)

// Reason explains an action, a hold or a condition. The stop reasons are the
// `reason` label of cucina_vm_stops_total.
type Reason string

// Stop reasons (cucina_vm_stops_total{reason}).
const (
	StopIdle           Reason = "idle"            // idle for idleTimeout with empty queues (R-SCALE-3)
	StopDrain          Reason = "drain"           // drained by an operator (cucinactl), stopped once idle
	StopRollout        Reason = "rollout"         // old image generation replaced (R-OPS-2)
	StopStartupTimeout Reason = "startup-timeout" // not registered within startupTimeout (R-SCALE-2)
	StopDeadman        Reason = "deadman"         // recycled before the worker's dead-man max uptime (R-POOL-7)
	StopMaintenance    Reason = "maintenance"     // its Mac host was cordoned (R-MAC-6)
	StopRetire         Reason = "retire"          // pool deleted or max lowered below the active VMs
	StopExternal       Reason = "external"        // vanished without a controller decision (dead-man switch, spot, operator)
)

// Launch, rescue, cleanup and hold reasons.
const (
	ReasonDemand  Reason = "demand"  // queued + executing work
	ReasonFloor   Reason = "floor"   // minRunning' (spec.minRunning or an active floor window)
	ReasonRescue  Reason = "rescue"  // undrain: demand returned while the VM was draining for idleness
	ReasonCleanup Reason = "cleanup" // remove-drain of a gone/restarted VM

	ReasonMaxZero          Reason = "max-zero"
	ReasonPaused           Reason = "paused"
	ReasonDeleting         Reason = "deleting"
	ReasonImageMissing     Reason = "image-missing"
	ReasonQueueNotDeclared Reason = "queue-not-declared"
	ReasonNoCapacity       Reason = "no-capacity" // capacity errors persisted beyond QueueFailAfter
	ReasonCapacity         Reason = "capacity"    // ICE / quota backoff in progress
	// ReasonStartupFailures: launched VMs fail to register within startupTimeout
	// (probe mode: one VM starting at a time, ADR 0502).
	ReasonStartupFailures Reason = "startup-failures"
	// ReasonLaunchError: the provider rejected a launch permanently
	// (ports.ErrInvalid) or a Tart start failed.
	ReasonLaunchError Reason = "launch-error"
	ReasonThrottled   Reason = "throttled"
	ReasonDailyCap    Reason = "daily-cap"
	ReasonAPIBudget   Reason = "api-budget"
	ReasonAtMax       Reason = "at-max"
	ReasonNoHostSlots Reason = "no-host-slots"
	ReasonIncomplete  Reason = "observation-incomplete"
)

// StatusFailedPrecondition is the gRPC status code (FAILED_PRECONDITION) used
// to fail queued work fast: Bazel reports it immediately instead of retrying.
const StatusFailedPrecondition int32 = 9

// LaunchIntent parameterizes one ActLaunch.
type LaunchIntent struct {
	// Token is the idempotency token (EC2 ClientToken, tag cucina:launch-token).
	// A retry of the same intent reuses the token, so a restarted or
	// re-executed launch never creates a second instance (R-SCALE-5).
	Token string
	// Seq is the token's position in the pool's launch ledger.
	Seq uint64
	// Generation is the image generation to launch (spec.Generation).
	Generation string
	// InstanceTypes and SubnetIDs are the preference lists for this attempt
	// (types that recently failed with ICE come last).
	InstanceTypes []string
	SubnetIDs     []string
}

// Action is one step for the executor. Actions are ordered; execute them in
// order (batching within a kind is fine).
type Action struct {
	Kind   ActionKind
	Reason Reason
	// VMs are node IDs: EC2 instance IDs or "<host>/<vm>" (drain, undrain,
	// remove-drain, terminate, stop).
	VMs []string
	// Launch is set for ActLaunch.
	Launch *LaunchIntent
	// Queues are the queues to fail (ActFailQueues).
	Queues []domain.QueueKey
	// Code and Message complete the failed operations (ActFailQueues).
	Code    int32
	Message string
}

// Demand explains how Desired was computed (contracts §3):
// desired = clamp(max(ceil(ΣD_r/N), max_r ceil(D_r/S_r)), minRunning', max).
type Demand struct {
	// PerRunner is D_r = queued_r + executing_r per runner name.
	PerRunner map[string]int
	Queued    int // Σ queued attributed to the pool
	Executing int // Σ executing threads of the pool's VMs
	ByTotal   int // ceil(ΣD_r / N)
	ByRunner  int // max_r ceil(D_r / S_r)
	Floor     int // minRunning'
	Max       int
	// Capacity is the VMs counted against Desired: registered (not draining)
	// plus launching plus launches in flight.
	Capacity int
}

// Status feeds WorkerPool.status and its conditions.
type Status struct {
	QueueDeclared     bool
	ImageResolved     bool
	CapacityAvailable bool
	// CapacityFailure is the last capacity problem (ReasonCapacity,
	// ReasonNoCapacity, ReasonImageMissing, ReasonThrottled, …) and
	// CapacityFailingSince when it started (zero when healthy).
	CapacityFailure      Reason
	CapacityFailingSince time.Time
	// Eligible is true when the pool could launch now (input of AttributeShared).
	Eligible bool
	// Empty is true when no VM of the pool is alive and no launch is in flight
	// (with Spec.Deleting: the finalizer may be removed).
	Empty bool
	// OldGenerationVMs counts alive VMs of a previous generation.
	OldGenerationVMs int
}

// Ledger is the durable part of PoolState: the launch-token sequence. The
// executor persists it (WorkerPool annotation) whenever Decision.LedgerChanged
// is set, BEFORE executing the decision's launches (write-ahead). A restarted
// controller passes the persisted value to NewPoolState and reuses exactly the
// tokens that may still be in flight, so EC2 deduplicates them (R-SCALE-5).
type Ledger struct {
	// Epoch is a random label fixed when the ledger is created; tokens of a
	// lost or recreated ledger can never collide with older ones.
	Epoch string `json:"epoch"`
	// Committed: every seq below it has a known outcome.
	Committed uint64 `json:"committed"`
	// Next is the next unused seq; seqs in [Committed, Next) may be in flight.
	Next uint64 `json:"next"`
	// At is when Next was last raised (bounds how long in-flight seqs count as
	// pending capacity, Config.ConsistencyGrace).
	At time.Time `json:"at"`
}

// Decision is the output of one Plan call.
type Decision struct {
	Pool domain.PoolName
	Now  time.Time
	// Desired is the number of VMs the pool should have (clamped).
	Desired int
	Demand  Demand
	// Actions to execute, in order.
	Actions []Action
	// Hold says why fewer VMs were launched than Desired-Capacity ("" if none).
	Hold Reason
	// VMs is the reconstructed per-VM view (sorted by ID): status counts and
	// cucina_pool_vms{state}. Terminated VMs are omitted.
	VMs    []domain.VM
	Status Status
	// Ledger is the launch ledger after this decision; persist it before
	// executing launches when LedgerChanged.
	Ledger        Ledger
	LedgerChanged bool
	// ThreadHeadroom is, per runner platform key, how many more threads the
	// pool could offer (free threads + (max-active)·slots); AttributeShared input.
	ThreadHeadroom map[string]int
	// Violations are invariant violations found while planning; the violating
	// actions were removed from Actions. Report them (invariants.Report).
	Violations []invariants.Violation
}

// --------------------------------------------------------------- planner

// Planner computes decisions. It is safe to share between pools only from a
// single goroutine (the injected Rand is not synchronized); use one Planner
// per pool loop or a synchronized Rand.
type Planner struct {
	cfg Config
	rnd ports.Rand
}

// NewPlanner returns a planner with cfg (zero fields defaulted) drawing
// jitter and ledger epochs from rnd.
func NewPlanner(cfg Config, rnd ports.Rand) *Planner {
	return &Planner{cfg: cfg.withDefaults(), rnd: rnd}
}

// Config returns the effective configuration.
func (p *Planner) Config() Config { return p.cfg }

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.PollInterval <= 0 {
		c.PollInterval = d.PollInterval
	}
	if c.QueueFailAfter <= 0 {
		c.QueueFailAfter = d.QueueFailAfter
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = d.BackoffMin
	}
	if c.BackoffMax < c.BackoffMin {
		c.BackoffMax = max(d.BackoffMax, c.BackoffMin)
	}
	if c.ThrottleBackoffMin <= 0 {
		c.ThrottleBackoffMin = d.ThrottleBackoffMin
	}
	if c.ThrottleBackoffMax < c.ThrottleBackoffMin {
		c.ThrottleBackoffMax = max(d.ThrottleBackoffMax, c.ThrottleBackoffMin)
	}
	if c.CapacityCooldown <= 0 {
		c.CapacityCooldown = d.CapacityCooldown
	}
	if c.ConsistencyGrace <= 0 {
		c.ConsistencyGrace = d.ConsistencyGrace
	}
	if c.Deadman.IdleLimit <= 0 {
		c.Deadman.IdleLimit = d.Deadman.IdleLimit
	}
	if c.Deadman.UnreachableLimit <= 0 {
		c.Deadman.UnreachableLimit = d.Deadman.UnreachableLimit
	}
	if c.Deadman.MaxUptime <= 0 {
		c.Deadman.MaxUptime = d.Deadman.MaxUptime
	}
	if c.RecycleMargin <= 0 {
		c.RecycleMargin = d.RecycleMargin
	}
	return c
}
