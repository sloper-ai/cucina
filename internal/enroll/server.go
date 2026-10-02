// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/proto"
)

// PoolSettingsProvider is implemented by the controller (internal/pools.Set):
// the WorkerSettings for a worker of pool on node (EC2 instance ID or
// "<serial>/<vm>") and the pool's current generation.
type PoolSettingsProvider interface {
	SettingsFor(pool, node string) (*cucinav1.WorkerSettings, string, error)
}

// Revoker deny-lists a workload identity (docs/security.md §Deny-list `sub`
// entries) until the given time; internal/keys implements the deny-list.
type Revoker interface {
	RevokeSubject(ctx context.Context, sub, reason, actor string, until time.Time) error
}

// Options tune the server; zero values take the defaults below.
type Options struct {
	// ClusterID is the cucina:cluster tag value of this installation (required for EC2).
	ClusterID string
	// AWSAccountID and AWSRegion are what identity documents must carry (required for EC2).
	AWSAccountID string
	AWSRegion    string
	// MaxBootAge bounds now - pendingTime of an identity document (default 20m:
	// longer than the slowest startupTimeout, 15m for Windows).
	MaxBootAge time.Duration
	// ClockSkew tolerated for pendingTime in the future (default 2m).
	ClockSkew time.Duration
	// ReplayWindow after the first issuance during which the same instance launch may
	// enroll again with the same key (crash recovery; default 10m).
	ReplayWindow time.Duration
	// MaxIssuancesPerLaunch bounds certificates per instance launch (default 3).
	MaxIssuancesPerLaunch int
	// HostEndpoint is returned to approved hosts (address of HostService).
	HostEndpoint string
	// PendingRetryAfter is the polling interval suggested to pending hosts (default 30s).
	PendingRetryAfter time.Duration
	// DefaultTokenTTL (default 7 days) and MaxTokenTTL (default 90 days) for site tokens.
	DefaultTokenTTL time.Duration
	MaxTokenTTL     time.Duration
	// Per-source rate limits (requests per minute, burst). Defaults: workers 120/60,
	// hosts 60/30 (a site's Macs share one NAT address).
	WorkerRatePerMinute, WorkerBurst int
	HostRatePerMinute, HostBurst     int
	// ExtraTrustPEM is appended to the CA bundle in responses (e.g. the CA of
	// endpoint certificates issued outside Cucina's CA: cert-manager/ACME).
	ExtraTrustPEM []byte
}

func (o *Options) defaults() {
	set := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	set(&o.MaxBootAge, 20*time.Minute)
	set(&o.ClockSkew, 2*time.Minute)
	set(&o.ReplayWindow, 10*time.Minute)
	set(&o.PendingRetryAfter, 30*time.Second)
	set(&o.DefaultTokenTTL, 7*24*time.Hour)
	set(&o.MaxTokenTTL, 90*24*time.Hour)
	seti := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	seti(&o.MaxIssuancesPerLaunch, 3)
	seti(&o.WorkerRatePerMinute, 120)
	seti(&o.WorkerBurst, 60)
	seti(&o.HostRatePerMinute, 60)
	seti(&o.HostBurst, 30)
}

// Deps are the server's dependencies. EC2 dependencies (Identity, Compute,
// Launches, Replay) are optional as a group: without them EnrollWorker answers
// FailedPrecondition (an installation with only Mac pools).
type Deps struct {
	Issuer *pki.Issuer
	Clock  ports.Clock
	Logger *slog.Logger
	Pools  PoolSettingsProvider

	Identity *IdentityVerifier
	Compute  ports.Compute
	Launches LaunchRecords
	Replay   ReplayStore

	Hosts  HostStore
	Tokens TokenStore
	// Revoker is optional; RemoveHost deny-lists the host identity when set.
	Revoker Revoker
	// Rand is the token randomness (default crypto/rand).
	Rand io.Reader

	Options Options
}

// Server implements cucinav1.EnrollmentServiceServer and the certificate side
// of HostService.
type Server struct {
	cucinav1.UnimplementedEnrollmentServiceServer
	d          Deps
	o          Options
	log        *slog.Logger
	workerRate *sourceLimiter
	hostRate   *sourceLimiter
	admin      *Admin
}

// New validates the dependencies and returns a Server.
func New(d Deps) (*Server, error) {
	var errs []error
	if d.Issuer == nil || d.Clock == nil || d.Pools == nil || d.Hosts == nil || d.Tokens == nil {
		errs = append(errs, errors.New("enroll: Issuer, Clock, Pools, Hosts and Tokens are required"))
	}
	ec2 := 0
	for _, set := range []bool{d.Identity != nil, d.Compute != nil, d.Launches != nil, d.Replay != nil} {
		if set {
			ec2++
		}
	}
	if ec2 != 0 && ec2 != 4 {
		errs = append(errs, errors.New("enroll: EC2 enrollment needs Identity, Compute, Launches and Replay together"))
	}
	if d.Compute != nil && (d.Options.ClusterID == "" || d.Options.AWSAccountID == "" || d.Options.AWSRegion == "") {
		errs = append(errs, errors.New("enroll: EC2 enrollment needs ClusterID, AWSAccountID and AWSRegion"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Rand == nil {
		d.Rand = rand.Reader
	}
	d.Options.defaults()
	s := &Server{d: d, o: d.Options, log: d.Logger.With("component", "enroll")}
	s.workerRate = newSourceLimiter(s.o.WorkerRatePerMinute, s.o.WorkerBurst, d.Clock)
	s.hostRate = newSourceLimiter(s.o.HostRatePerMinute, s.o.HostBurst, d.Clock)
	s.admin = &Admin{s: s}
	return s, nil
}

// Register registers the EnrollmentService.
func (s *Server) Register(r grpc.ServiceRegistrar) { cucinav1.RegisterEnrollmentServiceServer(r, s) }

// Admin returns the management hooks (cucinactl hosts/enroll-token).
func (s *Server) Admin() *Admin { return s.admin }

// Run prunes launch records older than a day (every replica; idempotent).
func (s *Server) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.d.Clock.After(10 * time.Minute):
			if s.d.Replay == nil {
				continue
			}
			if n, err := s.d.Replay.Prune(ctx, s.d.Clock.Now().Add(-24*time.Hour)); err != nil {
				s.log.Warn("pruning launch records failed", "error", err)
			} else if n > 0 {
				s.log.Debug("pruned launch records", "count", n)
			}
		}
	}
}

// bundle is the trust bundle sent with every certificate.
func (s *Server) bundle(issued *pki.Issued) []byte {
	return append(append([]byte(nil), issued.BundlePEM...), s.o.ExtraTrustPEM...)
}

// checkProtocol is the R-TEST-7 handshake: a major mismatch is refused precisely.
func checkProtocol(v *cucinav1.ProtocolVersion, role string) error {
	if err := proto.Check(proto.Version{Major: v.GetMajor(), Minor: v.GetMinor()}, role); err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return nil
}

// audit writes one audit line (never token material).
func (s *Server) audit(ctx context.Context, event, result string, attrs ...any) {
	base := []any{"audit", true, "event", event, "result", result, "source", sourceOf(ctx)}
	s.log.InfoContext(ctx, "audit", append(base, attrs...)...)
}

// unavailable logs an internal failure and returns a retryable status that does
// not echo internal details to unauthenticated callers.
func (s *Server) unavailable(ctx context.Context, op string, err error) error {
	s.log.ErrorContext(ctx, "enrollment backend failure", "op", op, "error", err)
	return status.Error(codes.Unavailable, fmt.Sprintf("%s: temporarily unavailable, retry later", op))
}
