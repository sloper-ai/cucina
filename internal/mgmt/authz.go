// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/api/v1alpha1"
)

// Access is the authorization class of a ManagementService method.
type Access int

const (
	// Read methods need a valid Cucina JWT granting `execute` or `admin` on at
	// least one instance name. They are not audited.
	Read Access = iota + 1
	// AdminRead methods expose sensitive data (worker logs, host diagnostics,
	// credential metadata, support bundles). They need cluster admin and are
	// audited, but are not mutations.
	AdminRead
	// Mutate methods change state. They need cluster admin and are audited as
	// mutations (R-CLI-2, R-AUTH-11).
	Mutate
)

func (a Access) String() string {
	switch a {
	case Read:
		return "read"
	case AdminRead:
		return "admin-read"
	case Mutate:
		return "mutate"
	}
	return fmt.Sprintf("Access(%d)", int(a))
}

// RequiresAdmin reports whether the method needs cluster admin.
func (a Access) RequiresAdmin() bool { return a == AdminRead || a == Mutate }

// Audited reports whether every call writes an audit record.
func (a Access) Audited() bool { return a == AdminRead || a == Mutate }

// Mutating reports whether the method changes state.
func (a Access) Mutating() bool { return a == Mutate }

// methodAccess is THE method→access table. Every RPC of ManagementService must have
// an entry (New refuses to start otherwise; TestMethodAccessCoversEveryRPC).
var methodAccess = map[string]Access{
	cucinav1.ManagementService_GetStatus_FullMethodName:     Read,
	cucinav1.ManagementService_WatchOverview_FullMethodName: Read,

	cucinav1.ManagementService_ListPools_FullMethodName:          Read,
	cucinav1.ManagementService_GetPool_FullMethodName:            Read,
	cucinav1.ManagementService_SetPoolFloor_FullMethodName:       Mutate,
	cucinav1.ManagementService_CordonPool_FullMethodName:         Mutate,
	cucinav1.ManagementService_GarbageCollectPool_FullMethodName: Mutate,

	cucinav1.ManagementService_ListWorkers_FullMethodName:      Read,
	cucinav1.ManagementService_DrainWorker_FullMethodName:      Mutate,
	cucinav1.ManagementService_UndrainWorker_FullMethodName:    Mutate,
	cucinav1.ManagementService_StreamWorkerLogs_FullMethodName: AdminRead,

	cucinav1.ManagementService_ListHosts_FullMethodName:           Read,
	cucinav1.ManagementService_DrainHost_FullMethodName:           Mutate,
	cucinav1.ManagementService_UncordonHost_FullMethodName:        Mutate,
	cucinav1.ManagementService_ReimageHost_FullMethodName:         Mutate,
	cucinav1.ManagementService_HostDiagnostics_FullMethodName:     AdminRead,
	cucinav1.ManagementService_RegisterHostSerials_FullMethodName: Mutate,
	cucinav1.ManagementService_ApproveHost_FullMethodName:         Mutate,
	cucinav1.ManagementService_RemoveHost_FullMethodName:          Mutate,
	cucinav1.ManagementService_CreateEnrollToken_FullMethodName:   Mutate,
	cucinav1.ManagementService_ListEnrollTokens_FullMethodName:    AdminRead,
	cucinav1.ManagementService_RevokeEnrollToken_FullMethodName:   Mutate,

	cucinav1.ManagementService_ListQueues_FullMethodName:      Read,
	cucinav1.ManagementService_ListOperations_FullMethodName:  Read,
	cucinav1.ManagementService_WatchOperations_FullMethodName: Read,
	cucinav1.ManagementService_GetOperation_FullMethodName:    Read,
	cucinav1.ManagementService_KillOperations_FullMethodName:  Mutate,

	cucinav1.ManagementService_CreateServiceKey_FullMethodName: Mutate,
	cucinav1.ManagementService_ListServiceKeys_FullMethodName:  AdminRead,
	cucinav1.ManagementService_RevokeServiceKey_FullMethodName: Mutate,
	cucinav1.ManagementService_RevokePrincipal_FullMethodName:  Mutate,
	cucinav1.ManagementService_ListRevocations_FullMethodName:  AdminRead,

	cucinav1.ManagementService_GetCost_FullMethodName:              Read,
	cucinav1.ManagementService_ListImages_FullMethodName:           Read,
	cucinav1.ManagementService_CollectSupportBundle_FullMethodName: AdminRead,
}

