// SPDX-License-Identifier: FSL-1.1-ALv2

// Package hostd is cucina-hostd, the macOS host agent (R-MAC-1..6, R-MAC-10):
// it enrolls the host once with the site token, keeps an outbound mTLS session
// to the controller, runs up to two Tart VMs through the lifecycle core, relays
// the VMs' traffic to the host L2 cache and the scheduler, supervises the L2
// bb_storage, enforces the dead-man limits for its VMs and reports facts,
// inventory and metrics. It is crash-only: every restart rebuilds state from
// Tart, the VM journal and the controller's resync.
package hostd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/hostd/config"
	"github.com/sloper-ai/cucina/internal/hostd/facts"
	"github.com/sloper-ai/cucina/internal/hostd/identity"
	"github.com/sloper-ai/cucina/internal/hostd/l2"
	"github.com/sloper-ai/cucina/internal/hostd/lifecycle"
	"github.com/sloper-ai/cucina/internal/hostd/link"
	"github.com/sloper-ai/cucina/internal/hostd/metrics"
	"github.com/sloper-ai/cucina/internal/hostd/relay"
	"github.com/sloper-ai/cucina/internal/hostd/render"
	"github.com/sloper-ai/cucina/internal/hostd/vmm"
	"github.com/sloper-ai/cucina/internal/hostlink/hostcmd"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Default listen addresses (docs/dev/hostd.md §2).
const (
	DefaultStorageRelay   = ":8981"
	DefaultSchedulerRelay = ":8983"
	DefaultL2Listen       = "127.0.0.1:8991"
	DefaultL2Metrics      = "127.0.0.1:9991"
	DefaultHostPort       = "8446"
	PruneBudgetBytes      = 250 * 1000 * 1000 * 1000
)

// VMNet configures the vmnet DHCP lease (root only; nil in user mode).
type VMNet interface {
	EnsureLease(ctx context.Context, seconds int) error
}

// Options wire an Agent.
type Options struct {
	Config   config.Config
	UserMode bool
	StateDir string // identity, journal, L2 state
	LogDir   string
	Version  string
	BootID   string

	Exec    ports.Exec
	FS      ports.FS
	Clock   ports.Clock
	Secrets ports.SecretStore
	Runtime ports.VMRuntime
	Facts   func(context.Context) (facts.Facts, error)
	Render  render.Renderer
	VMNet   VMNet

	// L2Binary is bb_storage; empty disables the L2 (tests).
	L2Binary       string
	L2CacheDir     string // default <StateDir>/l2/cache
	L2Listen       string
	L2Metrics      string
	StorageRelay   string
	SchedulerRelay string
	// Gateway and SameNetwork default to the host's interfaces (relay package).
	Gateway     func(netip.Addr) (netip.Addr, bool)
	SameNetwork func(local, remote netip.Addr) bool
	// StorageRelayListener/SchedulerRelayListener and RelayDial replace real
	// sockets (tests run in-memory under testing/synctest).
	StorageRelayListener, SchedulerRelayListener net.Listener
	RelayDial                                    func(ctx context.Context, addr string) (net.Conn, error)
	// ScrapeActivity overrides the bb_worker metrics probe (tests).
	ScrapeActivity func(ctx context.Context, ip netip.Addr, port uint32) (uint64, error)
	// EnrollDialOptions/HostDialOptions are appended to the gRPC dial options (tests).
	EnrollDialOptions []grpc.DialOption
	HostDialOptions   []grpc.DialOption
	Limits            lifecycle.Limits
	MinBackoff        time.Duration
	MaxBackoff        time.Duration
	TickInterval      time.Duration
	ProbeInterval     time.Duration
	HousekeepInterval time.Duration
	LevelVar          *slog.LevelVar
	Log               *slog.Logger
	// Ready, if set, is closed once enrollment finished and the services started.
	Ready chan struct{}
}

