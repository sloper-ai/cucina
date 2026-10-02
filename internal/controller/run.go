// SPDX-License-Identifier: FSL-1.1-ALv2

// Package controller assembles the cucina-controller process: strict
// configuration loading, logging, the controller-runtime manager with leader
// election and probes, the metrics/HTTP-SD listener, the gRPC/HTTPS servers of
// the components compiled in (STS, enrollment, host stream, management API),
// startup self-checks, and the reconcilers (internal/reconcile).
package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/httpsd"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/internal/ports"
	cucinaproto "github.com/sloper-ai/cucina/internal/proto"
	"github.com/sloper-ai/cucina/internal/reconcile"
	"github.com/sloper-ai/cucina/invariants"
)

// Paths of the probe endpoints (R-CP-6).
const (
	LivenessPath  = "/-/healthy"
	ReadinessPath = "/-/ready"
)

// Options configure Run.
type Options struct {
	ConfigPath string
	Mode       Mode
	// LogOutput defaults to stderr.
	LogOutput io.Writer
	// RESTConfig overrides the in-cluster/kubeconfig discovery (tests).
	RESTConfig *rest.Config
	// SkipSelfChecks disables the RBAC/AWS startup checks (tests only).
	SkipSelfChecks bool
	// CertsFile is the in-cluster certificate list (JSON []pki.CertSpec) the
	// leader keeps renewed; empty disables the certificate rotator.
	CertsFile string
}

