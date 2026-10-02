// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/mgmt"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Table tests per RPC group (R-TEST-3: one table per group, rows run in order and
// share the fixture's state). Handlers are called directly with an authenticated
// principal; the guard itself is covered over TLS in e2e_test.go.

type rpcCase struct {
	name  string
	ctx   context.Context // nil: cluster admin
	call  func(ctx context.Context, s *mgmt.Server) (proto.Message, error)
	code  codes.Code
	check func(t *testing.T, resp proto.Message)
}

func runCases(t *testing.T, s *mgmt.Server, cases []rpcCase) {
	t.Helper()
	for _, c := range cases {
		ctx := c.ctx
		if ctx == nil {
			ctx = as(admin)
		}
		resp, err := c.call(ctx, s)
		if got := status.Code(err); got != c.code {
			t.Errorf("%s: code %v (%v), want %v", c.name, got, err, c.code)
			continue
		}
		if err == nil && c.check != nil {
			c.check(t, resp)
		}
	}
}

func names[T any](items []T, name func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, name(i))
	}
	return out
}

func equal(t *testing.T, what string, got, want any) {
	t.Helper()
	if !equalValues(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func equalValues(a, b any) bool {
	if at, ok := a.(time.Time); ok {
		bt, ok := b.(time.Time)
		return ok && at.Equal(bt)
	}
	return reflect.DeepEqual(a, b)
}

// TestPoolRPCs guards UC11/UC13/R-SCALE-7/R-POOL-2: pool listing and description,
// the expiring floor override, cordon, and orphan garbage collection.
func TestPoolRPCs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.orphans.orphans = []ports.Orphan{
			{Kind: ports.OrphanVolume, ID: "vol-1", Pool: "linux-x86-64"},
			{Kind: ports.OrphanENI, ID: "eni-2", Pool: "linux-x86-64"},
			{Kind: ports.OrphanVolume, ID: "vol-3", Pool: "windows-x86-64"},
		}
		now := time.Now()
		floor := func(min uint32, d time.Duration) *cucinav1.SetPoolFloorRequest {
			r := &cucinav1.SetPoolFloorRequest{Name: "linux-x86-64", MinRunning: min}
			if d != 0 {
				r.ExpiresIn = durationpb.New(d)
			}
			return r
		}
		runCases(t, f.srv, []rpcCase{
			{name: "list", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.ListPools(ctx, &cucinav1.ListPoolsRequest{})
			}, check: func(t *testing.T, m proto.Message) {
				ps := m.(*cucinav1.ListPoolsResponse).GetPools()
				equal(t, "pools", names(ps, (*cucinav1.PoolSummary).GetName), []string{"linux-x86-64", "macos-arm64-xcode27.0", "windows-x86-64"})
				equal(t, "linux condition", ps[0].GetCondition(), "Ready")
				equal(t, "linux max", ps[0].GetMax(), uint32(4))
			}},
			{name: "describe", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.GetPool(ctx, &cucinav1.GetPoolRequest{Name: "linux-x86-64"})
			}, check: func(t *testing.T, m proto.Message) {
				r := m.(*cucinav1.GetPoolResponse)
				if !strings.Contains(r.GetSpecJson(), "c8i.8xlarge") {
					t.Errorf("spec_json = %s", r.GetSpecJson())
				}
				equal(t, "conditions", r.GetConditions(), []string{"Ready=True Ready"})
				equal(t, "workers", names(r.GetWorkers(), (*cucinav1.WorkerSummary).GetNode), []string{"i-0aaaaaaaaaaaaaaa1", "i-0aaaaaaaaaaaaaaa2"})
				equal(t, "events", len(r.GetEvents()), 1)
				equal(t, "starts", r.GetStarts()[0].GetToRegistered().AsDuration(), 31*time.Second)
			}},
			{name: "describe unknown", code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.GetPool(ctx, &cucinav1.GetPoolRequest{Name: "nope"})
			}},
			{name: "floor without expiry", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.SetPoolFloor(ctx, floor(2, 0))
			}},
			{name: "floor beyond max duration", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.SetPoolFloor(ctx, floor(2, 8*24*time.Hour))
			}},
			{name: "floor above pool max", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.SetPoolFloor(ctx, floor(5, time.Hour))
			}},
			{name: "floor", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.SetPoolFloor(ctx, floor(2, time.Hour))
			}, check: func(t *testing.T, m proto.Message) {
				equal(t, "expires_at", m.(*cucinav1.SetPoolFloorResponse).GetExpiresAt().AsTime(), now.Add(time.Hour).UTC())
				fl := f.fleet.floors["linux-x86-64"]
				equal(t, "stored floor", [2]any{fl.MinRunning, fl.SetBy}, [2]any{int32(2), "sa:break-glass"})
				equal(t, "stored floor expiry", fl.ExpiresAt, now.Add(time.Hour))
			}},
			{name: "floor shows as min_running", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.ListPools(ctx, &cucinav1.ListPoolsRequest{})
			}, check: func(t *testing.T, m proto.Message) {
				equal(t, "min_running", m.(*cucinav1.ListPoolsResponse).GetPools()[0].GetMinRunning(), uint32(2))
			}},
			{name: "clear floor", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.SetPoolFloor(ctx, floor(0, 0))
			}, check: func(t *testing.T, _ proto.Message) {
				equal(t, "floors", len(f.fleet.floors), 0)
			}},
			{name: "cordon", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.CordonPool(ctx, &cucinav1.CordonPoolRequest{Name: "windows-x86-64", Cordon: true})
			}, check: func(t *testing.T, _ proto.Message) {
				equal(t, "paused", f.fleet.paused["windows-x86-64"], true)
			}},
			{name: "cordon unknown", code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.CordonPool(ctx, &cucinav1.CordonPoolRequest{Name: "nope", Cordon: true})
			}},
			{name: "gc dry run", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.GarbageCollectPool(ctx, &cucinav1.GarbageCollectPoolRequest{Name: "linux-x86-64", DryRun: true})
			}, check: func(t *testing.T, m proto.Message) {
				equal(t, "would delete", m.(*cucinav1.GarbageCollectPoolResponse).GetDeleted(), []string{"eni eni-2", "volume vol-1"})
				equal(t, "deleted", len(f.orphans.deleted), 0)
			}},
			{name: "gc", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				f.orphans.failIDs = map[string]error{"eni-2": errors.New("InvalidNetworkInterface.InUse")}
				return s.GarbageCollectPool(ctx, &cucinav1.GarbageCollectPoolRequest{Name: "linux-x86-64"})
			}, check: func(t *testing.T, m proto.Message) {
				r := m.(*cucinav1.GarbageCollectPoolResponse)
				equal(t, "deleted", r.GetDeleted(), []string{"volume vol-1"})
				equal(t, "errors", r.GetErrors(), []string{"eni eni-2: InvalidNetworkInterface.InUse"})
				equal(t, "fleet deleted", f.orphans.deleted, []string{"vol-1"})
			}},
		})

		noEC2 := newFixture(t, func(d *mgmt.Deps, _ *mgmt.Options) { d.Orphans = nil })
		if _, err := noEC2.srv.GarbageCollectPool(as(admin), &cucinav1.GarbageCollectPoolRequest{}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("gc without EC2 provider: %v, want FailedPrecondition", err)
		}
	})
}

