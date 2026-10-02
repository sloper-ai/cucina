// SPDX-License-Identifier: FSL-1.1-ALv2

// Package hostlink is the controller side of HostService (R-MAC-6): the gRPC
// server the Mac hosts dial (mTLS; identity = URI SAN spiffe://cucina/host/<serial>),
// a per-host session manager with a command queue (timeouts, re-send after a
// reconnect, idempotent command ids), heartbeat staleness (host offline → its
// VMs unavailable) and the ports.HostFleet implementation the autoscaler and the
// MacHost reconciler use. Certificate renewal and VM identities are delegated
// to internal/enroll.Server (Certificates; it keeps the bound-key record of the
// host current) and pull credentials come from a RegistryCreds source.
//
// Wiring (agent coreb): s, _ := hostlink.New(deps); s.Register(grpcServer); go s.Run(ctx);
// use s as ports.HostFleet.
package hostlink

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/hostlink/hostcmd"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/ports"
	cproto "github.com/sloper-ai/cucina/internal/proto"
)

// Errors returned by the HostFleet methods.
var (
	ErrHostOffline = errors.New("host is offline")
	ErrHostUnknown = fmt.Errorf("host is unknown: %w", ports.ErrNotFound)
	ErrRejected    = errors.New("host rejected the command")
)

// Certificates is the certificate side of HostService, implemented by
// *internal/enroll.Server: it checks that the host is still approved and
// enrolled, records the bound key on renewal (key rotation) and returns the
// pool's WorkerSettings with VM identities. peer comes from the verified client
// certificate, never from the request.
type Certificates interface {
	RenewCertificate(ctx context.Context, peer pki.Identity, req *cucinav1.RenewCertificateRequest) (*cucinav1.RenewCertificateResponse, error)
	IssueVMIdentity(ctx context.Context, peer pki.Identity, req *cucinav1.IssueVMIdentityRequest) (*cucinav1.IssueVMIdentityResponse, error)
}

// RegistryCreds returns pull credentials for a host (ADR 0702).
type RegistryCreds interface {
	Credentials(ctx context.Context, serial, image string) (*cucinav1.GetRegistryCredentialsResponse, error)
}

// StaticRegistry hands out one read-only package credential (from a Secret),
// with an advisory expiry so hostd never caches it for long.
type StaticRegistry struct {
	Host, Username, Password string
	TTL                      time.Duration
	Clock                    ports.Clock
}

// Credentials implements RegistryCreds.
func (s StaticRegistry) Credentials(context.Context, string, string) (*cucinav1.GetRegistryCredentialsResponse, error) {
	ttl := s.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &cucinav1.GetRegistryCredentialsResponse{Host: s.Host, Username: s.Username, Password: s.Password,
		ExpiresAt: timestamppb.New(s.Clock.Now().Add(ttl))}, nil
}

// Authorizer admits enrolled hosts (MacHost not denied/removed). Nil allows every
// host with a valid certificate.
type Authorizer interface {
	Allowed(ctx context.Context, serial string) error
}

// WelcomeProvider builds the Welcome for a host (cluster id, heartbeat interval,
// desired images, slots and HostSettings from the MacHost CR and config).
type WelcomeProvider func(serial string) *cucinav1.Welcome

// Deps are the server's dependencies.
type Deps struct {
	Certs    Certificates
	Registry RegistryCreds
	Auth     Authorizer
	Welcome  WelcomeProvider
	Clock    ports.Clock
	Log      *slog.Logger
	// StaleAfter marks a host offline when no message arrived for this long (default 45s).
	StaleAfter time.Duration
	// CommandTimeout bounds commands (default 60s; PullImage 2h; diagnostics 5m).
	CommandTimeout time.Duration
	// OnChange is called (without locks held) when a host's state changes.
	OnChange func(serial string)
	// PeerSerial overrides peer identity extraction (tests without TLS); nil = mTLS URI SAN.
	PeerSerial func(ctx context.Context) (string, error)
}

// Server implements cucinav1.HostServiceServer and ports.HostFleet.
type Server struct {
	cucinav1.UnimplementedHostServiceServer
	d Deps

	mu    sync.Mutex
	hosts map[string]*host
	seq   uint64
}

var _ ports.HostFleet = (*Server)(nil)

type pendingCmd struct {
	msg    *cucinav1.ControllerMessage
	result chan *cucinav1.CommandResult
	logs   []byte
	// stream, when set, receives LogData as it arrives instead of buffering it.
	stream   chan []byte
	overflow bool
}

type host struct {
	serial    string
	session   *session
	lastSeen  time.Time
	facts     *cucinav1.HostFacts
	vms       map[string]*cucinav1.VMInfo
	images    []string
	cordoned  bool
	slots     int
	running   int
	metrics   *cucinav1.HostMetrics
	bootID    string
	pending   map[string]*pendingCmd // command id -> waiter (survives reconnects)
	order     []string               // pending ids in send order
	expectVMs map[string]string      // vm -> pool the controller asked this host to run
}

type session struct {
	id   uint64
	out  chan *cucinav1.ControllerMessage
	done chan struct{}
	once sync.Once
}

func (s *session) close() { s.once.Do(func() { close(s.done) }) }

// New returns a Server.
func New(d Deps) (*Server, error) {
	if d.Certs == nil || d.Clock == nil {
		return nil, errors.New("hostlink: Certs and Clock are required")
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.StaleAfter <= 0 {
		d.StaleAfter = 45 * time.Second
	}
	if d.CommandTimeout <= 0 {
		d.CommandTimeout = time.Minute
	}
	if d.Welcome == nil {
		d.Welcome = func(string) *cucinav1.Welcome { return &cucinav1.Welcome{} }
	}
	return &Server{d: d, hosts: map[string]*host{}}, nil
}

// Register registers the HostService on a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) { cucinav1.RegisterHostServiceServer(r, s) }

// Run sweeps for stale hosts until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	for {
		if err := s.d.Clock.Sleep(ctx, s.d.StaleAfter/3); err != nil {
			return nil
		}
		s.sweep()
	}
}

func (s *Server) sweep() {
	now := s.d.Clock.Now()
	var changed []string
	s.mu.Lock()
	for serial, h := range s.hosts {
		if h.session != nil && now.Sub(h.lastSeen) >= s.d.StaleAfter {
			s.d.Log.Warn("host heartbeat stale; marking offline", "serial", serial, "last_seen", h.lastSeen)
			h.session.close()
			h.session = nil
			changed = append(changed, serial)
		}
	}
	s.mu.Unlock()
	for _, serial := range changed {
		s.changed(serial)
	}
}

func (s *Server) changed(serial string) {
	if s.d.OnChange != nil {
		s.d.OnChange(serial)
	}
}

var serialRe = regexp.MustCompile(`^[A-Z0-9]{6,32}$`)

// peerSerial extracts the serial from the verified client certificate's URI SAN.
func (s *Server) peerSerial(ctx context.Context) (string, error) {
	if s.d.PeerSerial != nil {
		return s.d.PeerSerial(ctx)
	}
	id, err := PeerHost(ctx)
	if err != nil {
		return "", err
	}
	return id.Serial, nil
}

// PeerHost returns the host identity of a verified mTLS client
// (URI SAN spiffe://cucina/host/<serial>).
func PeerHost(ctx context.Context) (pki.Identity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return pki.Identity{}, status.Error(codes.Unauthenticated, "no peer")
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.VerifiedChains) == 0 || len(ti.State.VerifiedChains[0]) == 0 {
		return pki.Identity{}, status.Error(codes.Unauthenticated, "a verified client certificate is required")
	}
	leaf := ti.State.VerifiedChains[0][0]
	if len(leaf.URIs) != 1 {
		return pki.Identity{}, status.Error(codes.Unauthenticated, "client certificate must carry exactly one URI SAN")
	}
	id, err := pki.ParseIdentityURL(leaf.URIs[0])
	if err != nil || id.Role != pki.RoleHost {
		return pki.Identity{}, status.Errorf(codes.PermissionDenied, "client certificate is not a host identity (%s)", leaf.URIs[0])
	}
	return id, nil
}

