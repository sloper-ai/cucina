// SPDX-License-Identifier: FSL-1.1-ALv2

// Package buildqueue is the real ports.BuildQueue adapter: a client of
// bb_scheduler's BuildQueueState gRPC API (pinned bb-remote-execution
// 1a3be95, pkg/proto/buildqueuestate). It is the autoscaler's signal (polled
// every 1–2 s, R-SCALE-1) and its drain/kill control (R-SCALE-3, R-RE-2),
// reachable only in-cluster over mTLS with the controller certificate (R-SEC-4).
package buildqueue

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/buildqueuestate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Defaults.
const (
	DefaultCallTimeout = 5 * time.Second
	DefaultPageSize    = 1000
)

// Operation stages (ports.Operation.Stage, ports.OperationFilter.Stage).
const (
	StageQueued    = "queued"
	StageExecuting = "executing"
	StageCompleted = "completed"
)

// retryPolicy retries transient failures of every method except
// KillOperations, whose effect is not idempotent from the caller's view.
const retryPolicy = `{"methodConfig": [{
  "name": [
    {"service": "buildbarn.buildqueuestate.BuildQueueState", "method": "GetOperation"},
    {"service": "buildbarn.buildqueuestate.BuildQueueState", "method": "ListOperations"},
    {"service": "buildbarn.buildqueuestate.BuildQueueState", "method": "ListPlatformQueues"},
    {"service": "buildbarn.buildqueuestate.BuildQueueState", "method": "ListWorkers"},
    {"service": "buildbarn.buildqueuestate.BuildQueueState", "method": "ListDrains"},
    {"service": "buildbarn.buildqueuestate.BuildQueueState", "method": "AddDrain"},
    {"service": "buildbarn.buildqueuestate.BuildQueueState", "method": "RemoveDrain"}
  ],
  "retryPolicy": {"maxAttempts": 3, "initialBackoff": "0.1s", "maxBackoff": "1s", "backoffMultiplier": 2, "retryableStatusCodes": ["UNAVAILABLE"]}
}]}`

// Options configure a Client.
type Options struct {
	// Address is host:port of the scheduler's BuildQueueState listener.
	Address string
	// TLS is the mTLS client configuration (controller certificate and the
	// Cucina CA); see TLSFromFiles. Nil means plaintext (tests only).
	TLS *tls.Config
	// CallTimeout bounds every RPC (default DefaultCallTimeout).
	CallTimeout time.Duration
	// PageSize is the page size of ListWorkers and ListOperations (default
	// DefaultPageSize).
	PageSize int
}

// Client implements ports.BuildQueue.
type Client struct {
	c        buildqueuestate.BuildQueueStateClient
	closer   func() error
	timeout  time.Duration
	pageSize int

	mu        sync.Mutex
	platforms map[string][]*remoteexecution.Platform_Property // PlatformKey → exact properties
}

var _ ports.BuildQueue = (*Client)(nil)

// New dials the scheduler (lazily: the first RPC connects, so a scheduler that
// is still starting is not an error here).
func New(o Options) (*Client, error) {
	if o.Address == "" {
		return nil, errors.New("buildqueue: scheduler BuildQueueState address is empty")
	}
	creds := insecure.NewCredentials()
	if o.TLS != nil {
		creds = credentials.NewTLS(o.TLS)
	}
	conn, err := grpc.NewClient(o.Address,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultServiceConfig(retryPolicy),
	)
	if err != nil {
		return nil, fmt.Errorf("buildqueue: %w", err)
	}
	c := NewFromConn(conn, o)
	c.closer = conn.Close
	return c, nil
}

// NewFromConn uses an existing connection (Options.Address and TLS are ignored).
func NewFromConn(conn grpc.ClientConnInterface, o Options) *Client {
	c := &Client{
		c:         buildqueuestate.NewBuildQueueStateClient(conn),
		closer:    func() error { return nil },
		timeout:   o.CallTimeout,
		pageSize:  o.PageSize,
		platforms: map[string][]*remoteexecution.Platform_Property{},
	}
	if c.timeout <= 0 {
		c.timeout = DefaultCallTimeout
	}
	if c.pageSize <= 0 {
		c.pageSize = DefaultPageSize
	}
	return c
}

// Close releases the connection made by New.
func (c *Client) Close() error { return c.closer() }