// TestWorkerRPCs guards UC13/UC14/R-RE-4: worker listing and operator drains, which
// use the pattern {node} on every queue of the worker's pool (docs/dev/scaling.md).
func TestWorkerRPCs(t *testing.T) {
	f := newFixture(t)
	drains := func() map[domain.QueueKey][]string {
		f.sched.mu.Lock()
		defer f.sched.mu.Unlock()
		out := map[domain.QueueKey][]string{}
		for k, ps := range f.sched.drains {
			for p := range ps {
				out[k] = append(out[k], p)
			}
		}
		return out
	}
	runCases(t, f.srv, []rpcCase{
		{name: "list", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ListWorkers(ctx, &cucinav1.ListWorkersRequest{})
		}, check: func(t *testing.T, m proto.Message) {
			ws := m.(*cucinav1.ListWorkersResponse).GetWorkers()
			equal(t, "nodes", names(ws, (*cucinav1.WorkerSummary).GetNode),
				[]string{"i-0aaaaaaaaaaaaaaa1", "i-0aaaaaaaaaaaaaaa2", "mini1/vm-1", "mini1/vm-2", "i-0bbbbbbbbbbbbbbb1"})
			equal(t, "states", names(ws, (*cucinav1.WorkerSummary).GetState), []string{"busy", "idle", "idle", "stopped", "launching"})
		}},
		{name: "list by pool", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ListWorkers(ctx, &cucinav1.ListWorkersRequest{Pool: "windows-x86-64"})
		}, check: func(t *testing.T, m proto.Message) {
			equal(t, "workers", len(m.(*cucinav1.ListWorkersResponse).GetWorkers()), 1)
		}},
		{name: "list unknown pool", code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ListWorkers(ctx, &cucinav1.ListWorkersRequest{Pool: "nope"})
		}},
		{name: "drain EC2 worker", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.DrainWorker(ctx, &cucinav1.DrainWorkerRequest{Node: "i-0aaaaaaaaaaaaaaa1"})
		}, check: func(t *testing.T, _ proto.Message) {
			want := map[domain.QueueKey][]string{
				qk("main", linuxNative): {"node=i-0aaaaaaaaaaaaaaa1"},
				qk("main", linuxRV64):   {"node=i-0aaaaaaaaaaaaaaa1"},
			}
			got := drains()
			if len(got) != len(want) {
				t.Errorf("drains = %v, want %v", got, want)
			}
			for k, v := range want {
				equal(t, "drain "+k.PlatformKey, got[k], v)
			}
		}},
		{name: "undrain", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.UndrainWorker(ctx, &cucinav1.DrainWorkerRequest{Node: "i-0aaaaaaaaaaaaaaa1"})
		}, check: func(t *testing.T, _ proto.Message) {
			equal(t, "drains", f.sched.drainCount(), 0)
		}},
		{name: "drain Tart worker", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.DrainWorker(ctx, &cucinav1.DrainWorkerRequest{Node: "mini1/vm-1"})
		}, check: func(t *testing.T, _ proto.Message) {
			got := drains()
			equal(t, "xcode queue", got[qk("main", macXcode)], []string{"node=mini1/vm-1"})
			equal(t, "generic queue", got[qk("main", macGeneric)], []string{"node=mini1/vm-1"})
		}},
		{name: "drain without node", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.DrainWorker(ctx, &cucinav1.DrainWorkerRequest{})
		}},
		{name: "drain terminated worker", code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.DrainWorker(ctx, &cucinav1.DrainWorkerRequest{Node: "i-0ccccccccccccccc1"})
		}},
		{name: "scheduler down", code: codes.Unavailable, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			f.sched.mu.Lock()
			f.sched.err = errors.New("connection refused")
			f.sched.mu.Unlock()
			return s.DrainWorker(ctx, &cucinav1.DrainWorkerRequest{Node: "i-0aaaaaaaaaaaaaaa2"})
		}},
	})
}

