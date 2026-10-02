// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

const poolHistoryLimit = 50

func (s *Server) listPools(ctx context.Context) ([]Pool, error) {
	pools, err := s.deps.Pools.Pools(ctx)
	if err != nil {
		return nil, fail("pools", err)
	}
	return pools, nil
}

// findPool returns the named pool.
func (s *Server) findPool(ctx context.Context, name string) (Pool, error) {
	if name == "" {
		return Pool{}, invalid("pool name is required")
	}
	pools, err := s.listPools(ctx)
	if err != nil {
		return Pool{}, err
	}
	for _, p := range pools {
		if p.Name() == name {
			return p, nil
		}
	}
	return Pool{}, notFound("pool %q not found", name)
}

// ListPools implements `cucinactl pools list`.
func (s *Server) ListPools(ctx context.Context, _ *cucinav1.ListPoolsRequest) (*cucinav1.ListPoolsResponse, error) {
	pools, err := s.listPools(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := &cucinav1.ListPoolsResponse{}
	for _, p := range pools {
		out.Pools = append(out.Pools, poolSummary(p, now))
	}
	return out, nil
}

// GetPool implements `cucinactl pools describe`: spec, conditions, workers, scale
// timeline and start latencies.
func (s *Server) GetPool(ctx context.Context, req *cucinav1.GetPoolRequest) (*cucinav1.GetPoolResponse, error) {
	p, err := s.findPool(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	spec, err := RedactedJSON(p.Resource.Spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "rendering pool spec: %v", err)
	}
	now := time.Now()
	out := &cucinav1.GetPoolResponse{
		Summary:    poolSummary(p, now),
		SpecJson:   string(spec),
		Conditions: conditionStrings(p.Resource.Status.Conditions),
	}
	workers, err := s.deps.Workers.Workers(ctx)
	if err != nil {
		return nil, fail("workers", err)
	}
	workers = slices.Clone(workers)
	slices.SortFunc(workers, func(a, b Worker) int { return cmp.Compare(a.ID, b.ID) })
	for _, w := range workers {
		if string(w.Pool) == p.Name() && w.State != domain.VMTerminated {
			out.Workers = append(out.Workers, workerSummary(w, now))
		}
	}
	events, starts, err := s.deps.Pools.History(ctx, p.Name(), poolHistoryLimit)
	if err != nil {
		return nil, fail("pool history", err)
	}
	for _, e := range events {
		out.Events = append(out.Events, &cucinav1.PoolEvent{Time: ts(e.Time), Type: e.Type, Subject: e.Subject, Message: e.Message})
	}
	for _, st := range starts {
		out.Starts = append(out.Starts, startProto(st))
	}
	return out, nil
}

// SetPoolFloor sets a temporary minRunning override. A floor keeps workers
// running, which is standing cost, so it must always expire (min_running 0 clears
// the override and needs no expiry).
func (s *Server) SetPoolFloor(ctx context.Context, req *cucinav1.SetPoolFloorRequest) (*cucinav1.SetPoolFloorResponse, error) {
	if s.deps.PoolAdmin == nil {
		return nil, notConfigured("pool administration")
	}
	p, err := s.findPool(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	floor := FloorOverride{MinRunning: int32(min(req.GetMinRunning(), uint32(1<<31-1))), SetBy: caller(ctx)}
	out := &cucinav1.SetPoolFloorResponse{}
	if floor.MinRunning > 0 {
		d := req.GetExpiresIn().AsDuration()
		if req.GetExpiresIn() == nil || d <= 0 {
			return nil, invalid("expires_in is required: a floor keeps workers running (standing cost) and must expire")
		}
		if d > s.opts.MaxFloorDuration {
			return nil, invalid("expires_in %s exceeds the maximum %s; use spec.floorSchedule for recurring floors", d, s.opts.MaxFloorDuration)
		}
		if floor.MinRunning > p.Resource.Spec.Capacity.Max {
			return nil, invalid("min_running %d exceeds the pool's max %d", floor.MinRunning, p.Resource.Spec.Capacity.Max)
		}
		floor.ExpiresAt = time.Now().Add(d)
		out.ExpiresAt = ts(floor.ExpiresAt)
	}
	if err := s.deps.PoolAdmin.SetFloor(ctx, p.Name(), floor); err != nil {
		return nil, fail("setting pool floor", err)
	}
	return out, nil
}

// CordonPool pauses (cordon) or resumes (uncordon) launches for a pool.
func (s *Server) CordonPool(ctx context.Context, req *cucinav1.CordonPoolRequest) (*cucinav1.CordonPoolResponse, error) {
	if s.deps.PoolAdmin == nil {
		return nil, notConfigured("pool administration")
	}
	p, err := s.findPool(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.deps.PoolAdmin.SetPaused(ctx, p.Name(), req.GetCordon()); err != nil {
		return nil, fail("cordoning pool", err)
	}
	return &cucinav1.CordonPoolResponse{}, nil
}

// GarbageCollectPool deletes orphaned, pool-tagged volumes and ENIs (R-POOL-2); with
// dry_run it only reports what it would delete.
func (s *Server) GarbageCollectPool(ctx context.Context, req *cucinav1.GarbageCollectPoolRequest) (*cucinav1.GarbageCollectPoolResponse, error) {
	if s.deps.Orphans == nil {
		return nil, notConfigured("the EC2 provider")
	}
	if s.opts.ClusterID == "" {
		return nil, notConfigured("the cluster ID")
	}
	if req.GetName() != "" {
		if _, err := s.findPool(ctx, req.GetName()); err != nil {
			return nil, err
		}
	}
	all, err := s.deps.Orphans.ListOrphans(ctx, s.opts.ClusterID)
	if err != nil {
		return nil, fail("listing orphans", err)
	}
	var orphans []ports.Orphan
	for _, o := range all {
		if req.GetName() == "" || string(o.Pool) == req.GetName() {
			orphans = append(orphans, o)
		}
	}
	slices.SortFunc(orphans, func(a, b ports.Orphan) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
	})
	out := &cucinav1.GarbageCollectPoolResponse{}
	describe := func(o ports.Orphan) string { return fmt.Sprintf("%s %s", o.Kind, o.ID) }
	if req.GetDryRun() || len(orphans) == 0 {
		for _, o := range orphans {
			out.Deleted = append(out.Deleted, describe(o))
		}
		return out, nil
	}
	errs, err := s.deps.Orphans.DeleteOrphans(ctx, s.opts.ClusterID, orphans)
	if err != nil {
		return nil, fail("deleting orphans", err)
	}
	for _, o := range orphans {
		if e := errs[o.ID]; e != nil {
			out.Errors = append(out.Errors, describe(o)+": "+RedactText(e.Error()))
			continue
		}
		out.Deleted = append(out.Deleted, describe(o))
	}
	return out, nil
}

// ---------------------------------------------------------------- workers

// findWorker returns the worker VM with the given node label and its pool.
func (s *Server) findWorker(ctx context.Context, node string) (Worker, Pool, error) {
	if node == "" {
		return Worker{}, Pool{}, invalid("node is required (EC2 instance ID or <host>/<vm>)")
	}
	workers, err := s.deps.Workers.Workers(ctx)
	if err != nil {
		return Worker{}, Pool{}, fail("workers", err)
	}
	for _, w := range workers {
		if w.ID != node || w.State == domain.VMTerminated {
			continue
		}
		p, err := s.findPool(ctx, string(w.Pool))
		if err != nil {
			return Worker{}, Pool{}, err
		}
		return w, p, nil
	}
	return Worker{}, Pool{}, notFound("worker %q not found", node)
}

// ListWorkers implements `cucinactl workers list`.
func (s *Server) ListWorkers(ctx context.Context, req *cucinav1.ListWorkersRequest) (*cucinav1.ListWorkersResponse, error) {
	if req.GetPool() != "" {
		if _, err := s.findPool(ctx, req.GetPool()); err != nil {
			return nil, err
		}
	}
	workers, err := s.deps.Workers.Workers(ctx)
	if err != nil {
		return nil, fail("workers", err)
	}
	workers = slices.Clone(workers)
	slices.SortFunc(workers, func(a, b Worker) int {
		return cmp.Or(cmp.Compare(a.Pool, b.Pool), cmp.Compare(a.ID, b.ID))
	})
	now := time.Now()
	out := &cucinav1.ListWorkersResponse{}
	for _, w := range workers {
		if w.State == domain.VMTerminated || (req.GetPool() != "" && string(w.Pool) != req.GetPool()) {
			continue
		}
		out.Workers = append(out.Workers, workerSummary(w, now))
	}
	return out, nil
}

// DrainWorker drains a worker for maintenance (UC14): AddDrain with the operator
// pattern {node: <node>} on every queue of its pool. The autoscaler never removes
// operator drains; it stops the VM once idle (docs/dev/scaling.md).
func (s *Server) DrainWorker(ctx context.Context, req *cucinav1.DrainWorkerRequest) (*cucinav1.DrainWorkerResponse, error) {
	return &cucinav1.DrainWorkerResponse{}, s.setDrain(ctx, req.GetNode(), true)
}

// UndrainWorker removes the operator drain of a worker.
func (s *Server) UndrainWorker(ctx context.Context, req *cucinav1.DrainWorkerRequest) (*cucinav1.DrainWorkerResponse, error) {
	return &cucinav1.DrainWorkerResponse{}, s.setDrain(ctx, req.GetNode(), false)
}

func (s *Server) setDrain(ctx context.Context, node string, drain bool) error {
	_, pool, err := s.findWorker(ctx, node)
	if err != nil {
		return err
	}
	keys := poolQueueKeys(pool.Spec, s.opts.InstanceNames)
	if len(keys) == 0 {
		return status.Errorf(codes.FailedPrecondition, "pool %q has no runner queues", pool.Name())
	}
	pattern := ports.WorkerID{domain.LabelNode: node}
	for _, k := range keys {
		op, apply := "AddDrain", s.deps.Scheduler.AddDrain
		if !drain {
			op, apply = "RemoveDrain", s.deps.Scheduler.RemoveDrain
		}
		if err := apply(ctx, k, pattern); err != nil {
			return fail(fmt.Sprintf("%s on queue %s %s", op, k.InstanceNamePrefix, k.PlatformKey), err)
		}
	}
	return nil
}
