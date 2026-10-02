// SPDX-License-Identifier: FSL-1.1-ALv2

package fakes

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// BuildQueueConfig shapes the simulated bb_scheduler.
type BuildQueueConfig struct {
	// NoWorkersTimeout removes a non-predeclared queue that has had no workers
	// for this long (platformQueueWithNoWorkersTimeout, 900 s in Cucina).
	NoWorkersTimeout time.Duration
	// ReconnectDelay: after Restart, the workers that were connected re-register after this long.
	ReconnectDelay time.Duration
}

// DefaultBuildQueueConfig matches the rendered scheduler configuration.
func DefaultBuildQueueConfig() BuildQueueConfig {
	return BuildQueueConfig{NoWorkersTimeout: 900 * time.Second, ReconnectDelay: 5 * time.Second}
}

// Operation stages.
const (
	StageQueued    = "queued"
	StageExecuting = "executing"
	StageCompleted = "completed"
)

// Op is the ground-truth record of one operation in the fake scheduler.
type Op struct {
	Name         string
	Queue        domain.QueueKey
	Duration     time.Duration
	Tag          string // workload label (scenario bookkeeping)
	InvocationID string
	Stage        string
	SubmittedAt  time.Time
	FirstStarted time.Time // first start (queue wait = FirstStarted - SubmittedAt)
	StartedAt    time.Time // last start (after requeues)
	CompletedAt  time.Time
	Worker       ports.WorkerID
	Requeues     int    // times it was requeued after its worker vanished
	Failed       bool   // completed with an error status (killed, lost)
	Code         int32  // gRPC status code when Failed
	Message      string // status message when Failed
}

// Wait returns how long the operation waited before its first start; for an
// operation that never started, the time until it completed (failed) or until now.
func (o Op) Wait(now time.Time) time.Duration {
	switch {
	case !o.FirstStarted.IsZero():
		return o.FirstStarted.Sub(o.SubmittedAt)
	case !o.CompletedAt.IsZero():
		return o.CompletedAt.Sub(o.SubmittedAt)
	}
	return now.Sub(o.SubmittedAt)
}

type bqQueue struct {
	key         domain.QueueKey
	declared    bool
	queued      []*Op
	drains      []ports.Drain
	noWorkersAt time.Time // when the last thread left (zero while threads exist)
}

type bqThread struct {
	id     ports.WorkerID
	key    string
	queue  domain.QueueKey
	op     *Op
	doneAt time.Time
}

type reconnect struct {
	at      time.Time
	threads []bqThread
}

// BuildQueue is a fake bb_scheduler implementing ports.BuildQueue: predeclared
// queues, runner threads registered by VMs (hooks from the Compute/HostFleet
// fakes), FIFO dispatch to idle undrained threads, operations executing for
// their duration, drains, KillOperations and ErrQueueUnknown.
type BuildQueue struct {
	*Faults
	mu         sync.Mutex
	clock      ports.Clock
	cfg        BuildQueueConfig
	queues     map[domain.QueueKey]*bqQueue
	threads    map[string]*bqThread
	order      []string // sorted keys of threads
	ops        map[string]*Op
	opSeq      int
	now        time.Time
	reconnects []reconnect
}

var _ ports.BuildQueue = (*BuildQueue)(nil)

// NewBuildQueue returns an empty scheduler.
func NewBuildQueue(clock ports.Clock, rnd *Rand, cfg BuildQueueConfig) *BuildQueue {
	return &BuildQueue{Faults: newFaults(clock, rnd.Child("buildqueue-faults")), clock: clock, cfg: cfg,
		queues: map[domain.QueueKey]*bqQueue{}, threads: map[string]*bqThread{}, ops: map[string]*Op{}, now: clock.Now()}
}

