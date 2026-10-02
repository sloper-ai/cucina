// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"cmp"
	"context"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/grpc"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/api/v1alpha1"
)

const (
	maxSerialsPerCall   = 1000
	maxEnrollTokenHosts = 10000
	maxDescription      = 256
	chunkSize           = 64 << 10
	// maxDiagnosticsBytes bounds one diagnostics stream.
	maxDiagnosticsBytes = 1 << 30
)

// serialPattern matches Apple hardware serial numbers (normalized to upper case).
var serialPattern = regexp.MustCompile(`^[A-Z0-9]{6,32}$`)

func normalizeSerial(s string) (string, error) {
	n := strings.ToUpper(strings.TrimSpace(s))
	if !serialPattern.MatchString(n) {
		return "", invalid("invalid serial number %q (6–32 letters and digits)", s)
	}
	return n, nil
}

// resolveHost finds a host by serial (case-insensitive) or by object name.
func (s *Server) resolveHost(ctx context.Context, ref string) (v1alpha1.MacHost, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return v1alpha1.MacHost{}, invalid("host serial or name is required")
	}
	if s.deps.Hosts == nil {
		return v1alpha1.MacHost{}, notConfigured("the Mac host fleet")
	}
	hosts, err := s.deps.Hosts.Hosts(ctx)
	if err != nil {
		return v1alpha1.MacHost{}, fail("hosts", err)
	}
	for _, h := range hosts {
		if strings.EqualFold(h.Spec.Serial, ref) {
			return h, nil
		}
	}
	for _, h := range hosts {
		if h.Name == ref {
			return h, nil
		}
	}
	return v1alpha1.MacHost{}, notFound("host %q not found", ref)
}

// ListHosts implements `cucinactl hosts list`.
func (s *Server) ListHosts(ctx context.Context, _ *cucinav1.ListHostsRequest) (*cucinav1.ListHostsResponse, error) {
	out := &cucinav1.ListHostsResponse{}
	if s.deps.Hosts == nil {
		return out, nil
	}
	hosts, err := s.deps.Hosts.Hosts(ctx)
	if err != nil {
		return nil, fail("hosts", err)
	}
	hosts = slices.Clone(hosts) // sources may return shared slices
	slices.SortFunc(hosts, func(a, b v1alpha1.MacHost) int { return cmp.Compare(a.Spec.Serial, b.Spec.Serial) })
	for _, h := range hosts {
		out.Hosts = append(out.Hosts, hostDetail(h))
	}
	return out, nil
}

func (s *Server) hostAdmin(ctx context.Context, ref string) (v1alpha1.MacHost, error) {
	if s.deps.HostAdmin == nil {
		return v1alpha1.MacHost{}, notConfigured("Mac host administration")
	}
	return s.resolveHost(ctx, ref)
}

// DrainHost cordons a host for maintenance (UC14, R-MAC-6): its VMs finish their
// actions and shut down; no new VMs start until UncordonHost.
func (s *Server) DrainHost(ctx context.Context, req *cucinav1.HostRef) (*cucinav1.HostActionResponse, error) {
	h, err := s.hostAdmin(ctx, req.GetSerial())
	if err != nil {
		return nil, err
	}
	if err := s.deps.HostAdmin.SetCordon(ctx, h.Spec.Serial, true); err != nil {
		return nil, fail("draining host", err)
	}
	return &cucinav1.HostActionResponse{Message: "host " + h.Spec.Serial + " draining: running VMs finish their actions and shut down; no new VMs start"}, nil
}

// UncordonHost returns a host to service.
func (s *Server) UncordonHost(ctx context.Context, req *cucinav1.HostRef) (*cucinav1.HostActionResponse, error) {
	h, err := s.hostAdmin(ctx, req.GetSerial())
	if err != nil {
		return nil, err
	}
	if err := s.deps.HostAdmin.SetCordon(ctx, h.Spec.Serial, false); err != nil {
		return nil, fail("uncordoning host", err)
	}
	return &cucinav1.HostActionResponse{Message: "host " + h.Spec.Serial + " uncordoned"}, nil
}