func (s *Server) authorize(ctx context.Context) (string, error) {
	serial, err := s.peerSerial(ctx)
	if err != nil {
		return "", err
	}
	if !serialRe.MatchString(serial) {
		return "", status.Errorf(codes.PermissionDenied, "invalid host serial %q", serial)
	}
	if s.d.Auth != nil {
		if err := s.d.Auth.Allowed(ctx, serial); err != nil {
			return "", status.Errorf(codes.PermissionDenied, "host %s is not admitted: %v", serial, err)
		}
	}
	return serial, nil
}

func (s *Server) hostLocked(serial string) *host {
	h := s.hosts[serial]
	if h == nil {
		h = &host{serial: serial, vms: map[string]*cucinav1.VMInfo{}, pending: map[string]*pendingCmd{}, expectVMs: map[string]string{}}
		s.hosts[serial] = h
	}
	return h
}

// Connect implements HostService.Connect.
func (s *Server) Connect(stream cucinav1.HostService_ConnectServer) error {
	ctx := stream.Context()
	serial, err := s.authorize(ctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.FailedPrecondition, "the first message must be Hello")
	}
	pv := hello.GetProtocol()
	if err := cproto.Check(cproto.Version{Major: pv.GetMajor(), Minor: pv.GetMinor()}, "cucina-hostd"); err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	if hello.GetSerialNumber() != serial {
		s.d.Log.Warn("host certificate/serial mismatch", "cert_serial", serial, "hello_serial", hello.GetSerialNumber())
		return status.Errorf(codes.PermissionDenied, "certificate is for host %s but Hello claims %q", serial, hello.GetSerialNumber())
	}
	welcome := s.d.Welcome(serial)
	if welcome == nil {
		welcome = &cucinav1.Welcome{}
	}
	welcome.Protocol = &cucinav1.ProtocolVersion{Major: cproto.Major, Minor: cproto.Minor}
	if welcome.HeartbeatInterval == nil {
		welcome.HeartbeatInterval = durationpb.New(s.d.StaleAfter / 3)
	}
	if err := stream.Send(&cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_Welcome{Welcome: welcome}}); err != nil {
		return err
	}

	sess := &session{out: make(chan *cucinav1.ControllerMessage, 256), done: make(chan struct{})}
	s.mu.Lock()
	s.seq++
	sess.id = s.seq
	h := s.hostLocked(serial)
	if h.session != nil {
		h.session.close() // a newer connection replaces the old one
	}
	h.session = sess
	h.lastSeen = s.d.Clock.Now()
	h.facts = hello.GetFacts()
	h.images = append([]string(nil), hello.GetImages()...)
	h.bootID = hello.GetBootId()
	h.slots = int(welcome.GetSlots())
	h.vms = map[string]*cucinav1.VMInfo{} // full resync from the Hello inventory
	for _, vm := range hello.GetVms() {
		h.vms[vm.GetName()] = vm
		if vm.GetPool() != "" {
			h.expectVMs[vm.GetName()] = vm.GetPool()
		}
	}
	// Re-send commands that were pending when the previous session ended (same ids:
	// hostd executes each id once).
	for _, id := range h.order {
		if pc := h.pending[id]; pc != nil {
			select {
			case sess.out <- pc.msg:
			default: // queue full: the waiter times out and the controller retries
			}
		}
	}
	s.mu.Unlock()
	s.d.Log.Info("host connected", "serial", serial, "vms", len(hello.GetVms()), "images", len(hello.GetImages()))
	s.changed(serial)
	defer func() {
		s.mu.Lock()
		if h.session == sess {
			h.session = nil
		}
		s.mu.Unlock()
		sess.close()
		s.changed(serial)
	}()

	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-sess.done:
				sendErr <- nil
				return
			case m := <-sess.out:
				if err := stream.Send(m); err != nil {
					sendErr <- err
					return
				}
			}
		}
	}()
	recvErr := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			s.receive(serial, sess, m)
		}
	}()
	select {
	case err := <-recvErr:
		return err
	case err := <-sendErr:
		if err == nil {
			return status.Error(codes.Aborted, "session replaced or stale")
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) receive(serial string, sess *session, m *cucinav1.HostMessage) {
	changed := false
	s.mu.Lock()
	h := s.hosts[serial]
	if h == nil || h.session != sess {
		s.mu.Unlock()
		return
	}
	h.lastSeen = s.d.Clock.Now()
	switch x := m.GetMessage().(type) {
	case *cucinav1.HostMessage_Heartbeat:
		hb := x.Heartbeat
		h.cordoned = hb.GetCordoned()
		h.metrics = hb.GetMetrics()
		h.running = int(hb.GetMetrics().GetRunningVms())
		h.vms = map[string]*cucinav1.VMInfo{}
		for _, vm := range hb.GetVms() {
			h.vms[vm.GetName()] = vm
		}
		changed = true
	case *cucinav1.HostMessage_VmEvent:
		if vm := x.VmEvent.GetVm(); vm != nil {
			if x.VmEvent.GetEvent() == "deleted" {
				delete(h.vms, vm.GetName())
			} else {
				h.vms[vm.GetName()] = vm
			}
			changed = true
		}
	case *cucinav1.HostMessage_CommandResult:
		r := x.CommandResult
		if pc := h.pending[r.GetCommandId()]; pc != nil {
			delete(h.pending, r.GetCommandId())
			h.order = removeID(h.order, r.GetCommandId())
			pc.result <- r
		}
	case *cucinav1.HostMessage_Log:
		pc := h.pending[x.Log.GetCommandId()]
		switch {
		case pc == nil:
		case pc.stream != nil:
			if len(x.Log.GetData()) > 0 && !pc.overflow {
				select {
				case pc.stream <- x.Log.GetData():
				default:
					pc.overflow = true // a slow reader must not stall the host session
				}
			}
		case len(pc.logs) < 64<<20:
			pc.logs = append(pc.logs, x.Log.GetData()...)
		}
	}
	s.mu.Unlock()
	if changed {
		s.changed(serial)
	}
}

func removeID(ids []string, id string) []string {
	for i, x := range ids {
		if x == id {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}

func newCommandID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Command sends a command and waits for its result. The command stays queued
// across reconnects (re-sent with the same id) until it is answered or times out.
func (s *Server) Command(ctx context.Context, serial string, msg *cucinav1.ControllerMessage, timeout time.Duration) (*cucinav1.CommandResult, []byte, error) {
	if msg.CommandId == "" {
		msg.CommandId = newCommandID()
	}
	if timeout <= 0 {
		timeout = s.d.CommandTimeout
	}
	pc := &pendingCmd{msg: msg, result: make(chan *cucinav1.CommandResult, 1)}
	s.mu.Lock()
	h := s.hosts[serial]
	if h == nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("%w: %s", ErrHostUnknown, serial)
	}
	if h.session == nil {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("%w: %s", ErrHostOffline, serial)
	}
	h.pending[msg.CommandId] = pc
	h.order = append(h.order, msg.CommandId)
	if sm := msg.GetStartVm(); sm != nil {
		h.expectVMs[sm.GetVmName()] = sm.GetPool()
	}
	sess := h.session
	s.mu.Unlock()
	select {
	case sess.out <- msg:
	default:
		s.drop(serial, msg.CommandId)
		return nil, nil, status.Error(codes.ResourceExhausted, "host command queue is full")
	}
	timer := s.d.Clock.After(timeout)
	select {
	case r := <-pc.result:
		s.mu.Lock()
		logs := pc.logs
		s.mu.Unlock()
		if !r.GetOk() {
			return r, logs, fmt.Errorf("%w: %w", ErrRejected, hostcmd.Decode(r.GetError()))
		}
		return r, logs, nil
	case <-timer:
		s.drop(serial, msg.CommandId)
		return nil, nil, status.Errorf(codes.DeadlineExceeded, "host %s did not answer command %s within %s", serial, msg.CommandId, timeout)
	case <-ctx.Done():
		s.drop(serial, msg.CommandId)
		return nil, nil, ctx.Err()
	}
}

func (s *Server) drop(serial, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.hosts[serial]; h != nil {
		delete(h.pending, id)
		h.order = removeID(h.order, id)
	}
}

// ------------------------------------------------------------ unary RPCs

// RenewCertificate implements HostService by delegating to enroll
// (R-SEC-2: hosts renew before expiry over mTLS; the bound key is updated
// when hostd rotates it).
func (s *Server) RenewCertificate(ctx context.Context, req *cucinav1.RenewCertificateRequest) (*cucinav1.RenewCertificateResponse, error) {
	serial, err := s.authorize(ctx)
	if err != nil {
		return nil, err
	}
	id, err := pki.HostIdentity(serial)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return s.d.Certs.RenewCertificate(ctx, id, req)
}

var vmNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// IssueVMIdentity implements HostService: only for a VM of the pool this
// controller asked this host to run (the serial comes from the certificate);
// issuance and settings are delegated to enroll.
func (s *Server) IssueVMIdentity(ctx context.Context, req *cucinav1.IssueVMIdentityRequest) (*cucinav1.IssueVMIdentityResponse, error) {
	serial, err := s.authorize(ctx)
	if err != nil {
		return nil, err
	}
	if !vmNameRe.MatchString(req.GetVmName()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid vm name %q", req.GetVmName())
	}
	s.mu.Lock()
	pool := ""
	if h := s.hosts[serial]; h != nil {
		pool = h.expectVMs[req.GetVmName()]
	}
	s.mu.Unlock()
	if pool == "" || pool != req.GetPool() {
		return nil, status.Errorf(codes.PermissionDenied, "host %s was not asked to run vm %q of pool %q", serial, req.GetVmName(), req.GetPool())
	}
	id, err := pki.HostIdentity(serial)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	resp, err := s.d.Certs.IssueVMIdentity(ctx, id, req)
	if err != nil {
		return nil, err
	}
	if st := resp.GetSettings(); st != nil {
		st.Pool, st.Node = pool, serial+"/"+req.GetVmName()
	}
	return resp, nil
}

// GetRegistryCredentials implements HostService.
func (s *Server) GetRegistryCredentials(ctx context.Context, req *cucinav1.GetRegistryCredentialsRequest) (*cucinav1.GetRegistryCredentialsResponse, error) {
	serial, err := s.authorize(ctx)
	if err != nil {
		return nil, err
	}
	if s.d.Registry == nil {
		return &cucinav1.GetRegistryCredentialsResponse{}, nil
	}
	return s.d.Registry.Credentials(ctx, serial, req.GetImage())
}

// ------------------------------------------------------------ ports.HostFleet

func vmState(state string, online bool) domain.VMState {
	if !online {
		return domain.VMUnavailable
	}
	switch state {
	case "cloning", "starting":
		return domain.VMLaunching
	case "running":
		return domain.VMRegistered
	case "stopping":
		return domain.VMStopping
	case "failed":
		return domain.VMFailed
	}
	return domain.VMStopped
}

// Hosts implements ports.HostFleet.
func (s *Server) Hosts(context.Context) ([]ports.HostState, error) {
	now := s.d.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ports.HostState, 0, len(s.hosts))
	for serial, h := range s.hosts {
		online := h.session != nil && now.Sub(h.lastSeen) < s.d.StaleAfter
		hs := ports.HostState{Serial: serial, Name: serial, Online: online, Approved: true, Cordoned: h.cordoned,
			Slots: h.slots, RunningVMs: h.running, Images: append([]string(nil), h.images...), LastSeen: h.lastSeen}
		if h.facts != nil {
			hs.Site, hs.Labels, hs.AgentVersion = h.facts.GetSite(), h.facts.GetLabels(), h.facts.GetAgentVersion()
		}
		names := make([]string, 0, len(h.vms))
		for n := range h.vms {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			vm := h.vms[n]
			v := domain.VM{ID: serial + "/" + n, Pool: domain.PoolName(vm.GetPool()), Generation: vm.GetGeneration(),
				State: vmState(vm.GetState(), online), Host: serial}
			if vm.GetStarted() != nil {
				v.LaunchedAt = vm.GetStarted().AsTime()
			}
			hs.VMs = append(hs.VMs, v)
		}
		out = append(out, hs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out, nil
}

// Metrics returns the last heartbeat metrics of a host (MacHost status).
func (s *Server) Metrics(serial string) *cucinav1.HostMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.hosts[serial]; h != nil {
		return h.metrics
	}
	return nil
}

// Facts returns the facts a host reported at connect.
func (s *Server) Facts(serial string) *cucinav1.HostFacts {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.hosts[serial]; h != nil {
		return h.facts
	}
	return nil
}

func (s *Server) cmd(ctx context.Context, serial string, msg *cucinav1.ControllerMessage, timeout time.Duration) error {
	_, _, err := s.Command(ctx, serial, msg, timeout)
	return err
}

// StartVM implements ports.HostFleet.
func (s *Server) StartVM(ctx context.Context, serial string, r ports.StartVMRequest) error {
	return s.cmd(ctx, serial, &cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_StartVm{StartVm: &cucinav1.StartVM{
		VmName: r.VMName, Pool: string(r.Pool), Image: r.Image, Generation: r.Generation, Cpu: uint32(r.CPU),
		MemoryGib: uint32(r.MemoryGiB), DiskGib: uint32(r.DiskGiB), Node: serial + "/" + r.VMName,
		MaxAgeHours: uint32(r.MaxAge / time.Hour)}}}, 0)
}

// StopVM implements ports.HostFleet.
func (s *Server) StopVM(ctx context.Context, serial, vmName string, timeout time.Duration, reason string) error {
	return s.cmd(ctx, serial, &cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_StopVm{StopVm: &cucinav1.StopVM{
		VmName: vmName, Timeout: durationpb.New(timeout), Reason: reason}}}, 0)
}