// WorkerKey is the canonical string of a worker ID.
func WorkerKey(id ports.WorkerID) string {
	ks := make([]string, 0, len(id))
	for k := range id {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	for _, k := range ks {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(id[k])
		b.WriteByte(';')
	}
	return b.String()
}

// matches reports whether a drain pattern matches a worker ID (pattern ⊆ id).
func matches(pattern, id ports.WorkerID) bool {
	for k, v := range pattern {
		if id[k] != v {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------- controls

// Declare adds predeclared queues (predeclaredPlatformQueues, R-RE-2).
func (b *BuildQueue) Declare(keys ...domain.QueueKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range keys {
		q := b.queue(k)
		q.declared = true
	}
}

func (b *BuildQueue) queue(k domain.QueueKey) *bqQueue {
	q := b.queues[k]
	if q == nil {
		q = &bqQueue{key: k}
		b.queues[k] = q
	}
	return q
}

// Submit enqueues an operation that executes for d once started. It fails
// with ErrQueueUnknown (Buildbarn: FAILED_PRECONDITION "No workers exist")
// when the queue is neither declared nor served by a worker.
func (b *BuildQueue) Submit(key domain.QueueKey, d time.Duration, tag string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	b.advanceLocked(now)
	q := b.queues[key]
	if q == nil {
		return "", fmt.Errorf("no workers exist for instance name prefix %q platform %q: %w", key.InstanceNamePrefix, key.PlatformKey, ports.ErrQueueUnknown)
	}
	b.opSeq++
	op := &Op{Name: fmt.Sprintf("op-%06d", b.opSeq), Queue: key, Duration: d, Tag: tag, Stage: StageQueued, SubmittedAt: now}
	b.ops[op.Name] = op
	q.queued = append(q.queued, op)
	b.dispatchLocked(now)
	return op.Name, nil
}

// RegisterNode registers the runner threads of one VM: threads[q] threads on
// queue q, each with worker ID {pool, node, thread}.
func (b *BuildQueue) RegisterNode(pool domain.PoolName, node string, threads map[domain.QueueKey]int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	b.advanceLocked(now)
	b.registerLocked(pool, node, threads)
	b.dispatchLocked(now)
}

func (b *BuildQueue) registerLocked(pool domain.PoolName, node string, threads map[domain.QueueKey]int) {
	keys := slices.SortedFunc(maps.Keys(threads), func(a, c domain.QueueKey) int { return strings.Compare(qkString(a), qkString(c)) })
	n := 0
	for _, k := range keys {
		q := b.queue(k)
		q.noWorkersAt = time.Time{}
		for i := 0; i < threads[k]; i++ {
			id := ports.WorkerID{domain.LabelPool: string(pool), domain.LabelNode: node, "thread": fmt.Sprint(n)}
			n++
			key := WorkerKey(id) + "@" + qkString(k)
			if _, ok := b.threads[key]; !ok {
				b.addThread(&bqThread{id: id, key: key, queue: k})
			}
		}
	}
}

func (b *BuildQueue) addThread(t *bqThread) {
	b.threads[t.key] = t
	i, _ := slices.BinarySearch(b.order, t.key)
	b.order = slices.Insert(b.order, i, t.key)
}

func (b *BuildQueue) removeThread(key string) {
	delete(b.threads, key)
	if i, ok := slices.BinarySearch(b.order, key); ok {
		b.order = slices.Delete(b.order, i, i+1)
	}
}

// RemoveNode removes every thread of a node (VM terminated, crashed, shut
// down). Operations it was executing go back to the front of their queue.
func (b *BuildQueue) RemoveNode(node string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	b.advanceLocked(now)
	for _, key := range slices.Clone(b.order) {
		t := b.threads[key]
		if t.id[domain.LabelNode] != node {
			continue
		}
		if t.op != nil {
			t.op.Stage, t.op.Worker = StageQueued, nil
			t.op.Requeues++
			q := b.queue(t.queue)
			q.queued = append([]*Op{t.op}, q.queued...)
		}
		b.removeThread(key)
		b.markNoWorkers(t.queue, now)
	}
	b.dispatchLocked(now)
}

func (b *BuildQueue) markNoWorkers(k domain.QueueKey, now time.Time) {
	for _, t := range b.threads {
		if t.queue == k {
			return
		}
	}
	if q := b.queues[k]; q != nil && q.noWorkersAt.IsZero() {
		q.noWorkersAt = now
	}
}

// Restart simulates a scheduler restart (in-memory state lost): queued and
// executing operations fail (clients retry them), drains are forgotten, and
// connected workers re-register after ReconnectDelay.
func (b *BuildQueue) Restart() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	b.advanceLocked(now)
	var rc reconnect
	rc.at = now.Add(b.cfg.ReconnectDelay)
	for _, key := range b.order {
		t := b.threads[key]
		rc.threads = append(rc.threads, bqThread{id: maps.Clone(t.id), key: t.key, queue: t.queue})
	}
	b.reconnects = append(b.reconnects, rc)
	for _, name := range sortedKeys(b.ops) {
		op := b.ops[name]
		if op.Stage != StageCompleted {
			op.Stage, op.CompletedAt, op.Failed, op.Code, op.Message = StageCompleted, now, true, 14, "scheduler restarted"
		}
	}
	b.threads, b.order = map[string]*bqThread{}, nil
	for k, q := range b.queues {
		if !q.declared {
			delete(b.queues, k)
			continue
		}
		q.queued, q.drains, q.noWorkersAt = nil, nil, now
	}
}

// Tick advances the fake to the clock's time.
func (b *BuildQueue) Tick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advanceLocked(b.clock.Now())
}

// Ops returns every operation (ground truth), sorted by name.
func (b *BuildQueue) Ops() []Op {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advanceLocked(b.clock.Now())
	out := make([]Op, 0, len(b.ops))
	for _, name := range sortedKeys(b.ops) {
		o := *b.ops[name]
		o.Worker = maps.Clone(o.Worker)
		out = append(out, o)
	}
	return out
}

// Executing returns, per node, the number of threads executing an operation (ground truth).
func (b *BuildQueue) Executing() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advanceLocked(b.clock.Now())
	out := map[string]int{}
	for _, t := range b.threads {
		if t.op != nil {
			out[t.id[domain.LabelNode]]++
		}
	}
	return out
}

// Nodes returns the nodes that have at least one registered thread.
func (b *BuildQueue) Nodes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := map[string]bool{}
	for _, t := range b.threads {
		set[t.id[domain.LabelNode]] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// --------------------------------------------------------------- dispatch

func (b *BuildQueue) drained(q *bqQueue, id ports.WorkerID) bool {
	for _, d := range q.drains {
		if matches(d.Pattern, id) {
			return true
		}
	}
	return false
}

func (b *BuildQueue) dispatchLocked(now time.Time) {
	for _, key := range b.order {
		t := b.threads[key]
		if t.op != nil {
			continue
		}
		q := b.queues[t.queue]
		if q == nil || len(q.queued) == 0 || b.drained(q, t.id) {
			continue
		}
		op := q.queued[0]
		q.queued = q.queued[1:]
		op.Stage, op.StartedAt, op.Worker = StageExecuting, now, maps.Clone(t.id)
		if op.FirstStarted.IsZero() {
			op.FirstStarted = now
		}
		t.op, t.doneAt = op, now.Add(op.Duration)
	}
}

// advanceLocked completes operations in time order, dispatching queued work
// to the freed threads at each completion instant, then applies reconnects
// and queue expiry.
func (b *BuildQueue) advanceLocked(now time.Time) {
	if now.Before(b.now) {
		now = b.now
	}
	for {
		var next *bqThread
		for _, key := range b.order {
			t := b.threads[key]
			if t.op != nil && !t.doneAt.After(now) && (next == nil || t.doneAt.Before(next.doneAt)) {
				next = t
			}
		}
		var rc *reconnect
		for i := range b.reconnects {
			if !b.reconnects[i].at.After(now) && (rc == nil || b.reconnects[i].at.Before(rc.at)) {
				rc = &b.reconnects[i]
			}
		}
		switch {
		case rc != nil && (next == nil || !next.doneAt.Before(rc.at)):
			at := rc.at
			for _, t := range rc.threads {
				if _, ok := b.threads[t.key]; !ok {
					tt := t
					b.addThread(&tt)
					b.queue(t.queue).noWorkersAt = time.Time{}
				}
			}
			b.reconnects = slices.DeleteFunc(b.reconnects, func(r reconnect) bool { return r.at.Equal(at) })
			b.dispatchLocked(at)
		case next != nil:
			t := next
			op := t.op
			op.Stage, op.CompletedAt = StageCompleted, t.doneAt
			t.op = nil
			b.dispatchLocked(t.doneAt)
		default:
			b.now = now
			b.dispatchLocked(now)
			b.expireQueuesLocked(now)
			return
		}
	}
}

func (b *BuildQueue) expireQueuesLocked(now time.Time) {
	for k, q := range b.queues {
		if !q.declared && !q.noWorkersAt.IsZero() && now.Sub(q.noWorkersAt) >= b.cfg.NoWorkersTimeout {
			for _, op := range q.queued {
				op.Stage, op.CompletedAt, op.Failed, op.Code, op.Message = StageCompleted, now, true, 9, "no workers exist"
			}
			delete(b.queues, k)
		}
	}
}

func qkString(k domain.QueueKey) string {
	return fmt.Sprintf("%s|%s|%d", k.InstanceNamePrefix, k.PlatformKey, k.SizeClass)
}

// ------------------------------------------------------------- port methods

func (b *BuildQueue) begin(ctx context.Context, op string) (time.Time, error) {
	if err := b.enter(ctx, op); err != nil {
		return time.Time{}, err
	}
	b.mu.Lock()
	now := b.clock.Now()
	b.advanceLocked(now)
	return now, nil
}

// ListPlatformQueues implements ports.BuildQueue.
func (b *BuildQueue) ListPlatformQueues(ctx context.Context) ([]domain.QueueObservation, error) {
	if _, err := b.begin(ctx, "ListPlatformQueues"); err != nil {
		return nil, err
	}
	defer b.mu.Unlock()
	return b.queuesLocked(), nil
}

func (b *BuildQueue) queuesLocked() []domain.QueueObservation {
	keys := slices.SortedFunc(maps.Keys(b.queues), func(a, c domain.QueueKey) int { return strings.Compare(qkString(a), qkString(c)) })
	out := make([]domain.QueueObservation, 0, len(keys))
	for _, k := range keys {
		q := b.queues[k]
		o := domain.QueueObservation{Key: k, Queued: len(q.queued), Drains: len(q.drains)}
		for _, t := range b.threads {
			if t.queue != k {
				continue
			}
			o.Workers++
			if t.op != nil {
				o.Executing++
			} else {
				o.Idle++
			}
		}
		out = append(out, o)
	}
	return out
}

// ListWorkers implements ports.BuildQueue.
func (b *BuildQueue) ListWorkers(ctx context.Context, queue domain.QueueKey) ([]ports.Worker, error) {
	now, err := b.begin(ctx, "ListWorkers")
	if err != nil {
		return nil, err
	}
	defer b.mu.Unlock()
	q := b.queues[queue]
	if q == nil {
		return nil, ports.ErrQueueUnknown
	}
	var out []ports.Worker
	for _, key := range b.order {
		t := b.threads[key]
		if t.queue != queue {
			continue
		}
		w := ports.Worker{ID: maps.Clone(t.id), Queue: queue, Drained: b.drained(q, t.id), Timeout: now.Add(time.Minute)}
		if t.op != nil {
			w.Executing, w.Operation = true, t.op.Name
		}
		out = append(out, w)
	}
	return out, nil
}

// AddDrain implements ports.BuildQueue.
func (b *BuildQueue) AddDrain(ctx context.Context, queue domain.QueueKey, pattern ports.WorkerID) error {
	now, err := b.begin(ctx, "AddDrain")
	if err != nil {
		return err
	}
	defer b.mu.Unlock()
	q := b.queues[queue]
	if q == nil {
		return ports.ErrQueueUnknown
	}
	for _, d := range q.drains {
		if maps.Equal(d.Pattern, pattern) {
			return nil
		}
	}
	q.drains = append(q.drains, ports.Drain{Queue: queue, Pattern: maps.Clone(pattern), Created: now})
	return nil
}

// RemoveDrain implements ports.BuildQueue.
func (b *BuildQueue) RemoveDrain(ctx context.Context, queue domain.QueueKey, pattern ports.WorkerID) error {
	now, err := b.begin(ctx, "RemoveDrain")
	if err != nil {
		return err
	}
	defer b.mu.Unlock()
	q := b.queues[queue]
	if q == nil {
		return ports.ErrQueueUnknown
	}
	q.drains = slices.DeleteFunc(q.drains, func(d ports.Drain) bool { return maps.Equal(d.Pattern, pattern) })
	b.dispatchLocked(now)
	return nil
}

// ListDrains implements ports.BuildQueue.
func (b *BuildQueue) ListDrains(ctx context.Context, queue domain.QueueKey) ([]ports.Drain, error) {
	if _, err := b.begin(ctx, "ListDrains"); err != nil {
		return nil, err
	}
	defer b.mu.Unlock()
	q := b.queues[queue]
	if q == nil {
		return nil, ports.ErrQueueUnknown
	}
	out := make([]ports.Drain, 0, len(q.drains))
	for _, d := range q.drains {
		d.Pattern = maps.Clone(d.Pattern)
		out = append(out, d)
	}
	return out, nil
}

// KillOperations implements ports.BuildQueue.
func (b *BuildQueue) KillOperations(ctx context.Context, f ports.KillFilter, code int32, message string) error {
	now, err := b.begin(ctx, "KillOperations")
	if err != nil {
		return err
	}
	defer b.mu.Unlock()
	fail := func(op *Op) {
		op.Stage, op.CompletedAt, op.Failed, op.Code, op.Message = StageCompleted, now, true, code, message
	}
	switch {
	case f.OperationName != "":
		op := b.ops[f.OperationName]
		if op == nil || op.Stage == StageCompleted {
			return ports.ErrNotFound
		}
		if op.Stage == StageQueued {
			q := b.queues[op.Queue]
			q.queued = slices.DeleteFunc(q.queued, func(o *Op) bool { return o == op })
		} else {
			for _, t := range b.threads {
				if t.op == op {
					t.op = nil
				}
			}
		}
		fail(op)
		b.dispatchLocked(now)
	case f.QueueWithoutWorkers != nil:
		q := b.queues[*f.QueueWithoutWorkers]
		if q == nil {
			return ports.ErrQueueUnknown
		}
		for _, t := range b.threads {
			if t.queue == q.key {
				return nil // the queue has workers: nothing to kill
			}
		}
		for _, op := range q.queued {
			fail(op)
		}
		q.queued = nil
	default:
		return fmt.Errorf("kill filter is empty: %w", ports.ErrInvalid)
	}
	return nil
}

func toOperation(o *Op) ports.Operation {
	return ports.Operation{Name: o.Name, Queue: o.Queue, ActionDigest: fmt.Sprintf("%064x-%d", len(o.Name), 142),
		Stage: o.Stage, QueuedAt: o.SubmittedAt, InvocationID: o.InvocationID, Worker: maps.Clone(o.Worker)}
}

// ListOperations implements ports.BuildQueue.
func (b *BuildQueue) ListOperations(ctx context.Context, f ports.OperationFilter) ([]ports.Operation, error) {
	if _, err := b.begin(ctx, "ListOperations"); err != nil {
		return nil, err
	}
	defer b.mu.Unlock()
	var out []ports.Operation
	for _, name := range sortedKeys(b.ops) {
		o := b.ops[name]
		if f.Queue != nil && o.Queue != *f.Queue || f.Stage != "" && o.Stage != f.Stage || name <= f.StartAfter {
			continue
		}
		out = append(out, toOperation(o))
		if f.PageSize > 0 && len(out) == f.PageSize {
			break
		}
	}
	return out, nil
}

// GetOperation implements ports.BuildQueue.
func (b *BuildQueue) GetOperation(ctx context.Context, name string) (ports.Operation, error) {
	if _, err := b.begin(ctx, "GetOperation"); err != nil {
		return ports.Operation{}, err
	}
	defer b.mu.Unlock()
	o := b.ops[name]
	if o == nil {
		return ports.Operation{}, ports.ErrNotFound
	}
	return toOperation(o), nil
}

// ------------------------------------------------------- ground truth (no faults)

// Op returns one operation's ground-truth record.
func (b *BuildQueue) Op(name string) (Op, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advanceLocked(b.clock.Now())
	o, ok := b.ops[name]
	if !ok {
		return Op{}, false
	}
	c := *o
	c.Worker = maps.Clone(o.Worker)
	return c, true
}

// ListPlatformQueuesTruth is ListPlatformQueues without injected faults or
// latency (for simulation oracles).
func (b *BuildQueue) ListPlatformQueuesTruth() ([]domain.QueueObservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.advanceLocked(b.clock.Now())
	return b.queuesLocked(), nil
}

// ListDrainsTruth is ListDrains without injected faults or latency.
func (b *BuildQueue) ListDrainsTruth(queue domain.QueueKey) ([]ports.Drain, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.queues[queue]
	if q == nil {
		return nil, ports.ErrQueueUnknown
	}
	out := make([]ports.Drain, 0, len(q.drains))
	for _, d := range q.drains {
		d.Pattern = maps.Clone(d.Pattern)
		out = append(out, d)
	}
	return out, nil
}
