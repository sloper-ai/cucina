// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/enroll"
)

// EnrollAdmin is the enrollment service's management hook set; *enroll.Admin
// (enroll.Server.Admin()) implements it. Its methods return gRPC status errors,
// which the management API passes through unchanged.
type EnrollAdmin interface {
	CreateEnrollToken(ctx context.Context, req *cucinav1.CreateEnrollTokenRequest, actor string) (*cucinav1.CreateEnrollTokenResponse, error)
	ListEnrollTokens(ctx context.Context) (*cucinav1.ListEnrollTokensResponse, error)
	RevokeEnrollToken(ctx context.Context, id, actor string) error
	RegisterSerials(ctx context.Context, req *cucinav1.RegisterHostSerialsRequest, actor string) (*cucinav1.RegisterHostSerialsResponse, error)
	ApproveHost(ctx context.Context, serialOrName, actor string) (*cucinav1.HostActionResponse, error)
	RemoveHost(ctx context.Context, serialOrName, actor string) (*cucinav1.HostActionResponse, error)
}

var _ EnrollAdmin = (*enroll.Admin)(nil)

// EnrollAdapter implements Enrollment over the enrollment service (internal/enroll).
type EnrollAdapter struct{ Admin EnrollAdmin }

var _ Enrollment = (*EnrollAdapter)(nil)

// CreateEnrollToken implements Enrollment.
func (a *EnrollAdapter) CreateEnrollToken(ctx context.Context, req EnrollTokenRequest) (EnrollToken, string, error) {
	resp, err := a.Admin.CreateEnrollToken(ctx, &cucinav1.CreateEnrollTokenRequest{
		Site: req.Site, Ttl: durationpb.New(req.TTL), MaxHosts: u32(req.MaxHosts), Description: req.Description,
	}, req.CreatedBy)
	if err != nil {
		return EnrollToken{}, "", err
	}
	return EnrollToken{
		ID: resp.GetId(), Site: req.Site, Description: req.Description, Created: time.Now(),
		ExpiresAt: resp.GetExpiresAt().AsTime(), MaxHosts: req.MaxHosts,
	}, resp.GetToken(), nil
}

// ListEnrollTokens implements Enrollment.
func (a *EnrollAdapter) ListEnrollTokens(ctx context.Context) ([]EnrollToken, error) {
	resp, err := a.Admin.ListEnrollTokens(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]EnrollToken, 0, len(resp.GetTokens()))
	for _, t := range resp.GetTokens() {
		out = append(out, EnrollToken{
			ID: t.GetId(), Site: t.GetSite(), Description: t.GetDescription(), Created: t.GetCreated().AsTime(),
			ExpiresAt: t.GetExpiresAt().AsTime(), MaxHosts: int(t.GetMaxHosts()), UsedHosts: int(t.GetUsedHosts()),
			Revoked: t.GetRevoked(),
		})
	}
	return out, nil
}

// RevokeEnrollToken implements Enrollment.
func (a *EnrollAdapter) RevokeEnrollToken(ctx context.Context, id string) error {
	return a.Admin.RevokeEnrollToken(ctx, id, caller(ctx))
}

// RegisterSerials implements Enrollment.
func (a *EnrollAdapter) RegisterSerials(ctx context.Context, serials []string, site string, labels map[string]string) ([]string, []string, error) {
	resp, err := a.Admin.RegisterSerials(ctx, &cucinav1.RegisterHostSerialsRequest{Serials: serials, Site: site, Labels: labels}, caller(ctx))
	if err != nil {
		return nil, nil, err
	}
	return resp.GetRegistered(), resp.GetAlreadyPresent(), nil
}

// ApproveHost implements Enrollment.
func (a *EnrollAdapter) ApproveHost(ctx context.Context, serial string) error {
	_, err := a.Admin.ApproveHost(ctx, serial, caller(ctx))
	return err
}

// RemoveHost implements Enrollment.
func (a *EnrollAdapter) RemoveHost(ctx context.Context, serial string) error {
	_, err := a.Admin.RemoveHost(ctx, serial, caller(ctx))
	return err
}
