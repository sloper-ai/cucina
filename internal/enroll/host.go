// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/pki"
)

var (
	errTokenExhausted = errors.New("the enrollment token reached its maximum host count")
	errNotApproved    = errors.New("host is not approved")
	errKeyConflict    = errors.New("another key is bound to this serial")
	errHostRemoved    = errors.New("host was removed or is not enrolled")

	labelKeyRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)
	labelValueRE = regexp.MustCompile(`^[A-Za-z0-9._-]{0,63}$`)
)

const maxReportedLabels = 16

type hostResp = cucinav1.EnrollHostResponse

// EnrollHost implements cucinav1.EnrollmentServiceServer (R-SEC-3, UC19,
// T14). A host exchanges the site token once: token + serial + CSR →
// STATUS_PENDING until the serial is pre-registered or approved, then
// STATUS_APPROVED with its certificate. The first key presented for a serial is
// bound to it (idempotent polling with the same key); a different key, or a new
// enrollment of an already enrolled serial, is STATUS_DENIED until an admin
// removes the host. The same bound key may fetch a fresh certificate again
// (crash/outage recovery, ADR 0651). Token secrets are never logged.
func (s *Server) EnrollHost(ctx context.Context, req *cucinav1.EnrollHostRequest) (*cucinav1.EnrollHostResponse, error) {
	const event = "enroll.host"
	if !s.hostRate.allow(ctx) {
		s.audit(ctx, event, "rate-limited")
		return nil, status.Error(codes.ResourceExhausted, "too many enrollment requests from this address")
	}
	if err := checkProtocol(req.GetProtocol(), "cucina-hostd"); err != nil {
		s.audit(ctx, event, "protocol-mismatch")
		return nil, err
	}
	serial, err := pki.CanonicalSerial(req.GetSerialNumber())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	hostID, _ := pki.HostIdentity(serial)
	pub, err := pki.ParseCSR(req.GetCsrPem(), hostID)
	if err != nil {
		s.audit(ctx, event, "bad-request", "serial", serial, "reason", err.Error())
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	keyHash, err := pki.PublicKeyHash(pub)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	tok, reason, err := s.checkToken(ctx, req.GetSiteToken(), req.GetFacts().GetSite())
	if err != nil {
		return nil, s.unavailable(ctx, "checking the enrollment token", err)
	}
	if tok == nil {
		s.audit(ctx, event, "token-invalid", "serial", serial, "reason", reason)
		return &hostResp{Status: cucinav1.EnrollHostResponse_STATUS_TOKEN_INVALID, Message: reason}, nil
	}
	for range 3 {
		host, err := s.d.Hosts.Get(ctx, serial)
		switch {
		case errors.Is(err, ErrNotFound):
			resp, err := s.firstContact(ctx, tok, serial, keyHash, req)
			if errors.Is(err, ErrExists) {
				continue // a concurrent first contact created the record: decide again
			}
			return resp, err
		case err != nil:
			return nil, s.unavailable(ctx, "reading the host", err)
		}
		return s.knownHost(ctx, tok, host, keyHash, req)
	}
	return nil, s.unavailable(ctx, "enrolling the host", errors.New("concurrent enrollment of the same serial"))
}

// checkToken validates the site token. A nil record with a reason means
// STATUS_TOKEN_INVALID; unknown ids and wrong secrets get the same reason.
func (s *Server) checkToken(ctx context.Context, raw, reportedSite string) (*TokenRecord, string, error) {
	const invalid = "the enrollment token is invalid"
	id, secret, ok := parseToken(strings.TrimSpace(raw))
	if !ok {
		return nil, invalid, nil
	}
	rec, err := s.d.Tokens.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, invalid, nil
	}
	if err != nil {
		return nil, "", err
	}
	switch {
	case !secretMatches(rec, secret):
		return nil, invalid, nil
	case rec.RevokedAt != nil:
		return nil, "the enrollment token was revoked", nil
	case !s.d.Clock.Now().Before(rec.ExpiresAt):
		return nil, "the enrollment token expired", nil
	case reportedSite != "" && reportedSite != rec.Site:
		return nil, "the enrollment token is bound to another site", nil
	}
	return &rec, "", nil
}

