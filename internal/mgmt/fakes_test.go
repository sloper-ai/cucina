// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/mgmt"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Fakes for the management API's consumer-owned interfaces. They are small
// in-memory models with call counters, not interaction mocks.

// ---------------------------------------------------------------- principals

var (
	admin = mgmt.Principal{Subject: "sa:break-glass", DisplayName: "break-glass", SessionID: "s-admin",
		Grants: map[string][]string{"admin": {"main", "tenant-b"}, "execute": {"main", "tenant-b"}}}
	reader = mgmt.Principal{Subject: "google:42", DisplayName: "dev@example.com", SessionID: "s-reader",
		Grants: map[string][]string{"execute": {"main"}, "cas-read": {"main"}}}
	tenantAdmin = mgmt.Principal{Subject: "google:7", SessionID: "s-tenant",
		Grants: map[string][]string{"admin": {"tenant-b"}}}
	casOnly = mgmt.Principal{Subject: "github:1401027334:ci", SessionID: "s-cas",
		Grants: map[string][]string{"cas-read": {"main"}, "ac-read": {"main"}}}

	tokens = map[string]mgmt.Principal{
		"tok-admin": admin, "tok-reader": reader, "tok-tenant": tenantAdmin, "tok-casonly": casOnly,
	}
)

type fakeAuth map[string]mgmt.Principal

func (f fakeAuth) Authenticate(_ context.Context, tok string) (mgmt.Principal, error) {
	if p, ok := f[tok]; ok {
		return p, nil
	}
	return mgmt.Principal{}, errors.New("signature verification failed")
}

func as(p mgmt.Principal) context.Context {
	return mgmt.ContextWithPrincipal(context.Background(), p)
}

// ---------------------------------------------------------------- fleet

var (
	linuxNative = map[string]string{"OSFamily": "linux", "ISA": "x86-64"}
	linuxRV64   = map[string]string{"OSFamily": "linux", "ISA": "rv64g", "cucina-emulation": "qemu"}
	windowsX64  = map[string]string{"OSFamily": "windows", "ISA": "x86-64"}
	macXcode    = map[string]string{"OSFamily": "macos", "ISA": "arm-a64", "xcode-version": "27.0"}
	macGeneric  = map[string]string{"OSFamily": "macos", "ISA": "arm-a64"}
)

func qk(instance string, props map[string]string) domain.QueueKey {
	return domain.QueueKey{InstanceNamePrefix: instance, PlatformKey: domain.PropertiesKey(props)}
}

func pool(name, platform string, provider domain.Provider, max int32, runners ...domain.Runner) mgmt.Pool {
	return mgmt.Pool{
		Resource: v1alpha1.WorkerPool{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha1.WorkerPoolSpec{
				Platform: platform, Provider: string(provider), Capacity: v1alpha1.CapacitySpec{Max: max},
				EC2: &v1alpha1.EC2Spec{InstanceTypes: []string{"c8i.8xlarge"}},
			},
			Status: v1alpha1.WorkerPoolStatus{
				Desired: 1, Registered: 1, Busy: 1, ImageGeneration: "g7",
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"}},
			},
		},
		Spec: domain.PoolSpec{
			Name: domain.PoolName(name), Provider: provider, Platform: platform, InstanceNames: []string{"main"},
			Runners: runners, Max: int(max), Generation: "g7",
		},
	}
}

type fakeFleet struct {
	mu      sync.Mutex
	pools   []mgmt.Pool
	workers []mgmt.Worker
	events  []mgmt.PoolEvent
	starts  []mgmt.StartLatency
	floors  map[string]mgmt.FloorOverride
	paused  map[string]bool
	err     error

	poolCalls    atomic.Int64
	historyCalls atomic.Int64
}