// MethodAccess returns a copy of the method→access table, keyed by full gRPC method
// name ("/cucina.v1.ManagementService/GetStatus").
func MethodAccess() map[string]Access {
	out := make(map[string]Access, len(methodAccess))
	for k, v := range methodAccess {
		out[k] = v
	}
	return out
}

// serviceMethods returns the full names of every RPC in the generated descriptor.
func serviceMethods(desc *grpc.ServiceDesc) []string {
	var out []string
	for _, m := range desc.Methods {
		out = append(out, "/"+desc.ServiceName+"/"+m.MethodName)
	}
	for _, s := range desc.Streams {
		out = append(out, "/"+desc.ServiceName+"/"+s.StreamName)
	}
	slices.Sort(out)
	return out
}

// checkAccessTable fails unless the table covers exactly the descriptor's RPCs.
func checkAccessTable(desc *grpc.ServiceDesc, table map[string]Access) error {
	var missing, extra []string
	seen := map[string]bool{}
	for _, m := range serviceMethods(desc) {
		seen[m] = true
		if a, ok := table[m]; !ok || a < Read || a > Mutate {
			missing = append(missing, m)
		}
	}
	for m := range table {
		if !seen[m] {
			extra = append(extra, m)
		}
	}
	slices.Sort(extra)
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("management access table out of date: missing %v, unknown %v", missing, extra)
	}
	return nil
}

// Principal is an authenticated caller.
type Principal struct {
	Subject     string // JWT sub, e.g. "google:1234", "sa:ci"
	DisplayName string // optional, for audit only
	SessionID   string // JWT sid
	// Grants maps a verb (v1alpha1.VerbAdmin, v1alpha1.VerbExecute, …) to the
	// instance names it is granted on ("*" means every instance name).
	Grants map[string][]string
}

// has reports whether the principal holds verb on instance.
func (p Principal) has(verb, instance string) bool {
	for _, n := range p.Grants[verb] {
		if n == instance || n == "*" {
			return true
		}
	}
	return false
}

// hasAny reports whether the principal holds verb on at least one instance name.
func (p Principal) hasAny(verb string) bool { return len(p.Grants[verb]) > 0 }

// Authenticator verifies the Cucina JWT a management client presents
// (`authorization: Bearer <jwt>`): signature and `kid` against the published keys,
// `iss`, `aud`, `exp`, and the deny-list. It is provided by internal/keys. Any
// failure is an error (fail closed); return a status error with code UNAVAILABLE
// when verification is temporarily impossible.
type Authenticator interface {
	Authenticate(ctx context.Context, bearerToken string) (Principal, error)
}

type principalKey struct{}

// ContextWithPrincipal returns ctx carrying p. The guard installs the authenticated
// principal this way; tests that call handlers directly do the same.
func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the caller set by the guard.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// maxTokenBytes bounds the bearer token we are willing to verify.
const maxTokenBytes = 16 << 10

// bearerToken extracts the single bearer token from the request metadata.
func bearerToken(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return "", errors.New("missing credentials: send `authorization: Bearer <Cucina JWT>` (run `cucinactl login`)")
	}
	if len(vals) > 1 {
		return "", errors.New("more than one authorization header")
	}
	scheme, tok, ok := strings.Cut(strings.TrimSpace(vals[0]), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "bearer") || tok == "" {
		return "", errors.New("authorization must use the Bearer scheme")
	}
	if len(tok) > maxTokenBytes {
		return "", errors.New("bearer token too large")
	}
	return tok, nil
}

// clusterAdmin reports whether p holds `admin` on every configured instance name.
func (s *Server) clusterAdmin(p Principal) bool {
	for _, n := range s.opts.InstanceNames {
		if !p.has(v1alpha1.VerbAdmin, n) {
			return false
		}
	}
	return true
}

// canSee reports whether p may see data (operations) of instance.
func (s *Server) canSee(p Principal, instance string) bool {
	return s.clusterAdmin(p) || p.has(v1alpha1.VerbExecute, instance) || p.has(v1alpha1.VerbAdmin, instance)
}

