// SPDX-License-Identifier: FSL-1.1-ALv2

package hostlinktest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
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
	Token      = "cst_test-site-token"
)

// Enrollment is a fake EnrollmentService (host side of R-SEC-3): the token is
// multi-use, serials stay pending until approved, re-enrollment is denied.
type Enrollment struct {
	cucinav1.UnimplementedEnrollmentServiceServer
	Issuer       *pki.Issuer
	HostEndpoint string

	mu        sync.Mutex
	approved  map[string]bool
	enrolled  map[string]bool
	exchanges int
	calls     int
}

// Approve admits a serial (cucinactl hosts approve).
func (e *Enrollment) Approve(serial string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.approved[serial] = true
}

// Exchanges is the number of successful token exchanges; Calls every EnrollHost call.
func (e *Enrollment) Exchanges() (exchanges, calls int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exchanges, e.calls
}

// EnrollHost implements EnrollmentServiceServer.
func (e *Enrollment) EnrollHost(_ context.Context, req *cucinav1.EnrollHostRequest) (*cucinav1.EnrollHostResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if req.GetSiteToken() != Token {
		return &cucinav1.EnrollHostResponse{Status: cucinav1.EnrollHostResponse_STATUS_TOKEN_INVALID, Message: "token rejected"}, nil
	}
	serial, err := pki.CanonicalSerial(req.GetSerialNumber())
	if err != nil {
		return &cucinav1.EnrollHostResponse{Status: cucinav1.EnrollHostResponse_STATUS_DENIED, Message: err.Error()}, nil
	}
	if e.enrolled[serial] {
		return &cucinav1.EnrollHostResponse{Status: cucinav1.EnrollHostResponse_STATUS_DENIED, Message: "already enrolled"}, nil
	}
	if !e.approved[serial] {
		return &cucinav1.EnrollHostResponse{Status: cucinav1.EnrollHostResponse_STATUS_PENDING, RetryAfter: durationpb.New(30 * time.Second)}, nil
	}
	is, err := e.Issuer.IssueHost(req.GetCsrPem(), serial)
	if err != nil {
		return &cucinav1.EnrollHostResponse{Status: cucinav1.EnrollHostResponse_STATUS_DENIED, Message: err.Error()}, nil
	}
	e.enrolled[serial] = true
	e.exchanges++
	return &cucinav1.EnrollHostResponse{Status: cucinav1.EnrollHostResponse_STATUS_APPROVED, CertificatePem: is.ChainPEM,
		CaPem: is.BundlePEM, ExpiresAt: timestamppb.New(is.NotAfter), HostEndpoint: e.HostEndpoint}, nil
}

// Settings returns macOS WorkerSettings for any pool (shape of bbconfig's fixture).
type Settings struct{}

// VMSettings implements hostlink.SettingsProvider.
func (Settings) VMSettings(_ context.Context, pool, serial, vm string) (*cucinav1.WorkerSettings, error) {
	p := func(kv ...string) []*cucinav1.PlatformProperty {
		var out []*cucinav1.PlatformProperty
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, &cucinav1.PlatformProperty{Name: kv[i], Value: kv[i+1]})
		}
		return out
	}
	return &cucinav1.WorkerSettings{
		Pool: pool, Node: serial + "/" + vm,
		Runners: []*cucinav1.RunnerSettings{
			{Name: "xcode", Platform: p("OSFamily", "macos", "ISA", "arm-a64", "xcode-version", "27.0")},
			{Name: "generic", Platform: p("OSFamily", "macos", "ISA", "arm-a64")},
		},
		SchedulerEndpoint: "placeholder:1", StorageEndpoint: "placeholder:1", ServerName: "workers.cucina.test",
		BuildDirectory: "native", L1Placement: "vm-disk", L1SizeBytes: 40 << 30, MaximumMessageSizeBytes: 16 << 20,
		WanCompression: true, SizeClass: 1, InstanceNamePrefixes: []string{"main"}, MetricsPort: 9986,
	}, nil
}

// Controller is an in-memory controller: EnrollmentService (TLS) and
// HostService (mTLS) backed by a real pki.Issuer and hostlink.Server.
type Controller struct {
	Net    *Net
	CA     *pki.CA
	Issuer *pki.Issuer
	Enroll *Enrollment
	Clock  ports.Clock
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

// NewController starts the enrollment and host services on n.
func NewController(t testing.TB, n *Net) *Controller {
	t.Helper()
	ca := pkitest.NewCA(t, time.Now())
	iss, err := pki.NewIssuer(ca, Clock{}, pki.Policy{})
	if err != nil {
		t.Fatal(err)
	}
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
	c := &Controller{Net: n, CA: ca, Issuer: iss, Clock: Clock{}, welcome: DefaultWelcome, Changed: make(chan struct{}, 1),
		Enroll: &Enrollment{Issuer: iss, HostEndpoint: HostAddr, approved: map[string]bool{}, enrolled: map[string]bool{}}}
	c.serverTC = &tls.Config{Certificates: []tls.Certificate{tc}, MinVersion: tls.VersionTLS12}

	enrollSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(c.serverTC)))
	cucinav1.RegisterEnrollmentServiceServer(enrollSrv, c.Enroll)
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
		Issuer: hostlink.PKIIssuer{I: c.Issuer}, Settings: Settings{},
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