// ReimageHost re-clones one VM (or every VM) of a host from its golden image.
func (s *Server) ReimageHost(ctx context.Context, req *cucinav1.ReimageHostRequest) (*cucinav1.HostActionResponse, error) {
	h, err := s.hostAdmin(ctx, req.GetHost().GetSerial())
	if err != nil {
		return nil, err
	}
	vm := strings.TrimSpace(req.GetVm())
	if vm != "" && !slices.ContainsFunc(h.Status.VMs, func(v v1alpha1.HostVMStatus) bool { return v.Name == vm }) {
		return nil, notFound("host %s has no VM %q", h.Spec.Serial, vm)
	}
	if err := s.deps.HostAdmin.Reimage(ctx, h.Spec.Serial, vm); err != nil {
		return nil, fail("re-imaging", err)
	}
	what := "every VM"
	if vm != "" {
		what = "VM " + vm
	}
	return &cucinav1.HostActionResponse{Message: what + " of host " + h.Spec.Serial + " is re-cloned from its golden image at its next start"}, nil
}

// HostDiagnostics streams a host's diagnostics (hostd redacts them at the source).
func (s *Server) HostDiagnostics(req *cucinav1.HostRef, stream grpc.ServerStreamingServer[cucinav1.LogChunk]) error {
	ctx := stream.Context()
	h, err := s.hostAdmin(ctx, req.GetSerial())
	if err != nil {
		return err
	}
	release, err := s.logStreams.acquire("log and diagnostics streams")
	if err != nil {
		return err
	}
	defer release()
	rc, err := s.deps.HostAdmin.Diagnostics(ctx, h.Spec.Serial, DiagnosticsRequest{IncludeVMLogs: true})
	if err != nil {
		return fail("collecting diagnostics", err)
	}
	defer func() { _ = rc.Close() }()
	return s.sendChunks(ctx, stream, io.LimitReader(rc, maxDiagnosticsBytes))
}

// sendChunks streams r in chunks and ends with an empty chunk marked last.
func (s *Server) sendChunks(ctx context.Context, stream grpc.ServerStreamingServer[cucinav1.LogChunk], r io.Reader) error {
	buf := make([]byte, chunkSize)
	for {
		if ctx.Err() != nil {
			return streamEnd(ctx)
		}
		select {
		case <-s.done:
			return errStopping
		default:
		}
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if serr := stream.Send(&cucinav1.LogChunk{Data: slices.Clone(buf[:n])}); serr != nil {
				return serr
			}
		}
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return stream.Send(&cucinav1.LogChunk{Last: true})
		case err != nil:
			return fail("reading stream", err)
		}
	}
}

// ---------------------------------------------------------------- enrollment

func (s *Server) enrollment() (Enrollment, error) {
	if s.deps.Enrollment == nil {
		return nil, notConfigured("host enrollment")
	}
	return s.deps.Enrollment, nil
}

// RegisterHostSerials pre-registers (and approves) serial numbers (R-SEC-3).
func (s *Server) RegisterHostSerials(ctx context.Context, req *cucinav1.RegisterHostSerialsRequest) (*cucinav1.RegisterHostSerialsResponse, error) {
	e, err := s.enrollment()
	if err != nil {
		return nil, err
	}
	if len(req.GetSerials()) == 0 {
		return nil, invalid("at least one serial number is required")
	}
	if len(req.GetSerials()) > maxSerialsPerCall {
		return nil, invalid("at most %d serial numbers per call", maxSerialsPerCall)
	}
	var serials []string
	for _, raw := range req.GetSerials() {
		n, err := normalizeSerial(raw)
		if err != nil {
			return nil, err
		}
		serials = append(serials, n)
	}
	slices.Sort(serials)
	serials = slices.Compact(serials)
	registered, already, err := e.RegisterSerials(ctx, serials, strings.TrimSpace(req.GetSite()), req.GetLabels())
	if err != nil {
		return nil, fail("registering serials", err)
	}
	return &cucinav1.RegisterHostSerialsResponse{Registered: registered, AlreadyPresent: already}, nil
}

// ApproveHost admits a pending host's serial (R-SEC-3).
func (s *Server) ApproveHost(ctx context.Context, req *cucinav1.ApproveHostRequest) (*cucinav1.HostActionResponse, error) {
	e, err := s.enrollment()
	if err != nil {
		return nil, err
	}
	serial, err := normalizeSerial(req.GetSerial())
	if err != nil {
		return nil, err
	}
	if err := e.ApproveHost(ctx, serial); err != nil {
		return nil, fail("approving host", err)
	}
	return &cucinav1.HostActionResponse{Message: "host " + serial + " approved"}, nil
}

