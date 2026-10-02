// SPDX-License-Identifier: FSL-1.1-ALv2

// Integration tier: a pinned bb_scheduler (and a bb_storage for its CAS)
// booted by internal/bbtest on loopback, driven by an REAPI client and
// hand-driven fake workers that call Synchronize.
package buildqueue_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sloper-ai/cucina/internal/bbtest"
	"github.com/sloper-ai/cucina/internal/buildqueue"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/ports/porttest"
)

var (
	x86        = map[string]string{"OSFamily": "linux", "ISA": "x86-64"}
	rv64       = map[string]string{"OSFamily": "linux", "ISA": "rv64g", "cucina-emulation": "qemu"}
	declared   = domain.QueueKey{InstanceNamePrefix: "main", PlatformKey: domain.PropertiesKey(x86), SizeClass: 1}
	noWorkers  = domain.QueueKey{InstanceNamePrefix: "main", PlatformKey: domain.PropertiesKey(rv64), SizeClass: 1}
	undeclared = domain.QueueKey{InstanceNamePrefix: "main", PlatformKey: "ISA=s390x;OSFamily=linux", SizeClass: 1}
)

// env is one scheduler with two predeclared queues (declared, noWorkers).
type env struct {
	queue      *buildqueue.Client
	pki        *bbtest.PKI
	bqsAddr    string
	workerConn *grpc.ClientConn
	cas, exec  *bbtest.REClient
	ctx        context.Context
}

func newEnv(t *testing.T, pageSize int) *env {
	t.Helper()
	pki := bbtest.NewPKI(t)
	server := pki.LoopbackServer(t, "scheduler", "spiffe://cucina/server/scheduler")
	controller := pki.Workload(t, "controller", bbtest.ControllerURI)
	worker := pki.Workload(t, "worker", "spiffe://cucina/worker/porttest/node")
	storage, client, workers, bqs := bbtest.FreeAddr(t), bbtest.FreeAddr(t), bbtest.FreeAddr(t), bbtest.FreeAddr(t)

	bbtest.BootStorage(t, bbtest.StorageConfig(bbtest.StorageOptions{ClientListen: storage}), bbtest.GRPCReady(storage, nil))
	serverTLS := pki.ClientTLS(nil, "localhost")
	bbtest.BootScheduler(t, bbtest.SchedulerConfig(bbtest.SchedulerOptions{
		ClientListen: client, WorkerListen: workers, BuildQueueStateListen: bqs,
		ServerKeyPair: &server, CAPEM: pki.CAPEM, StorageAddress: storage,
		Queues: []bbtest.Queue{
			{InstanceNamePrefix: "main", Properties: x86, SizeClasses: []uint32{1}},
			{InstanceNamePrefix: "main", Properties: rv64, SizeClasses: []uint32{1}},
		},
	}), bbtest.AllReady(bbtest.GRPCReady(client, nil), bbtest.GRPCReady(workers, serverTLS), bbtest.GRPCReady(bqs, serverTLS)))

	tlsConfig, err := buildqueue.TLSFromFiles(controller.CertPath, controller.KeyPath, filepath.Join(pki.Dir, "ca.crt"), "localhost")
	require.NoError(t, err)
	q, err := buildqueue.New(buildqueue.Options{Address: bqs, TLS: tlsConfig, PageSize: pageSize})
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // ends pending Execute streams and long polls first
	e := &env{queue: q, pki: pki, bqsAddr: bqs, ctx: ctx}
	e.cas = bbtest.NewREClient(dial(t, storage, nil), "main")
	e.exec = bbtest.NewREClient(dial(t, client, nil), "main")
	e.workerConn = dial(t, workers, pki.ClientTLS(&worker, "localhost"))
	return e
}

func dial(t *testing.T, addr string, tlsConfig *tls.Config) *grpc.ClientConn {
	t.Helper()
	conn, err := bbtest.Dial(addr, tlsConfig)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

var actionSeq atomic.Uint64

// submit enqueues a distinct action (no in-flight deduplication) on key and
// keeps its Execute stream open; it returns the operation name.
func (e *env) submit(t *testing.T, key domain.QueueKey, md *remoteexecution.RequestMetadata) (string, *bbtest.Execution) {
	t.Helper()
	props := map[string]string{}
	for _, kv := range strings.Split(key.PlatformKey, ";") {
		k, v, _ := strings.Cut(kv, "=")
		props[k] = v
	}
	digest, err := e.cas.Upload(e.ctx, bbtest.Action{
		Args:     []string{"true", fmt.Sprint(actionSeq.Add(1))},
		Platform: props,
	})
	require.NoError(t, err)
	exec, err := e.exec.Start(e.ctx, digest, md)
	require.NoError(t, err)
	return exec.Name, exec
}

func (e *env) worker(id ports.WorkerID, key domain.QueueKey) *bbtest.FakeWorker {
	props := map[string]string{}
	for _, kv := range strings.Split(key.PlatformKey, ";") {
		k, v, _ := strings.Cut(kv, "=")
		props[k] = v
	}
	return bbtest.NewFakeWorker(e.workerConn, id, key.InstanceNamePrefix, props, key.SizeClass)
}

func (e *env) observe(t *testing.T, key domain.QueueKey) domain.QueueObservation {
	t.Helper()
	qs, err := e.queue.ListPlatformQueues(e.ctx)
	require.NoError(t, err)
	for _, q := range qs {
		if q.Key == key {
			return q
		}
	}
	t.Fatalf("queue %+v not listed", key)
	return domain.QueueObservation{}
}

// await polls cond on wall-clock time: the porttest contract for real
// backends, whose state changes asynchronously.
func await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		<-tick.C
	}
}