// authorize authenticates the caller and applies the access rule of method.
func (s *Server) authorize(ctx context.Context, method string) (Principal, context.Context, error) {
	access, ok := methodAccess[method]
	if !ok {
		return Principal{}, ctx, status.Errorf(codes.PermissionDenied, "%s has no access rule", method)
	}
	tok, err := bearerToken(ctx)
	if err != nil {
		return Principal{}, ctx, status.Error(codes.Unauthenticated, err.Error())
	}
	p, err := s.deps.Authenticator.Authenticate(ctx, tok)
	if err != nil {
		if st, ok := status.FromError(err); ok && st.Code() == codes.Unavailable {
			return Principal{}, ctx, status.Error(codes.Unavailable, "token verification unavailable, retry")
		}
		return Principal{}, ctx, status.Error(codes.Unauthenticated, "invalid, expired or revoked credentials (run `cucinactl login`)")
	}
	if p.Subject == "" {
		return Principal{}, ctx, status.Error(codes.Unauthenticated, "credentials carry no subject")
	}
	switch {
	case access.RequiresAdmin():
		if !s.clusterAdmin(p) {
			return p, ctx, status.Errorf(codes.PermissionDenied,
				"%s requires the admin verb on every instance name (%s)", shortMethod(method), strings.Join(s.opts.InstanceNames, ", "))
		}
	default:
		if !p.hasAny(v1alpha1.VerbExecute) && !p.hasAny(v1alpha1.VerbAdmin) {
			return p, ctx, status.Errorf(codes.PermissionDenied, "%s requires the execute or admin verb", shortMethod(method))
		}
	}
	return p, ContextWithPrincipal(ctx, p), nil
}

func shortMethod(full string) string {
	if i := strings.LastIndexByte(full, '/'); i >= 0 {
		return full[i+1:]
	}
	return full
}

// guardedDesc returns a copy of the generated descriptor whose handlers enforce
// authentication, authorization and auditing before the implementation runs,
// independent of the interceptors configured on the gRPC server.
func (s *Server) guardedDesc() *grpc.ServiceDesc {
	src := &cucinav1.ManagementService_ServiceDesc
	desc := *src
	desc.Methods = slices.Clone(src.Methods)
	desc.Streams = slices.Clone(src.Streams)
	for i := range desc.Methods {
		h := desc.Methods[i].Handler
		desc.Methods[i].Handler = func(srv any, ctx context.Context, dec func(any) error, outer grpc.UnaryServerInterceptor) (any, error) {
			return h(srv, ctx, dec, s.chainUnary(outer))
		}
	}
	for i := range desc.Streams {
		h := desc.Streams[i].Handler
		full := "/" + desc.ServiceName + "/" + desc.Streams[i].StreamName
		desc.Streams[i].Handler = func(srv any, ss grpc.ServerStream) error {
			return s.guardStream(srv, ss, full, h)
		}
	}
	return &desc
}

// chainUnary runs the server's own interceptors (outer) first, then the guard.
func (s *Server) chainUnary(outer grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	if outer == nil {
		return s.guardUnary
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return outer(ctx, req, info, func(ctx context.Context, req any) (any, error) {
			return s.guardUnary(ctx, req, info, handler)
		})
	}
}

func (s *Server) guardUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	p, actx, err := s.authorize(ctx, info.FullMethod)
	var resp any
	if err == nil {
		resp, err = handler(actx, req)
		err = toStatus(err)
	}
	if a, ok := methodAccess[info.FullMethod]; ok && a.Audited() {
		m, _ := req.(proto.Message)
		s.audit(ctx, p, info.FullMethod, a, m, err, time.Since(start))
	}
	return resp, err
}

// recordingStream carries the authorized context and remembers the request message
// (server-streaming RPCs receive exactly one) for the audit record.
type recordingStream struct {
	grpc.ServerStream
	ctx context.Context
	req proto.Message
}

func (r *recordingStream) Context() context.Context { return r.ctx }

func (r *recordingStream) RecvMsg(m any) error {
	err := r.ServerStream.RecvMsg(m)
	if err == nil && r.req == nil {
		r.req, _ = m.(proto.Message)
	}
	return err
}

func (s *Server) guardStream(srv any, ss grpc.ServerStream, full string, h grpc.StreamHandler) error {
	start := time.Now()
	p, actx, err := s.authorize(ss.Context(), full)
	rs := &recordingStream{ServerStream: ss, ctx: actx}
	if err == nil {
		err = toStatus(h(srv, rs))
	}
	if a, ok := methodAccess[full]; ok && a.Audited() {
		s.audit(ss.Context(), p, full, a, rs.req, err, time.Since(start))
	}
	return err
}