// DeleteVM implements ports.HostFleet.
func (s *Server) DeleteVM(ctx context.Context, serial, vmName string) error {
	return s.cmd(ctx, serial, &cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_DeleteVm{DeleteVm: &cucinav1.DeleteVM{VmName: vmName}}}, 0)
}

// ReimageVM implements ports.HostFleet.
func (s *Server) ReimageVM(ctx context.Context, serial, vmName, image string) error {
	return s.cmd(ctx, serial, &cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_ReimageVm{ReimageVm: &cucinav1.ReimageVM{VmName: vmName, Image: image}}}, 0)
}

// PullImage implements ports.HostFleet (pulls can take long: 2h timeout).
func (s *Server) PullImage(ctx context.Context, serial, image string) error {
	return s.cmd(ctx, serial, &cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_PullImage{PullImage: &cucinav1.PullImage{Image: image}}}, 2*time.Hour)
}

// SetCordon implements ports.HostFleet.
func (s *Server) SetCordon(ctx context.Context, serial string, cordoned bool) error {
	return s.cmd(ctx, serial, &cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_SetCordon{SetCordon: &cucinav1.SetCordon{Cordoned: cordoned}}}, 0)
}

// CollectDiagnostics asks a host for a diagnostics bundle (cucinactl hosts diagnose).
func (s *Server) CollectDiagnostics(ctx context.Context, serial string, vmLogs bool) ([]byte, error) {
	_, logs, err := s.Command(ctx, serial, &cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_CollectDiagnostics{
		CollectDiagnostics: &cucinav1.CollectDiagnostics{IncludeVmLogs: vmLogs}}}, 5*time.Minute)
	return logs, err
}

