// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Build-time optional wiring.
//
// cucina-controller is assembled from packages owned by several agents (STS,
// enrollment, host stream, management API, EC2 and BuildQueue adapters). To
// keep the binary compiling while those packages are being written, each one
// is wired by a small file internal/controller/components_<name>.go whose
// init() calls RegisterComponent / ProvideCompute / ProvideBuildQueue /
// RegisterBootstrapStep. Deleting such a file removes the feature from the
// binary; nothing else refers to the package. See docs/dev/controller.md.

// Listener names a server the controller process runs (config.Listeners).
type Listener string

const (
	// ListenerNone: the component serves nothing (background job, port adapter).
	ListenerNone Listener = ""
	// ListenerSTS: HTTPS (TLS, no client certificate): /token, /jwks.json, discovery.
	ListenerSTS Listener = "sts"
	// ListenerManagement: gRPC ManagementService (TLS; Cucina JWT checked by the service).
	ListenerManagement Listener = "management"
	// ListenerEnrollment: gRPC EnrollmentService (TLS; callers have no certificate yet).
	ListenerEnrollment Listener = "enrollment"
	// ListenerHost: gRPC HostService (mTLS with the Cucina CA).
	ListenerHost Listener = "host"
)

// Deps are the shared dependencies handed to component factories. Fields that
// a mode does not provide are nil (e.g. no Manager for one-shot tools, no
// Compute without AWS configuration).
type Deps struct {
	Mode    Mode
	Config  *config.Controller
	Log     *slog.Logger
	Clock   ports.Clock
	Rand    ports.Rand
	Metrics *metrics.Metrics
	// Registry is where component-private metrics are registered (the same
	// registry /metrics serves). Contract metrics live in Metrics.
	Registry prometheus.Registerer

	RESTConfig *rest.Config
	Scheme     *runtime.Scheme
	// Manager is the controller-runtime manager (nil for tools). Components may
	// add runnables, informers or controllers to it.
	Manager manager.Manager
	// Client reads from the informer cache and writes to the API server;
	// APIReader always reads from the API server.
	Client    client.Client
	APIReader client.Reader

	TLS *TLSConfigs

	// Catalog is the validated platform catalog; Pools the currently resolved pools
	// (implements the enrollment side's SettingsFor lookup).
	Catalog *pools.Catalog
	Pools   *pools.Set

	// Port adapters (nil when not compiled in or not configured).
	Compute    ports.Compute
	BuildQueue ports.BuildQueue
	HostFleet  ports.HostFleet

	// Fleet is the autoscaler runtime (leader-only); the management API uses it
	// for pool snapshots and on-demand orphan sweeps. Nil in the sts mode.
	Fleet FleetAPI

	// IsLeader reports whether this replica currently holds the lease (always
	// true without leader election).
	IsLeader func() bool
	// Elected is closed when this replica becomes the leader.
	Elected <-chan struct{}
	// CertsFile lists the in-cluster certificates (JSON []pki.CertSpec) the
	// bootstrap hook creates and the leader renews; empty disables both.
	CertsFile string

	shared map[string]any
	grpc   map[Listener]*grpcServer
}

// Share publishes an object built by one component for components built later
// (construction runs in Factory.Order).
func (d *Deps) Share(key string, v any) {
	if d.shared == nil {
		d.shared = map[string]any{}
	}
	d.shared[key] = v
}

// Shared returns an object published with Share.
func Shared[T any](d *Deps, key string) (T, bool) {
	v, ok := d.shared[key].(T)
	return v, ok
}

// FleetAPI is what other components may ask of the autoscaler runtime.
type FleetAPI interface {
	// SweepOrphans runs the orphaned volume/ENI sweep now (cucinactl pools gc)
	// and returns how many orphans were deleted.
	SweepOrphans(ctx context.Context) (int, error)
}

