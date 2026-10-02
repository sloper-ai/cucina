// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/sts"
)

// Client authentication (agent `auth`): the signing-key manager (every replica
// refreshes keys and the deny-list; the controller leader rotates keys), the
// TrustPolicy engine (every replica evaluates; the controller leader writes the
// `Valid` conditions), the STS HTTPS server, and the bootstrap steps that
// create the signing keys and the break-glass key.
func init() {
	RegisterComponent(Factory{Name: "keys", Modes: []Mode{ModeController, ModeSTS}, Order: -30, New: newKeys})
	RegisterComponent(Factory{Name: "trust-policies", Modes: []Mode{ModeController, ModeSTS}, Order: -20, New: newTrustPolicies})
	RegisterComponent(Factory{Name: "sts", Modes: []Mode{ModeController, ModeSTS}, Order: 0, New: newSTS})
	RegisterBootstrapStep(BootstrapStep{Name: "signing-keys", Order: 30, Run: func(ctx context.Context, bd *BootstrapDeps) error {
		if bd.Config == nil {
			return fmt.Errorf("signing keys: --config is required (it names the auth Secrets and ConfigMaps)")
		}
		return keys.EnsureSigningKeys(ctx, bd.Kube, bd.Namespace, bd.Config.Auth)
	}})
	RegisterBootstrapStep(BootstrapStep{Name: "break-glass-key", Order: 40, Run: func(ctx context.Context, bd *BootstrapDeps) error {
		if bd.Config == nil {
			return fmt.Errorf("break-glass key: --config is required")
		}
		if err := keys.EnsureBreakGlass(ctx, bd.Kube, bd.Namespace, bd.Config.Auth); err != nil {
			return err
		}
		bd.Log.Info("break-glass key ready (read it with kubectl; it is never printed)", "secret", bd.Config.Auth.BreakGlassKeySecret)
		return nil
	}})
}

// Shared object keys.
const (
	SharedKeys   = "keys.manager"
	SharedEngine = "auth.engine"
)

func newKeys(ctx context.Context, d *Deps) (any, error) {
	cs, err := kubernetes.NewForConfig(d.RESTConfig)
	if err != nil {
		return nil, err
	}
	cfg := d.Config
	log := d.Log.With("component", "keys")
	opts := keys.Options{Clock: d.Clock, Log: log}
	if d.Mode == ModeController {
		// Rotation (leader) promotes a key only once every frontend and the
		// scheduler accept it; a compromise restarts them (R-AUTH-9).
		opts.Restarter = &deploymentRestarter{c: d.Client, ns: cfg.Namespace, release: cfg.ReleaseName, clock: d.Clock.Now, log: log}
	}
	m, err := keys.NewManager(keys.NewKubeObjects(cs, cfg.Namespace), cfg.Auth, cfg.Endpoints.STSURL, opts)
	if err != nil {
		return nil, err
	}
	if d.Mode == ModeController {
		m.Rotator.Loaded = &frontendProbe{c: d.APIReader, ns: cfg.Namespace, release: cfg.ReleaseName, minter: m.Minter,
			ca: cfg.TLS.CAFile, instance: cfg.InstanceNames[0], log: log}
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := m.Start(sctx); err != nil {
		return nil, fmt.Errorf("%w (did the bootstrap hook run?)", err)
	}
	d.Share(SharedKeys, m)
	return &keysRunner{m: m, d: d}, nil
}

// keysRunner refreshes the key ring and the deny-list on every replica and, on
// the controller leader only, also drives key rotation and deny-list pruning.
type keysRunner struct {
	m *keys.Manager
	d *Deps
}

func (k *keysRunner) Run(ctx context.Context) error {
	if k.d.Mode != ModeController {
		return k.m.Run(ctx, false, 0)
	}
	follower, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- k.m.Run(follower, false, 0) }()
	select {
	case <-ctx.Done():
		stop()
		return <-done
	case <-k.d.Elected:
	}
	stop()
	<-done
	return k.m.Run(ctx, true, 30*time.Second)
}

func newTrustPolicies(ctx context.Context, d *Deps) (any, error) {
	cfg := d.Config
	cs, err := kubernetes.NewForConfig(d.RESTConfig)
	if err != nil {
		return nil, err
	}
	groups, err := groupResolver(ctx, cs, cfg, d)
	if err != nil {
		return nil, err
	}
	eng, err := auth.NewEngine(auth.EngineOptions{
		IdentityProvider: auth.NewOIDCVerifier(d.Clock),
		Groups:           groups,
		Clock:            d.Clock,
		InstanceNames:    cfg.InstanceNames,
		TokenTTL:         cfg.Auth.TokenTTL.Duration,
		Log:              d.Log.With("component", "trust-policies"),
	})
	if err != nil {
		return nil, err
	}
	d.Share(SharedEngine, eng)
	tp := &trustPolicySync{d: d, eng: eng}
	// Every replica keeps its engine current (NeedLeaderElection=false); only the
	// controller leader writes status.
	notLeader := false
	err = builder.ControllerManagedBy(d.Manager).
		Named("trustpolicy").
		For(&v1alpha1.TrustPolicy{}).
		WithOptions(crcontroller.Options{NeedLeaderElection: &notLeader}).
		Complete(tp)
	if err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

// trustPolicySync recompiles all policies on any change (level-triggered).
type trustPolicySync struct {
	d   *Deps
	eng *auth.Engine
}

func (t *trustPolicySync) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	var list v1alpha1.TrustPolicyList
	if err := t.d.Client.List(ctx, &list, client.InNamespace(t.d.Config.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	statuses := t.eng.Update(list.Items)
	if t.d.Mode != ModeController || !t.d.IsLeader() {
		return ctrl.Result{}, nil
	}
	now := t.d.Clock.Now()
	for _, s := range statuses {
		var tp v1alpha1.TrustPolicy
		if err := t.d.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, &tp); err != nil {
			continue
		}
		before := tp.DeepCopy()
		tp.Status.ObservedGeneration = s.Generation
		meta.SetStatusCondition(&tp.Status.Conditions, s.Condition(now))
		if equality.Semantic.DeepEqual(before.Status, tp.Status) {
			continue
		}
		if err := t.d.Client.Status().Patch(ctx, &tp, client.MergeFrom(before)); err != nil {
			t.d.Log.Warn("writing TrustPolicy status failed", "policy", s.Name, "err", err)
		}
	}
	return ctrl.Result{}, nil
}

func newSTS(ctx context.Context, d *Deps) (any, error) {
	if d.Mode == ModeController && d.Config.Listeners.STS == "" {
		return nil, nil // served by the separate STS Deployment
	}
	eng, ok := Shared[*auth.Engine](d, SharedEngine)
	if !ok {
		return nil, fmt.Errorf("the STS needs the trust-policy engine")
	}
	m, ok := Shared[*keys.Manager](d, SharedKeys)
	if !ok {
		return nil, fmt.Errorf("the STS needs the key manager")
	}
	return sts.New(sts.Deps{
		Config:     *d.Config,
		Engine:     eng,
		Keys:       m,
		Clock:      d.Clock,
		Log:        d.Log.With("component", "sts"),
		Audit:      d.Log.With("log", "audit", "component", "sts"),
		Registerer: d.Registry,
	})
}
