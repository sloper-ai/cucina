// SPDX-License-Identifier: FSL-1.1-ALv2

package hostlinktest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/durationpb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/enroll"
	"github.com/sloper-ai/cucina/internal/hostlink"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Addresses of the test controller on the in-memory network.
const (
	EnrollAddr = "controller.test:8445"
	HostAddr   = "controller.test:8446"
	ServerName = "controller.test"
)

// Counter counts EnrollHost calls and approved exchanges (an interceptor on
// the enrollment server: "the token is exchanged once" is observable).
type Counter struct {
	mu        sync.Mutex
	calls     int
	exchanges int
}

// Exchanges returns the approved token exchanges and all EnrollHost calls.
func (c *Counter) Exchanges() (exchanges, calls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exchanges, c.calls
}

func (c *Counter) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	resp, err := h(ctx, req)
	if r, ok := resp.(*cucinav1.EnrollHostResponse); ok {
		c.mu.Lock()
		c.calls++
		if err == nil && r.GetStatus() == cucinav1.EnrollHostResponse_STATUS_APPROVED {
			c.exchanges++
		}
		c.mu.Unlock()
	}
	return resp, err
}

// Settings returns macOS WorkerSettings for any pool (shape of bbconfig's fixture).
type Settings struct{}

// SettingsFor implements enroll.PoolSettingsProvider.
func (Settings) SettingsFor(pool, node string) (*cucinav1.WorkerSettings, string, error) {
	p := func(kv ...string) []*cucinav1.PlatformProperty {
		var out []*cucinav1.PlatformProperty
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, &cucinav1.PlatformProperty{Name: kv[i], Value: kv[i+1]})
		}
		return out
	}
	return &cucinav1.WorkerSettings{
		Pool: pool, Node: node,
		Runners: []*cucinav1.RunnerSettings{
			{Name: "xcode", Platform: p("OSFamily", "macos", "ISA", "arm-a64", "xcode-version", "27.0")},
			{Name: "generic", Platform: p("OSFamily", "macos", "ISA", "arm-a64")},
		},
		SchedulerEndpoint: "placeholder:1", StorageEndpoint: "placeholder:1", ServerName: "workers.cucina.test",
		BuildDirectory: "native", L1Placement: "vm-disk", L1SizeBytes: 40 << 30, MaximumMessageSizeBytes: 16 << 20,
		WanCompression: true, SizeClass: 1, InstanceNamePrefixes: []string{"main"}, MetricsPort: 9986,
	}, "g1", nil
}

// Controller is an in-memory controller: the real internal/enroll server
// (EnrollmentService over TLS, host renewal and VM identities) and
// hostlink.Server (HostService over mTLS), backed by a real pki.Issuer.
type Controller struct {
	Net    *Net
	CA     *pki.CA
	Issuer *pki.Issuer
	Enroll *enroll.Server
	// Token is a valid site enrollment token (created at start).
	Token   string
	Counter *Counter
	Clock   ports.Clock
	// Changed receives a token whenever a host's state changes (event-driven waits).
	Changed chan struct{}

	mu       sync.Mutex
	host     *hostlink.Server
	hostSrv  *grpc.Server
	cancel   context.CancelFunc
	serverTC *tls.Config
	welcome  hostlink.WelcomeProvider
}

// Clock adapts the time package (virtualised inside testing/synctest bubbles).
type Clock struct{}

// Now implements ports.Clock.
func (Clock) Now() time.Time { return time.Now() }

