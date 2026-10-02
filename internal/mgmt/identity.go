// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
	"unicode"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
)

const (
	maxIdentifier = 512
	maxReason     = 512
)

// identifier validates a principal, session, account or key identifier: non-empty,
// bounded, printable, no whitespace. The key store applies its exact format rules.
func identifier(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", invalid("%s is required", field)
	}
	if len(v) > maxIdentifier {
		return "", invalid("%s longer than %d bytes", field, maxIdentifier)
	}
	for _, r := range v {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return "", invalid("%s contains whitespace or control characters", field)
		}
	}
	return v, nil
}

// CreateServiceKey creates an opt-in service-account key (R-AUTH-10). The key is
// returned exactly once and never audited.
func (s *Server) CreateServiceKey(ctx context.Context, req *cucinav1.CreateServiceKeyRequest) (*cucinav1.CreateServiceKeyResponse, error) {
	if s.deps.Keys == nil {
		return nil, notConfigured("service-account keys")
	}
	account, err := identifier("account", req.GetAccount())
	if err != nil {
		return nil, err
	}
	if len(req.GetDescription()) > maxDescription {
		return nil, invalid("description longer than %d bytes", maxDescription)
	}
	var ttl time.Duration
	if req.GetTtl() != nil {
		if ttl = req.GetTtl().AsDuration(); ttl <= 0 {
			return nil, invalid("ttl must be positive (omit it for a key without expiry)")
		}
	}
	key, secret, err := s.deps.Keys.CreateServiceKey(ctx, ServiceKeyRequest{
		Account: account, Description: req.GetDescription(), TTL: ttl, CreatedBy: caller(ctx),
	})
	if err != nil {
		return nil, fail("creating service key", err)
	}
	return &cucinav1.CreateServiceKeyResponse{KeyId: key.ID, Key: secret}, nil
}