// DiagnosticsRequest scopes a diagnostics stream (mgmt StreamWorkerLogs).
type DiagnosticsRequest struct {
	IncludeVMLogs bool
	VM            string
	Unit          string // bb-worker | bb-runner | agent
	TailLines     int
	Follow        bool
}

// Diagnostics streams a host's diagnostics (or one VM unit's log, optionally
// followed) as it arrives. Hosts redact secrets at the source. Closing the
// reader stops forwarding; a followed stream ends on the host after its limit.
func (s *Server) Diagnostics(ctx context.Context, serial string, req DiagnosticsRequest) (io.ReadCloser, error) {
	msg := &cucinav1.ControllerMessage{CommandId: newCommandID(), Message: &cucinav1.ControllerMessage_CollectDiagnostics{
		CollectDiagnostics: &cucinav1.CollectDiagnostics{IncludeVmLogs: req.IncludeVMLogs, VmName: req.VM, Unit: req.Unit,
			TailLines: uint32(max(req.TailLines, 0)), Follow: req.Follow}}}
	pc := &pendingCmd{msg: msg, result: make(chan *cucinav1.CommandResult, 1), stream: make(chan []byte, 256)}
	s.mu.Lock()
	h := s.hosts[serial]
	if h == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrHostUnknown, serial)
	}
	if h.session == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrHostOffline, serial)
	}
	h.pending[msg.CommandId] = pc
	h.order = append(h.order, msg.CommandId)
	sess := h.session
	s.mu.Unlock()
	select {
	case sess.out <- msg:
	default:
		s.drop(serial, msg.CommandId)
		return nil, status.Error(codes.ResourceExhausted, "host command queue is full")
	}
	pr, pw := io.Pipe()
	cancelRemote := func() {
		// The reader left before the host finished (e.g. a followed log): stop it there.
		cm := &cucinav1.ControllerMessage{CommandId: newCommandID(), Message: &cucinav1.ControllerMessage_CancelCommand{
			CancelCommand: &cucinav1.CancelCommand{TargetCommandId: msg.CommandId}}}
		select {
		case sess.out <- cm:
		case <-sess.done:
		default:
		}
	}
	go func() {
		defer s.drop(serial, msg.CommandId)
		limit := s.d.Clock.After(15 * time.Minute)
		for {
			select {
			case b := <-pc.stream:
				if _, err := pw.Write(b); err != nil {
					cancelRemote()
					return // reader closed
				}
			case r := <-pc.result:
				for {
					select {
					case b := <-pc.stream:
						if _, err := pw.Write(b); err != nil {
							return
						}
						continue
					default:
					}
					break
				}
				s.mu.Lock()
				overflow := pc.overflow
				s.mu.Unlock()
				switch {
				case !r.GetOk():
					pw.CloseWithError(fmt.Errorf("%w: %w", ErrRejected, hostcmd.Decode(r.GetError())))
				case overflow:
					pw.CloseWithError(errors.New("diagnostics stream truncated: reader too slow"))
				default:
					pw.Close()
				}
				return
			case <-limit:
				cancelRemote()
				pw.CloseWithError(status.Error(codes.DeadlineExceeded, "diagnostics stream ended without a result"))
				return
			case <-ctx.Done():
				cancelRemote()
				pw.CloseWithError(ctx.Err())
				return
			}
		}
	}()
	return pr, nil
}

// Reimage re-clones a VM ("" = every VM on the host) at its next start (R-MAC-6).
func (s *Server) Reimage(ctx context.Context, serial, vm string) error {
	if vm != "" {
		return s.ReimageVM(ctx, serial, vm, "")
	}
	s.mu.Lock()
	var names []string
	if h := s.hosts[serial]; h != nil {
		for n := range h.vms {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	sort.Strings(names)
	var errs []error
	for _, n := range names {
		errs = append(errs, s.ReimageVM(ctx, serial, n, ""))
	}
	return errors.Join(errs...)
}

// UpdateHostConfig pushes HostSettings to a connected host.
func (s *Server) UpdateHostConfig(ctx context.Context, serial string, hs *cucinav1.HostSettings) error {
	return s.cmd(ctx, serial, &cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_UpdateConfig{UpdateConfig: &cucinav1.UpdateHostConfig{Settings: hs}}}, 0)
}

// Ping round-trips a no-op command.
func (s *Server) Ping(ctx context.Context, serial string) error {
	return s.cmd(ctx, serial, &cucinav1.ControllerMessage{Message: &cucinav1.ControllerMessage_Ping{Ping: &cucinav1.Ping{}}}, 0)
}
