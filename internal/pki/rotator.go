// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Rotator is the leader-only component that keeps Cucina-managed in-cluster
// certificate Secrets fresh: it re-issues each one at 2/3 of its lifetime (or
// when its SANs, usages or issuing CA changed) and refreshes ca.crt after a CA
// rotation phase. Buildbarn picks the new files up through its TLS
// refreshInterval; the controller through its file reloaders. It also exports
// cucina_cert_expiry_seconds for the roles ca, server and controller.
//
// It is disabled (not constructed) when the TLS source is an existing Secret or
// cert-manager: those Secrets lack Cucina's label and would be left alone anyway.
type Rotator struct {
	Client    client.Client
	Namespace string
	CASecret  string
	Specs     []CertSpec
	Clock     ports.Clock
	// Interval between passes (default 10m).
	Interval time.Duration
	Policy   Policy
	Expiry   *ExpiryTracker
	Logger   *slog.Logger
}

// RunOnce performs one pass.
func (r *Rotator) RunOnce(ctx context.Context) ([]CertResult, error) {
	if r.Client == nil || r.Clock == nil || r.Namespace == "" || r.CASecret == "" {
		return nil, errors.New("pki: Rotator needs Client, Clock, Namespace and CASecret")
	}
	ca, err := LoadCA(ctx, r.Client, r.Namespace, r.CASecret)
	if err != nil {
		return nil, err
	}
	r.Expiry.Set(ExpiryRoleCA, "issuer", ca.cert.NotAfter)
	results, err := ensureCerts(ctx, r.Client, r.Namespace, ca, r.Specs, r.Clock, r.Policy)
	for _, res := range results {
		if !res.NotAfter.IsZero() {
			role := ExpiryRoleServer
			if res.Role == RoleController {
				role = ExpiryRoleController
			}
			r.Expiry.Set(role, res.SecretName, res.NotAfter)
		}
		if res.Action == CertCreated || res.Action == CertRenewed || res.Action == CertBundle {
			r.logger().Info("certificate secret updated", "secret", res.SecretName, "action", res.Action,
				"reason", res.Reason, "notAfter", res.NotAfter.UTC().Format(time.RFC3339))
		}
	}
	return results, err
}

// Start implements manager.Runnable.
func (r *Rotator) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	for {
		if _, err := r.RunOnce(ctx); err != nil {
			r.logger().Error("certificate rotation pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-r.Clock.After(interval):
		}
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: only the leader rotates.
func (r *Rotator) NeedLeaderElection() bool { return true }

func (r *Rotator) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
