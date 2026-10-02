// SPDX-License-Identifier: FSL-1.1-ALv2

// Package link is hostd's side of HostService (R-MAC-1, R-MAC-6): it dials OUT
// to the controller over mTLS (works behind NAT), sends Hello (facts and the
// full VM/image inventory, so a reconnect is a full resync), heartbeats with
// metrics, executes controller commands idempotently by command_id, queues
// results and events across reconnects, and reconnects with jittered
// exponential backoff (backoff/v7). It also serves the unary RPCs hostd needs
// (IssueVMIdentity, GetRegistryCredentials, RenewCertificate) on the same
// connection.
package link

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v7"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/ports"
	cproto "github.com/sloper-ai/cucina/internal/proto"
)

// ErrDisconnected is returned by unary calls while no session is up.
var ErrDisconnected = status.Error(codes.Unavailable, "hostd is not connected to the controller")

// Handler executes one controller command. send streams LogData for
// diagnostics. The returned result's command_id is filled in by the link.
type Handler interface {
	Handle(ctx context.Context, msg *cucinav1.ControllerMessage, send func(*cucinav1.HostMessage)) *cucinav1.CommandResult
}

// Options configure a Link.
type Options struct {
	// Endpoint returns the HostService address (host:port).
	Endpoint func() string
	// TLS returns the mTLS client configuration (host certificate + Cucina CA).
	TLS func() *tls.Config
	// Dial overrides the connection factory (tests inject fault-injecting dialers).
	DialOptions []grpc.DialOption
	Hello       func() *cucinav1.Hello
	Heartbeat   func() *cucinav1.Heartbeat
	Handler     Handler
	// OnWelcome applies the controller's settings (slots, endpoints, images).
	OnWelcome func(*cucinav1.Welcome)
	// OnConnected/OnDisconnected observe session changes (metrics, dead-man inputs).
	OnConnected    func()
	OnDisconnected func(error)
	Clock          ports.Clock
	Log            *slog.Logger
	// Backoff bounds (defaults 1s .. 60s).
	MinBackoff, MaxBackoff time.Duration
	// DefaultHeartbeat is used when the Welcome carries none (default 10s).
	DefaultHeartbeat time.Duration
	// OutboxSize bounds queued messages (default 1024); the oldest events are dropped first.
	OutboxSize int
}

// Link maintains the controller session.
type Link struct {
	o Options

	mu          sync.Mutex
	conn        *grpc.ClientConn
	connected   bool
	lastContact time.Time
	welcome     *cucinav1.Welcome
	outbox      []*cucinav1.HostMessage
	notify      chan struct{}
	commands    *dedup
	sessions    int
}

// New returns a Link.
func New(o Options) *Link {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Minute
	}
	if o.DefaultHeartbeat <= 0 {
		o.DefaultHeartbeat = 10 * time.Second
	}
	if o.OutboxSize <= 0 {
		o.OutboxSize = 1024
	}
	return &Link{o: o, notify: make(chan struct{}, 1), commands: newDedup(1024)}
}

// Connected reports whether a session is up.
func (l *Link) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connected
}

// LastContact is the time of the last message from the controller.
func (l *Link) LastContact() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastContact
}

// Sessions counts established sessions (reconnect tests).
func (l *Link) Sessions() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessions
}

