// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Deps are the server's collaborators. Authenticator, Auditor, Pools, Workers and
// Scheduler are required; every other dependency is optional and the methods that
// need a missing one fail with FAILED_PRECONDITION ("not configured").
type Deps struct {
	Authenticator Authenticator
	Auditor       Auditor
	Pools         PoolView
	Workers       WorkerView
	Scheduler     Scheduler

	PoolAdmin   PoolAdmin
	Orphans     OrphanSweeper // EC2 only
	QueueStats  QueueStats
	Shell       InstanceShell // EC2 worker logs (SSM)
	Hosts       HostView
	HostAdmin   HostAdmin
	Enrollment  Enrollment
	Keys        KeyAdmin
	Revocations RevocationAdmin
	Cost        CostSource
	Images      ImageSource
	Components  ComponentSource
	Alerts      AlertSource
	Support     SupportSource
	LogTail     LogTail
}

// Options tune the server. Zero values select the documented defaults.
type Options struct {
	ClusterID string
	Version   string
	// InstanceNames are the configured Buildbarn instance names; `admin` on all of
	// them makes a principal a cluster admin. Required.
	InstanceNames []string
	// Config is the effective controller configuration, included (redacted) in
	// support bundles.
	Config any

	DefaultEnrollTokenTTL time.Duration // default 24h
	MaxEnrollTokenTTL     time.Duration // default 30 days
	MaxFloorDuration      time.Duration // default 7 days: a floor is standing cost and must expire
	RevocationPropagation time.Duration // default 3m (R-AUTH-9: deny-list effective in ≤ 2–3 min)

	MaxOverviewWatchers   int           // default 64 concurrent WatchOverview streams
	MaxOperationWatchers  int           // default 16 concurrent WatchOperations streams
	OperationsInterval    time.Duration // default 2s between WatchOperations diffs
	MaxOperationsSnapshot int           // default 10000 operations per shared snapshot

	MaxLogStreams     int           // default 8 concurrent StreamWorkerLogs
	LogPollInterval   time.Duration // default 5s between follow polls of one stream
	LogFollowLimit    time.Duration // default 30m: a follow stream ends after this
	SSMCallsPerSecond float64       // default 1: SSM SendCommand budget shared by all log streams
	SSMBurst          int           // default 5

	Logger *slog.Logger // operational log (not the audit log)
}

func (o *Options) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	defInt := func(i *int, v int) {
		if *i <= 0 {
			*i = v
		}
	}
	def(&o.DefaultEnrollTokenTTL, 24*time.Hour)
	def(&o.MaxEnrollTokenTTL, 30*24*time.Hour)
	def(&o.MaxFloorDuration, 7*24*time.Hour)
	def(&o.RevocationPropagation, 3*time.Minute)
	defInt(&o.MaxOverviewWatchers, 64)
	defInt(&o.MaxOperationWatchers, 16)
	def(&o.OperationsInterval, 2*time.Second)
	defInt(&o.MaxOperationsSnapshot, 10000)
	defInt(&o.MaxLogStreams, 8)
	def(&o.LogPollInterval, 5*time.Second)
	def(&o.LogFollowLimit, 30*time.Minute)
	if o.SSMCallsPerSecond <= 0 {
		o.SSMCallsPerSecond = 1
	}
	defInt(&o.SSMBurst, 5)
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Server implements cucinav1.ManagementServiceServer. Register it with Register,
// never with cucinav1.RegisterManagementServiceServer (which would skip the guard).
type Server struct {
	cucinav1.UnimplementedManagementServiceServer

	deps Deps
	opts Options
	log  *slog.Logger

	overview snapshot[*cucinav1.Overview]
	ops      snapshot[[]ports.Operation]

	overviewWatchers gate
	opWatchers       gate
	logStreams       gate
	bundles          gate
	ssm              *limiter

	base     context.Context // server lifetime: shared snapshot builds run under it
	stopBase context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// New validates the dependencies and the method→access table and returns a server.
func New(deps Deps, opts Options) (*Server, error) {
	var missing []string
	for name, v := range map[string]any{
		"Authenticator": deps.Authenticator, "Auditor": deps.Auditor, "Pools": deps.Pools,
		"Workers": deps.Workers, "Scheduler": deps.Scheduler,
	} {
		if v == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("mgmt: missing required dependencies %v", missing)
	}
	if len(opts.InstanceNames) == 0 {
		return nil, errors.New("mgmt: Options.InstanceNames is empty; cluster admin would be undefined")
	}
	for _, n := range opts.InstanceNames {
		if n == "*" {
			return nil, errors.New(`mgmt: "*" is not an instance name`)
		}
	}
	if err := checkAccessTable(&cucinav1.ManagementService_ServiceDesc, methodAccess); err != nil {
		return nil, err
	}
	opts.defaults()
	base, stop := context.WithCancel(context.Background())
	s := &Server{
		deps:             deps,
		opts:             opts,
		log:              opts.Logger.With("component", "mgmt"),
		overviewWatchers: gate{max: opts.MaxOverviewWatchers},
		opWatchers:       gate{max: opts.MaxOperationWatchers},
		logStreams:       gate{max: opts.MaxLogStreams},
		bundles:          gate{max: 1},
		ssm:              &limiter{rate: opts.SSMCallsPerSecond, burst: float64(opts.SSMBurst)},
		base:             base,
		stopBase:         stop,
		done:             make(chan struct{}),
	}
	return s, nil
}

// Register installs the guarded ManagementService on r. Every handler authenticates,
// authorizes and audits before the implementation runs.
func (s *Server) Register(r grpc.ServiceRegistrar) {
	r.RegisterService(s.guardedDesc(), s)
}

// Run blocks until ctx is done, then ends every open stream with UNAVAILABLE so
// clients reconnect to another replica.
func (s *Server) Run(ctx context.Context) error {
	<-ctx.Done()
	s.Stop()
	return nil
}

// Stop ends open streams; it is idempotent.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.done)
		s.stopBase()
	})
}