// ListServiceKeys lists key metadata (never keys), optionally for one account.
func (s *Server) ListServiceKeys(ctx context.Context, req *cucinav1.ListServiceKeysRequest) (*cucinav1.ListServiceKeysResponse, error) {
	if s.deps.Keys == nil {
		return nil, notConfigured("service-account keys")
	}
	keys, err := s.deps.Keys.ListServiceKeys(ctx, strings.TrimSpace(req.GetAccount()))
	if err != nil {
		return nil, fail("listing service keys", err)
	}
	keys = slices.Clone(keys)
	slices.SortFunc(keys, func(a, b ServiceKey) int {
		return cmp.Or(cmp.Compare(a.Account, b.Account), a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	out := &cucinav1.ListServiceKeysResponse{}
	for _, k := range keys {
		out.Keys = append(out.Keys, &cucinav1.ServiceKeyInfo{
			KeyId: k.ID, Account: k.Account, Description: k.Description, Created: ts(k.Created),
			ExpiresAt: ts(k.ExpiresAt), LastUsed: ts(k.LastUsed), Revoked: k.Revoked,
		})
	}
	return out, nil
}

// RevokeServiceKey revokes a key instantly (the STS refuses it from then on).
func (s *Server) RevokeServiceKey(ctx context.Context, req *cucinav1.RevokeServiceKeyRequest) (*cucinav1.RevokeServiceKeyResponse, error) {
	if s.deps.Keys == nil {
		return nil, notConfigured("service-account keys")
	}
	id, err := identifier("key_id", req.GetKeyId())
	if err != nil {
		return nil, err
	}
	if err := s.deps.Keys.RevokeServiceKey(ctx, id); err != nil {
		return nil, fail("revoking service key", err)
	}
	return &cucinav1.RevokeServiceKeyResponse{}, nil
}

// RevokePrincipal adds a subject or session to the deny-list and reports when every
// frontend enforces it (≤ 3 min, R-AUTH-9).
func (s *Server) RevokePrincipal(ctx context.Context, req *cucinav1.RevokePrincipalRequest) (*cucinav1.RevokePrincipalResponse, error) {
	if s.deps.Revocations == nil {
		return nil, notConfigured("revocation")
	}
	r := Revocation{Created: time.Now(), CreatedBy: caller(ctx), Reason: strings.TrimSpace(req.GetReason())}
	if strings.TrimSpace(req.GetSub()) == "" && strings.TrimSpace(req.GetSid()) == "" {
		return nil, invalid("sub or sid is required")
	}
	var err error
	if strings.TrimSpace(req.GetSub()) != "" {
		if r.Subject, err = identifier("sub", req.GetSub()); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(req.GetSid()) != "" {
		if r.SessionID, err = identifier("sid", req.GetSid()); err != nil {
			return nil, err
		}
	}
	if len(r.Reason) > maxReason {
		return nil, invalid("reason longer than %d bytes", maxReason)
	}
	effective, err := s.deps.Revocations.Revoke(ctx, r)
	if err != nil {
		return nil, fail("revoking", err)
	}
	if effective.IsZero() {
		effective = r.Created.Add(s.opts.RevocationPropagation)
	}
	return &cucinav1.RevokePrincipalResponse{EffectiveBy: ts(effective)}, nil
}

// ListRevocations lists the deny-list.
func (s *Server) ListRevocations(ctx context.Context, _ *cucinav1.ListRevocationsRequest) (*cucinav1.ListRevocationsResponse, error) {
	if s.deps.Revocations == nil {
		return nil, notConfigured("revocation")
	}
	rs, err := s.deps.Revocations.ListRevocations(ctx)
	if err != nil {
		return nil, fail("listing revocations", err)
	}
	rs = slices.Clone(rs)
	slices.SortFunc(rs, func(a, b Revocation) int { return a.Created.Compare(b.Created) })
	out := &cucinav1.ListRevocationsResponse{}
	for _, r := range rs {
		out.Revocations = append(out.Revocations, &cucinav1.Revocation{
			Sub: r.Subject, Sid: r.SessionID, Reason: r.Reason, Created: ts(r.Created), CreatedBy: r.CreatedBy,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------- cost and images

// GetCost implements `cucinactl cost` (R-OBS-5, UC17).
func (s *Server) GetCost(ctx context.Context, req *cucinav1.GetCostRequest) (*cucinav1.GetCostResponse, error) {
	if s.deps.Cost == nil {
		return nil, notConfigured("cost reporting (observability.costEnabled)")
	}
	if req.GetPool() != "" {
		if _, err := s.findPool(ctx, req.GetPool()); err != nil {
			return nil, err
		}
	}
	q := CostQuery{Pool: req.GetPool()}
	if req.GetSince() != nil {
		q.Since = req.GetSince().AsTime()
		if q.Since.After(time.Now()) {
			return nil, invalid("since is in the future")
		}
	}
	r, err := s.deps.Cost.Cost(ctx, q)
	if err != nil {
		return nil, fail("cost", err)
	}
	out := &cucinav1.GetCostResponse{Cost: costSummary(r), Assumptions: r.Assumptions}
	for _, l := range r.Lines {
		out.Lines = append(out.Lines, &cucinav1.CostLine{
			Pool: l.Pool, Category: l.Category, Detail: l.Detail, Quantity: l.Quantity, Unit: l.Unit, Amount: money(l.Amount),
		})
	}
	return out, nil
}

// ListImages lists every pool's current and previous image with the number of
// workers still running each (R-OPS-2).
func (s *Server) ListImages(ctx context.Context, _ *cucinav1.ListImagesRequest) (*cucinav1.ListImagesResponse, error) {
	if s.deps.Images == nil {
		return nil, notConfigured("image tracking")
	}
	imgs, err := s.deps.Images.Images(ctx)
	if err != nil {
		return nil, fail("images", err)
	}
	workers, err := s.deps.Workers.Workers(ctx)
	if err != nil {
		return nil, fail("workers", err)
	}
	type genKey struct{ pool, generation string }
	running := map[genKey]uint32{}
	for _, w := range workers {
		if countsAsInstance(w.VM) {
			running[genKey{string(w.Pool), w.Generation}]++
		}
	}
	imgs = slices.Clone(imgs)
	slices.SortFunc(imgs, func(a, b Image) int {
		rank := func(i Image) int {
			switch {
			case i.Current:
				return 0
			case i.Previous:
				return 1
			}
			return 2
		}
		return cmp.Or(cmp.Compare(a.Pool, b.Pool), cmp.Compare(rank(a), rank(b)), b.Created.Compare(a.Created))
	})
	out := &cucinav1.ListImagesResponse{}
	for _, i := range imgs {
		out.Images = append(out.Images, &cucinav1.ImageInfo{
			Pool: i.Pool, Reference: i.Reference, Version: i.Version, Generation: i.Generation,
			Current: i.Current, Previous: i.Previous, WorkersRunning: running[genKey{i.Pool, i.Generation}],
			Created: ts(i.Created), FastLaunch: i.FastLaunch,
		})
	}
	return out, nil
}
