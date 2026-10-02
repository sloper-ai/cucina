// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

const (
	defaultOperationsPage = 100
	maxOperationsPage     = 1000
	// operationsFetch is the scheduler page size used while scanning.
	operationsFetch = 500
	// operationsScanFactor bounds the operations scanned for one page (filters are
	// applied here, so a selective filter may need several scheduler pages).
	operationsScanFactor = 20
	// operationsMaxAge bounds how old the shared operations snapshot may be.
	operationsMaxAge = time.Second
	// killCode is the status operations are failed with. Bazel does not retry
	// FAILED_PRECONDITION without missing-blob violations, so a kill sticks.
	killCode       = int32(codes.FailedPrecondition)
	maxKillMessage = 512
)

// ListQueues implements `cucinactl queues`.
func (s *Server) ListQueues(ctx context.Context, _ *cucinav1.ListQueuesRequest) (*cucinav1.ListQueuesResponse, error) {
	pools, err := s.listPools(ctx)
	if err != nil {
		return nil, err
	}
	qs, err := s.queueSummaries(ctx, pools)
	if err != nil {
		return nil, fail("scheduler", err)
	}
	return &cucinav1.ListQueuesResponse{Queues: qs}, nil
}

// opFilter is a validated operation filter bound to the caller's visibility.
type opFilter struct {
	queue      *domain.QueueKey
	stage      string
	instance   string
	invocation string
	principal  Principal
	all        bool // cluster admin: sees every instance name
}

func (s *Server) newOpFilter(ctx context.Context, req *cucinav1.ListOperationsRequest) (opFilter, error) {
	f := opFilter{stage: req.GetStage(), instance: req.GetInstanceName(), invocation: req.GetInvocationId()}
	switch f.stage {
	case "", "queued", "executing", "completed":
	default:
		return f, invalid("stage must be queued, executing or completed")
	}
	if req.GetQueue() != nil {
		k, err := queueKey(req.GetQueue())
		if err != nil {
			return f, err
		}
		f.queue = &k
	}
	f.principal, _ = PrincipalFromContext(ctx)
	f.all = s.clusterAdmin(f.principal)
	return f, nil
}

// match applies the filter and the caller's visibility: operations are only
// visible on instance names the caller holds execute or admin on.
func (s *Server) match(f opFilter, o ports.Operation) bool {
	switch {
	case f.queue != nil && o.Queue != *f.queue,
		f.stage != "" && o.Stage != f.stage,
		f.instance != "" && o.Queue.InstanceNamePrefix != f.instance,
		f.invocation != "" && o.InvocationID != f.invocation:
		return false
	}
	return f.all || s.canSee(f.principal, o.Queue.InstanceNamePrefix)
}

// ListOperations pages through the scheduler's operations with filters. The page
// token is opaque (it encodes the last scanned operation name).
func (s *Server) ListOperations(ctx context.Context, req *cucinav1.ListOperationsRequest) (*cucinav1.ListOperationsResponse, error) {
	f, err := s.newOpFilter(ctx, req)
	if err != nil {
		return nil, err
	}
	size := int(req.GetPage().GetSize())
	switch {
	case size <= 0:
		size = defaultOperationsPage
	case size > maxOperationsPage:
		size = maxOperationsPage
	}
	after := ""
	if tok := req.GetPage().GetToken(); tok != "" {
		b, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(b) == 0 {
			return nil, invalid("invalid page token")
		}
		after = string(b)
	}
	out := &cucinav1.ListOperationsResponse{}
	scanned, exhausted := 0, false
scan:
	for scanned < size*operationsScanFactor {
		batch, err := s.deps.Scheduler.ListOperations(ctx, ports.OperationFilter{
			Queue: f.queue, Stage: f.stage, PageSize: operationsFetch, StartAfter: after,
		})
		if err != nil {
			return nil, fail("scheduler", err)
		}
		for i, o := range batch {
			after = o.Name
			scanned++
			if s.match(f, o) {
				out.Operations = append(out.Operations, operationSummary(o))
				if len(out.Operations) == size {
					exhausted = i == len(batch)-1 && len(batch) < operationsFetch
					break scan
				}
			}
		}
		if len(batch) < operationsFetch {
			exhausted = true
			break
		}
	}
	if !exhausted && after != "" {
		out.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(after))
	}
	return out, nil
}

// GetOperation returns one operation; operations on instance names the caller
// cannot see are reported as not found.
func (s *Server) GetOperation(ctx context.Context, req *cucinav1.GetOperationRequest) (*cucinav1.GetOperationResponse, error) {
	if req.GetName() == "" {
		return nil, invalid("operation name is required")
	}
	op, err := s.deps.Scheduler.GetOperation(ctx, req.GetName())
	if err != nil {
		return nil, fail("scheduler", err)
	}
	p, _ := PrincipalFromContext(ctx)
	if !s.canSee(p, op.Queue.InstanceNamePrefix) {
		return nil, notFound("operation %q not found", req.GetName())
	}
	return &cucinav1.GetOperationResponse{Operation: operationSummary(op), ExecuteResponse: op.ExecuteResponse}, nil
}