// TestConformance runs the shared ports.BuildQueue suite (R-TEST-8b) against
// the adapter and the pinned bb_scheduler.
func TestConformance(t *testing.T) {
	porttest.RunBuildQueue(t, func(t *testing.T) porttest.BuildQueueHarness {
		e := newEnv(t, 0)
		return porttest.BuildQueueHarness{
			Queue: e.queue, Declared: noWorkers, Undeclared: undeclared,
			Submit: func(t *testing.T, key domain.QueueKey) string {
				name, _ := e.submit(t, key, nil)
				return name
			},
			// A synchronizing thread: registered at once, then long-polling for work.
			AddWorker: func(t *testing.T, key domain.QueueKey, id ports.WorkerID) {
				w := e.worker(id, key)
				require.NoError(t, w.Register(e.ctx))
				go func() { _, _ = w.Take(e.ctx) }()
			},
			Await: await,
		}
	})
}

// R-SCALE-1, contracts §3: the observation of a size class queue counts queued
// operations and executing, idle and total runner threads.
func TestQueueObservation(t *testing.T) {
	e := newEnv(t, 0)
	threads := make([]*bbtest.FakeWorker, 3)
	for i := range threads {
		threads[i] = e.worker(ports.WorkerID{"pool": "linux", "node": "i-1", "thread": fmt.Sprint(i)}, declared)
		require.NoError(t, threads[i].Register(e.ctx))
	}
	e.submit(t, declared, nil)
	e.submit(t, declared, nil)
	assert.Equal(t, domain.QueueObservation{Key: declared, Queued: 2, Executing: 0, Idle: 3, Workers: 3}, e.observe(t, declared))

	task, err := threads[0].Take(e.ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.QueueObservation{Key: declared, Queued: 1, Executing: 1, Idle: 2, Workers: 3}, e.observe(t, declared))

	require.NoError(t, threads[0].Complete(e.ctx, task, &remoteexecution.ExecuteResponse{Result: &remoteexecution.ActionResult{}}))
	assert.Equal(t, domain.QueueObservation{Key: declared, Queued: 1, Executing: 0, Idle: 3, Workers: 3}, e.observe(t, declared))

	require.NoError(t, e.queue.AddDrain(e.ctx, declared, ports.WorkerID{"node": "i-1"}))
	assert.Equal(t, 1, e.observe(t, declared).Drains)
	assert.Equal(t, domain.QueueObservation{Key: noWorkers}, e.observe(t, noWorkers))
}