// RemoveHost removes a host and its identity; re-enrolling the serial then needs a
// new registration or approval.
func (s *Server) RemoveHost(ctx context.Context, req *cucinav1.HostRef) (*cucinav1.HostActionResponse, error) {
	e, err := s.enrollment()
	if err != nil {
		return nil, err
	}
	h, err := s.resolveHost(ctx, req.GetSerial())
	if err != nil {
		return nil, err
	}
	if err := e.RemoveHost(ctx, h.Spec.Serial); err != nil {
		return nil, fail("removing host", err)
	}
	return &cucinav1.HostActionResponse{Message: "host " + h.Spec.Serial + " removed"}, nil
}

// CreateEnrollToken creates a site enrollment token; the token is returned exactly
// once (only its hash is stored) and is never written to the audit log.
func (s *Server) CreateEnrollToken(ctx context.Context, req *cucinav1.CreateEnrollTokenRequest) (*cucinav1.CreateEnrollTokenResponse, error) {
	e, err := s.enrollment()
	if err != nil {
		return nil, err
	}
	site := strings.TrimSpace(req.GetSite())
	if site == "" {
		return nil, invalid("site is required: enrollment tokens are bound to a site")
	}
	ttl := s.opts.DefaultEnrollTokenTTL
	if req.GetTtl() != nil {
		ttl = req.GetTtl().AsDuration()
	}
	if ttl <= 0 || ttl > s.opts.MaxEnrollTokenTTL {
		return nil, invalid("ttl must be between 0 and %s", s.opts.MaxEnrollTokenTTL)
	}
	if req.GetMaxHosts() == 0 || req.GetMaxHosts() > maxEnrollTokenHosts {
		return nil, invalid("max_hosts must be between 1 and %d", maxEnrollTokenHosts)
	}
	if len(req.GetDescription()) > maxDescription {
		return nil, invalid("description longer than %d bytes", maxDescription)
	}
	tok, secret, err := e.CreateEnrollToken(ctx, EnrollTokenRequest{
		Site: site, TTL: ttl, MaxHosts: int(req.GetMaxHosts()), Description: req.GetDescription(), CreatedBy: caller(ctx),
	})
	if err != nil {
		return nil, fail("creating enrollment token", err)
	}
	return &cucinav1.CreateEnrollTokenResponse{Id: tok.ID, Token: secret, ExpiresAt: ts(tok.ExpiresAt)}, nil
}

// ListEnrollTokens lists token metadata (never the tokens).
func (s *Server) ListEnrollTokens(ctx context.Context, _ *cucinav1.ListEnrollTokensRequest) (*cucinav1.ListEnrollTokensResponse, error) {
	e, err := s.enrollment()
	if err != nil {
		return nil, err
	}
	toks, err := e.ListEnrollTokens(ctx)
	if err != nil {
		return nil, fail("listing enrollment tokens", err)
	}
	toks = slices.Clone(toks)
	slices.SortFunc(toks, func(a, b EnrollToken) int { return a.Created.Compare(b.Created) })
	out := &cucinav1.ListEnrollTokensResponse{}
	for _, t := range toks {
		out.Tokens = append(out.Tokens, &cucinav1.EnrollTokenInfo{
			Id: t.ID, Site: t.Site, Description: t.Description, Created: ts(t.Created), ExpiresAt: ts(t.ExpiresAt),
			MaxHosts: u32(t.MaxHosts), UsedHosts: u32(t.UsedHosts), Revoked: t.Revoked,
		})
	}
	return out, nil
}

// RevokeEnrollToken revokes a token; hosts already enrolled are unaffected.
func (s *Server) RevokeEnrollToken(ctx context.Context, req *cucinav1.RevokeEnrollTokenRequest) (*cucinav1.RevokeEnrollTokenResponse, error) {
	e, err := s.enrollment()
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, invalid("token id is required")
	}
	if err := e.RevokeEnrollToken(ctx, req.GetId()); err != nil {
		return nil, fail("revoking enrollment token", err)
	}
	return &cucinav1.RevokeEnrollTokenResponse{}, nil
}