// TestQueueAndOperationRPCs guards UC13 and R-RE-2/R-RE-3: queue attribution, paged
// and filtered operation listings scoped to the caller's instance names, and kills.
func TestQueueAndOperationRPCs(t *testing.T) {
	f := newFixture(t)
	opNames := func(m proto.Message) []string {
		return names(m.(*cucinav1.ListOperationsResponse).GetOperations(), (*cucinav1.OperationSummary).GetName)
	}
	list := func(req *cucinav1.ListOperationsRequest) func(context.Context, *mgmt.Server) (proto.Message, error) {
		return func(ctx context.Context, s *mgmt.Server) (proto.Message, error) { return s.ListOperations(ctx, req) }
	}
	rv64Ref := &cucinav1.QueueRef{InstanceNamePrefix: "main", Platform: []*cucinav1.PlatformProperty{
		{Name: "OSFamily", Value: "linux"}, {Name: "ISA", Value: "rv64g"}, {Name: "cucina-emulation", Value: "qemu"},
	}}
	var token string
	runCases(t, f.srv, []rpcCase{
		{name: "queues", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ListQueues(ctx, &cucinav1.ListQueuesRequest{})
		}, check: func(t *testing.T, m proto.Message) {
			qs := m.(*cucinav1.ListQueuesResponse).GetQueues()
			// Sorted by queue key: macOS generic, linux rv64g (qemu), linux x86-64, windows x86-64.
			equal(t, "serving pools", names(qs, (*cucinav1.QueueSummary).GetPool),
				[]string{"macos-arm64-xcode27.0", "linux-x86-64", "linux-x86-64", "windows-x86-64"})
			equal(t, "queued", names(qs, func(q *cucinav1.QueueSummary) string { return fmt.Sprint(q.GetQueued()) }),
				[]string{"0", "1", "7", "0"})
		}},
		{name: "page 1", call: list(&cucinav1.ListOperationsRequest{Page: &cucinav1.Page{Size: 2}}), check: func(t *testing.T, m proto.Message) {
			equal(t, "ops", opNames(m), []string{"op-1", "op-2"})
			token = m.(*cucinav1.ListOperationsResponse).GetNextPageToken()
			if token == "" {
				t.Error("no next page token")
			}
		}},
		{name: "page 2", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ListOperations(ctx, &cucinav1.ListOperationsRequest{Page: &cucinav1.Page{Size: 2, Token: token}})
		}, check: func(t *testing.T, m proto.Message) {
			equal(t, "ops", opNames(m), []string{"op-3", "op-4"})
			equal(t, "next token", m.(*cucinav1.ListOperationsResponse).GetNextPageToken(), "")
		}},
		{name: "reader sees its instance names only", ctx: as(reader), call: list(&cucinav1.ListOperationsRequest{}), check: func(t *testing.T, m proto.Message) {
			equal(t, "ops", opNames(m), []string{"op-1", "op-2", "op-3"})
		}},
		{name: "tenant admin sees its tenant only", ctx: as(tenantAdmin), call: list(&cucinav1.ListOperationsRequest{}), check: func(t *testing.T, m proto.Message) {
			equal(t, "ops", opNames(m), []string{"op-4"})
		}},
		{name: "stage filter", call: list(&cucinav1.ListOperationsRequest{Stage: "queued", InstanceName: "main"}), check: func(t *testing.T, m proto.Message) {
			equal(t, "ops", opNames(m), []string{"op-2", "op-3"})
		}},
		{name: "invocation filter", call: list(&cucinav1.ListOperationsRequest{InvocationId: "inv-1"}), check: func(t *testing.T, m proto.Message) {
			equal(t, "ops", opNames(m), []string{"op-1", "op-2"})
		}},
		{name: "queue filter", call: list(&cucinav1.ListOperationsRequest{Queue: rv64Ref}), check: func(t *testing.T, m proto.Message) {
			equal(t, "ops", opNames(m), []string{"op-3"})
		}},
		{name: "bad stage", code: codes.InvalidArgument, call: list(&cucinav1.ListOperationsRequest{Stage: "running"})},
		{name: "bad token", code: codes.InvalidArgument, call: list(&cucinav1.ListOperationsRequest{Page: &cucinav1.Page{Token: "%%%"}})},
		{name: "get", ctx: as(reader), call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.GetOperation(ctx, &cucinav1.GetOperationRequest{Name: "op-1"})
		}, check: func(t *testing.T, m proto.Message) {
			op := m.(*cucinav1.GetOperationResponse).GetOperation()
			equal(t, "digest", op.GetActionDigest(), "aa11/140")
			equal(t, "worker", op.GetWorkerNode(), "i-0aaaaaaaaaaaaaaa1")
			equal(t, "thread", op.GetWorkerThread(), uint32(3))
		}},
		{name: "get invisible", ctx: as(reader), code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.GetOperation(ctx, &cucinav1.GetOperationRequest{Name: "op-4"})
		}},
		{name: "get missing", code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.GetOperation(ctx, &cucinav1.GetOperationRequest{Name: "op-404"})
		}},
		{name: "kill operation", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.KillOperations(ctx, &cucinav1.KillOperationsRequest{Target: &cucinav1.KillOperationsRequest_OperationName{OperationName: "op-2"}, Message: "stuck test"})
		}, check: func(t *testing.T, _ proto.Message) {
			k := f.sched.kills[0]
			equal(t, "operation", k.f.OperationName, "op-2")
			equal(t, "code", codes.Code(k.code), codes.FailedPrecondition)
			if !strings.HasSuffix(k.msg, ": stuck test") {
				t.Errorf("kill message = %q", k.msg)
			}
		}},
		{name: "kill queue without workers", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.KillOperations(ctx, &cucinav1.KillOperationsRequest{Target: &cucinav1.KillOperationsRequest_QueueWithoutWorkers{QueueWithoutWorkers: rv64Ref}})
		}, check: func(t *testing.T, _ proto.Message) {
			equal(t, "queue", *f.sched.kills[1].f.QueueWithoutWorkers, qk("main", linuxRV64))
		}},
		{name: "kill without target", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.KillOperations(ctx, &cucinav1.KillOperationsRequest{})
		}},
	})
}