// Factory builds one component. The returned value may implement any of:
//
//   - GRPCService: registered on the gRPC server of Factory.Listener;
//   - HTTPService: mounted on the HTTPS server of Factory.Listener (ListenerSTS);
//   - manager.Runnable (Start(ctx) error) or Runner (Run(ctx) error): started by
//     the manager; implement manager.LeaderElectionRunnable to choose
//     leader-only (default for Runner: every replica);
//   - ReadyChecker: added to /-/ready.
type Factory struct {
	Name string
	// Modes in which the component runs (ModeController, ModeSTS).
	Modes []Mode
	// Listener the component serves on (ListenerNone for background work).
	Listener Listener
	// Order sorts construction (lower first). Components with Order < 0 are
	// built before the reconcilers (port providers such as the host stream,
	// which is the HostFleet, and shared objects such as the PKI issuer and the
	// key manager); the others after them (they may use Deps.Fleet).
	Order int
	New   func(ctx context.Context, d *Deps) (any, error)
}

// GRPCService is a gRPC server component: (*Server).Register(grpc.ServiceRegistrar).
type GRPCService interface {
	Register(grpc.ServiceRegistrar)
}

// HTTPService is an HTTP server component (the STS).
type HTTPService interface {
	Handler() http.Handler
}

// Runner is a long-running component: (*Server).Run(ctx) error.
type Runner interface {
	Run(ctx context.Context) error
}

// ReadyChecker contributes to the readiness probe.
type ReadyChecker interface {
	Ready(req *http.Request) error
}

// BootstrapDeps are handed to bootstrap steps (Helm pre-install/pre-upgrade hook).
type BootstrapDeps struct {
	Client client.Client
	// Kube is a client-go clientset for steps written against it.
	Kube      kubernetes.Interface
	Namespace string
	Release   string
	// Config is nil when the hook runs without a mounted controller.json.
	Config *config.Controller
	// CertsFile lists the server certificates to create (JSON []pki.CertSpec).
	CertsFile string
	Log       *slog.Logger
}

// BootstrapStep is one idempotent bootstrap action (create-if-absent; never
// overwrite existing Secrets).
type BootstrapStep struct {
	Name  string
	Order int
	Run   func(ctx context.Context, d *BootstrapDeps) error
}

var registry struct {
	sync.Mutex
	factories  []Factory
	compute    func(ctx context.Context, d *Deps) (ports.Compute, error)
	buildQueue func(ctx context.Context, d *Deps) (ports.BuildQueue, error)
	bootstrap  []BootstrapStep
}

// RegisterComponent adds a component factory (call from init()).
func RegisterComponent(f Factory) {
	registry.Lock()
	defer registry.Unlock()
	registry.factories = append(registry.factories, f)
}

// ProvideCompute installs the ports.Compute adapter factory (EC2). It is only
// called when the configuration has an aws section.
func ProvideCompute(f func(ctx context.Context, d *Deps) (ports.Compute, error)) {
	registry.Lock()
	defer registry.Unlock()
	registry.compute = f
}

// ProvideBuildQueue installs the ports.BuildQueue adapter factory (BuildQueueState client).
func ProvideBuildQueue(f func(ctx context.Context, d *Deps) (ports.BuildQueue, error)) {
	registry.Lock()
	defer registry.Unlock()
	registry.buildQueue = f
}

// RegisterBootstrapStep adds a bootstrap step (call from init()).
func RegisterBootstrapStep(s BootstrapStep) {
	registry.Lock()
	defer registry.Unlock()
	registry.bootstrap = append(registry.bootstrap, s)
}

// componentsFor returns the factories of a mode in construction order.
func componentsFor(mode Mode) []Factory {
	registry.Lock()
	defer registry.Unlock()
	var out []Factory
	for _, f := range registry.factories {
		if slices.Contains(f.Modes, mode) {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func bootstrapSteps() []BootstrapStep {
	registry.Lock()
	defer registry.Unlock()
	out := slices.Clone(registry.bootstrap)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CompiledIn lists the registered components and adapters (for `version` and startup logs).
func CompiledIn() []string {
	registry.Lock()
	defer registry.Unlock()
	var out []string
	for _, f := range registry.factories {
		out = append(out, f.Name)
	}
	if registry.compute != nil {
		out = append(out, "compute")
	}
	if registry.buildQueue != nil {
		out = append(out, "buildqueue")
	}
	for _, s := range registry.bootstrap {
		out = append(out, "bootstrap:"+s.Name)
	}
	sort.Strings(out)
	return out
}