// Agent is a running hostd.
type Agent struct {
	o       Options
	log     *slog.Logger
	store   identity.Store
	holder  identity.Holder
	key     *ecdsa.PrivateKey
	state   *identity.State
	facts   facts.Facts
	vmm     *vmm.Manager
	link    *link.Link
	l2      *l2.Supervisor
	relays  []*relay.Relay
	desired []string

	mu       sync.Mutex
	welcome  *cucinav1.Welcome
	tunables config.Tunables
}

// New validates options and returns an Agent.
func New(o Options) (*Agent, error) {
	if o.Exec == nil || o.FS == nil || o.Clock == nil || o.Secrets == nil || o.Runtime == nil || o.Render == nil || o.Facts == nil {
		return nil, errors.New("hostd: missing dependency")
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Limits == (lifecycle.Limits{}) {
		o.Limits = lifecycle.DefaultLimits()
	}
	o.Limits.DefaultMaxAge = o.Config.VMMaxAge
	if o.StorageRelay == "" {
		o.StorageRelay = DefaultStorageRelay
	}
	if o.SchedulerRelay == "" {
		o.SchedulerRelay = DefaultSchedulerRelay
	}
	if o.L2Listen == "" {
		o.L2Listen = DefaultL2Listen
	}
	if o.L2Metrics == "" {
		o.L2Metrics = DefaultL2Metrics
	}
	if o.Gateway == nil {
		o.Gateway = relay.GatewayFor
	}
	if o.SameNetwork == nil {
		o.SameNetwork = relay.InterfaceNetworks
	}
	if o.HousekeepInterval <= 0 {
		o.HousekeepInterval = 10 * time.Minute
	}
	if o.BootID == "" {
		o.BootID = fmt.Sprintf("%d", o.Clock.Now().UnixNano())
	}
	return &Agent{o: o, log: o.Log, store: identity.Store{Secrets: o.Secrets, FS: o.FS, StateDir: o.StateDir}}, nil
}

// CertificateExpiry is when the host's current mTLS certificate expires (zero before enrollment).
func (a *Agent) CertificateExpiry() time.Time {
	if leaf := a.holder.Leaf(); leaf != nil {
		return leaf.NotAfter
	}
	return time.Time{}
}

// VMs exposes the VM manager (tests and diagnostics).
func (a *Agent) VMs() *vmm.Manager { return a.vmm }

// Link exposes the controller link (tests).
func (a *Agent) Link() *link.Link { return a.link }

// EnsureIdentity loads the host identity or performs the one-time enrollment.
// With IdentityLabel the MDM-issued keychain identity is used instead (the
// stronger path, R-SEC-3): no token is exchanged.
func (a *Agent) EnsureIdentity(ctx context.Context) error {
	if label := a.o.Config.IdentityLabel; label != "" {
		var caPEM []byte
		for _, c := range a.o.Config.CACertificates {
			caPEM = append(caPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
		}
		if len(caPEM) == 0 {
			return &config.Error{Key: "CACertificate", Msg: "IdentityLabel needs CACertificate (the Cucina CA bundle)"}
		}
		if err := a.holder.SetKeychain(func() (*tls.Certificate, error) { return identity.KeychainIdentity(label) }, caPEM); err != nil {
			return fmt.Errorf("MDM identity %q: %w", label, err)
		}
		host, _, _ := net.SplitHostPort(a.o.Config.EnrollAddress())
		a.state = &identity.State{Serial: a.facts.Serial, CAPEM: caPEM, HostEndpoint: net.JoinHostPort(host, DefaultHostPort)}
		a.log.Info("using the MDM-issued keychain identity", "label", label)
		return nil
	}
	key, err := a.store.Key(ctx)
	if err != nil {
		return err
	}
	a.key = key
	st, err := a.store.Load()
	if err != nil {
		return err
	}
	if st == nil {
		cfg := a.o.Config
		if cfg.SiteEnrollmentToken == "" {
			return identity.ErrNoToken
		}
		csr, err := identity.CSR(key, a.facts.Serial)
		if err != nil {
			return err
		}
		opts := append([]grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(identity.EnrollTLS(cfg)))}, a.o.EnrollDialOptions...)
		conn, err := grpc.NewClient("passthrough:///"+cfg.EnrollAddress(), opts...)
		if err != nil {
			return err
		}
		defer conn.Close()
		a.log.Info("enrolling host", "serial", a.facts.Serial, "endpoint", cfg.EnrollAddress())
		resp, err := identity.Enroll(ctx, cucinav1.NewEnrollmentServiceClient(conn), identity.EnrollParams{
			Token: cfg.SiteEnrollmentToken, Serial: a.facts.Serial, Hostname: a.facts.Hostname, CSRPEM: csr,
			Facts: a.facts.Proto(cfg.Site, cfg.Labels),
		}, a.o.Clock, a.log)
		if err != nil {
			return err
		}
		ep := resp.GetHostEndpoint()
		if ep == "" {
			host, _, _ := net.SplitHostPort(cfg.EnrollAddress())
			ep = net.JoinHostPort(host, DefaultHostPort)
		}
		st = &identity.State{Serial: a.facts.Serial, CertPEM: resp.GetCertificatePem(), CAPEM: resp.GetCaPem(),
			HostEndpoint: ep, ExpiresAt: resp.GetExpiresAt().AsTime(), EnrolledAt: a.o.Clock.Now()}
		if err := a.store.Save(st); err != nil {
			return err
		}
		a.log.Info("host enrolled", "serial", st.Serial, "host_endpoint", ep, "expires", st.ExpiresAt)
	}
	leaf, err := identity.ParseLeaf(st.CertPEM)
	if err != nil {
		return fmt.Errorf("stored host certificate: %w", err)
	}
	if !a.o.Clock.Now().Before(leaf.NotAfter) {
		return identity.ErrExpired
	}
	if err := a.holder.Set(st.CertPEM, st.CAPEM, key); err != nil {
		return err
	}
	a.state = st
	return nil
}