// firstContact handles an unknown serial: bind a token slot and create a
// Pending MacHost so an admin can approve it.
func (s *Server) firstContact(ctx context.Context, tok *TokenRecord, serial, keyHash string, req *cucinav1.EnrollHostRequest) (*hostResp, error) {
	if resp, err := s.bindToken(ctx, tok, serial); resp != nil || err != nil {
		return resp, err
	}
	now := s.d.Clock.Now()
	rec := HostRecord{
		Serial: serial, Name: strings.ToLower(serial), Site: tok.Site, Labels: reportedLabels(req.GetFacts().GetLabels()),
		TokenID: tok.ID, KeySHA256: keyHash, PendingSince: now, Hostname: req.GetHostname(),
	}
	if err := s.d.Hosts.Create(ctx, rec); err != nil {
		if errors.Is(err, ErrExists) {
			return nil, ErrExists
		}
		return nil, s.unavailable(ctx, "recording the pending host", err)
	}
	s.audit(ctx, "enroll.host", "pending", "serial", serial, "site", tok.Site, "token", tok.ID, "new", true)
	return s.pending(serial), nil
}

// knownHost decides for an existing MacHost.
func (s *Server) knownHost(ctx context.Context, tok *TokenRecord, host HostRecord, keyHash string, req *cucinav1.EnrollHostRequest) (*hostResp, error) {
	const event = "enroll.host"
	serial := host.Serial
	if host.Site != "" && host.Site != tok.Site {
		s.audit(ctx, event, "token-invalid", "serial", serial, "token", tok.ID, "reason", "site mismatch")
		return &hostResp{Status: cucinav1.EnrollHostResponse_STATUS_TOKEN_INVALID, Message: "the enrollment token's site is not this host's site"}, nil
	}
	switch {
	case host.Enrolled() && host.KeySHA256 != keyHash:
		s.audit(ctx, event, "denied", "serial", serial, "token", tok.ID, "reason", "re-enrollment of an enrolled serial")
		return s.denied(fmt.Sprintf("serial %s is already enrolled; an administrator must remove it (cucinactl hosts remove %s) before it can enroll again", serial, serial)), nil
	case host.KeySHA256 != "" && host.KeySHA256 != keyHash:
		s.audit(ctx, event, "denied", "serial", serial, "token", tok.ID, "reason", "another key is pending for this serial")
		return s.denied(fmt.Sprintf("another key is already pending for serial %s; an administrator must remove the pending host (cucinactl hosts remove %s)", serial, serial)), nil
	case host.KeySHA256 == "":
		// Pre-registered (or admin-created) serial: first contact binds a token slot and the key.
		if resp, err := s.bindToken(ctx, tok, serial); resp != nil || err != nil {
			return resp, err
		}
		_, err := s.d.Hosts.Update(ctx, serial, func(h *HostRecord) error {
			if h.KeySHA256 != "" && h.KeySHA256 != keyHash {
				return errKeyConflict
			}
			h.KeySHA256, h.TokenID, h.Hostname = keyHash, tok.ID, req.GetHostname()
			if h.Site == "" {
				h.Site = tok.Site
			}
			if !h.Approved && h.PendingSince.IsZero() {
				h.PendingSince = s.d.Clock.Now()
			}
			return nil
		})
		if errors.Is(err, errKeyConflict) {
			return s.denied(fmt.Sprintf("another key is already pending for serial %s", serial)), nil
		}
		if err != nil {
			return nil, s.unavailable(ctx, "binding the host key", err)
		}
	}
	if !host.Approved {
		s.audit(ctx, event, "pending", "serial", serial, "site", tok.Site, "token", tok.ID)
		return s.pending(serial), nil
	}
	return s.issueHost(ctx, tok, serial, keyHash, req, host.Enrolled())
}