// Send queues a message (VMEvent, CommandResult, LogData) for the controller;
// it survives reconnects. When the outbox is full the oldest VM event is dropped.
func (l *Link) Send(m *cucinav1.HostMessage) {
	l.mu.Lock()
	if len(l.outbox) >= l.o.OutboxSize {
		dropped := false
		for i, q := range l.outbox {
			if q.GetVmEvent() != nil {
				l.outbox = append(l.outbox[:i], l.outbox[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			l.outbox = l.outbox[1:]
		}
	}
	l.outbox = append(l.outbox, m)
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// Event queues a VM event.
func (l *Link) Event(ev *cucinav1.VMEvent) {
	l.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_VmEvent{VmEvent: ev}})
}

func (l *Link) client() (cucinav1.HostServiceClient, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil || !l.connected {
		return nil, ErrDisconnected
	}
	return cucinav1.NewHostServiceClient(l.conn), nil
}

// IssueVMIdentity implements vmm.Controller.
func (l *Link) IssueVMIdentity(ctx context.Context, req *cucinav1.IssueVMIdentityRequest) (*cucinav1.IssueVMIdentityResponse, error) {
	c, err := l.client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.IssueVMIdentity(ctx, req)
}

// RegistryCreds implements vmm.Controller (short-lived pull credentials, R-MAC-5).
func (l *Link) RegistryCreds(ctx context.Context, image string) (*ports.RegistryCreds, error) {
	c, err := l.client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := c.GetRegistryCredentials(ctx, &cucinav1.GetRegistryCredentialsRequest{Image: image})
	if err != nil {
		return nil, err
	}
	if r.GetUsername() == "" {
		return nil, nil
	}
	return &ports.RegistryCreds{Host: r.GetHost(), Username: r.GetUsername(), Password: r.GetPassword()}, nil
}

// RenewCertificate renews the host certificate over the current session.
func (l *Link) RenewCertificate(ctx context.Context, csrPEM []byte) (*cucinav1.RenewCertificateResponse, error) {
	c, err := l.client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.RenewCertificate(ctx, &cucinav1.RenewCertificateRequest{CsrPem: csrPEM})
}

// Run keeps a session up until ctx is done.
func (l *Link) Run(ctx context.Context) error {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = l.o.MinBackoff
	bo.MaxInterval = l.o.MaxBackoff
	for {
		started := l.o.Clock.Now()
		err := l.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if l.o.Clock.Now().Sub(started) > 2*l.o.MaxBackoff {
			bo.Reset()
		}
		wait := bo.NextBackOff()
		l.o.Log.Warn("controller session ended; reconnecting", "err", err, "retry_in", wait)
		if l.o.OnDisconnected != nil {
			l.o.OnDisconnected(err)
		}
		if err := l.o.Clock.Sleep(ctx, wait); err != nil {
			return nil
		}
	}
}

func (l *Link) session(ctx context.Context) error {
	addr := l.o.Endpoint()
	if addr == "" {
		return errors.New("no HostService endpoint")
	}
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(l.o.TLS())),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 20 * time.Second, PermitWithoutStream: true}),
	}, l.o.DialOptions...)
	conn, err := grpc.NewClient("passthrough:///"+addr, opts...)
	if err != nil {
		return err
	}
	defer conn.Close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := cucinav1.NewHostServiceClient(conn).Connect(sctx)
	if err != nil {
		return err
	}
	hello := l.o.Hello()
	hello.Protocol = &cucinav1.ProtocolVersion{Major: cproto.Major, Minor: cproto.Minor}
	if err := stream.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_Hello{Hello: hello}}); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	w := first.GetWelcome()
	if w == nil {
		return errors.New("controller did not answer Hello with Welcome")
	}
	if err := cproto.Check(cproto.Version{Major: w.GetProtocol().GetMajor(), Minor: w.GetProtocol().GetMinor()}, "controller"); err != nil {
		return err
	}
	l.mu.Lock()
	l.conn, l.connected, l.welcome = conn, true, w
	l.lastContact = l.o.Clock.Now()
	l.sessions++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.connected = false
		l.conn = nil
		l.mu.Unlock()
	}()
	l.o.Log.Info("connected to controller", "endpoint", addr, "cluster", w.GetClusterId())
	if l.o.OnWelcome != nil {
		l.o.OnWelcome(w)
	}
	if l.o.OnConnected != nil {
		l.o.OnConnected()
	}
	interval := w.GetHeartbeatInterval().AsDuration()
	if interval <= 0 {
		interval = l.o.DefaultHeartbeat
	}
	sendErr := make(chan error, 1)
	go func() { sendErr <- l.sender(sctx, stream, interval) }()
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			l.mu.Lock()
			l.lastContact = l.o.Clock.Now()
			l.mu.Unlock()
			l.dispatch(ctx, msg)
		}
	}()
	select {
	case err := <-sendErr:
		return err
	case err := <-recvErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Link) sender(ctx context.Context, stream cucinav1.HostService_ConnectClient, interval time.Duration) error {
	hb := l.o.Clock.After(0)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hb:
			msg := l.o.Heartbeat()
			msg.Time = timestamppb.New(l.o.Clock.Now())
			if err := stream.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_Heartbeat{Heartbeat: msg}}); err != nil {
				return err
			}
			hb = l.o.Clock.After(interval)
		case <-l.notify:
		}
		for {
			l.mu.Lock()
			if len(l.outbox) == 0 {
				l.mu.Unlock()
				break
			}
			m := l.outbox[0]
			l.mu.Unlock()
			if err := stream.Send(m); err != nil {
				return err // m stays queued for the next session
			}
			l.mu.Lock()
			if len(l.outbox) > 0 && l.outbox[0] == m {
				l.outbox = l.outbox[1:]
			}
			l.mu.Unlock()
		}
	}
}