// Run starts every component and blocks until ctx is done or one fails fatally.
func (a *Agent) Run(ctx context.Context) error {
	f, err := a.o.Facts(ctx)
	if err != nil {
		return fmt.Errorf("host facts: %w", err)
	}
	a.facts = f
	if err := a.EnsureIdentity(ctx); err != nil {
		return err
	}
	if a.o.VMNet != nil && !a.o.UserMode {
		if err := a.o.VMNet.EnsureLease(ctx, 600); err != nil {
			a.log.Warn("could not set the vmnet DHCP lease to 600 s", "err", err)
		}
	}
	cfg := a.o.Config
	a.tunables = cfg.Tunables(0, nil)
	if a.o.L2Binary != "" {
		a.l2 = &l2.Supervisor{Exec: a.o.Exec, FS: a.o.FS, Secrets: a.o.Secrets, Clock: a.o.Clock, Render: a.o.Render,
			Binary: a.o.L2Binary, Dir: filepath.Join(a.o.StateDir, "l2"), Log: a.log.With("component", "l2")}
	}
	a.link = link.New(link.Options{
		Endpoint:    func() string { return a.state.HostEndpoint },
		TLS:         func() *tls.Config { return a.holder.ClientTLS(a.o.Config.ServerName()) },
		DialOptions: a.o.HostDialOptions,
		Hello:       a.hello,
		Heartbeat:   a.heartbeat,
		Handler:     a,
		OnWelcome:   a.onWelcome,
		Clock:       a.o.Clock,
		Log:         a.log.With("component", "link"),
		MinBackoff:  a.o.MinBackoff,
		MaxBackoff:  a.o.MaxBackoff,
	})
	a.vmm = vmm.New(vmm.Options{
		Runtime: a.o.Runtime, Clock: a.o.Clock, FS: a.o.FS, Render: a.o.Render, Controller: a.link,
		HostCAPEM: a.hostCAPEM, Prefix: cfg.VMNamePrefix, Host: a.facts.Serial,
		Capacity:    lifecycle.HostCapacity{Cores: f.Cores, MemoryGiB: f.MemoryGiB},
		Limits:      a.o.Limits,
		JournalPath: filepath.Join(a.o.StateDir, "vms.json"),
		StoragePort: portOf(a.o.StorageRelay, 8981), SchedulerPort: portOf(a.o.SchedulerRelay, 8983),
		Gateway: a.o.Gateway, Events: a.link.Event, Log: a.log.With("component", "vmm"), ScrapeActivity: a.o.ScrapeActivity,
		TickInterval: a.o.TickInterval, ProbeInterval: a.o.ProbeInterval,
	})
	if err := a.vmm.Load(); err != nil {
		return fmt.Errorf("vm journal: %w", err)
	}
	a.vmm.SetTunables(vmm.Tunables{Slots: a.tunables.Slots, VMCPU: a.tunables.VMCPU, VMMemoryGiB: a.tunables.VMMemoryGiB})
	storage := &relay.Relay{Name: "storage", Listen: a.o.StorageRelay, Admit: a.vmm, SameNetwork: a.o.SameNetwork,
		Upstream: func() string {
			if a.l2 == nil {
				return ""
			}
			return a.o.L2Listen
		}, Log: a.log, Listener: a.o.StorageRelayListener, DialUpstream: a.o.RelayDial}
	scheduler := &relay.Relay{Name: "scheduler", Listen: a.o.SchedulerRelay, Admit: a.vmm, SameNetwork: a.o.SameNetwork,
		Upstream: func() string {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.tunables.SchedulerEndpoint
		}, OnUpstream: a.vmm.UpstreamOK, Log: a.log, Listener: a.o.SchedulerRelayListener, DialUpstream: a.o.RelayDial}
	a.relays = []*relay.Relay{storage, scheduler}
	for _, r := range a.relays {
		if err := r.Start(); err != nil {
			return fmt.Errorf("relay %s on %s: %w", r.Name, r.Listen, err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 8)
	var wg sync.WaitGroup
	run := func(name string, f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(ctx); err != nil && ctx.Err() == nil {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}
	run("vmm", a.vmm.Run)
	run("link", a.link.Run)
	for _, r := range a.relays {
		run("relay-"+r.Name, r.Serve)
	}
	if a.l2 != nil {
		run("l2", a.l2.Run)
	}
	if cfg.MetricsListen != "" {
		reg := metrics.Registry(a.metricsSource())
		run("metrics", func(ctx context.Context) error { return metrics.Serve(ctx, cfg.MetricsListen, reg) })
	}
	run("housekeeping", a.housekeeping)
	run("deadman-inputs", func(ctx context.Context) error { return a.upstreamLiveness(ctx, scheduler) })
	a.log.Info("hostd running", "serial", a.facts.Serial, "user_mode", a.o.UserMode, "slots", a.tunables.Slots,
		"version", a.o.Version)
	if a.o.Ready != nil {
		close(a.o.Ready)
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
		a.log.Error("component failed; exiting (launchd restarts hostd)", "err", runErr)
	}
	cancel()
	wg.Wait()
	return runErr
}

func portOf(addr string, def int) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n == 0 {
		return def
	}
	return n
}

func (a *Agent) hostCAPEM(ctx context.Context) ([]byte, error) {
	if a.l2 == nil {
		return nil, nil
	}
	return a.l2.HostCAPEM(ctx)
}

func (a *Agent) hello() *cucinav1.Hello {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var images []string
	if imgs, err := a.o.Runtime.Images(ctx); err == nil {
		for _, i := range imgs {
			images = append(images, i.Reference)
		}
	}
	sort.Strings(images)
	return &cucinav1.Hello{SerialNumber: a.facts.Serial, Facts: a.facts.Proto(a.o.Config.Site, a.o.Config.Labels),
		Vms: a.vmm.Inventory(), Images: images, BootId: a.o.BootID}
}

func (a *Agent) heartbeat() *cucinav1.Heartbeat {
	m := &cucinav1.HostMetrics{RunningVms: uint32(a.vmm.RunningCount())}
	if _, free, err := a.o.FS.DiskUsage(a.o.StateDir); err == nil {
		m.DiskFreeBytes = free
	}
	if a.l2 != nil {
		st := a.l2.Stats()
		m.L2Hits, m.L2Misses, m.WanBytesReceived, m.WanBytesSent, m.L2SizeBytes = st.Hits, st.Misses, st.WANReceived, st.WANSent, st.SizeBytes
	}
	return &cucinav1.Heartbeat{Vms: a.vmm.Inventory(), Metrics: m, Cordoned: a.vmm.Cordoned()}
}

func (a *Agent) onWelcome(w *cucinav1.Welcome) {
	a.mu.Lock()
	a.welcome = w
	a.tunables = a.o.Config.Tunables(w.GetSlots(), w.GetSettings())
	t := a.tunables
	a.desired = append([]string(nil), w.GetDesiredImages()...)
	a.mu.Unlock()
	a.applyTunables(t)
}

func (a *Agent) applyTunables(t config.Tunables) {
	a.vmm.SetTunables(vmm.Tunables{Slots: t.Slots, VMCPU: t.VMCPU, VMMemoryGiB: t.VMMemoryGiB})
	if a.o.LevelVar != nil {
		a.o.LevelVar.Set(levelOf(t.LogLevel))
	}
	if a.l2 != nil && t.CentralEndpoint != "" {
		go a.configureL2(t)
	}
}

// L2ClientKeyName holds the L2 upstream key when the host identity is a
// non-extractable keychain identity (bb_storage needs key files).
const L2ClientKeyName = "host-l2-client-key"

func (a *Agent) configureL2(t config.Tunables) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	certPEM, key := a.state.CertPEM, a.key
	if a.o.Config.IdentityLabel != "" {
		// A Cucina host certificate for a dedicated file key, issued over the
		// session the keychain identity authenticated.
		k, err := identity.Store{Secrets: a.o.Secrets, FS: a.o.FS, StateDir: a.o.StateDir}.KeyNamed(ctx, L2ClientKeyName)
		if err != nil {
			a.log.Error("l2: client key", "err", err)
			return
		}
		csr, err := identity.CSR(k, a.facts.Serial)
		if err != nil {
			return
		}
		resp, err := a.link.RenewCertificate(ctx, csr)
		if err != nil {
			a.log.Error("l2: client certificate", "err", err)
			return
		}
		certPEM, key = resp.GetCertificatePem(), k
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		a.log.Error("l2: host key", "err", err)
		return
	}
	host, _, _ := net.SplitHostPort(t.CentralEndpoint)
	a.l2.Configure(l2.Settings{
		ListenAddress: a.o.L2Listen, UpstreamAddress: t.CentralEndpoint, UpstreamServerName: host,
		CABundlePEM: a.state.CAPEM, HostSerial: strings.ToUpper(a.facts.Serial),
		CacheDir: a.l2CacheDir(), CacheSizeBytes: uint64(t.L2SizeGiB) << 30,
		MaximumMessageSizeBytes: t.MaximumMessageSizeBytes, MetricsListenAddress: a.o.L2Metrics,
	}, certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func (a *Agent) l2CacheDir() string {
	if a.o.L2CacheDir != "" {
		return a.o.L2CacheDir
	}
	return filepath.Join(a.o.StateDir, "l2", "cache")
}

func levelOf(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// upstreamLiveness feeds the dead-man "scheduler reachable" input (R-POOL-7):
// a VM whose worker holds an open relayed connection to the scheduler reaches
// it; dial outcomes are reported by the relay itself.
func (a *Agent) upstreamLiveness(ctx context.Context, scheduler *relay.Relay) error {
	for {
		if err := a.o.Clock.Sleep(ctx, 30*time.Second); err != nil {
			return nil
		}
		for _, vm := range scheduler.LiveVMs() {
			a.vmm.UpstreamOK(vm, true)
		}
	}
}

// housekeeping renews the host certificate, pre-pulls desired images while no
// VM runs (R-DATA-5 idle hours) and prunes the image cache (R-MAC-5).
func (a *Agent) housekeeping(ctx context.Context) error {
	for {
		if err := a.o.Clock.Sleep(ctx, a.o.HousekeepInterval); err != nil {
			return nil
		}
		a.renewIfDue(ctx)
		a.mu.Lock()
		desired := append([]string(nil), a.desired...)
		a.mu.Unlock()
		if a.vmm.RunningCount() == 0 && a.link.Connected() {
			for _, img := range desired {
				if err := a.vmm.Pull(ctx, img); err != nil {
					a.log.Warn("pre-pull failed", "image", img, "err", err)
				}
			}
		}
		if err := a.o.Runtime.Prune(ctx, PruneBudgetBytes); err != nil {
			a.log.Warn("tart prune failed", "err", err)
		}
	}
}

func (a *Agent) renewIfDue(ctx context.Context) {
	if a.o.Config.IdentityLabel != "" {
		return // MDM renews its identity
	}
	leaf := a.holder.Leaf()
	if leaf == nil || !identity.NeedsRenewal(leaf, a.o.Clock.Now()) || !a.link.Connected() {
		return
	}
	csr, err := identity.CSR(a.key, a.facts.Serial)
	if err != nil {
		return
	}
	resp, err := a.link.RenewCertificate(ctx, csr)
	if err != nil {
		a.log.Warn("certificate renewal failed; retrying later", "err", err)
		return
	}
	if err := a.holder.Set(resp.GetCertificatePem(), resp.GetCaPem(), a.key); err != nil {
		a.log.Error("renewed certificate unusable", "err", err)
		return
	}
	st := *a.state
	st.CertPEM, st.CAPEM, st.ExpiresAt = resp.GetCertificatePem(), resp.GetCaPem(), resp.GetExpiresAt().AsTime()
	if err := a.store.Save(&st); err != nil {
		a.log.Error("saving renewed certificate", "err", err)
		return
	}
	a.state = &st
	a.log.Info("host certificate renewed", "expires", st.ExpiresAt)
}

// Handle implements link.Handler: controller commands (R-MAC-6).
func (a *Agent) Handle(ctx context.Context, msg *cucinav1.ControllerMessage, send func(*cucinav1.HostMessage)) *cucinav1.CommandResult {
	var err error
	switch m := msg.GetMessage().(type) {
	case *cucinav1.ControllerMessage_StartVm:
		s := m.StartVm
		err = a.vmm.StartVM(lifecycle.StartRequest{Name: s.GetVmName(), Pool: s.GetPool(), Node: s.GetNode(),
			Image: s.GetImage(), Generation: s.GetGeneration(), CPU: int(s.GetCpu()), MemoryGiB: int(s.GetMemoryGib()),
			DiskGiB: int(s.GetDiskGib()), MaxAge: time.Duration(s.GetMaxAgeHours()) * time.Hour})
	case *cucinav1.ControllerMessage_StopVm:
		err = a.vmm.StopVM(m.StopVm.GetVmName(), m.StopVm.GetTimeout().AsDuration(), m.StopVm.GetReason())
	case *cucinav1.ControllerMessage_DeleteVm:
		err = a.vmm.DeleteVM(m.DeleteVm.GetVmName())
	case *cucinav1.ControllerMessage_ReimageVm:
		err = a.vmm.ReimageVM(m.ReimageVm.GetVmName(), m.ReimageVm.GetImage())
	case *cucinav1.ControllerMessage_PullImage:
		err = a.vmm.Pull(ctx, m.PullImage.GetImage())
	case *cucinav1.ControllerMessage_SetCordon:
		a.vmm.SetCordon(m.SetCordon.GetCordoned())
	case *cucinav1.ControllerMessage_CollectDiagnostics:
		err = a.collectDiagnostics(ctx, msg.GetCommandId(), m.CollectDiagnostics, send)
	case *cucinav1.ControllerMessage_UpdateConfig:
		a.mu.Lock()
		slots := uint32(0)
		if a.welcome != nil {
			slots = a.welcome.GetSlots()
		}
		a.tunables = a.o.Config.Tunables(slots, m.UpdateConfig.GetSettings())
		t := a.tunables
		a.mu.Unlock()
		a.applyTunables(t)
	case *cucinav1.ControllerMessage_Ping:
	default:
		err = fmt.Errorf("unsupported command %T", m)
	}
	if err != nil {
		return &cucinav1.CommandResult{Ok: false, Error: hostcmd.Encode(errorCode(err), err.Error())}
	}
	return &cucinav1.CommandResult{Ok: true}
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, vmm.ErrNoSlot):
		return hostcmd.CodeVMLimit
	case errors.Is(err, vmm.ErrCordoned):
		return hostcmd.CodeCordoned
	case errors.Is(err, lifecycle.ErrUnknownVM):
		return hostcmd.CodeNotFound
	case errors.Is(err, vmm.ErrBadName):
		return hostcmd.CodeInvalid
	}
	return hostcmd.CodeFailed
}

func (a *Agent) diagnostics(ctx context.Context, vmLogs bool) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"facts": a.facts, "config": a.o.Config.String(), "inventory": a.vmm.Inventory(),
		"connected": a.link.Connected(), "version": a.o.Version})
	if res, err := a.o.Runtime.List(ctx); err == nil {
		_ = enc.Encode(map[string]any{"tart_list": res})
	}
	if a.o.LogDir != "" {
		if data, err := a.o.FS.ReadFile(filepath.Join(a.o.LogDir, "hostd.log")); err == nil {
			if len(data) > 256<<10 {
				data = data[len(data)-256<<10:]
			}
			b.WriteString("\n--- hostd.log (tail) ---\n")
			b.Write(data)
		}
	}
	if vmLogs {
		for _, vm := range a.vmm.Inventory() {
			if vm.GetState() != "running" {
				continue
			}
			for _, f := range []string{"/var/log/cucina/bb_worker.log", "/var/log/cucina/bb_runner.log"} {
				res, err := a.o.Runtime.GuestExec(ctx, a.o.Config.VMNamePrefix+vm.GetName(),
					ports.Command{Path: "/usr/bin/tail", Args: []string{"-c", "131072", f}})
				if err == nil && res.ExitCode == 0 {
					fmt.Fprintf(&b, "\n--- %s %s (tail) ---\n", vm.GetName(), f)
					b.Write(res.Stdout)
				}
			}
		}
	}
	return b.Bytes()
}

func (a *Agent) metricsSource() metrics.Source {
	return metrics.Source{
		VMStates: func() map[string]int {
			out := map[string]int{}
			for _, vm := range a.vmm.Inventory() {
				out[vm.GetState()]++
			}
			return out
		},
		L2: func() (uint64, uint64, uint64, uint64, uint64) {
			if a.l2 == nil {
				return 0, 0, 0, 0, 0
			}
			st := a.l2.Stats()
			return st.Hits, st.Misses, st.WANReceived, st.WANSent, st.SizeBytes
		},
		DiskFree: func() uint64 {
			_, free, _ := a.o.FS.DiskUsage(a.o.StateDir)
			return free
		},
		Connected: a.link.Connected,
		RelayBytes: func() map[string][2]uint64 {
			out := map[string][2]uint64{}
			for _, r := range a.relays {
				out[r.Name] = [2]uint64{r.BytesFromVMs.Load(), r.BytesToVMs.Load()}
			}
			return out
		},
	}
}