func newFakeFleet() *fakeFleet {
	launched := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	return &fakeFleet{
		pools: []mgmt.Pool{
			pool("linux-x86-64", "linux-x86-64", domain.ProviderEC2, 4,
				domain.Runner{Name: "native", Properties: linuxNative, Concurrency: 32},
				domain.Runner{Name: "qemu-rv64g", Properties: linuxRV64, Concurrency: 4}),
			pool("macos-arm64-xcode27.0", "macos-arm64-xcode27.0", domain.ProviderTart, 2,
				domain.Runner{Name: "xcode", Properties: macXcode, Concurrency: 4},
				domain.Runner{Name: "generic", Properties: macGeneric, Concurrency: 4}),
			pool("windows-x86-64", "windows-x86-64", domain.ProviderEC2, 4,
				domain.Runner{Name: "native", Properties: windowsX64, Concurrency: 32}),
		},
		workers: []mgmt.Worker{
			{VM: domain.VM{ID: "i-0aaaaaaaaaaaaaaa1", Pool: "linux-x86-64", Generation: "g7", State: domain.VMRegistered,
				LaunchedAt: launched, Threads: 36, Busy: 3, InstanceType: "c8i.8xlarge"}, PrivateIP: "10.0.1.10"},
			{VM: domain.VM{ID: "i-0aaaaaaaaaaaaaaa2", Pool: "linux-x86-64", Generation: "g6", State: domain.VMRegistered,
				LaunchedAt: launched, IdleSince: launched.Add(time.Minute), Threads: 36, InstanceType: "c8i.8xlarge"}},
			{VM: domain.VM{ID: "i-0bbbbbbbbbbbbbbb1", Pool: "windows-x86-64", Generation: "g7", State: domain.VMLaunching,
				LaunchedAt: launched, InstanceType: "c7a.8xlarge"}},
			{VM: domain.VM{ID: "mini1/vm-1", Pool: "macos-arm64-xcode27.0", Generation: "g7", State: domain.VMRegistered,
				Host: "mini1", Threads: 8}},
			{VM: domain.VM{ID: "mini1/vm-2", Pool: "macos-arm64-xcode27.0", Generation: "g7", State: domain.VMStopped, Host: "mini1"}},
			{VM: domain.VM{ID: "i-0ccccccccccccccc1", Pool: "linux-x86-64", State: domain.VMTerminated}},
		},
		events: []mgmt.PoolEvent{{Time: launched, Type: "launch", Subject: "i-0aaaaaaaaaaaaaaa1", Message: "c8i.8xlarge"}},
		starts: []mgmt.StartLatency{{Pool: "linux-x86-64", VM: "i-0aaaaaaaaaaaaaaa1", Launched: launched,
			ToRunning: 9 * time.Second, ToRegistered: 31 * time.Second, ToFirstAction: 33 * time.Second, Path: "slow"}},
		floors: map[string]mgmt.FloorOverride{},
		paused: map[string]bool{},
	}
}

func (f *fakeFleet) Pools(context.Context) ([]mgmt.Pool, error) {
	f.poolCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := slices.Clone(f.pools)
	for i := range out {
		if fl, ok := f.floors[out[i].Name()]; ok {
			out[i].Floor = &fl
		}
	}
	return out, nil
}

func (f *fakeFleet) History(_ context.Context, pool string, limit int) ([]mgmt.PoolEvent, []mgmt.StartLatency, error) {
	f.historyCalls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	var st []mgmt.StartLatency
	for _, s := range f.starts {
		if pool == "" || s.Pool == pool {
			st = append(st, s)
		}
	}
	return slices.Clone(f.events), st, nil
}

func (f *fakeFleet) SetFloor(_ context.Context, pool string, fl mgmt.FloorOverride) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl.MinRunning == 0 {
		delete(f.floors, pool)
		return nil
	}
	f.floors[pool] = fl
	return nil
}

func (f *fakeFleet) SetPaused(_ context.Context, pool string, paused bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused[pool] = paused
	return nil
}

func (f *fakeFleet) Workers(context.Context) ([]mgmt.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.workers), nil
}

// ---------------------------------------------------------------- scheduler