// TestHostRPCs guards UC14/UC19/R-SEC-3/R-MAC-6: host listing, drain/uncordon,
// re-image, diagnostics, serial registration/approval/removal and site tokens.
func TestHostRPCs(t *testing.T) {
	f := newFixture(t)
	runCases(t, f.srv, []rpcCase{
		{name: "list", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ListHosts(ctx, &cucinav1.ListHostsRequest{})
		}, check: func(t *testing.T, m proto.Message) {
			hs := m.(*cucinav1.ListHostsResponse).GetHosts()
			equal(t, "hosts", len(hs), 2)
			h := hs[0]
			equal(t, "serial", h.GetSummary().GetSerial(), "C02XYZ123456")
			equal(t, "phase", h.GetSummary().GetPhase(), "Online")
			equal(t, "l2", h.GetSummary().GetL2HitRatio(), "0.93")
			equal(t, "vms", len(h.GetVms()), 2)
			equal(t, "chip", h.GetChip(), "M5 Pro")
		}},
		{name: "drain by name", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.DrainHost(ctx, &cucinav1.HostRef{Serial: "mini1"})
		}, check: func(t *testing.T, _ proto.Message) { equal(t, "cordoned", f.hosts.cordons["C02XYZ123456"], true) }},
		{name: "uncordon by serial (any case)", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.UncordonHost(ctx, &cucinav1.HostRef{Serial: "c02xyz123456"})
		}, check: func(t *testing.T, _ proto.Message) { equal(t, "cordoned", f.hosts.cordons["C02XYZ123456"], false) }},
		{name: "drain unknown", code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.DrainHost(ctx, &cucinav1.HostRef{Serial: "mini9"})
		}},
		{name: "reimage unknown VM", code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ReimageHost(ctx, &cucinav1.ReimageHostRequest{Host: &cucinav1.HostRef{Serial: "mini1"}, Vm: "vm-9"})
		}},
		{name: "reimage every VM", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ReimageHost(ctx, &cucinav1.ReimageHostRequest{Host: &cucinav1.HostRef{Serial: "mini1"}})
		}, check: func(t *testing.T, _ proto.Message) { equal(t, "reimages", f.hosts.reimages, []string{"C02XYZ123456/"}) }},
		{name: "register serials", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.RegisterHostSerials(ctx, &cucinav1.RegisterHostSerialsRequest{Serials: []string{" c02abc111111 ", "C02ABC111111", "C02XYZ123456"}, Site: "hq"})
		}, check: func(t *testing.T, m proto.Message) {
			r := m.(*cucinav1.RegisterHostSerialsResponse)
			equal(t, "registered", r.GetRegistered(), []string{"C02ABC111111"})
			equal(t, "already present", r.GetAlreadyPresent(), []string{"C02XYZ123456"})
		}},
		{name: "register invalid serial", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.RegisterHostSerials(ctx, &cucinav1.RegisterHostSerialsRequest{Serials: []string{"C02 ABC; rm -rf"}})
		}},
		{name: "approve", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ApproveHost(ctx, &cucinav1.ApproveHostRequest{Serial: "c02xyz654321"})
		}, check: func(t *testing.T, _ proto.Message) { equal(t, "approved", f.hosts.approved, []string{"C02XYZ654321"}) }},
		{name: "remove by name", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.RemoveHost(ctx, &cucinav1.HostRef{Serial: "mini2"})
		}, check: func(t *testing.T, _ proto.Message) { equal(t, "removed", f.hosts.removed, []string{"C02XYZ654321"}) }},
		{name: "token without site", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.CreateEnrollToken(ctx, &cucinav1.CreateEnrollTokenRequest{MaxHosts: 5})
		}},
		{name: "token without host limit", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.CreateEnrollToken(ctx, &cucinav1.CreateEnrollTokenRequest{Site: "hq"})
		}},
		{name: "token", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.CreateEnrollToken(ctx, &cucinav1.CreateEnrollTokenRequest{Site: "hq", MaxHosts: 5})
		}, check: func(t *testing.T, m proto.Message) {
			r := m.(*cucinav1.CreateEnrollTokenResponse)
			if !strings.HasPrefix(r.GetToken(), "cuc_et_") || r.GetId() != "et-1" {
				t.Errorf("token response = %v", r)
			}
			req := f.hosts.tokenReqs[0]
			equal(t, "default ttl", req.TTL, 24*time.Hour)
			equal(t, "created by", req.CreatedBy, "sa:break-glass")
		}},
		{name: "list tokens", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ListEnrollTokens(ctx, &cucinav1.ListEnrollTokensRequest{})
		}, check: func(t *testing.T, m proto.Message) {
			equal(t, "tokens", len(m.(*cucinav1.ListEnrollTokensResponse).GetTokens()), 1)
		}},
		{name: "revoke token", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.RevokeEnrollToken(ctx, &cucinav1.RevokeEnrollTokenRequest{Id: "et-1"})
		}, check: func(t *testing.T, _ proto.Message) { equal(t, "revoked", f.hosts.revoked, []string{"et-1"}) }},
		{name: "revoke unknown token", code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.RevokeEnrollToken(ctx, &cucinav1.RevokeEnrollTokenRequest{Id: "et-9"})
		}},
	})

	st := newStream[cucinav1.LogChunk](as(admin))
	if err := f.srv.HostDiagnostics(&cucinav1.HostRef{Serial: "mini1"}, st); err != nil {
		t.Fatalf("HostDiagnostics: %v", err)
	}
	chunks := st.drain()
	if len(chunks) != 2 || string(chunks[0].GetData()) != "host diagnostics archive" || !chunks[1].GetLast() {
		t.Errorf("diagnostics chunks = %v", chunks)
	}

	noHosts := newFixture(t, func(d *mgmt.Deps, _ *mgmt.Options) { d.Hosts, d.HostAdmin, d.Enrollment = nil, nil, nil })
	if r, err := noHosts.srv.ListHosts(as(admin), &cucinav1.ListHostsRequest{}); err != nil || len(r.GetHosts()) != 0 {
		t.Errorf("ListHosts without a fleet = %v, %v", r, err)
	}
	if _, err := noHosts.srv.DrainHost(as(admin), &cucinav1.HostRef{Serial: "mini1"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DrainHost without a fleet: %v, want FailedPrecondition", err)
	}
}