// NewScheme returns the scheme with client-go's and Cucina's types.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// Run starts the controller (ModeController) or the STS-only process
// (ModeSTS) and blocks until ctx is cancelled (SIGTERM: graceful shutdown).
func Run(ctx context.Context, o Options) error {
	cfg, err := LoadConfig(o.ConfigPath, o.Mode)
	if err != nil {
		return err
	}
	log := SetupLogging(cfg.Observability.LogLevel, o.LogOutput)
	log.Info("starting cucina-controller", "mode", o.Mode, "version", Version, "commit", Commit,
		"protocol", cucinaproto.Current.String(), "compiledIn", CompiledIn())

	shutdownTracing, err := SetupTracing(ctx, cfg.Observability.OTLPEndpoint, log)
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() { _ = shutdownTracing(context.WithoutCancel(ctx)) }()

	restCfg := o.RESTConfig
	if restCfg == nil {
		if restCfg, err = ctrl.GetConfig(); err != nil {
			return fmt.Errorf("kubernetes client configuration: %w", err)
		}
	}
	scheme, err := NewScheme()
	if err != nil {
		return err
	}
	tlsCfgs, err := NewTLSConfigs(cfg)
	if err != nil {
		return fmt.Errorf("TLS material: %w", err)
	}

	clock, rnd := SystemClock{}, NewSystemRand()
	sd := httpsd.NewCache(nil, 30*time.Second, clock.Now)
	extra := map[string]http.Handler{}
	if o.Mode == ModeController {
		extra[httpsd.Path] = &httpsd.Handler{Source: sd, Port: cfg.Worker.MetricsPort, Log: log}
	}
	grace := 30 * time.Second
	le := cfg.LeaderElection
	mopts := ctrl.Options{
		Scheme:                  scheme,
		Cache:                   cache.Options{DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}}},
		Metrics:                 metricsserver.Options{BindAddress: cfg.Listeners.Metrics, ExtraHandlers: extra},
		HealthProbeBindAddress:  cfg.Listeners.Probes,
		LivenessEndpointName:    LivenessPath,
		ReadinessEndpointName:   ReadinessPath,
		GracefulShutdownTimeout: &grace,
		// The STS Deployment is leaderless; the controller elects one leader
		// that runs the reconcilers and the autoscaler (R-SCALE-5).
		LeaderElection:                o.Mode == ModeController && le.Enabled,
		LeaderElectionID:              le.ID,
		LeaderElectionNamespace:       cfg.Namespace,
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 &le.LeaseDuration.Duration,
		RenewDeadline:                 &le.RenewDeadline.Duration,
		RetryPeriod:                   &le.RetryPeriod.Duration,
	}
	mgr, err := ctrl.NewManager(restCfg, mopts)
	if err != nil {
		return fmt.Errorf("controller manager: %w", err)
	}

	m, err := metrics.New(crmetrics.Registry)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	for _, inv := range invariants.All {
		m.InvariantViolations.WithLabelValues(string(inv)).Add(0)
	}
	invariants.SetReporter(invariants.ReporterFunc(func(v invariants.Violation) {
		m.InvariantViolated(string(v.Invariant))
		log.Error("invariant violated", "invariant", string(v.Invariant), "pool", v.Pool, "subject", v.Subject, "detail", v.Detail)
	}))

	leader := mgr.Elected()
	d := &Deps{
		Mode: o.Mode, Config: cfg, Log: log, Clock: clock, Rand: rnd, Metrics: m, Registry: crmetrics.Registry,
		RESTConfig: restCfg, Scheme: scheme, Manager: mgr, Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		TLS: tlsCfgs, Elected: leader, CertsFile: o.CertsFile,
		IsLeader: func() bool {
			select {
			case <-leader:
				return true
			default:
				return false
			}
		},
	}

	if o.Mode == ModeController {
		if d.Catalog, err = pools.LoadCatalog(cfg.PlatformsFile); err != nil {
			return err
		}
		d.Pools = pools.NewSet(cfg)
		if err := buildAdapters(ctx, d); err != nil {
			return err
		}
		if d.Compute != nil {
			// Non-leaders (and a leader before its first decision) answer HTTP SD
			// from a cached tag-filtered Describe; the leader's loops publish fresher data.
			compute, cluster := d.Compute, cfg.ClusterID
			sd.SetFetch(func(ctx context.Context) ([]ports.Instance, error) {
				return compute.Describe(ctx, ports.InstanceFilter{Cluster: cluster, States: []ports.InstanceState{ports.InstanceRunning}})
			})
		}
	}

	servers, err := buildComponents(ctx, d, func(f Factory) bool { return f.Order < 0 })
	if err != nil {
		return err
	}

	if o.Mode == ModeController {
		comps, err := reconcile.Setup(mgr, reconcile.Options{
			Config: cfg, Catalog: d.Catalog, Pools: d.Pools,
			Compute: d.Compute, BuildQueue: d.BuildQueue, HostFleet: d.HostFleet,
			Clock: clock, Rand: rnd, Metrics: m, Log: log, SD: sd, UserData: UserData(cfg),
		})
		if err != nil {
			return fmt.Errorf("reconcilers: %w", err)
		}
		d.Fleet = comps.Fleet
		if comps.Cost != nil {
			d.Share(SharedCost, comps.Cost)
		}
	}
	more, err := buildComponents(ctx, d, func(f Factory) bool { return f.Order >= 0 })
	if err != nil {
		return err
	}
	servers = append(servers, more...)
	servers = append(servers, grpcRunnables(d)...)
	for _, s := range servers {
		if err := mgr.Add(s); err != nil {
			return err
		}
	}

	if o.Mode == ModeController {
		if ll := newLeaderLabeler(mgr.GetClient(), cfg.Namespace, leader, log); ll != nil {
			if err := mgr.Add(ll); err != nil {
				return err
			}
		}
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("informers", func(req *http.Request) error {
		cctx, cancel := context.WithTimeout(req.Context(), time.Second)
		defer cancel()
		if !mgr.GetCache().WaitForCacheSync(cctx) {
			return errors.New("informer caches not synced")
		}
		return nil
	}); err != nil {
		return err
	}

	if !o.SkipSelfChecks {
		if err := selfChecks(ctx, d, o.Mode); err != nil {
			return err
		}
	}
	log.Info("manager starting", "namespace", cfg.Namespace, "leaderElection", mopts.LeaderElection)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	log.Info("shut down cleanly")
	return nil
}

// selfChecks fails fast on missing permissions (R-TEST-7).
func selfChecks(ctx context.Context, d *Deps, mode Mode) error {
	rc := rest.CopyConfig(d.RESTConfig)
	rc.QPS, rc.Burst = 50, 100 // ~60 access reviews at startup
	direct, err := client.New(rc, client.Options{Scheme: d.Scheme})
	if err != nil {
		return err
	}
	if err := CheckRBAC(ctx, direct, d.Config.Namespace, RequiredRules(mode)); err != nil {
		return err
	}
	if mode == ModeController {
		if err := CheckAWS(ctx, d.Compute, d.Config); err != nil {
			return err
		}
	}
	d.Log.Info("self-checks passed", "rbacRules", len(RequiredRules(mode)), "aws", d.Config.AWS != nil)
	return nil
}

