// SPDX-License-Identifier: FSL-1.1-ALv2

package bbtest

import (
	"context"
	"fmt"
	"maps"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteworker"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// FakeWorker is one runner thread driven by hand through the scheduler's
// Synchronize API: it registers, picks up a task when told to and reports it
// completed. Tests use it to put a real bb_scheduler into precise states
// (idle, executing, drained) without running bb_worker.
type FakeWorker struct {
	client    remoteworker.OperationQueueClient
	ID        map[string]string
	Prefix    string
	Platform  *remoteexecution.Platform
	SizeClass uint32
}

// NewFakeWorker returns a thread with worker ID id (for example {pool, node,
// thread}) for the queue (prefix, properties, sizeClass). conn must reach the
// scheduler's worker listener (with a worker certificate when it uses mTLS).
func NewFakeWorker(conn grpc.ClientConnInterface, id map[string]string, prefix string, properties map[string]string, sizeClass uint32) *FakeWorker {
	return &FakeWorker{
		client:    remoteworker.NewOperationQueueClient(conn),
		ID:        maps.Clone(id),
		Prefix:    prefix,
		Platform:  Platform(properties),
		SizeClass: sizeClass,
	}
}

func (w *FakeWorker) synchronize(ctx context.Context, state *remoteworker.CurrentState, preferIdle bool) (*remoteworker.SynchronizeResponse, error) {
	return w.client.Synchronize(ctx, &remoteworker.SynchronizeRequest{
		WorkerId:           w.ID,
		InstanceNamePrefix: w.Prefix,
		Platform:           w.Platform,
		SizeClass:          w.SizeClass,
		CurrentState:       state,
		PreferBeingIdle:    preferIdle,
	})
}

func idle() *remoteworker.CurrentState {
	return &remoteworker.CurrentState{WorkerState: &remoteworker.CurrentState_Idle{Idle: &emptypb.Empty{}}}
}

// Register announces the thread as idle without blocking (prefer_being_idle):
// the scheduler now counts it as an idle worker of the queue. Threads that do
// not synchronize again are dropped after one minute.
func (w *FakeWorker) Register(ctx context.Context) error {
	_, err := w.synchronize(ctx, idle(), true)
	return err
}

// Take synchronizes as an idle thread willing to work and blocks until the
// scheduler assigns a task (or ctx ends). The thread then counts as executing.
func (w *FakeWorker) Take(ctx context.Context) (*remoteworker.DesiredState_Executing, error) {
	for {
		resp, err := w.synchronize(ctx, idle(), false)
		if err != nil {
			return nil, err
		}
		if e := resp.GetDesiredState().GetExecuting(); e != nil {
			// Acknowledge, so the scheduler sees the task running.
			_, err := w.synchronize(ctx, &remoteworker.CurrentState{WorkerState: &remoteworker.CurrentState_Executing_{
				Executing: &remoteworker.CurrentState_Executing{
					ActionDigest:   e.ActionDigest,
					ExecutionState: &remoteworker.CurrentState_Executing_Running{Running: &emptypb.Empty{}},
				},
			}}, false)
			return e, err
		}
		// Idle timeout of the long poll: synchronize again.
	}
}

// Complete reports the task finished with result and goes idle without
// blocking for the next task.
func (w *FakeWorker) Complete(ctx context.Context, task *remoteworker.DesiredState_Executing, result *remoteexecution.ExecuteResponse) error {
	resp, err := w.synchronize(ctx, &remoteworker.CurrentState{WorkerState: &remoteworker.CurrentState_Executing_{
		Executing: &remoteworker.CurrentState_Executing{
			ActionDigest:   task.ActionDigest,
			ExecutionState: &remoteworker.CurrentState_Executing_Completed{Completed: result},
		},
	}}, true)
	if err != nil {
		return err
	}
	if resp.GetDesiredState().GetExecuting() != nil {
		return fmt.Errorf("scheduler assigned another task although the worker prefers being idle")
	}
	return nil
}