// TestIdentityRPCs guards R-AUTH-9/-10/UC15: service keys are shown once, revocation
// reports when it is enforced (≤ 3 min) and records who revoked.
func TestIdentityRPCs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		now := time.Now()
		runCases(t, f.srv, []rpcCase{
			{name: "key without account", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.CreateServiceKey(ctx, &cucinav1.CreateServiceKeyRequest{})
			}},
			{name: "key with negative ttl", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.CreateServiceKey(ctx, &cucinav1.CreateServiceKeyRequest{Account: "ci", Ttl: durationpb.New(-time.Hour)})
			}},
			{name: "key", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.CreateServiceKey(ctx, &cucinav1.CreateServiceKeyRequest{Account: "ci", Ttl: durationpb.New(90 * 24 * time.Hour)})
			}, check: func(t *testing.T, m proto.Message) {
				r := m.(*cucinav1.CreateServiceKeyResponse)
				equal(t, "key", r.GetKey(), plantedServiceKey)
				equal(t, "created by", f.ids.keyReqs[0].CreatedBy, "sa:break-glass")
				equal(t, "ttl", f.ids.keyReqs[0].TTL, 90*24*time.Hour)
			}},
			{name: "list keys", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.ListServiceKeys(ctx, &cucinav1.ListServiceKeysRequest{Account: "ci"})
			}, check: func(t *testing.T, m proto.Message) {
				equal(t, "keys", names(m.(*cucinav1.ListServiceKeysResponse).GetKeys(), (*cucinav1.ServiceKeyInfo).GetKeyId), []string{"k1"})
			}},
			{name: "revoke key", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.RevokeServiceKey(ctx, &cucinav1.RevokeServiceKeyRequest{KeyId: "k1"})
			}, check: func(t *testing.T, _ proto.Message) { equal(t, "revoked", f.ids.revokedKeys, []string{"k1"}) }},
			{name: "revoke nobody", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.RevokePrincipal(ctx, &cucinav1.RevokePrincipalRequest{Reason: "?"})
			}},
			{name: "revoke malformed subject", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.RevokePrincipal(ctx, &cucinav1.RevokePrincipalRequest{Sub: "google:42 OR 1=1"})
			}},
			{name: "revoke subject", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.RevokePrincipal(ctx, &cucinav1.RevokePrincipalRequest{Sub: "google:42", Reason: "offboarded"})
			}, check: func(t *testing.T, m proto.Message) {
				equal(t, "effective_by", m.(*cucinav1.RevokePrincipalResponse).GetEffectiveBy().AsTime(), now.Add(3*time.Minute).UTC())
				r := f.ids.revocations[0]
				if r.Subject != "google:42" || r.CreatedBy != "sa:break-glass" || r.Reason != "offboarded" {
					t.Errorf("stored revocation = %+v", r)
				}
			}},
			{name: "revoke session (store reports enforcement time)", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				f.ids.effective = now.Add(90 * time.Second)
				return s.RevokePrincipal(ctx, &cucinav1.RevokePrincipalRequest{Sid: "c2Vzc2lvbi1pZC0xMjM0NQ"})
			}, check: func(t *testing.T, m proto.Message) {
				equal(t, "effective_by", m.(*cucinav1.RevokePrincipalResponse).GetEffectiveBy().AsTime(), now.Add(90*time.Second).UTC())
			}},
			{name: "list revocations", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
				return s.ListRevocations(ctx, &cucinav1.ListRevocationsRequest{})
			}, check: func(t *testing.T, m proto.Message) {
				equal(t, "revocations", len(m.(*cucinav1.ListRevocationsResponse).GetRevocations()), 2)
			}},
		})
		noKeys := newFixture(t, func(d *mgmt.Deps, _ *mgmt.Options) { d.Keys = nil })
		if _, err := noKeys.srv.CreateServiceKey(as(admin), &cucinav1.CreateServiceKeyRequest{Account: "ci"}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("CreateServiceKey without a key store: %v, want FailedPrecondition", err)
		}
	})
}