// KillOperations fails one operation, or every queued operation of a queue without
// workers, with FAILED_PRECONDITION and an explanatory message.
func (s *Server) KillOperations(ctx context.Context, req *cucinav1.KillOperationsRequest) (*cucinav1.KillOperationsResponse, error) {
	msg := "operation killed by a Cucina administrator"
	if m := strings.TrimSpace(req.GetMessage()); m != "" {
		if len(m) > maxKillMessage {
			return nil, invalid("message longer than %d bytes", maxKillMessage)
		}
		msg += ": " + m
	}
	var f ports.KillFilter
	switch t := req.GetTarget().(type) {
	case *cucinav1.KillOperationsRequest_OperationName:
		if t.OperationName == "" {
			return nil, invalid("operation_name is empty")
		}
		f.OperationName = t.OperationName
	case *cucinav1.KillOperationsRequest_QueueWithoutWorkers:
		k, err := queueKey(t.QueueWithoutWorkers)
		if err != nil {
			return nil, err
		}
		f.QueueWithoutWorkers = &k
	default:
		return nil, invalid("target is required: operation_name or queue_without_workers")
	}
	if err := s.deps.Scheduler.KillOperations(ctx, f, killCode, msg); err != nil {
		return nil, fail("scheduler", err)
	}
	return &cucinav1.KillOperationsResponse{}, nil
}

// buildOperations lists every operation (up to MaxOperationsSnapshot) for the
// shared WatchOperations snapshot.
func (s *Server) buildOperations() ([]ports.Operation, error) {
	ctx, cancel := context.WithTimeout(s.base, buildTimeout)
	defer cancel()
	var all []ports.Operation
	after := ""
	for len(all) < s.opts.MaxOperationsSnapshot {
		n := min(operationsFetch, s.opts.MaxOperationsSnapshot-len(all))
		batch, err := s.deps.Scheduler.ListOperations(ctx, ports.OperationFilter{PageSize: n, StartAfter: after})
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < n {
			break
		}
		after = batch[len(batch)-1].Name
	}
	return all, nil
}

// diffOperations returns the events turning prev into cur, ordered by name.
func diffOperations(prev, cur map[string]*cucinav1.OperationSummary) []*cucinav1.OperationEvent {
	names := make([]string, 0, len(prev)+len(cur))
	for n := range prev {
		names = append(names, n)
	}
	for n := range cur {
		if _, ok := prev[n]; !ok {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	var out []*cucinav1.OperationEvent
	for _, n := range names {
		p, inPrev := prev[n]
		c, inCur := cur[n]
		switch {
		case !inPrev:
			out = append(out, &cucinav1.OperationEvent{Kind: cucinav1.OperationEvent_KIND_ADDED, Operation: c})
		case !inCur:
			out = append(out, &cucinav1.OperationEvent{Kind: cucinav1.OperationEvent_KIND_REMOVED, Operation: p})
		case !proto.Equal(p, c):
			out = append(out, &cucinav1.OperationEvent{Kind: cucinav1.OperationEvent_KIND_CHANGED, Operation: c})
		}
	}
	return out
}

// WatchOperations streams ADDED/CHANGED/REMOVED events for the filter: first the
// current operations as ADDED, then a diff every OperationsInterval. Subscribers
// share one scheduler listing per second.
func (s *Server) WatchOperations(req *cucinav1.ListOperationsRequest, stream grpc.ServerStreamingServer[cucinav1.OperationEvent]) error {
	ctx := stream.Context()
	f, err := s.newOpFilter(ctx, req)
	if err != nil {
		return err
	}
	release, err := s.opWatchers.acquire("operation watchers")
	if err != nil {
		return err
	}
	defer release()
	if err := stream.SendHeader(metadata.MD{}); err != nil {
		return err
	}
	ticker := time.NewTicker(s.opts.OperationsInterval)
	defer ticker.Stop()
	prev := map[string]*cucinav1.OperationSummary{}
	for {
		ops, err := s.ops.get(operationsMaxAge, s.buildOperations)
		if err != nil {
			return fail("scheduler", err)
		}
		cur := make(map[string]*cucinav1.OperationSummary, len(prev))
		for _, o := range ops {
			if s.match(f, o) {
				cur[o.Name] = operationSummary(o)
			}
		}
		for _, ev := range diffOperations(prev, cur) {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
		prev = cur
		select {
		case <-ctx.Done():
			return streamEnd(ctx)
		case <-s.done:
			return errStopping
		case <-ticker.C:
		}
	}
}
