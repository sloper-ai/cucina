// SPDX-License-Identifier: FSL-1.1-ALv2

// Package domain holds the pure data types shared by the controller core,
// the providers (EC2, Tart) and the fakes. It has no dependencies beyond the
// standard library, so every tier of test can use it.
package domain

import (
	"sort"
	"strings"
	"time"
)

// Provider enumerates worker providers.
type Provider string

const (
	ProviderEC2  Provider = "ec2"
	ProviderTart Provider = "tart"
)

// PoolName is the name of a WorkerPool. It is also the value of the "pool"
// worker-id label, of the cucina:pool tag and of the cucina_pool metric label.
type PoolName string

// Tag/label keys used on every cloud resource the controller creates (R-POOL-2).
// Extra operator tags (for example the campaign tags cucina:env, cucina:run,
// cucina:expires) are passed through verbatim from the controller configuration.
const (
	TagManagedBy    = "cucina:managed-by" // always "cucina-controller"
	TagCluster      = "cucina:cluster"    // controller installation ID
	TagPool         = "cucina:pool"
	TagGeneration   = "cucina:generation"
	TagImageVersion = "cucina:image-version"
	TagLaunchToken  = "cucina:launch-token"
	TagRole         = "cucina:role" // "worker"
	ManagedByValue  = "cucina-controller"
)

// Well-known worker-id labels (R-RE-4). Buildbarn adds "thread" itself.
const (
	LabelPool = "pool"
	LabelNode = "node"
)

// RolloutPolicy controls how old-generation workers are replaced (R-OPS-2).
type RolloutPolicy string

const (
	RolloutLazy  RolloutPolicy = "lazy"  // old workers finish and are replaced when idle
	RolloutEager RolloutPolicy = "eager" // old workers are drained and replaced immediately
)

// Runner is one bb_runner platform advertised by every worker in a pool.
type Runner struct {
	Name        string            // e.g. "native", "qemu-rv64g", "xcode", "generic"
	Properties  map[string]string // exact REAPI platform property set
	Concurrency int               // execution slots (bb_worker threads) for this runner on one VM
}

// PropertiesKey returns a canonical key for a platform property set. Buildbarn
// matches the entire property set exactly, so equal keys mean the same queue.
func PropertiesKey(p map[string]string) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(p[k])
	}
	return b.String()
}

// QueueKey identifies one Buildbarn size class queue.
type QueueKey struct {
	InstanceNamePrefix string
	PlatformKey        string // PropertiesKey of the platform properties
	SizeClass          uint32
}

// PoolSpec is the resolved, provider-neutral description of a pool: the
// WorkerPool resource merged with its entry in platforms/pools.json.
type PoolSpec struct {
	Name           PoolName
	Provider       Provider
	Platform       string // pools.json platform name, e.g. "linux-x86-64"
	SizeClass      uint32 // Buildbarn size class (uint32)
	InstanceNames  []string
	Runners        []Runner
	MinRunning     int // 0 unless the operator opted in to standing cost
	Max            int
	IdleTimeout    time.Duration
	DrainTimeout   time.Duration
	StartupTimeout time.Duration
	Generation     string // current image generation; changes on image rollout
	Rollout        RolloutPolicy
	Paused         bool // cordoned: no new launches
}

// SlotsPerVM is the number of execution slots one VM of the pool offers on the
// given runner, 0 if the pool has no such runner.
func (p PoolSpec) SlotsPerVM(runner string) int {
	for _, r := range p.Runners {
		if r.Name == runner {
			return r.Concurrency
		}
	}
	return 0
}

// VMState is the controller's view of one worker VM's lifecycle. The state
// machine lives in internal/scaling; the values are shared with metrics and the
// management API.
type VMState string

const (
	VMLaunching   VMState = "launching"   // API call accepted, instance pending/booting, not yet registered
	VMRegistered  VMState = "registered"  // registered with the scheduler (at least one runner thread seen)
	VMDraining    VMState = "draining"    // AddDrain issued, waiting for running actions to finish
	VMStopping    VMState = "stopping"    // terminate/stop issued
	VMStopped     VMState = "stopped"     // tart only: shut down, disk kept
	VMTerminated  VMState = "terminated"  // gone (EC2) — terminal
	VMFailed      VMState = "failed"      // did not register within startupTimeout, or provider error
	VMUnavailable VMState = "unavailable" // host offline (tart)
)

// VM is one worker VM (EC2 instance or Tart VM) as observed.
type VM struct {
	ID         string // EC2 instance ID, or "<host>/<vm>" for Tart (the "node" label value)
	Pool       PoolName
	Generation string
	State      VMState
	LaunchedAt time.Time
	// RegisteredAt is zero until the first runner thread of the VM appears in the scheduler.
	RegisteredAt time.Time
	// IdleSince is zero while any thread of the VM runs an operation.
	IdleSince    time.Time
	Threads      int // runner threads currently known to the scheduler
	Busy         int // threads currently executing an operation
	Drained      bool
	InstanceType string // EC2 only
	Host         string // Tart only
}

// QueueObservation is what the scheduler reports for one size class queue
// (ListPlatformQueues): queued operations and worker counts (one "worker" is one
// runner thread, R-RE-4).
type QueueObservation struct {
	Key       QueueKey
	Queued    int
	Executing int // executing threads
	Idle      int // idle threads (incl. idle-synchronizing)
	Workers   int // total threads
	Drains    int
}

// Observation is the complete input of one autoscaler decision for one pool.
type Observation struct {
	Now    time.Time
	VMs    []VM
	Queues []QueueObservation // queues served by this pool's runners
}