// issueHost issues the certificate, then records the enrollment atomically
// (re-checking approval and key); the certificate is returned only if that
// record succeeded.
func (s *Server) issueHost(ctx context.Context, tok *TokenRecord, serial, keyHash string, req *cucinav1.EnrollHostRequest, recovery bool) (*hostResp, error) {
	const event = "enroll.host"
	issued, err := s.d.Issuer.IssueHost(req.GetCsrPem(), serial)
	if err != nil {
		if errors.Is(err, pki.ErrBadCSR) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, s.unavailable(ctx, "issuing the host certificate", err)
	}
	now := s.d.Clock.Now()
	_, err = s.d.Hosts.Update(ctx, serial, func(h *HostRecord) error {
		switch {
		case !h.Approved:
			return errNotApproved
		case h.KeySHA256 != "" && h.KeySHA256 != keyHash:
			return errKeyConflict
		}
		h.KeySHA256 = keyHash
		if h.TokenID == "" {
			h.TokenID = tok.ID
		}
		if h.EnrolledAt.IsZero() {
			h.EnrolledAt = now
		}
		h.CertExpiry = issued.NotAfter
		if req.GetHostname() != "" {
			h.Hostname = req.GetHostname()
		}
		return nil
	})
	switch {
	case errors.Is(err, errNotApproved):
		return s.pending(serial), nil
	case errors.Is(err, errKeyConflict):
		return s.denied(fmt.Sprintf("serial %s is bound to another key", serial)), nil
	case errors.Is(err, ErrNotFound):
		return s.denied(fmt.Sprintf("serial %s was removed during enrollment", serial)), nil
	case err != nil:
		return nil, s.unavailable(ctx, "recording the enrollment", err)
	}
	result := "approved"
	if recovery {
		result = "recovered"
	}
	s.audit(ctx, event, result, "serial", serial, "site", tok.Site, "token", tok.ID, "notAfter", issued.NotAfter.UTC().Format(time.RFC3339))
	return &hostResp{
		Status:         cucinav1.EnrollHostResponse_STATUS_APPROVED,
		Message:        "enrolled",
		CertificatePem: issued.ChainPEM,
		CaPem:          s.bundle(issued),
		ExpiresAt:      timestamppb.New(issued.NotAfter),
		HostEndpoint:   s.o.HostEndpoint,
	}, nil
}

// bindToken counts serial against the token's host limit. It returns a
// TOKEN_INVALID response when the token is exhausted (after dropping serials
// whose MacHost no longer exists) or was revoked/expired concurrently.
func (s *Server) bindToken(ctx context.Context, tok *TokenRecord, serial string) (*hostResp, error) {
	_, err := s.d.Tokens.Update(ctx, tok.ID, func(t *TokenRecord) error {
		if t.RevokedAt != nil || !s.d.Clock.Now().Before(t.ExpiresAt) {
			return errTokenExhausted
		}
		if slices.Contains(t.Hosts, serial) {
			return nil
		}
		if len(t.Hosts) >= t.MaxHosts {
			kept := t.Hosts[:0:0]
			for _, h := range t.Hosts {
				if _, err := s.d.Hosts.Get(ctx, h); !errors.Is(err, ErrNotFound) {
					kept = append(kept, h)
				}
			}
			t.Hosts = kept
		}
		if len(t.Hosts) >= t.MaxHosts {
			return errTokenExhausted
		}
		t.Hosts = append(t.Hosts, serial)
		return nil
	})
	if errors.Is(err, errTokenExhausted) {
		s.audit(ctx, "enroll.host", "token-invalid", "serial", serial, "token", tok.ID, "reason", "maximum host count reached")
		return &hostResp{Status: cucinav1.EnrollHostResponse_STATUS_TOKEN_INVALID, Message: errTokenExhausted.Error()}, nil
	}
	if err != nil {
		return nil, s.unavailable(ctx, "binding the enrollment token", err)
	}
	return nil, nil
}

func (s *Server) pending(serial string) *hostResp {
	return &hostResp{
		Status:     cucinav1.EnrollHostResponse_STATUS_PENDING,
		Message:    fmt.Sprintf("serial %s is waiting for approval (cucinactl hosts approve %s)", serial, serial),
		RetryAfter: durationpb.New(s.o.PendingRetryAfter),
	}
}

func (s *Server) denied(msg string) *hostResp {
	return &hostResp{Status: cucinav1.EnrollHostResponse_STATUS_DENIED, Message: msg}
}

func reportedLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if len(out) == maxReportedLabels {
			break
		}
		if labelKeyRE.MatchString(k) && labelValueRE.MatchString(v) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ------------------------------------------------------- HostService helpers