type killCall struct {
	f    ports.KillFilter
	code int32
	msg  string
}

type fakeScheduler struct {
	mu     sync.Mutex
	queues []domain.QueueObservation
	ops    []ports.Operation // kept sorted by name
	drains map[domain.QueueKey]map[string]bool
	kills  []killCall
	err    error

	queueCalls atomic.Int64
	opsCalls   atomic.Int64
}

func newFakeScheduler() *fakeScheduler {
	q := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	return &fakeScheduler{
		queues: []domain.QueueObservation{
			{Key: qk("main", linuxNative), Queued: 7, Executing: 3, Idle: 33, Workers: 36},
			{Key: qk("main", linuxRV64), Queued: 1},
			{Key: qk("main", macGeneric), Workers: 4, Idle: 4},
			{Key: qk("main", windowsX64)},
		},
		ops: []ports.Operation{
			{Name: "op-1", Queue: qk("main", linuxNative), ActionDigest: "aa11-140", Stage: "executing", QueuedAt: q,
				InvocationID: "inv-1", Worker: ports.WorkerID{"node": "i-0aaaaaaaaaaaaaaa1", "pool": "linux-x86-64", "thread": "3"}},
			{Name: "op-2", Queue: qk("main", linuxNative), ActionDigest: "bb22-99", Stage: "queued", QueuedAt: q, InvocationID: "inv-1"},
			{Name: "op-3", Queue: qk("main", linuxRV64), ActionDigest: "cc33-12", Stage: "queued", QueuedAt: q, InvocationID: "inv-2"},
			{Name: "op-4", Queue: qk("tenant-b", linuxNative), ActionDigest: "dd44-7", Stage: "queued", QueuedAt: q, InvocationID: "inv-9"},
		},
		drains: map[domain.QueueKey]map[string]bool{},
	}
}

func patternKey(p ports.WorkerID) string {
	keys := make([]string, 0, len(p))
	for k, v := range p {
		keys = append(keys, k+"="+v)
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

func (s *fakeScheduler) ListPlatformQueues(context.Context) ([]domain.QueueObservation, error) {
	s.queueCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return slices.Clone(s.queues), nil
}

func (s *fakeScheduler) AddDrain(_ context.Context, q domain.QueueKey, p ports.WorkerID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.drains[q] == nil {
		s.drains[q] = map[string]bool{}
	}
	s.drains[q][patternKey(p)] = true
	return nil
}

func (s *fakeScheduler) RemoveDrain(_ context.Context, q domain.QueueKey, p ports.WorkerID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.drains[q], patternKey(p))
	return nil
}

func (s *fakeScheduler) drainCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.drains {
		n += len(d)
	}
	return n
}

func (s *fakeScheduler) KillOperations(_ context.Context, f ports.KillFilter, code int32, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kills = append(s.kills, killCall{f, code, msg})
	return nil
}

func (s *fakeScheduler) ListOperations(_ context.Context, f ports.OperationFilter) ([]ports.Operation, error) {
	s.opsCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	var out []ports.Operation
	for _, o := range s.ops {
		if o.Name <= f.StartAfter || (f.Queue != nil && o.Queue != *f.Queue) || (f.Stage != "" && o.Stage != f.Stage) {
			continue
		}
		out = append(out, o)
		if f.PageSize > 0 && len(out) == f.PageSize {
			break
		}
	}
	return out, nil
}

func (s *fakeScheduler) GetOperation(_ context.Context, name string) (ports.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.ops {
		if o.Name == name {
			return o, nil
		}
	}
	return ports.Operation{}, ports.ErrNotFound
}

func (s *fakeScheduler) setOps(ops ...ports.Operation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = slices.Clone(ops)
	slices.SortFunc(s.ops, func(a, b ports.Operation) int { return strings.Compare(a.Name, b.Name) })
}

// ---------------------------------------------------------------- hosts and enrollment