// R-SCALE-3, R-RE-4: ListWorkers follows pagination and reports every thread
// with its labels; operations report digest, stage, target and invocation, and
// an executing operation names its worker thread.
func TestWorkersAndOperations(t *testing.T) {
	e := newEnv(t, 2) // pages of two
	var threads []*bbtest.FakeWorker
	for i := 0; i < 5; i++ {
		w := e.worker(ports.WorkerID{"pool": "linux", "node": "i-2", "thread": fmt.Sprint(i)}, declared)
		require.NoError(t, w.Register(e.ctx))
		threads = append(threads, w)
	}
	md := &remoteexecution.RequestMetadata{ToolInvocationId: "invocation-1", TargetId: "//pkg:target"}
	running, _ := e.submit(t, declared, md)
	queued, _ := e.submit(t, declared, md)
	other, _ := e.submit(t, noWorkers, nil)
	task, err := threads[3].Take(e.ctx)
	require.NoError(t, err)

	workers, err := e.queue.ListWorkers(e.ctx, declared)
	require.NoError(t, err)
	require.Len(t, workers, 5)
	executing := 0
	for i, w := range workers {
		assert.Equal(t, ports.WorkerID{"pool": "linux", "node": "i-2", "thread": fmt.Sprint(i)}, w.ID)
		assert.Equal(t, declared, w.Queue)
		if w.Executing {
			executing++
			assert.Equal(t, running, w.Operation)
		}
	}
	assert.Equal(t, 1, executing)

	op, err := e.queue.GetOperation(e.ctx, running)
	require.NoError(t, err)
	assert.Equal(t, buildqueue.StageExecuting, op.Stage)
	assert.Equal(t, declared, op.Queue)
	assert.Equal(t, task.ActionDigest.Hash+"-"+fmt.Sprint(task.ActionDigest.SizeBytes), op.ActionDigest)
	assert.Equal(t, "//pkg:target", op.TargetID)
	assert.Equal(t, "invocation-1", op.InvocationID)
	assert.Equal(t, ports.WorkerID{"pool": "linux", "node": "i-2", "thread": "3"}, op.Worker)
	assert.False(t, op.QueuedAt.IsZero())

	names := func(ops []ports.Operation) []string {
		var out []string
		for _, o := range ops {
			out = append(out, o.Name)
		}
		return out
	}
	all, err := e.queue.ListOperations(e.ctx, ports.OperationFilter{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{running, queued, other}, names(all))
	q := declared
	queuedOnly, err := e.queue.ListOperations(e.ctx, ports.OperationFilter{Queue: &q, Stage: buildqueue.StageQueued})
	require.NoError(t, err)
	assert.Equal(t, []string{queued}, names(queuedOnly))
	first, err := e.queue.ListOperations(e.ctx, ports.OperationFilter{PageSize: 1})
	require.NoError(t, err)
	require.Len(t, first, 1)
	rest, err := e.queue.ListOperations(e.ctx, ports.OperationFilter{StartAfter: first[0].Name})
	require.NoError(t, err)
	assert.ElementsMatch(t, names(all), append(names(first), names(rest)...))

	_, err = e.queue.GetOperation(e.ctx, "no-such-operation")
	assert.ErrorIs(t, err, ports.ErrNotFound)
}

// R-RE-2: a pool that cannot obtain capacity fails its queued work with a clear
// status instead of letting clients hang; queues that have workers are left
// alone; single operations can be killed by name.
func TestKillOperations(t *testing.T) {
	e := newEnv(t, 0)
	_, stranded := e.submit(t, noWorkers, nil)
	require.NoError(t, e.queue.KillOperations(e.ctx, ports.KillFilter{QueueWithoutWorkers: &noWorkers},
		int32(codes.Unavailable), "pool linux-riscv cannot obtain capacity"))
	_, err := stranded.Wait()
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Contains(t, err.Error(), "pool linux-riscv cannot obtain capacity")

	w := e.worker(ports.WorkerID{"pool": "linux", "node": "i-3", "thread": "0"}, declared)
	require.NoError(t, w.Register(e.ctx))
	name, waiting := e.submit(t, declared, nil)
	require.NoError(t, e.queue.KillOperations(e.ctx, ports.KillFilter{QueueWithoutWorkers: &declared}, int32(codes.Unavailable), "no capacity"))
	op, err := e.queue.GetOperation(e.ctx, name)
	require.NoError(t, err)
	assert.Equal(t, buildqueue.StageQueued, op.Stage, "a queue with workers keeps its work")

	require.NoError(t, e.queue.KillOperations(e.ctx, ports.KillFilter{OperationName: name}, int32(codes.Canceled), "cancelled by an admin"))
	_, err = waiting.Wait()
	assert.Equal(t, codes.Canceled, status.Code(err))
	require.ErrorIs(t, e.queue.KillOperations(e.ctx, ports.KillFilter{OperationName: "no-such-operation"}, int32(codes.Canceled), "x"), ports.ErrNotFound)
	require.ErrorIs(t, e.queue.KillOperations(e.ctx, ports.KillFilter{QueueWithoutWorkers: &undeclared}, int32(codes.Canceled), "x"), ports.ErrQueueUnknown)
}

// R-SEC-4: BuildQueueState answers only the controller's certificate.
func TestRequiresControllerCertificate(t *testing.T) {
	e := newEnv(t, 0)
	worker := e.pki.Workload(t, "not-the-controller", "spiffe://cucina/worker/linux/i-0123456789abcdef0")
	tlsConfig, err := buildqueue.TLSFromFiles(worker.CertPath, worker.KeyPath, filepath.Join(e.pki.Dir, "ca.crt"), "localhost")
	require.NoError(t, err)
	q, err := buildqueue.New(buildqueue.Options{Address: e.bqsAddr, TLS: tlsConfig})
	require.NoError(t, err)
	defer func() { _ = q.Close() }()
	_, err = q.ListPlatformQueues(e.ctx)
	assert.Equal(t, codes.Unauthenticated, status.Code(err), "%v", err)
	assert.Equal(t, codes.Unauthenticated, status.Code(q.AddDrain(e.ctx, declared, ports.WorkerID{"node": "x"})))
}
