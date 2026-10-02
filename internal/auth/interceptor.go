// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/keys"
)

// Requirement is what a management method needs from the caller's Cucina JWT (R-AUTH-11).
type Requirement int

const (
	// RequireRead: a valid token with `execute` or `admin` on at least one instance name.
	RequireRead Requirement = iota + 1
	// RequireAdmin: `admin` on every configured instance name (cluster administrator).
	// Every mutating method requires it.
	RequireAdmin
)

// MethodRequirements maps full gRPC method names ("/pkg.Service/Method") to their
// requirement. Methods missing from the table are denied (fail closed).
type MethodRequirements map[string]Requirement

// readOnlyPrefixes are the read-only method name prefixes of ManagementService; every
// other method mutates. The table is maintained with the management API.
var readOnlyPrefixes = []string{"Get", "List", "Watch", "Stream", "Collect"}

// sensitiveReads are read-only methods that expose other tenants' data or secrets'
// metadata; like mutations they need cluster admin (ADR 0580, internal/mgmt).
var sensitiveReads = []string{
	"StreamWorkerLogs", "HostDiagnostics", "CollectSupportBundle",
	"ListEnrollTokens", "ListServiceKeys", "ListRevocations",
}

// ManagementRequirements derives the table from the ManagementService descriptor.
func ManagementRequirements() MethodRequirements {
	out := MethodRequirements{}
	svc := cucinav1.File_cucina_v1_management_proto.Services().ByName("ManagementService")
	for i := 0; i < svc.Methods().Len(); i++ {
		m := svc.Methods().Get(i)
		name := string(m.Name())
		req := RequireAdmin
		if slices.ContainsFunc(readOnlyPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) &&
			!slices.Contains(sensitiveReads, name) {
			req = RequireRead
		}
		out["/"+string(svc.FullName())+"/"+name] = req
	}
	return out
}

// TokenVerifier validates a Cucina JWT (keys.Verifier: signature by a published key,
// iss, aud, exp, deny-list).
type TokenVerifier interface {
	Verify(raw string) (*keys.Claims, error)
}

// Caller is the authenticated principal of a management call.
type Caller struct {
	Subject string
	Session string
	TokenID string
	Grants  Grants
}

type callerKey struct{}

// CallerFromContext returns the principal the interceptor attached.
func CallerFromContext(ctx context.Context) (*Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(*Caller)
	return c, ok
}

// Interceptor authenticates and authorizes management API calls locally (no STS call).
type Interceptor struct {
	Verifier TokenVerifier
	Methods  MethodRequirements
	// InstanceNames are the configured instance names (admin must hold all of them).
	InstanceNames []string
}

// NewInterceptor builds the interceptor; methods usually come from ManagementRequirements.
func NewInterceptor(v TokenVerifier, methods MethodRequirements, instanceNames []string) *Interceptor {
	return &Interceptor{Verifier: v, Methods: methods, InstanceNames: slices.Clone(instanceNames)}
}

// Authorize checks the call and returns a context carrying the Caller.
func (i *Interceptor) Authorize(ctx context.Context, fullMethod string) (context.Context, error) {
	req, known := i.Methods[fullMethod]
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) != 1 {
		return nil, status.Error(codes.Unauthenticated, "a Cucina bearer token is required (cucinactl login)")
	}
	scheme, raw, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
		return nil, status.Error(codes.Unauthenticated, "a Cucina bearer token is required (cucinactl login)")
	}
	claims, err := i.Verifier.Verify(strings.TrimSpace(raw))
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid, expired or revoked Cucina token")
	}
	c := &Caller{Subject: claims.Subject, Session: claims.Session, TokenID: claims.ID, Grants: GrantsFromScopes(claims.Cucina)}
	if !known {
		return nil, status.Error(codes.PermissionDenied, "method not allowed")
	}
	switch req {
	case RequireRead:
		if len(c.Grants[VerbExecute]) == 0 && len(c.Grants[VerbAdmin]) == 0 {
			return nil, status.Error(codes.PermissionDenied, "read access needs the execute or admin verb on an instance name")
		}
	case RequireAdmin:
		if len(i.InstanceNames) == 0 || slices.ContainsFunc(i.InstanceNames, func(n string) bool { return !c.Grants.Has(VerbAdmin, n) }) {
			return nil, status.Error(codes.PermissionDenied, "this method needs the admin verb on every instance name")
		}
	default:
		return nil, status.Error(codes.PermissionDenied, "method not allowed")
	}
	return context.WithValue(ctx, callerKey{}, c), nil
}

// Unary returns the unary server interceptor.
func (i *Interceptor) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := i.Authorize(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream returns the stream server interceptor.
func (i *Interceptor) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := i.Authorize(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }
