// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/enroll"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/reconcile"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// EnrollmentService (agent `enroll`): EC2 workers present their signed
// instance identity document; Mac hosts a site token, serial and CSR
// (R-SEC-3). The controller supplies the pool settings and confirms its own
// launches from the launch ledger (ADR 0551).
func init() {
	// Built before the host stream (Order -15 < -10): hostlink's RenewCertificate
	// delegates to the enrollment server.
	RegisterComponent(Factory{Name: "enroll", Modes: []Mode{ModeController}, Listener: ListenerEnrollment, Order: -15, New: newEnroll})
}

// SharedEnroll is the key of the enrollment server (its Admin backs the management API).
const SharedEnroll = "enroll.server"

func newEnroll(ctx context.Context, d *Deps) (any, error) {
	cfg := d.Config
	if cfg.Listeners.Enrollment == "" {
		return nil, nil
	}
	issuer, ok := Shared[*pki.Issuer](d, SharedPKIIssuer)
	if !ok {
		return nil, errors.New("enrollment needs the PKI issuer")
	}
	deps := enroll.Deps{
		Issuer: issuer,
		Clock:  d.Clock,
		Logger: d.Log,
		Pools:  d.Pools,
		Hosts:  &enroll.MacHostStore{Reader: d.APIReader, Client: d.Client, Namespace: cfg.Namespace},
		Tokens: &enroll.SecretTokens{Reader: d.APIReader, Client: d.Client, Namespace: cfg.Namespace, Name: cfg.ReleaseName + "-enroll-tokens"},
		Options: enroll.Options{
			ClusterID:       cfg.ClusterID,
			HostEndpoint:    cfg.Endpoints.HostEndpoint,
			DefaultTokenTTL: cfg.Hosts.DefaultTokenTTL.Duration,
		},
	}
	if m, ok := Shared[*keys.Manager](d, SharedKeys); ok {
		deps.Revoker = revoker{m: m}
	}
	if cfg.AWS != nil && d.Compute != nil {
		iv, err := enroll.NewIdentityVerifier()
		if err != nil {
			return nil, err
		}
		deps.Identity, deps.Compute = iv, d.Compute
		deps.Launches = LedgerLaunches{Reader: d.APIReader, Namespace: cfg.Namespace, Cluster: cfg.ClusterID}
		deps.Replay = &enroll.LeaseReplay{Reader: d.APIReader, Client: d.Client, Namespace: cfg.Namespace}
		deps.Options.AWSAccountID, deps.Options.AWSRegion = cfg.AWS.AccountID, cfg.AWS.Region
	}
	srv, err := enroll.New(deps)
	if err != nil {
		return nil, err
	}
	d.Share(SharedEnroll, srv)
	return srv, nil
}

// revoker deny-lists a workload subject (RemoveHost) through the key manager.
type revoker struct{ m *keys.Manager }

func (r revoker) RevokeSubject(ctx context.Context, sub, reason, actor string, until time.Time) error {
	_, _, err := r.m.Revoke(ctx, keys.RevokeRequest{Kind: keys.RevokeSubject, Value: sub, Reason: reason, Actor: actor, ExpiresAt: until})
	return err
}

// LedgerLaunches implements enroll.LaunchRecords from the pools' launch
// ledgers (ADR 0551): an instance enrolls only if its cucina:launch-token was
// issued by this controller for its pool — the token's prefix is the hash of
// (cluster, pool), its epoch is the pool's current ledger epoch, and its
// sequence number was persisted (write-ahead) before the launch.
type LedgerLaunches struct {
	Reader    client.Reader
	Namespace string
	Cluster   string
}

// VerifyLaunch implements enroll.LaunchRecords.
func (l LedgerLaunches) VerifyLaunch(ctx context.Context, inst ports.Instance) error {
	pool := domain.PoolName(inst.Tags[domain.TagPool])
	prefix, epoch, seq, ok := scaling.ParseToken(inst.Tags[domain.TagLaunchToken])
	if !ok || pool == "" || prefix != scaling.TokenPrefix(l.Cluster, pool) {
		return fmt.Errorf("%w: launch token of %s is not one of this cluster's tokens for pool %q", enroll.ErrUnknownLaunch, inst.ID, pool)
	}
	var wp v1alpha1.WorkerPool
	if err := l.Reader.Get(ctx, client.ObjectKey{Namespace: l.Namespace, Name: string(pool)}, &wp); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: pool %q does not exist", enroll.ErrUnknownLaunch, pool)
		}
		return err
	}
	led := reconcile.ReadLedger(&wp)
	if led.Epoch == "" || epoch != led.Epoch || seq >= led.Next {
		return fmt.Errorf("%w: launch token of %s is not in pool %q's launch ledger", enroll.ErrUnknownLaunch, inst.ID, pool)
	}
	return nil
}