type fakeHosts struct {
	mu        sync.Mutex
	hosts     []v1alpha1.MacHost
	cordons   map[string]bool
	reimages  []string // "serial/vm"
	diag      map[string]string
	diagReqs  []mgmt.DiagnosticsRequest
	tokens    []mgmt.EnrollToken
	tokenReqs []mgmt.EnrollTokenRequest
	approved  []string
	removed   []string
	revoked   []string
	serials   map[string]bool
}

func newFakeHosts() *fakeHosts {
	hb := metav1.NewTime(time.Date(2026, 10, 2, 9, 59, 0, 0, time.UTC))
	return &fakeHosts{
		hosts: []v1alpha1.MacHost{
			{ObjectMeta: metav1.ObjectMeta{Name: "mini1"},
				Spec: v1alpha1.MacHostSpec{Serial: "C02XYZ123456", Site: "hq", Approved: true},
				Status: v1alpha1.MacHostStatus{Phase: v1alpha1.MacHostOnline, LastHeartbeat: &hb, RunningVMs: 1,
					Facts: v1alpha1.HostFacts{Chip: "M5 Pro", Cores: 18, MemoryGiB: 64, AgentVersion: "0.1.0", FileVault: "off"},
					VMs:   []v1alpha1.HostVMStatus{{Name: "vm-1", State: "running"}, {Name: "vm-2", State: "stopped"}},
					L2:    v1alpha1.HostCacheStatus{HitRatio: "0.93", WANBytesReceived: 1 << 30}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "mini2"},
				Spec:   v1alpha1.MacHostSpec{Serial: "C02XYZ654321", Site: "hq"},
				Status: v1alpha1.MacHostStatus{Phase: v1alpha1.MacHostPending}},
		},
		cordons: map[string]bool{},
		diag:    map[string]string{"C02XYZ123456": "host diagnostics archive"},
		serials: map[string]bool{"C02XYZ123456": true},
	}
}

func (h *fakeHosts) Hosts(context.Context) ([]v1alpha1.MacHost, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.hosts), nil
}

func (h *fakeHosts) SetCordon(_ context.Context, serial string, c bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cordons[serial] = c
	return nil
}

func (h *fakeHosts) Reimage(_ context.Context, serial, vm string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reimages = append(h.reimages, serial+"/"+vm)
	return nil
}

func (h *fakeHosts) Diagnostics(_ context.Context, serial string, req mgmt.DiagnosticsRequest) (io.ReadCloser, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.diagReqs = append(h.diagReqs, req)
	d, ok := h.diag[serial]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(d)), nil
}

func (h *fakeHosts) CreateEnrollToken(_ context.Context, req mgmt.EnrollTokenRequest) (mgmt.EnrollToken, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokenReqs = append(h.tokenReqs, req)
	t := mgmt.EnrollToken{ID: "et-1", Site: req.Site, MaxHosts: req.MaxHosts, Created: time.Now(), ExpiresAt: time.Now().Add(req.TTL)}
	h.tokens = append(h.tokens, t)
	return t, "cuc_et_et1_Zm9vYmFyYmF6cXV4c2VjcmV0", nil
}

func (h *fakeHosts) ListEnrollTokens(context.Context) ([]mgmt.EnrollToken, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.tokens), nil
}

func (h *fakeHosts) RevokeEnrollToken(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.tokens {
		if h.tokens[i].ID == id {
			h.tokens[i].Revoked = true
			h.revoked = append(h.revoked, id)
			return nil
		}
	}
	return ports.ErrNotFound
}

func (h *fakeHosts) RegisterSerials(_ context.Context, serials []string, _ string, _ map[string]string) ([]string, []string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var reg, already []string
	for _, s := range serials {
		if h.serials[s] {
			already = append(already, s)
			continue
		}
		h.serials[s] = true
		reg = append(reg, s)
	}
	return reg, already, nil
}

func (h *fakeHosts) ApproveHost(_ context.Context, serial string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.approved = append(h.approved, serial)
	return nil
}