// TLSFromFiles builds the controller's mTLS client configuration. The key pair
// is re-read whenever its files change, so certificate rotation needs no
// restart; caFile verifies the scheduler's certificate.
func TLSFromFiles(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("buildqueue: read CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("buildqueue: %s holds no PEM certificate", caFile)
	}
	kp := &reloadingKeyPair{certFile: certFile, keyFile: keyFile}
	if _, err := kp.get(); err != nil {
		return nil, err
	}
	return &tls.Config{
		RootCAs:    pool,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return kp.get()
		},
	}, nil
}

type reloadingKeyPair struct {
	certFile, keyFile string
	mu                sync.Mutex
	stamp             string
	cert              *tls.Certificate
}

func (k *reloadingKeyPair) get() (*tls.Certificate, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var stamp strings.Builder
	for _, f := range []string{k.certFile, k.keyFile} {
		st, err := os.Stat(f)
		if err != nil {
			return nil, fmt.Errorf("buildqueue: client key pair: %w", err)
		}
		fmt.Fprintf(&stamp, "%d/%d;", st.ModTime().UnixNano(), st.Size())
	}
	if k.cert != nil && stamp.String() == k.stamp {
		return k.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(k.certFile, k.keyFile)
	if err != nil {
		return nil, fmt.Errorf("buildqueue: client key pair: %w", err)
	}
	k.cert, k.stamp = &cert, stamp.String()
	return k.cert, nil
}

func (c *Client) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

// ListPlatformQueues returns one observation per size class queue. Counts are
// those of the queue's root invocation, which aggregates every invocation
// below it: queued = direct + indirect queued operations; executing = workers
// running an operation of the queue; idle = idle workers, synchronizing or not
// (one Buildbarn worker is one runner thread, R-RE-4).
func (c *Client) ListPlatformQueues(ctx context.Context) ([]domain.QueueObservation, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.c.ListPlatformQueues(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, mapError("ListPlatformQueues", err, false)
	}
	var out []domain.QueueObservation
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pq := range resp.PlatformQueues {
		name := pq.GetName()
		key := platformKey(name.GetPlatform().GetProperties())
		c.platforms[key] = name.GetPlatform().GetProperties()
		for _, scq := range pq.SizeClassQueues {
			root := scq.GetRootInvocation()
			out = append(out, domain.QueueObservation{
				Key: domain.QueueKey{
					InstanceNamePrefix: name.GetInstanceNamePrefix(),
					PlatformKey:        key,
					SizeClass:          scq.GetSizeClass(),
				},
				Queued:    int(root.GetQueuedOperationsCount().GetDirect() + root.GetQueuedOperationsCount().GetIndirect()),
				Executing: int(root.GetExecutingWorkersCount()),
				Idle:      int(root.GetIdleWorkersCount()),
				Workers:   int(scq.GetWorkersCount()),
				Drains:    int(scq.GetDrainsCount()),
			})
		}
	}
	return out, nil
}

// ListWorkers returns every runner thread of a queue, following pagination.
func (c *Client) ListWorkers(ctx context.Context, queue domain.QueueKey) ([]ports.Worker, error) {
	name, err := c.queueName(queue)
	if err != nil {
		return nil, err
	}
	var out []ports.Worker
	var startAfter *buildqueuestate.ListWorkersRequest_StartAfter
	for {
		resp, err := c.listWorkersPage(ctx, name, startAfter)
		if err != nil {
			return nil, err
		}
		for _, w := range resp.Workers {
			out = append(out, toWorker(w, queue))
		}
		page := resp.GetPaginationInfo()
		if len(resp.Workers) == 0 || int(page.GetStartIndex())+len(resp.Workers) >= int(page.GetTotalEntries()) {
			return out, nil
		}
		startAfter = &buildqueuestate.ListWorkersRequest_StartAfter{WorkerId: resp.Workers[len(resp.Workers)-1].Id}
	}
}

func (c *Client) listWorkersPage(ctx context.Context, name *buildqueuestate.SizeClassQueueName, startAfter *buildqueuestate.ListWorkersRequest_StartAfter) (*buildqueuestate.ListWorkersResponse, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.c.ListWorkers(ctx, &buildqueuestate.ListWorkersRequest{
		Filter:     &buildqueuestate.ListWorkersRequest_Filter{Type: &buildqueuestate.ListWorkersRequest_Filter_All{All: name}},
		PageSize:   uint32(c.pageSize),
		StartAfter: startAfter,
	})
	return resp, mapError("ListWorkers", err, true)
}

func toWorker(w *buildqueuestate.WorkerState, queue domain.QueueKey) ports.Worker {
	out := ports.Worker{ID: ports.WorkerID(w.GetId()), Queue: queue, Drained: w.GetDrained()}
	if op := w.GetCurrentOperation(); op != nil {
		out.Executing, out.Operation = true, op.GetName()
	}
	if ts := w.GetTimeout(); ts != nil {
		out.Timeout = ts.AsTime()
	}
	return out
}

// AddDrain stops the workers matching pattern (a subset of their worker ID,
// for example {"node": "i-…"}) from receiving new tasks. Idempotent.
func (c *Client) AddDrain(ctx context.Context, queue domain.QueueKey, pattern ports.WorkerID) error {
	return c.modifyDrain(ctx, "AddDrain", c.c.AddDrain, queue, pattern)
}

// RemoveDrain removes a drain pattern. Idempotent.
func (c *Client) RemoveDrain(ctx context.Context, queue domain.QueueKey, pattern ports.WorkerID) error {
	return c.modifyDrain(ctx, "RemoveDrain", c.c.RemoveDrain, queue, pattern)
}

func (c *Client) modifyDrain(ctx context.Context, op string, f func(context.Context, *buildqueuestate.AddOrRemoveDrainRequest, ...grpc.CallOption) (*emptypb.Empty, error), queue domain.QueueKey, pattern ports.WorkerID) error {
	name, err := c.queueName(queue)
	if err != nil {
		return err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err = f(ctx, &buildqueuestate.AddOrRemoveDrainRequest{SizeClassQueueName: name, WorkerIdPattern: pattern})
	return mapError(op, err, true)
}

// ListDrains returns the drain patterns of a queue.
func (c *Client) ListDrains(ctx context.Context, queue domain.QueueKey) ([]ports.Drain, error) {
	name, err := c.queueName(queue)
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.c.ListDrains(ctx, &buildqueuestate.ListDrainsRequest{SizeClassQueueName: name})
	if err != nil {
		return nil, mapError("ListDrains", err, true)
	}
	out := make([]ports.Drain, 0, len(resp.Drains))
	for _, d := range resp.Drains {
		out = append(out, ports.Drain{Queue: queue, Pattern: ports.WorkerID(d.GetWorkerIdPattern()), Created: d.GetCreatedTimestamp().AsTime()})
	}
	return out, nil
}

// KillOperations completes operations with status {code, message}: one
// operation by name, or every queued operation of a queue that has no workers
// (R-RE-2: fail fast instead of letting clients wait). For a queue that has
// workers the latter does nothing and returns nil (the scheduler answers
// FailedPrecondition).
func (c *Client) KillOperations(ctx context.Context, f ports.KillFilter, code int32, message string) error {
	filter := &buildqueuestate.KillOperationsRequest_Filter{}
	queueScoped := false
	switch {
	case f.OperationName != "" && f.QueueWithoutWorkers != nil, f.OperationName == "" && f.QueueWithoutWorkers == nil:
		return fmt.Errorf("KillOperations: %w: set exactly one of OperationName and QueueWithoutWorkers", ports.ErrInvalid)
	case f.OperationName != "":
		filter.Type = &buildqueuestate.KillOperationsRequest_Filter_OperationName{OperationName: f.OperationName}
	default:
		name, err := c.queueName(*f.QueueWithoutWorkers)
		if err != nil {
			return err
		}
		filter.Type = &buildqueuestate.KillOperationsRequest_Filter_SizeClassQueueWithoutWorkers{SizeClassQueueWithoutWorkers: name}
		queueScoped = true
	}
	if codes.Code(code) == codes.OK {
		return fmt.Errorf("KillOperations: %w: status code must not be OK", ports.ErrInvalid)
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.c.KillOperations(ctx, &buildqueuestate.KillOperationsRequest{
		Filter: filter,
		Status: status.New(codes.Code(code), message).Proto(),
	})
	if queueScoped && status.Code(err) == codes.FailedPrecondition {
		// "size class queue still has workers": nothing to fail, and workers
		// appearing is exactly what the caller hoped for (port contract).
		return nil
	}
	return mapError("KillOperations", err, queueScoped)
}

// ListOperations lists operations in name order, starting after f.StartAfter.
// It returns at most f.PageSize operations (0: all). The scheduler cannot
// filter by queue, so f.Queue is applied here while paging.
func (c *Client) ListOperations(ctx context.Context, f ports.OperationFilter) ([]ports.Operation, error) {
	stage, err := stageFilter(f.Stage)
	if err != nil {
		return nil, err
	}
	var out []ports.Operation
	startAfter := f.StartAfter
	for f.PageSize <= 0 || len(out) < f.PageSize {
		resp, err := c.listOperationsPage(ctx, startAfter, stage)
		if err != nil {
			return nil, err
		}
		for _, op := range resp.Operations {
			o := toOperation(op, op.GetName())
			if f.Queue != nil && o.Queue != *f.Queue {
				continue
			}
			out = append(out, o)
			if f.PageSize > 0 && len(out) == f.PageSize {
				break
			}
		}
		if len(resp.Operations) < c.pageSize {
			break
		}
		startAfter = resp.Operations[len(resp.Operations)-1].GetName()
	}
	if err := c.attachWorkers(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) listOperationsPage(ctx context.Context, startAfter string, stage remoteexecution.ExecutionStage_Value) (*buildqueuestate.ListOperationsResponse, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	req := &buildqueuestate.ListOperationsRequest{PageSize: uint32(c.pageSize), FilterStage: stage}
	if startAfter != "" {
		req.StartAfter = &buildqueuestate.ListOperationsRequest_StartAfter{OperationName: startAfter}
	}
	resp, err := c.c.ListOperations(ctx, req)
	return resp, mapError("ListOperations", err, false)
}

// GetOperation returns one operation (ports.ErrNotFound when the scheduler
// does not know it).
func (c *Client) GetOperation(ctx context.Context, name string) (ports.Operation, error) {
	callCtx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.c.GetOperation(callCtx, &buildqueuestate.GetOperationRequest{OperationName: name})
	if err != nil {
		return ports.Operation{}, mapError("GetOperation", err, false)
	}
	ops := []ports.Operation{toOperation(resp.GetOperation(), name)}
	if done := resp.GetOperation().GetCompleted(); done != nil {
		if b, err := proto.Marshal(done); err == nil {
			ops[0].ExecuteResponse = b
		}
	}
	if err := c.attachWorkers(ctx, ops); err != nil {
		return ports.Operation{}, err
	}
	return ops[0], nil
}

// attachWorkers fills Worker of executing operations: OperationState does not
// carry it, so the workers of each involved queue are listed once.
func (c *Client) attachWorkers(ctx context.Context, ops []ports.Operation) error {
	byQueue := map[domain.QueueKey]map[string]ports.WorkerID{}
	for i := range ops {
		if ops[i].Stage != StageExecuting {
			continue
		}
		q := ops[i].Queue
		if _, ok := byQueue[q]; !ok {
			workers, err := c.ListWorkers(ctx, q)
			if err != nil && !errors.Is(err, ports.ErrQueueUnknown) {
				return err
			}
			m := map[string]ports.WorkerID{}
			for _, w := range workers {
				if w.Executing {
					m[w.Operation] = w.ID
				}
			}
			byQueue[q] = m
		}
		ops[i].Worker = byQueue[q][ops[i].Name]
	}
	return nil
}

func stageFilter(stage string) (remoteexecution.ExecutionStage_Value, error) {
	switch stage {
	case "":
		return remoteexecution.ExecutionStage_UNKNOWN, nil
	case StageQueued:
		return remoteexecution.ExecutionStage_QUEUED, nil
	case StageExecuting:
		return remoteexecution.ExecutionStage_EXECUTING, nil
	case StageCompleted:
		return remoteexecution.ExecutionStage_COMPLETED, nil
	}
	return 0, fmt.Errorf("ListOperations: %w: unknown stage %q", ports.ErrInvalid, stage)
}

func toOperation(op *buildqueuestate.OperationState, name string) ports.Operation {
	o := ports.Operation{Name: name, TargetID: op.GetTargetId(), Priority: op.GetPriority()}
	if d := op.GetActionDigest(); d != nil {
		o.ActionDigest = d.GetHash() + "-" + strconv.FormatInt(d.GetSizeBytes(), 10)
	}
	if ts := op.GetQueuedTimestamp(); ts != nil {
		o.QueuedAt = ts.AsTime()
	}
	if inv := op.GetInvocationName(); inv != nil {
		scq := inv.GetSizeClassQueueName()
		o.Queue = domain.QueueKey{
			InstanceNamePrefix: scq.GetPlatformQueueName().GetInstanceNamePrefix(),
			PlatformKey:        platformKey(scq.GetPlatformQueueName().GetPlatform().GetProperties()),
			SizeClass:          scq.GetSizeClass(),
		}
		o.InvocationID = invocationID(inv.GetIds())
	}
	switch op.GetStage().(type) {
	case *buildqueuestate.OperationState_Queued:
		o.Stage = StageQueued
	case *buildqueuestate.OperationState_Executing:
		o.Stage = StageExecuting
	case *buildqueuestate.OperationState_Completed:
		o.Stage = StageCompleted
	}
	return o
}

// invocationID picks the Bazel invocation (tool_invocation_id) from the
// scheduler's invocation key chain; failing that the correlated invocations
// ID, then the key's message type (for example authentication metadata when
// the scheduler keys fairness on it).
func invocationID(ids []*anypb.Any) string {
	var tool, correlated, other string
	for _, id := range ids {
		var md remoteexecution.RequestMetadata
		switch {
		case id.MessageIs(&md):
			if id.UnmarshalTo(&md) != nil {
				continue
			}
			if md.GetToolInvocationId() != "" {
				tool = md.GetToolInvocationId()
			}
			if md.GetCorrelatedInvocationsId() != "" {
				correlated = md.GetCorrelatedInvocationsId()
			}
		case id.MessageIs(&buildqueuestate.BackgroundLearning{}):
			other = "background-learning"
		default:
			other = string(id.MessageName())
		}
	}
	switch {
	case tool != "":
		return tool
	case correlated != "":
		return correlated
	}
	return other
}

// queueName turns a QueueKey back into the scheduler's SizeClassQueueName,
// with the exact properties last listed for its PlatformKey (or, for a queue
// not listed yet, parsed from the key).
func (c *Client) queueName(q domain.QueueKey) (*buildqueuestate.SizeClassQueueName, error) {
	c.mu.Lock()
	props, ok := c.platforms[q.PlatformKey]
	c.mu.Unlock()
	if !ok {
		var err error
		if props, err = parsePlatformKey(q.PlatformKey); err != nil {
			return nil, err
		}
	}
	return &buildqueuestate.SizeClassQueueName{
		PlatformQueueName: &buildqueuestate.PlatformQueueName{
			InstanceNamePrefix: q.InstanceNamePrefix,
			Platform:           &remoteexecution.Platform{Properties: props},
		},
		SizeClass: q.SizeClass,
	}, nil
}

// platformKey mirrors domain.PropertiesKey for the scheduler's (sorted)
// property lists.
func platformKey(props []*remoteexecution.Platform_Property) string {
	m := make(map[string]string, len(props))
	for _, p := range props {
		m[p.GetName()] = p.GetValue()
	}
	return domain.PropertiesKey(m)
}

func parsePlatformKey(key string) ([]*remoteexecution.Platform_Property, error) {
	var props []*remoteexecution.Platform_Property
	if key != "" {
		for _, kv := range strings.Split(key, ";") {
			name, value, ok := strings.Cut(kv, "=")
			if !ok || name == "" {
				return nil, fmt.Errorf("%w: platform key %q is not name=value;…", ports.ErrInvalid, key)
			}
			props = append(props, &remoteexecution.Platform_Property{Name: name, Value: value})
		}
	}
	// Buildbarn requires properties sorted by name, then value.
	slices.SortFunc(props, func(a, b *remoteexecution.Platform_Property) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
	return props, nil
}

// mapError wraps scheduler errors with the port's sentinels, keeping the gRPC
// status reachable (status.Code works on the result).
func mapError(op string, err error, queueScoped bool) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.NotFound:
		if queueScoped {
			return fmt.Errorf("%s: %w: %w", op, ports.ErrQueueUnknown, err)
		}
		return fmt.Errorf("%s: %w: %w", op, ports.ErrNotFound, err)
	case codes.InvalidArgument:
		return fmt.Errorf("%s: %w: %w", op, ports.ErrInvalid, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}
