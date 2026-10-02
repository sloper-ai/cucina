// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/controller"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/reconcile"
	"github.com/sloper-ai/cucina/internal/scaling"
	"github.com/sloper-ai/cucina/invariants"
)

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

const testAMI = "ami-0123456789abcdef0"

// harness wires the reconcile runtime to the stateful fakes: instances that
// become ready register their runner threads with the fake scheduler, and
// terminated ones disappear from it.
type harness struct {
	t       *testing.T
	ctx     context.Context
	clock   *fakes.Clock
	compute *fakes.Compute
	bq      *fakes.BuildQueue
	hosts   *fakes.HostFleet
	cfg     *config.Controller
	catalog *pools.Catalog
	reg     *prometheus.Registry
	metrics *metrics.Metrics
	ledgers *memLedgers
	comps   *reconcile.Components

	mu         sync.Mutex
	threads    map[domain.PoolName]map[domain.QueueKey]int
	violations []invariants.Violation
}

// memUsage is an in-memory reconcile.UsageStore.
type memUsage struct{ b []byte }

func (m *memUsage) LoadUsage(context.Context) ([]byte, error) { return m.b, nil }
func (m *memUsage) SaveUsage(_ context.Context, b []byte) error {
	m.b = append([]byte(nil), b...)
	return nil
}

type memLedgers struct {
	mu sync.Mutex
	m  map[domain.PoolName]scaling.Ledger
}

func (s *memLedgers) SaveLedger(_ context.Context, pool domain.PoolName, l scaling.Ledger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[pool] = l
	return nil
}

func testConfig(t *testing.T) *config.Controller {
	t.Helper()
	cfg := &config.Controller{
		ClusterID: "test", Namespace: "cucina", ReleaseName: "cucina", InstanceNames: []string{"main"},
		AWS: &config.AWS{Region: "us-west-1", AccountID: "000000000000", ExtraTags: map[string]string{"cucina:env": "test"}},
	}
	cfg.Observability.CostEnabled = true
	controller.ApplyDefaults(cfg)
	return cfg
}

// kubeOpt points the reconcilers at an API server (envtest) and a namespace;
// ledgers are then persisted as WorkerPool annotations.
type kubeOpt struct {
	c  client.Client
	ns string
}

// newHarness wires everything to the fakes.
func newHarness(t *testing.T, seed uint64, kube ...kubeOpt) *harness {
	t.Helper()
	clock := fakes.NewClock(t0)
	rnd := fakes.NewRand(seed)
	ccfg := fakes.DefaultComputeConfig()
	ccfg.Subnets = map[string]string{"subnet-a": "us-west-1b"}
	h := &harness{
		t: t, ctx: context.Background(), clock: clock,
		compute: fakes.NewCompute(clock, rnd.Child("compute"), ccfg),
		bq:      fakes.NewBuildQueue(clock, rnd.Child("buildqueue"), fakes.DefaultBuildQueueConfig()),
		hosts:   fakes.NewHostFleet(clock, rnd.Child("hosts"), fakes.DefaultHostFleetConfig()),
		cfg:     testConfig(t),
		reg:     prometheus.NewRegistry(),
		ledgers: &memLedgers{m: map[domain.PoolName]scaling.Ledger{}},
		threads: map[domain.PoolName]map[domain.QueueKey]int{},
	}
	var err error
	h.catalog, err = pools.LoadCatalog("../../platforms/pools.json")
	require.NoError(t, err)
	h.metrics, err = metrics.New(h.reg)
	require.NoError(t, err)
	h.compute.AddImage(ports.Image{ID: testAMI, Name: "cucina-linux", Arch: "x86_64", Version: "v1", Platform: "linux", SizeGiB: 8})
	h.compute.OnReady(func(in ports.Instance) {
		h.mu.Lock()
		th := h.threads[in.Pool]
		h.mu.Unlock()
		h.bq.RegisterNode(in.Pool, in.ID, th)
	})
	h.compute.OnTerminated(func(in ports.Instance) { h.bq.RemoveNode(in.ID) })
	invariants.SetReporter(invariants.ReporterFunc(func(v invariants.Violation) {
		h.mu.Lock()
		h.violations = append(h.violations, v)
		h.mu.Unlock()
	}))
	t.Cleanup(func() { invariants.SetReporter(nil) })

	var ledgers reconcile.LedgerStore = h.ledgers
	if len(kube) > 0 {
		h.cfg.Namespace = kube[0].ns
		ledgers = reconcile.AnnotationLedgers{Client: kube[0].c, Namespace: kube[0].ns}
	}
	h.comps, err = reconcile.New(reconcile.Options{
		Config: h.cfg, Catalog: h.catalog, Pools: pools.NewSet(h.cfg),
		Compute: h.compute, BuildQueue: h.bq, HostFleet: h.hosts,
		Clock: clock, Rand: rnd.Child("planner"), Metrics: h.metrics,
	}, ledgers, nil)
	require.NoError(t, err)
	if len(kube) > 0 {
		h.comps.UseClient(kube[0].c)
	}
	return h
}