// buildAdapters creates the port adapters compiled in.
func buildAdapters(ctx context.Context, d *Deps) error {
	registry.Lock()
	computeF, bqF := registry.compute, registry.buildQueue
	registry.Unlock()
	if d.Config.AWS != nil {
		if computeF == nil {
			return errors.New("the configuration has an aws section but no EC2 adapter is compiled in")
		}
		c, err := computeF(ctx, d)
		if err != nil {
			return fmt.Errorf("EC2 adapter: %w", err)
		}
		d.Compute = c
	}
	if bqF == nil {
		return errors.New("no BuildQueue adapter is compiled in (internal/controller/components_buildqueue.go): the autoscaler cannot run")
	}
	bq, err := bqF(ctx, d)
	if err != nil {
		return fmt.Errorf("BuildQueue adapter: %w", err)
	}
	d.BuildQueue = bq
	return nil
}

// buildComponents constructs every registered component of the mode and
// returns the servers (runnables) to add to the manager.
func buildComponents(ctx context.Context, d *Deps, phase func(Factory) bool) ([]manager.Runnable, error) {
	cfg := d.Config
	addr := map[Listener]string{
		ListenerSTS: cfg.Listeners.STS, ListenerManagement: cfg.Listeners.Management,
		ListenerEnrollment: cfg.Listeners.Enrollment, ListenerHost: cfg.Listeners.Host,
	}
	maxMsg := int(cfg.Worker.MaximumMessageSizeBytes)
	if d.grpc == nil {
		d.grpc = map[Listener]*grpcServer{}
	}
	grpcs := d.grpc
	grpcFor := func(l Listener) (*grpcServer, error) {
		if s, ok := grpcs[l]; ok {
			return s, nil
		}
		if addr[l] == "" {
			return nil, fmt.Errorf("listener %s is not configured (/listeners/%s)", l, l)
		}
		tlsCfg := d.TLS.Server
		if l == ListenerHost {
			tlsCfg = d.TLS.MutualServer
		}
		if tlsCfg == nil {
			return nil, fmt.Errorf("listener %s needs TLS (/tls)", l)
		}
		s := newGRPCServer(l, addr[l], tlsCfg, maxMsg, d.Log)
		grpcs[l] = s
		return s, nil
	}
	var out []manager.Runnable
	for _, f := range componentsFor(d.Mode) {
		if !phase(f) {
			continue
		}
		v, err := f.New(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", f.Name, err)
		}
		if v == nil {
			continue
		}
		used := false
		if g, ok := v.(GRPCService); ok && f.Listener != ListenerNone && f.Listener != ListenerSTS {
			s, err := grpcFor(f.Listener)
			if err != nil {
				return nil, fmt.Errorf("component %s: %w", f.Name, err)
			}
			g.Register(s.srv)
			s.services++
			used = true
		}
		if h, ok := v.(HTTPService); ok && f.Listener == ListenerSTS {
			if addr[ListenerSTS] == "" || d.TLS.Server == nil {
				return nil, fmt.Errorf("component %s needs /listeners/sts and /tls", f.Name)
			}
			out = append(out, newHTTPSServer(ListenerSTS, addr[ListenerSTS], h.Handler(), d.TLS.Server, d.Log))
			used = true
		}
		if r, ok := v.(manager.Runnable); ok {
			out = append(out, r)
			used = true
		} else if r, ok := v.(Runner); ok {
			leader := false
			if lr, ok := v.(manager.LeaderElectionRunnable); ok {
				leader = lr.NeedLeaderElection()
			}
			out = append(out, runnerAdapter{name: f.Name, r: r, leader: leader})
			used = true
		}
		if rc, ok := v.(ReadyChecker); ok && d.Manager != nil {
			if err := d.Manager.AddReadyzCheck(f.Name, rc.Ready); err != nil {
				return nil, err
			}
		}
		if !used {
			d.Log.Debug("component has no server or runner", "component", f.Name)
		}
	}
	return out, nil
}

// grpcRunnables returns the gRPC servers that got at least one service.
func grpcRunnables(d *Deps) []manager.Runnable {
	var out []manager.Runnable
	for _, l := range []Listener{ListenerManagement, ListenerEnrollment, ListenerHost} {
		if s := d.grpc[l]; s != nil && s.services > 0 {
			out = append(out, s)
		}
	}
	return out
}

// UserDataFunc builds the EC2 boot-data renderer (public, non-secret
// internal/workeragent/bootdata); installed by components_bootdata.go.
var UserDataFunc func(cfg *config.Controller) func(pool, generation string) ([]byte, error)

// UserData returns the boot data renderer for EC2 launches (nil when not compiled in).
func UserData(cfg *config.Controller) func(pool domain.PoolName, generation string) ([]byte, error) {
	if UserDataFunc == nil {
		return nil
	}
	f := UserDataFunc(cfg)
	return func(pool domain.PoolName, generation string) ([]byte, error) { return f(string(pool), generation) }
}
