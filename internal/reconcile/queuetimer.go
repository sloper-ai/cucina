// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/ports"
)

// QueueStat is the timing of one size class queue.
type QueueStat struct {
	// OldestQueuedAge is the age of the oldest queued operation (0 when empty).
	OldestQueuedAge time.Duration
	// QueueTimeP95 is the 95th percentile of the time operations spent queued
	// over Window (poll resolution).
	QueueTimeP95 time.Duration
}

// QueueTimer measures queue timing from the scheduler's operation lists
// (cucina_queue_oldest_seconds, the management API's queue summaries, UC16
// "queue time above threshold"). The BuildQueueState queue listing carries
// counts only, so every Every it lists the queued operations of each non-empty
// queue: the smallest QueuedAt is the oldest, and an operation that left the
// queued list since the last poll contributes now-QueuedAt as a queue-time
// sample. Leader-only.
type QueueTimer struct {
	BuildQueue ports.BuildQueue
	Clock      ports.Clock
	Metrics    *metrics.Metrics
	Log        *slog.Logger
	// Every is the poll period (default 5 s); Window the p95 window (default 15 min).
	Every  time.Duration
	Window time.Duration
	// MaxOps bounds the operations listed per queue and poll (default 5000).
	MaxOps int

	mu      sync.Mutex
	queued  map[domain.QueueKey]map[string]time.Time // operation → QueuedAt, last poll
	samples map[domain.QueueKey][]queueSample
	stats   map[domain.QueueKey]QueueStat
}

type queueSample struct {
	at   time.Time
	wait time.Duration
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (q *QueueTimer) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (q *QueueTimer) Start(ctx context.Context) error {
	every := q.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	for {
		q.Poll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-q.Clock.After(every):
		}
	}
}

// Poll performs one measurement.
func (q *QueueTimer) Poll(ctx context.Context) {
	qs, err := q.BuildQueue.ListPlatformQueues(ctx)
	if err != nil {
		return // the autoscaler loops report the scheduler's health
	}
	now := q.Clock.Now()
	window := q.Window
	if window <= 0 {
		window = 15 * time.Minute
	}
	maxOps := q.MaxOps
	if maxOps <= 0 {
		maxOps = 5000
	}
	cur := map[domain.QueueKey]map[string]time.Time{}
	for _, o := range qs {
		ops := map[string]time.Time{}
		if o.Queued > 0 {
			k := o.Key
			after := ""
			for len(ops) < maxOps {
				page, err := q.BuildQueue.ListOperations(ctx, ports.OperationFilter{Queue: &k, Stage: "queued", PageSize: 1000, StartAfter: after})
				if err != nil || len(page) == 0 {
					break
				}
				for _, op := range page {
					ops[op.Name] = op.QueuedAt
				}
				after = page[len(page)-1].Name
				if len(page) < 1000 {
					break
				}
			}
		}
		cur[o.Key] = ops
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.samples == nil {
		q.samples = map[domain.QueueKey][]queueSample{}
	}
	stats := map[domain.QueueKey]QueueStat{}
	for _, o := range qs {
		k := o.Key
		for name, at := range q.queued[k] {
			if _, still := cur[k][name]; !still && !at.IsZero() {
				q.samples[k] = append(q.samples[k], queueSample{at: now, wait: now.Sub(at)})
			}
		}
		q.samples[k] = slices.DeleteFunc(q.samples[k], func(s queueSample) bool { return now.Sub(s.at) > window })
		var st QueueStat
		for _, at := range cur[k] {
			if !at.IsZero() && now.Sub(at) > st.OldestQueuedAge {
				st.OldestQueuedAge = now.Sub(at)
			}
		}
		if n := len(q.samples[k]); n > 0 {
			waits := make([]time.Duration, n)
			for i, s := range q.samples[k] {
				waits[i] = s.wait
			}
			slices.Sort(waits)
			st.QueueTimeP95 = waits[min(n-1, (n*95+99)/100-1)]
		}
		stats[k] = st
		if q.Metrics != nil {
			q.Metrics.QueueOldestSeconds.WithLabelValues(k.PlatformKey, itoa(k.SizeClass), k.InstanceNamePrefix).Set(st.OldestQueuedAge.Seconds())
		}
	}
	q.queued, q.stats = cur, stats
}

// Stats returns the latest timing per queue.
func (q *QueueTimer) Stats() map[domain.QueueKey]QueueStat {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make(map[domain.QueueKey]QueueStat, len(q.stats))
	for k, v := range q.stats {
		out[k] = v
	}
	return out
}
