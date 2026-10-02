// SPDX-License-Identifier: FSL-1.1-ALv2

package scaling

import (
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/invariants"
)

// PoolState is the planner's memory of one pool between Plan calls. It holds
// soft state only (idle timers, drains issued, launches in flight, backoff),
// all of which is reconstructed conservatively from observations after a
// controller restart, plus the Ledger, which the executor persists.
//
// A PoolState belongs to one pool loop; it is not safe for concurrent use.
type PoolState struct {
	pool   domain.PoolName
	ledger Ledger
	// lastLedger is the ledger value last handed out in a Decision.
	lastLedger Ledger

	vms     map[string]*vmRecord // by node ID
	intents map[string]*intent   // launches in flight, by token
	reuse   []uint64             // ledger seqs to reuse before Next (restart)
	ghosts  map[uint64]time.Time // seqs possibly in flight after a restart, until expiry
	// unseen: seqs whose launch succeeded but whose instance Describe has not
	// listed yet (eventual consistency). They hold Committed back, so that a
	// restart in that window still accounts for them, until the deadline.
	unseen map[uint64]time.Time

	// providerSeen is set once a provider observation succeeded since creation;
	// no launch happens before it (a restarted controller must see the fleet first).
	providerSeen bool

	capacity capacityState
	// startupStreak counts VMs that failed to register since the last
	// successful registration (probe circuit breaker).
	startupStreak int
	// typeCooldown / subnetCooldown: entry -> until (moved to the end of the preference list).
	typeCooldown   map[string]time.Time
	subnetCooldown map[string]time.Time
	// queueEmptySince: zero while any pool queue has queued work.
	queueEmptySince time.Time
	// noCapacitySince: queued work waits, no VM can serve it and none can be launched.
	noCapacitySince time.Time
	// lastFailQueues is when queued work was last failed.
	lastFailQueues time.Time
	// drainCleanup: own drains of unknown nodes, by node, first noticed at.
	drainCleanup map[string]time.Time
	// violations found while applying results, reported with the next decision.
	violations []invariants.Violation
}

type capacityState struct {
	failures int       // consecutive capacity failures
	since    time.Time // first failure of the streak
	reason   Reason    // last capacity failure kind (status)
	hold     Reason    // why launches are held until `until` (capacity kind or throttled)
	until    time.Time // no launch before this (backoff)
	throttle int       // consecutive throttles/ambiguous errors
}

// NewPoolState returns the state for a pool, starting from the persisted
// ledger (zero Ledger for a new pool or a lost annotation: a fresh epoch is
// drawn on the first decision).
func NewPoolState(pool domain.PoolName, ledger Ledger) *PoolState {
	return &PoolState{
		pool:           pool,
		ledger:         ledger,
		lastLedger:     ledger,
		vms:            map[string]*vmRecord{},
		intents:        map[string]*intent{},
		ghosts:         map[uint64]time.Time{},
		unseen:         map[uint64]time.Time{},
		typeCooldown:   map[string]time.Time{},
		subnetCooldown: map[string]time.Time{},
		drainCleanup:   map[string]time.Time{},
	}
}

// Ledger returns the current launch ledger.
func (s *PoolState) Ledger() Ledger { return s.ledger }

// Pool returns the pool name.
func (s *PoolState) Pool() domain.PoolName { return s.pool }