// errStopping is returned to streams when the server shuts down.
var errStopping = status.Error(codes.Unavailable, "management server shutting down; reconnect")

// streamEnd converts the end of a stream's context into a status error.
func streamEnd(ctx context.Context) error {
	return status.FromContextError(ctx.Err()).Err()
}

// fail maps a source error to a gRPC status error. Status errors pass through.
func fail(op string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	code := codes.Unavailable
	switch {
	case errors.Is(err, context.Canceled):
		code = codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, ports.ErrVMNotFound):
		code = codes.NotFound
	case errors.Is(err, ports.ErrInvalid):
		code = codes.InvalidArgument
	case errors.Is(err, ports.ErrQueueUnknown), errors.Is(err, ports.ErrNotOwned):
		code = codes.FailedPrecondition
	}
	if op == "" {
		return status.Error(code, RedactText(err.Error()))
	}
	return status.Errorf(code, "%s: %s", op, RedactText(err.Error()))
}

// toStatus is the guard's safety net for handlers returning plain errors.
func toStatus(err error) error { return fail("", err) }

func notConfigured(what string) error {
	return status.Errorf(codes.FailedPrecondition, "%s is not configured on this controller", what)
}

func invalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

func notFound(format string, args ...any) error {
	return status.Errorf(codes.NotFound, format, args...)
}

// caller returns the authenticated principal's subject (for created_by fields).
func caller(ctx context.Context) string {
	p, _ := PrincipalFromContext(ctx)
	return p.Subject
}

// ---------------------------------------------------------------- small helpers

// snapshot is a single-flight cache: callers within maxAge of the last build share
// it; at most one build runs at a time and its result (or error) is shared too, so
// N concurrent subscribers cause one build per maxAge, never N.
type snapshot[T any] struct {
	mu  sync.Mutex
	val T
	err error
	at  time.Time
	ok  bool
}

func (c *snapshot[T]) get(maxAge time.Duration, build func() (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && time.Since(c.at) < maxAge {
		return c.val, c.err
	}
	c.val, c.err = build()
	c.at, c.ok = time.Now(), true
	return c.val, c.err
}

// gate bounds concurrent streams.
type gate struct {
	mu  sync.Mutex
	n   int
	max int
}

func (g *gate) acquire(what string) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n >= g.max {
		return nil, status.Errorf(codes.ResourceExhausted, "too many concurrent %s (limit %d); retry later", what, g.max)
	}
	g.n++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.n--
			g.mu.Unlock()
		})
	}, nil
}

// InUse reports how many slots are taken (for tests and metrics).
func (g *gate) inUse() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

// StreamCounts reports the open streams per kind (watchers, operation watchers, log
// streams, support bundles).
func (s *Server) StreamCounts() (overview, operations, logs, bundles int) {
	return s.overviewWatchers.inUse(), s.opWatchers.inUse(), s.logStreams.inUse(), s.bundles.inUse()
}

// limiter is a token bucket shared by every caller.
type limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func (l *limiter) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		if l.last.IsZero() {
			l.tokens = l.burst
		} else {
			l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		}
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
