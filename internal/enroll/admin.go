// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/pki"
)

// MaxTokenHosts bounds a site token's host count.
const MaxTokenHosts = 10_000

// Admin implements the management hooks behind `cucinactl hosts register|
// approve|remove` and `cucinactl hosts enroll-token` (ManagementService, agent
// mgmt). Methods take and return the management API's protobuf types and
// return gRPC status errors, so the ManagementService delegates directly after
// checking the caller's admin grant; actor is the authenticated principal for
// the audit log.
type Admin struct{ s *Server }

// CreateEnrollToken creates a site enrollment token. The token string is
// returned exactly once; only its hash is stored.
func (a *Admin) CreateEnrollToken(ctx context.Context, req *cucinav1.CreateEnrollTokenRequest, actor string) (*cucinav1.CreateEnrollTokenResponse, error) {
	s := a.s
	if err := validateSite(req.GetSite()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	ttl := req.GetTtl().AsDuration()
	if req.GetTtl() == nil || ttl == 0 {
		ttl = s.o.DefaultTokenTTL
	}
	if ttl < time.Minute || ttl > s.o.MaxTokenTTL {
		return nil, status.Errorf(codes.InvalidArgument, "ttl %s out of range [1m, %s]", ttl, s.o.MaxTokenTTL)
	}
	maxHosts := int(req.GetMaxHosts())
	if maxHosts < 1 || maxHosts > MaxTokenHosts {
		return nil, status.Errorf(codes.InvalidArgument, "max_hosts must be between 1 and %d", MaxTokenHosts)
	}
	if len(req.GetDescription()) > 256 {
		return nil, status.Error(codes.InvalidArgument, "description longer than 256 bytes")
	}
	id, secret, err := newToken(s.d.Rand)
	if err != nil {
		return nil, s.unavailable(ctx, "generating a token", err)
	}
	now := s.d.Clock.Now()
	rec := TokenRecord{
		ID: id, Site: req.GetSite(), SecretHash: hashSecret(secret), MaxHosts: maxHosts,
		ExpiresAt: now.Add(ttl), CreatedAt: now, CreatedBy: actor, Description: req.GetDescription(),
	}
	if err := s.d.Tokens.Create(ctx, rec); err != nil {
		return nil, s.unavailable(ctx, "storing the token", err)
	}
	s.audit(ctx, "admin.token.create", "ok", "actor", actor, "token", id, "site", rec.Site, "maxHosts", maxHosts,
		"expiresAt", rec.ExpiresAt.UTC().Format(time.RFC3339))
	return &cucinav1.CreateEnrollTokenResponse{Id: id, Token: formatToken(id, secret), ExpiresAt: timestamppb.New(rec.ExpiresAt)}, nil
}

// ListEnrollTokens lists tokens (never secrets or hashes).
func (a *Admin) ListEnrollTokens(ctx context.Context) (*cucinav1.ListEnrollTokensResponse, error) {
	recs, err := a.s.d.Tokens.List(ctx)
	if err != nil {
		return nil, a.s.unavailable(ctx, "listing tokens", err)
	}
	out := &cucinav1.ListEnrollTokensResponse{}
	for _, r := range recs {
		out.Tokens = append(out.Tokens, &cucinav1.EnrollTokenInfo{
			Id: r.ID, Site: r.Site, Description: r.Description, Created: timestamppb.New(r.CreatedAt),
			ExpiresAt: timestamppb.New(r.ExpiresAt), MaxHosts: uint32(r.MaxHosts), UsedHosts: uint32(len(r.Hosts)),
			Revoked: r.RevokedAt != nil,
		})
	}
	return out, nil
}

// RevokeEnrollToken revokes a token. Hosts already enrolled with it are not
// affected (R-SEC-3); pending hosts can no longer complete enrollment with it.
func (a *Admin) RevokeEnrollToken(ctx context.Context, id, actor string) error {
	s := a.s
	_, err := s.d.Tokens.Update(ctx, id, func(t *TokenRecord) error {
		if t.RevokedAt == nil {
			now := s.d.Clock.Now()
			t.RevokedAt, t.RevokedBy = &now, actor
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return status.Errorf(codes.NotFound, "enrollment token %q not found", id)
	}
	if err != nil {
		return s.unavailable(ctx, "revoking the token", err)
	}
	s.audit(ctx, "admin.token.revoke", "ok", "actor", actor, "token", id)
	return nil
}

// RegisterSerials pre-registers and approves serial numbers (e.g. pasted from
// Apple Business). Existing hosts are approved and reported as already present.
func (a *Admin) RegisterSerials(ctx context.Context, req *cucinav1.RegisterHostSerialsRequest, actor string) (*cucinav1.RegisterHostSerialsResponse, error) {
	s := a.s
	if req.GetSite() != "" {
		if err := validateSite(req.GetSite()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	labels := maps.Clone(req.GetLabels())
	for k, v := range labels {
		if !labelKeyRE.MatchString(k) || !labelValueRE.MatchString(v) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid label %q=%q", k, v)
		}
	}
	serials := make([]string, 0, len(req.GetSerials()))
	for _, raw := range req.GetSerials() {
		serial, err := pki.CanonicalSerial(raw)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if !slices.Contains(serials, serial) {
			serials = append(serials, serial)
		}
	}
	if len(serials) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no serial numbers")
	}
	out := &cucinav1.RegisterHostSerialsResponse{}
	for _, serial := range serials {
		created, err := a.approve(ctx, serial, req.GetSite(), labels)
		if err != nil {
			return out, err
		}
		if created {
			out.Registered = append(out.Registered, serial)
		} else {
			out.AlreadyPresent = append(out.AlreadyPresent, serial)
		}
	}
	s.audit(ctx, "admin.hosts.register", "ok", "actor", actor, "registered", strings.Join(out.Registered, ","),
		"alreadyPresent", strings.Join(out.AlreadyPresent, ","))
	return out, nil
}

// ApproveHost approves a pending host (or pre-registers an unknown serial).
func (a *Admin) ApproveHost(ctx context.Context, serialOrName, actor string) (*cucinav1.HostActionResponse, error) {
	serial, err := pki.CanonicalSerial(serialOrName)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	created, err := a.approve(ctx, serial, "", nil)
	if err != nil {
		return nil, err
	}
	a.s.audit(ctx, "admin.hosts.approve", "ok", "actor", actor, "serial", serial, "preRegistered", created)
	msg := fmt.Sprintf("host %s approved; it receives its certificate at its next enrollment poll", serial)
	if created {
		msg = fmt.Sprintf("serial %s pre-registered and approved", serial)
	}
	return &cucinav1.HostActionResponse{Message: msg}, nil
}

// approve sets spec.approved, creating the MacHost if needed.
func (a *Admin) approve(ctx context.Context, serial, site string, labels map[string]string) (created bool, err error) {
	s := a.s
	for range 3 {
		_, err := s.d.Hosts.Update(ctx, serial, func(h *HostRecord) error {
			h.Approved = true
			if h.Site == "" {
				h.Site = site
			}
			for k, v := range labels {
				if h.Labels == nil {
					h.Labels = map[string]string{}
				}
				h.Labels[k] = v
			}
			return nil
		})
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return false, s.unavailable(ctx, "approving the host", err)
		}
		err = s.d.Hosts.Create(ctx, HostRecord{Serial: serial, Name: strings.ToLower(serial), Site: site, Labels: maps.Clone(labels), Approved: true})
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, ErrExists) {
			return false, s.unavailable(ctx, "registering the host", err)
		}
	}
	return false, s.unavailable(ctx, "approving the host", errors.New("concurrent changes"))
}

// RemoveHost deletes the MacHost: its certificate is no longer renewed, the
// serial may enroll again (with a token and approval), its token slot is
// released, and — with a Revoker — its identity is deny-listed until any
// certificate it holds has expired.
func (a *Admin) RemoveHost(ctx context.Context, serialOrName, actor string) (*cucinav1.HostActionResponse, error) {
	s := a.s
	serial, err := pki.CanonicalSerial(serialOrName)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	host, err := s.d.Hosts.Get(ctx, serial)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "host %s not found", serial)
	}
	if err != nil {
		return nil, s.unavailable(ctx, "reading the host", err)
	}
	if err := s.d.Hosts.Delete(ctx, serial); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, s.unavailable(ctx, "removing the host", err)
	}
	if host.TokenID != "" {
		_, err := s.d.Tokens.Update(ctx, host.TokenID, func(t *TokenRecord) error {
			t.Hosts = slices.DeleteFunc(t.Hosts, func(h string) bool { return h == serial })
			return nil
		})
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.log.WarnContext(ctx, "releasing the token slot failed; it is reclaimed when the token is full", "serial", serial, "error", err)
		}
	}
	revoked := false
	if s.d.Revoker != nil && host.Enrolled() {
		id, _ := pki.HostIdentity(serial)
		until := host.CertExpiry
		if until.IsZero() || until.Before(s.d.Clock.Now()) {
			until = s.d.Clock.Now().Add(s.d.Issuer.Policy().HostTTL)
		}
		if err := s.d.Revoker.RevokeSubject(ctx, id.String(), "host removed", actor, until.Add(time.Hour)); err != nil {
			return nil, s.unavailable(ctx, "deny-listing the host identity", err)
		}
		revoked = true
	}
	s.audit(ctx, "admin.hosts.remove", "ok", "actor", actor, "serial", serial, "denyListed", revoked)
	return &cucinav1.HostActionResponse{Message: fmt.Sprintf("host %s removed", serial)}, nil
}

// ListHosts returns the enrollment view of every host (for `cucinactl hosts list`).
func (a *Admin) ListHosts(ctx context.Context) ([]HostRecord, error) {
	hosts, err := a.s.d.Hosts.List(ctx)
	if err != nil {
		return nil, a.s.unavailable(ctx, "listing hosts", err)
	}
	return hosts, nil
}