func linuxPool(name string, max int32) *v1alpha1.WorkerPool {
	return &v1alpha1.WorkerPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cucina", Generation: 1},
		Spec: v1alpha1.WorkerPoolSpec{
			Platform: "linux-x86-64", Provider: "ec2", SizeClass: "default",
			Capacity: v1alpha1.CapacitySpec{Max: max},
			Image:    v1alpha1.ImageSpec{AMI: testAMI},
			EC2: &v1alpha1.EC2Spec{
				InstanceTypes: []string{"c8i.8xlarge", "c7i.8xlarge"}, SubnetIDs: []string{"subnet-a"},
				SecurityGroupIDs: []string{"sg-1"}, InstanceProfile: "cucina-worker",
			},
		},
	}
}

// addPool resolves wp, declares its queues in the fake scheduler (as the chart
// does), registers the runner threads its VMs will offer, and starts its loop.
func (h *harness) addPool(wp *v1alpha1.WorkerPool) *reconcile.PoolRuntime {
	h.t.Helper()
	rt := h.declare(wp, true)
	h.comps.Fleet.Upsert(rt)
	return rt
}

// declare resolves wp and prepares the fake scheduler for it: queues are
// declared when declareQueues (the chart's predeclaredPlatformQueues) and the
// threads its VMs offer are remembered for the registration hook.
func (h *harness) declare(wp *v1alpha1.WorkerPool, declareQueues bool) *reconcile.PoolRuntime {
	h.t.Helper()
	rt, err := h.comps.WorkerPool.Builder.Build(h.ctx, wp, nil, "")
	require.NoError(h.t, err)
	if declareQueues {
		h.bq.Declare(rt.Queues...)
	}
	th := map[domain.QueueKey]int{}
	for _, in := range rt.Spec.InstanceNames {
		for _, r := range rt.Spec.Runners {
			th[domain.QueueKey{InstanceNamePrefix: in, PlatformKey: domain.PropertiesKey(r.Properties), SizeClass: rt.Spec.SizeClass}] = r.Concurrency
		}
	}
	h.mu.Lock()
	h.threads[rt.Spec.Name] = th
	h.mu.Unlock()
	return rt
}

// reconcilePool runs the WorkerPool reconciler once for name.
func (h *harness) reconcilePool(name string) {
	h.t.Helper()
	_, err := h.comps.WorkerPool.Reconcile(h.ctx, reconcile.Requests(h.cfg.Namespace, domain.PoolName(name)))
	require.NoError(h.t, err)
}

// step advances simulated time by one poll interval and runs one decision per pool.
func (h *harness) step() {
	h.clock.Advance(time.Second)
	h.compute.Tick()
	h.bq.Tick()
	h.comps.Fleet.StepAll(h.ctx)
}

func (h *harness) alive(pool domain.PoolName) []ports.Instance {
	var out []ports.Instance
	for _, in := range h.compute.All() {
		if in.Pool == pool && (in.State == ports.InstancePending || in.State == ports.InstanceRunning) {
			out = append(out, in)
		}
	}
	return out
}

func (h *harness) noViolations() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Empty(h.t, h.violations, "invariant violations")
}
