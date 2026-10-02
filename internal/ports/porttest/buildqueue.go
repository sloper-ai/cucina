// SPDX-License-Identifier: FSL-1.1-ALv2

package porttest

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// BuildQueueHarness configures RunBuildQueue.
type BuildQueueHarness struct {
	Queue ports.BuildQueue
	// Declared is a predeclared queue without workers; Undeclared a queue the
	// scheduler does not know.
	Declared, Undeclared domain.QueueKey
	// Submit enqueues one operation on a queue and returns its name (nil:
	// operation checks are skipped; real: an REAPI Execute client).
	Submit func(t *testing.T, key domain.QueueKey) string
	// AddWorker registers one runner thread with the given ID on a queue (nil:
	// worker checks are skipped).
	AddWorker func(t *testing.T, key domain.QueueKey, id ports.WorkerID)
	Await     Await
}

func (h BuildQueueHarness) queue(t *testing.T, key domain.QueueKey) (domain.QueueObservation, bool) {
	t.Helper()
	qs, err := h.Queue.ListPlatformQueues(context.Background())
	require.NoError(t, err)
	for _, q := range qs {
		if q.Key == key {
			return q, true
		}
	}
	return domain.QueueObservation{}, false
}

// RunBuildQueue checks the ports.BuildQueue contract (R-RE-2, R-SCALE-1/3):
// predeclared queues exist without workers, unknown queues are reported as
// ErrQueueUnknown, drains are listed and idempotent, a drained worker gets no
// new work, KillOperations fails only queues without workers.
func RunBuildQueue(t *testing.T, newQueue func(t *testing.T) BuildQueueHarness) {
	ctx := context.Background()
	pattern := ports.WorkerID{domain.LabelPool: "porttest", domain.LabelNode: "porttest-node"}

	t.Run("PredeclaredQueueExistsWithoutWorkers", func(t *testing.T) {
		h := newQueue(t)
		q, ok := h.queue(t, h.Declared)
		require.True(t, ok, "predeclared queue %+v not listed", h.Declared)
		require.Zero(t, q.Workers)
		ws, err := h.Queue.ListWorkers(ctx, h.Declared)
		require.NoError(t, err)
		require.Empty(t, ws)
	})

	t.Run("UnknownQueueIsReported", func(t *testing.T) {
		h := newQueue(t)
		_, ok := h.queue(t, h.Undeclared)
		require.False(t, ok)
		_, err := h.Queue.ListWorkers(ctx, h.Undeclared)
		require.ErrorIs(t, err, ports.ErrQueueUnknown)
		require.ErrorIs(t, h.Queue.AddDrain(ctx, h.Undeclared, pattern), ports.ErrQueueUnknown)
	})

	t.Run("DrainsAreListedAndIdempotent", func(t *testing.T) {
		h := newQueue(t)
		require.NoError(t, h.Queue.AddDrain(ctx, h.Declared, pattern))
		require.NoError(t, h.Queue.AddDrain(ctx, h.Declared, pattern))
		ds, err := h.Queue.ListDrains(ctx, h.Declared)
		require.NoError(t, err)
		n := 0
		for _, d := range ds {
			if d.Pattern[domain.LabelNode] == pattern[domain.LabelNode] {
				n++
				require.False(t, d.Created.IsZero(), "drains carry their creation time")
			}
		}
		require.Equal(t, 1, n, "adding the same drain twice registers it once")
		require.NoError(t, h.Queue.RemoveDrain(ctx, h.Declared, pattern))
		require.NoError(t, h.Queue.RemoveDrain(ctx, h.Declared, pattern), "removing an absent drain is not an error")
		ds, err = h.Queue.ListDrains(ctx, h.Declared)
		require.NoError(t, err)
		require.False(t, slices.ContainsFunc(ds, func(d ports.Drain) bool { return d.Pattern[domain.LabelNode] == pattern[domain.LabelNode] }))
	})

	t.Run("KillFailsQueuedWorkOfQueueWithoutWorkers", func(t *testing.T) {
		h := newQueue(t)
		if h.Submit == nil {
			t.Skip("harness cannot submit operations")
		}
		name := h.Submit(t, h.Declared)
		q, _ := h.queue(t, h.Declared)
		require.Equal(t, 1, q.Queued)
		require.NoError(t, h.Queue.KillOperations(ctx, ports.KillFilter{QueueWithoutWorkers: &h.Declared}, 9, "porttest"))
		op, err := h.Queue.GetOperation(ctx, name)
		require.NoError(t, err)
		require.Equal(t, "completed", op.Stage)
		q, _ = h.queue(t, h.Declared)
		require.Zero(t, q.Queued)
	})

	t.Run("DrainedWorkerGetsNoNewWork", func(t *testing.T) {
		h := newQueue(t)
		if h.Submit == nil || h.AddWorker == nil {
			t.Skip("harness cannot submit operations or add workers")
		}
		id := ports.WorkerID{domain.LabelPool: "porttest", domain.LabelNode: uniq(t, "node"), "thread": "0"}
		node := ports.WorkerID{domain.LabelPool: id[domain.LabelPool], domain.LabelNode: id[domain.LabelNode]}
		h.AddWorker(t, h.Declared, id)
		require.NoError(t, h.Queue.AddDrain(ctx, h.Declared, node))
		ws, err := h.Queue.ListWorkers(ctx, h.Declared)
		require.NoError(t, err)
		require.Len(t, ws, 1)
		require.True(t, ws[0].Drained, "a worker matching a drain pattern is reported drained")
		name := h.Submit(t, h.Declared)
		op, err := h.Queue.GetOperation(ctx, name)
		require.NoError(t, err)
		require.Equal(t, "queued", op.Stage, "a drained worker must not take new work")
		require.NoError(t, h.Queue.KillOperations(ctx, ports.KillFilter{QueueWithoutWorkers: &h.Declared}, 9, "porttest"))
		op, err = h.Queue.GetOperation(ctx, name)
		require.NoError(t, err)
		require.Equal(t, "queued", op.Stage, "KillOperations{queue without workers} must not kill work of a queue that has workers")
		require.NoError(t, h.Queue.RemoveDrain(ctx, h.Declared, node))
		h.Await(t, "operation assigned after RemoveDrain", func() bool {
			op, err := h.Queue.GetOperation(ctx, name)
			require.NoError(t, err)
			return op.Stage != "queued"
		})
	})
}