func (h *fakeHosts) RemoveHost(_ context.Context, serial string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removed = append(h.removed, serial)
	return nil
}

func (h *fakeHosts) mutations() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.cordons) + len(h.reimages) + len(h.tokenReqs) + len(h.approved) + len(h.removed) + len(h.revoked)
}

// ---------------------------------------------------------------- identity

const plantedServiceKey = "cuc_sk_k1_c2VjcmV0LXNlcnZpY2Uta2V5LXZhbHVl"

type fakeIdentity struct {
	mu          sync.Mutex
	keys        []mgmt.ServiceKey
	keyReqs     []mgmt.ServiceKeyRequest
	revokedKeys []string
	revocations []mgmt.Revocation
	effective   time.Time
}

func (f *fakeIdentity) CreateServiceKey(_ context.Context, req mgmt.ServiceKeyRequest) (mgmt.ServiceKey, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyReqs = append(f.keyReqs, req)
	k := mgmt.ServiceKey{ID: "k1", Account: req.Account, Description: req.Description, Created: time.Now()}
	f.keys = append(f.keys, k)
	return k, plantedServiceKey, nil
}

func (f *fakeIdentity) ListServiceKeys(_ context.Context, account string) ([]mgmt.ServiceKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mgmt.ServiceKey
	for _, k := range f.keys {
		if account == "" || k.Account == account {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeIdentity) RevokeServiceKey(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokedKeys = append(f.revokedKeys, id)
	return nil
}

func (f *fakeIdentity) Revoke(_ context.Context, r mgmt.Revocation) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revocations = append(f.revocations, r)
	return f.effective, nil
}

func (f *fakeIdentity) ListRevocations(context.Context) ([]mgmt.Revocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.revocations), nil
}

func (f *fakeIdentity) mutations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.keyReqs) + len(f.revokedKeys) + len(f.revocations)
}

// ---------------------------------------------------------------- misc sources

type fakeOrphans struct {
	mu      sync.Mutex
	orphans []ports.Orphan
	deleted []string
	failIDs map[string]error
}

func (o *fakeOrphans) ListOrphans(_ context.Context, cluster string) ([]ports.Orphan, error) {
	if cluster == "" {
		return nil, ports.ErrInvalid
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.orphans), nil
}

func (o *fakeOrphans) DeleteOrphans(_ context.Context, _ string, orphans []ports.Orphan) (map[string]error, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	errs := map[string]error{}
	for _, x := range orphans {
		if e := o.failIDs[x.ID]; e != nil {
			errs[x.ID] = e
			continue
		}
		o.deleted = append(o.deleted, x.ID)
	}
	return errs, nil
}

type fakeCost struct {
	report mgmt.CostReport
	err    error
	last   mgmt.CostQuery
}

func (c *fakeCost) Cost(_ context.Context, q mgmt.CostQuery) (mgmt.CostReport, error) {
	c.last = q
	return c.report, c.err
}

type fakeImages []mgmt.Image

func (f fakeImages) Images(context.Context) ([]mgmt.Image, error) { return slices.Clone(f), nil }

type fakeComponents struct {
	comps []mgmt.Component
	err   error
}

func (f fakeComponents) Components(context.Context) ([]mgmt.Component, error) { return f.comps, f.err }

type fakeAlerts []mgmt.Alert

func (f fakeAlerts) Alerts(context.Context) ([]mgmt.Alert, error) { return f, nil }

type fakeSupport struct {
	policies []v1alpha1.TrustPolicy
	metrics  string
}

func (f fakeSupport) TrustPolicies(context.Context) ([]v1alpha1.TrustPolicy, error) {
	return f.policies, nil
}
func (f fakeSupport) Metrics(context.Context) ([]byte, error) { return []byte(f.metrics), nil }

// fakeShell answers SSM scripts.
type fakeShell struct {
	mu      sync.Mutex
	scripts []string
	at      []time.Time
	reply   func(n int, script string) string
}