// After implements ports.Clock.
func (Clock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Sleep implements ports.Clock.
func (Clock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DefaultWelcome gives every host 2 slots and the relay upstreams.
func DefaultWelcome(string) *cucinav1.Welcome {
	return &cucinav1.Welcome{ClusterId: "test", HeartbeatInterval: durationpb.New(10 * time.Second), Slots: 2,
		Settings: &cucinav1.HostSettings{CentralEndpoint: "storage.test:8981", SchedulerEndpoint: "scheduler.test:8983"}}
}

// Enroll is the real internal/enroll server. Create it OUTSIDE a
// testing/synctest bubble: its rate-limiter caches start goroutines that never
// exit. The CA is set later, inside the bubble, so certificates are valid on
// the bubble's fake clock.
type Enroll struct {
	Server *enroll.Server
	Issuer *pki.Issuer
	ca     *lazyCA
}

type lazyCA struct {
	mu sync.Mutex
	ca *pki.CA
}

// Current implements pki.CASource.
func (l *lazyCA) Current() *pki.CA {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ca
}

// NewEnroll builds the enrollment server (call before synctest.Test).
func NewEnroll(t testing.TB) *Enroll {
	t.Helper()
	l := &lazyCA{}
	iss, err := pki.NewIssuer(l, Clock{}, pki.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	es, err := enroll.New(enroll.Deps{Issuer: iss, Clock: Clock{}, Pools: Settings{}, Hosts: enroll.NewMemoryHosts(),
		Tokens: enroll.NewMemoryTokens(), Logger: slog.New(slog.DiscardHandler),
		Options: enroll.Options{HostEndpoint: HostAddr, PendingRetryAfter: 30 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	return &Enroll{Server: es, Issuer: iss, ca: l}
}

// NewController starts the enrollment and host services on n. e may be nil
// outside synctest bubbles.
func NewController(t testing.TB, n *Net, e *Enroll) *Controller {
	t.Helper()
	if e == nil {
		e = NewEnroll(t)
	}
	ca := pkitest.NewCA(t, time.Now())
	e.ca.mu.Lock()
	e.ca.ca = ca
	e.ca.mu.Unlock()
	iss, es := e.Issuer, e.Server
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvCert, err := iss.IssueServer(&key.PublicKey, "controller", []string{ServerName}, nil, false, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := tls.X509KeyPair(srvCert.ChainPEM, mustKeyPEM(t, key))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := es.Admin().CreateEnrollToken(context.Background(), &cucinav1.CreateEnrollTokenRequest{Site: "test-site", MaxHosts: 10}, "test")
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Net: n, CA: ca, Issuer: iss, Clock: Clock{}, welcome: DefaultWelcome, Changed: make(chan struct{}, 1),
		Enroll: es, Token: tok.GetToken(), Counter: &Counter{}}
	c.serverTC = &tls.Config{Certificates: []tls.Certificate{tc}, MinVersion: tls.VersionTLS12}

	enrollSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(c.serverTC)), grpc.UnaryInterceptor(c.Counter.intercept))
	es.Register(enrollSrv)
	go func() { _ = enrollSrv.Serve(n.Listen(EnrollAddr)) }()
	t.Cleanup(enrollSrv.Stop)
	c.StartHostService(t)
	t.Cleanup(c.StopHostService)
	return c
}

func mustKeyPEM(t testing.TB, key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pemBlock("PRIVATE KEY", der)
}

// Approve admits a serial (cucinactl hosts approve / register).
func (c *Controller) Approve(t testing.TB, serial string) {
	t.Helper()
	if _, err := c.Enroll.Admin().ApproveHost(context.Background(), serial, "test"); err != nil {
		t.Fatal(err)
	}
}

// Host returns the current HostService server (ports.HostFleet).
func (c *Controller) Host() *hostlink.Server {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.host
}

// SetWelcome replaces the Welcome provider for new sessions.
func (c *Controller) SetWelcome(w hostlink.WelcomeProvider) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.welcome = w
}

// StartHostService starts a fresh HostService (a restarted controller has no state).
func (c *Controller) StartHostService(t testing.TB) {
	t.Helper()
	pool := x509.NewCertPool()
	for _, r := range c.CA.Roots() {
		pool.AddCert(r)
	}
	mtls := c.serverTC.Clone()
	mtls.ClientCAs = pool
	mtls.ClientAuth = tls.RequireAndVerifyClientCert
	h, err := hostlink.New(hostlink.Deps{
		Certs:    c.Enroll,
		Registry: hostlink.StaticRegistry{Host: "ghcr.io", Username: "cucina-bot", Password: "short-lived", Clock: Clock{}},
		Welcome: func(serial string) *cucinav1.Welcome {
			c.mu.Lock()
			w := c.welcome
			c.mu.Unlock()
			return w(serial)
		},
		Clock: Clock{}, StaleAfter: 45 * time.Second,
		OnChange: func(string) {
			select {
			case c.Changed <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(mtls)),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 20 * time.Second}))
	h.Register(srv)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = h.Run(ctx) }()
	ln := c.Net.Listen(HostAddr)
	go func() { _ = srv.Serve(ln) }()
	c.mu.Lock()
	c.host, c.hostSrv, c.cancel = h, srv, cancel
	c.mu.Unlock()
}

// StopHostService stops the HostService (controller crash).
func (c *Controller) StopHostService() {
	c.mu.Lock()
	srv, cancel := c.hostSrv, c.cancel
	c.hostSrv, c.cancel = nil, nil
	c.mu.Unlock()
	if srv != nil {
		srv.Stop()
		c.Net.Unlisten(HostAddr)
	}
	if cancel != nil {
		cancel()
	}
}

// CAPool returns the CA roots as certificates (for hostd's CACertificate).
func (c *Controller) CAPool() []*x509.Certificate { return c.CA.Roots() }