// dispatch runs a command once per command_id; duplicates (a controller retry
// after a reconnect) get the stored result again, or nothing while running.
// CancelCommand cancels the context of a running command (e.g. a followed log).
func (l *Link) dispatch(ctx context.Context, msg *cucinav1.ControllerMessage) {
	if msg.GetWelcome() != nil {
		if l.o.OnWelcome != nil {
			l.o.OnWelcome(msg.GetWelcome())
		}
		return
	}
	id := msg.GetCommandId()
	if id == "" {
		l.o.Log.Warn("command without command_id ignored")
		return
	}
	if c := msg.GetCancelCommand(); c != nil {
		found := l.commands.cancel(c.GetTargetCommandId())
		l.o.Log.Info("command cancelled by the controller", "target", c.GetTargetCommandId(), "running", found)
		l.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_CommandResult{CommandResult: &cucinav1.CommandResult{CommandId: id, Ok: true}}})
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	res, state := l.commands.begin(id, cancel)
	switch state {
	case dupDone:
		cancel()
		l.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_CommandResult{CommandResult: res}})
		return
	case dupRunning:
		cancel()
		return
	}
	go func() {
		defer cancel()
		r := l.o.Handler.Handle(cctx, msg, l.Send)
		if r == nil {
			r = &cucinav1.CommandResult{Ok: true}
		}
		r.CommandId = id
		l.commands.finish(id, r)
		l.Send(&cucinav1.HostMessage{Message: &cucinav1.HostMessage_CommandResult{CommandResult: r}})
	}()
}

// Running is the number of commands currently executing.
func (l *Link) Running() int { return l.commands.running() }

type dupState int

const (
	dupNew dupState = iota
	dupRunning
	dupDone
)

// dedup remembers the last N command ids and their results, and the cancel
// functions of running commands.
type dedup struct {
	mu      sync.Mutex
	max     int
	order   []string
	res     map[string]*cucinav1.CommandResult // nil while running
	cancels map[string]context.CancelFunc
}

func newDedup(n int) *dedup {
	return &dedup{max: n, res: map[string]*cucinav1.CommandResult{}, cancels: map[string]context.CancelFunc{}}
}

func (d *dedup) begin(id string, cancel context.CancelFunc) (*cucinav1.CommandResult, dupState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.res[id]; ok {
		if r == nil {
			return nil, dupRunning
		}
		return r, dupDone
	}
	d.res[id] = nil
	d.cancels[id] = cancel
	d.order = append(d.order, id)
	for len(d.order) > d.max {
		old := d.order[0]
		if d.res[old] == nil {
			break // the oldest command is still running; keep it (bounded by concurrency)
		}
		d.order = d.order[1:]
		delete(d.res, old)
	}
	return nil, dupNew
}

func (d *dedup) finish(id string, r *cucinav1.CommandResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.res[id]; ok {
		d.res[id] = r
	}
	delete(d.cancels, id)
}

func (d *dedup) cancel(id string) bool {
	d.mu.Lock()
	c := d.cancels[id]
	d.mu.Unlock()
	if c != nil {
		c()
	}
	return c != nil
}

func (d *dedup) running() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.cancels)
}

// Errorf builds a failed CommandResult.
func Errorf(format string, a ...any) *cucinav1.CommandResult {
	return &cucinav1.CommandResult{Ok: false, Error: fmt.Sprintf(format, a...)}
}