func (s *fakeShell) RunScript(_ context.Context, instanceID string, windows bool, script string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.HasPrefix(instanceID, "i-") {
		return nil, ports.ErrNotOwned
	}
	s.scripts = append(s.scripts, script)
	s.at = append(s.at, time.Now())
	return []byte(s.reply(len(s.scripts)-1, script)), nil
}

func (s *fakeShell) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.scripts)
}

// ---------------------------------------------------------------- fixture

// syncBuffer is a goroutine-safe audit sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	srv     *mgmt.Server
	fleet   *fakeFleet
	sched   *fakeScheduler
	hosts   *fakeHosts
	ids     *fakeIdentity
	orphans *fakeOrphans
	cost    *fakeCost
	shell   *fakeShell
	audit   *syncBuffer
	deps    mgmt.Deps
	opts    mgmt.Options
}

// newFixture builds a server over the fakes; tweak adjusts deps/options first.
func newFixture(t *testing.T, tweak ...func(*mgmt.Deps, *mgmt.Options)) *fixture {
	t.Helper()
	f := &fixture{
		fleet: newFakeFleet(), sched: newFakeScheduler(), hosts: newFakeHosts(), ids: &fakeIdentity{},
		orphans: &fakeOrphans{}, cost: &fakeCost{}, audit: &syncBuffer{},
		shell: &fakeShell{reply: func(int, string) string { return "" }},
	}
	f.deps = mgmt.Deps{
		Authenticator: fakeAuth(tokens),
		Auditor:       mgmt.NewSlogAuditor(slog.New(slog.NewJSONHandler(f.audit, nil))),
		Pools:         f.fleet, Workers: f.fleet, PoolAdmin: f.fleet, Scheduler: f.sched,
		Orphans: f.orphans, Shell: f.shell,
		Hosts: f.hosts, HostAdmin: f.hosts, Enrollment: f.hosts,
		Keys: f.ids, Revocations: f.ids, Cost: f.cost,
	}
	f.opts = mgmt.Options{
		ClusterID: "c-test", Version: "0.1.0-test", InstanceNames: []string{"main", "tenant-b"},
		Logger: slog.New(slog.DiscardHandler),
	}
	for _, fn := range tweak {
		fn(&f.deps, &f.opts)
	}
	srv, err := mgmt.New(f.deps, f.opts)
	if err != nil {
		t.Fatalf("mgmt.New: %v", err)
	}
	f.srv = srv
	t.Cleanup(srv.Stop)
	return f
}

// ---------------------------------------------------------------- in-process streams

// fakeStream is an in-process grpc.ServerStreamingServer. Send blocks while gate is
// closed (a slow client), and fails once ctx is done, like a real stream.
type fakeStream[T any] struct {
	ctx  context.Context
	sent chan *T
	gate chan struct{} // nil: never blocks
}

func newStream[T any](ctx context.Context) *fakeStream[T] {
	return &fakeStream[T]{ctx: ctx, sent: make(chan *T, 10000)}
}

func (s *fakeStream[T]) Context() context.Context { return s.ctx }

func (s *fakeStream[T]) Send(m *T) error {
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	select {
	case s.sent <- m:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *fakeStream[T]) SetHeader(metadata.MD) error  { return nil }
func (s *fakeStream[T]) SendHeader(metadata.MD) error { return nil }
func (s *fakeStream[T]) SetTrailer(metadata.MD)       {}
func (s *fakeStream[T]) SendMsg(m any) error          { return s.Send(m.(*T)) }
func (s *fakeStream[T]) RecvMsg(any) error            { return nil }

// drain returns everything sent so far.
func (s *fakeStream[T]) drain() []*T {
	var out []*T
	for {
		select {
		case m := <-s.sent:
			out = append(out, m)
		default:
			return out
		}
	}
}

var _ grpc.ServerStreamingServer[int] = (*fakeStream[int])(nil)