// RenewCertificate implements the certificate side of HostService.RenewCertificate
// (R-SEC-2: hosts renew before expiry over mTLS). peer is the caller's verified
// identity (pki.Verifier.PeerIdentity with pki.RoleHost); the host must still
// exist, be approved and be enrolled. The key may rotate (new CSR key).
func (s *Server) RenewCertificate(ctx context.Context, peer pki.Identity, req *cucinav1.RenewCertificateRequest) (*cucinav1.RenewCertificateResponse, error) {
	const event = "host.renew"
	host, err := s.liveHost(ctx, peer)
	if err != nil {
		s.audit(ctx, event, "denied", "identity", peer.String(), "reason", err.Error())
		return nil, err
	}
	pub, err := pki.ParseCSR(req.GetCsrPem(), peer)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	keyHash, err := pki.PublicKeyHash(pub)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	issued, err := s.d.Issuer.IssueHost(req.GetCsrPem(), host.Serial)
	if err != nil {
		if errors.Is(err, pki.ErrBadCSR) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, s.unavailable(ctx, "issuing the host certificate", err)
	}
	_, err = s.d.Hosts.Update(ctx, host.Serial, func(h *HostRecord) error {
		if !h.Approved || !h.Enrolled() {
			return errHostRemoved
		}
		h.KeySHA256, h.CertExpiry = keyHash, issued.NotAfter
		return nil
	})
	if errors.Is(err, errHostRemoved) || errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.PermissionDenied, errHostRemoved.Error())
	}
	if err != nil {
		return nil, s.unavailable(ctx, "recording the renewal", err)
	}
	s.audit(ctx, event, "issued", "serial", host.Serial, "notAfter", issued.NotAfter.UTC().Format(time.RFC3339), "keyRotated", keyHash != host.KeySHA256)
	return &cucinav1.RenewCertificateResponse{CertificatePem: issued.ChainPEM, CaPem: s.bundle(issued), ExpiresAt: timestamppb.New(issued.NotAfter)}, nil
}

// IssueVMIdentity implements the certificate side of HostService.IssueVMIdentity:
// a short-lived worker certificate spiffe://cucina/worker/<pool>/<serial>/<vm>
// for one VM of the calling host (R-MAC-4: VMs get credentials only through
// their host) plus its WorkerSettings. The serial comes from the caller's
// certificate, never from the request. Callers (internal/hostlink) override the
// storage/scheduler endpoints with the host-local L2 and relay.
func (s *Server) IssueVMIdentity(ctx context.Context, peer pki.Identity, req *cucinav1.IssueVMIdentityRequest) (*cucinav1.IssueVMIdentityResponse, error) {
	const event = "host.vm-identity"
	host, err := s.liveHost(ctx, peer)
	if err != nil {
		s.audit(ctx, event, "denied", "identity", peer.String(), "reason", err.Error())
		return nil, err
	}
	id, err := pki.VMIdentity(req.GetPool(), host.Serial, req.GetVmName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	settings, _, err := s.d.Pools.SettingsFor(id.Pool, id.Node())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("pool %q is not configured on this controller", id.Pool))
	}
	issued, err := s.d.Issuer.IssueVM(req.GetCsrPem(), id.Pool, id.Serial, id.VM)
	if err != nil {
		if errors.Is(err, pki.ErrBadCSR) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, s.unavailable(ctx, "issuing the VM certificate", err)
	}
	s.audit(ctx, event, "issued", "identity", issued.Identity.String(), "notAfter", issued.NotAfter.UTC().Format(time.RFC3339))
	return &cucinav1.IssueVMIdentityResponse{CertificatePem: issued.ChainPEM, CaPem: s.bundle(issued), ExpiresAt: timestamppb.New(issued.NotAfter), Settings: settings}, nil
}

// liveHost returns the record of an authenticated host that is still admitted.
func (s *Server) liveHost(ctx context.Context, peer pki.Identity) (HostRecord, error) {
	if peer.Role != pki.RoleHost {
		return HostRecord{}, status.Error(codes.PermissionDenied, "only Mac host identities may call this")
	}
	host, err := s.d.Hosts.Get(ctx, peer.Serial)
	if errors.Is(err, ErrNotFound) {
		return HostRecord{}, status.Error(codes.PermissionDenied, errHostRemoved.Error())
	}
	if err != nil {
		return HostRecord{}, s.unavailable(ctx, "reading the host", err)
	}
	if !host.Approved || !host.Enrolled() {
		return HostRecord{}, status.Error(codes.PermissionDenied, errHostRemoved.Error())
	}
	return host, nil
}