// TestStatusCostAndImages guards UC13/UC17/R-OBS-5/R-OPS-2: status counts standing
// worker instances (zero when idle) and reports failing sources as alerts; cost and
// image views convert the controller's data and count workers per image generation.
func TestStatusCostAndImages(t *testing.T) {
	f := newFixture(t, func(d *mgmt.Deps, _ *mgmt.Options) {
		d.Components = fakeComponents{err: errors.New("deployments: forbidden")}
		d.Alerts = fakeAlerts{{Name: "CucinaHostOffline", Severity: "warning", Summary: "mini2 offline"}}
		d.Images = fakeImages{
			{Pool: "windows-x86-64", Reference: "ami-w7", Generation: "g7", Current: true, FastLaunch: "enabled (4 snapshots)"},
			{Pool: "linux-x86-64", Reference: "ami-l6", Generation: "g6", Previous: true},
			{Pool: "linux-x86-64", Reference: "ami-l7", Generation: "g7", Current: true},
		}
	})
	f.cost.report = mgmt.CostReport{
		Today: 1_250_000, StandingPerMonth: 7_500_000,
		Pools: []mgmt.PoolCost{{Pool: "linux-x86-64", InstanceSeconds: 3600, Compute: 1_200_000, EBS: 50_000}},
		Lines: []mgmt.CostLine{{Pool: "linux-x86-64", Category: "compute", Detail: "c8i.8xlarge", Quantity: 1, Unit: "h", Amount: 1_200_000}},
	}
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	runCases(t, f.srv, []rpcCase{
		{name: "status", ctx: as(reader), call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.GetStatus(ctx, &cucinav1.GetStatusRequest{})
		}, check: func(t *testing.T, m proto.Message) {
			r := m.(*cucinav1.GetStatusResponse)
			equal(t, "cluster", r.GetClusterId(), "c-test")
			equal(t, "protocol", r.GetProtocol().GetMajor(), uint32(1))
			equal(t, "pools", len(r.GetPools()), 3)
			equal(t, "worker instances (running + launching, not stopped/terminated)", r.GetWorkerInstances(), uint32(4))
			equal(t, "hosts", [2]uint32{r.GetHostsOnline(), r.GetHostsTotal()}, [2]uint32{1, 2})
			equal(t, "alerts", names(r.GetAlerts(), (*cucinav1.Alert).GetName), []string{"ManagementSourceUnavailable", "CucinaHostOffline"})
		}},
		{name: "cost", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.GetCost(ctx, &cucinav1.GetCostRequest{Pool: "linux-x86-64", Since: timestamppb.New(since)})
		}, check: func(t *testing.T, m proto.Message) {
			r := m.(*cucinav1.GetCostResponse)
			equal(t, "today", r.GetCost().GetToday().GetMicros(), int64(1_250_000))
			equal(t, "standing", r.GetCost().GetStandingPerMonth().GetMicros(), int64(7_500_000))
			equal(t, "pool ebs", r.GetCost().GetPools()[0].GetEbs().GetMicros(), int64(50_000))
			equal(t, "line", r.GetLines()[0].GetDetail(), "c8i.8xlarge")
			equal(t, "query", f.cost.last, mgmt.CostQuery{Pool: "linux-x86-64", Since: since})
		}},
		{name: "cost of unknown pool", code: codes.NotFound, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.GetCost(ctx, &cucinav1.GetCostRequest{Pool: "nope"})
		}},
		{name: "cost since the future", code: codes.InvalidArgument, call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.GetCost(ctx, &cucinav1.GetCostRequest{Since: timestamppb.New(time.Now().Add(time.Hour))})
		}},
		{name: "images", call: func(ctx context.Context, s *mgmt.Server) (proto.Message, error) {
			return s.ListImages(ctx, &cucinav1.ListImagesRequest{})
		}, check: func(t *testing.T, m proto.Message) {
			imgs := m.(*cucinav1.ListImagesResponse).GetImages()
			equal(t, "order", names(imgs, (*cucinav1.ImageInfo).GetReference), []string{"ami-l7", "ami-l6", "ami-w7"})
			running := names(imgs, func(i *cucinav1.ImageInfo) string { return fmt.Sprint(i.GetWorkersRunning()) })
			equal(t, "workers running", running, []string{"1", "1", "1"})
		}},
	})
	noCost := newFixture(t, func(d *mgmt.Deps, _ *mgmt.Options) { d.Cost = nil })
	if _, err := noCost.srv.GetCost(as(admin), &cucinav1.GetCostRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("GetCost without a cost source: %v, want FailedPrecondition", err)
	}
}
