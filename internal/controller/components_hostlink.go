// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/enroll"
	"github.com/sloper-ai/cucina/internal/hostlink"
	"github.com/sloper-ai/cucina/internal/pki"
	cucinaproto "github.com/sloper-ai/cucina/internal/proto"
)

// Mac host stream (agent `hostd`): HostService on the host listener (mTLS)
// and the ports.HostFleet the reconcilers drive. Built before the reconcilers
// (Order < 0) because it is their HostFleet.
func init() {
	RegisterComponent(Factory{Name: "hostlink", Modes: []Mode{ModeController}, Listener: ListenerHost, Order: -10, New: newHostlink})
}

func newHostlink(ctx context.Context, d *Deps) (any, error) {
	cfg := d.Config
	if cfg.Listeners.Host == "" {
		return nil, nil // no Mac hosts in this installation
	}
	certs, ok := Shared[*enroll.Server](d, SharedEnroll)
	if !ok {
		return nil, errors.New("the host stream needs the enrollment server (certificate renewal and VM identities)")
	}
	deps := hostlink.Deps{
		Certs:      certs,
		Auth:       macHostAuth{c: d.Client, ns: cfg.Namespace},
		Welcome:    welcome(d),
		Clock:      d.Clock,
		Log:        d.Log.With("component", "hostlink"),
		StaleAfter: cfg.Hosts.StaleAfter.Duration,
	}
	if cfg.Registry != nil && cfg.Hosts.RegistrySecret != "" {
		deps.Registry = registrySecret{c: d.Client, ns: cfg.Namespace, name: cfg.Hosts.RegistrySecret, host: cfg.Registry.Host, d: d}
	}
	srv, err := hostlink.New(deps)
	if err != nil {
		return nil, err
	}
	if d.Registry != nil {
		if err := d.Registry.Register(srv); err != nil {
			return nil, fmt.Errorf("host aggregate metrics: %w", err)
		}
	}
	if d.Manager != nil {
		handler := srv.MetricsHandler(cfg.Namespace)
		for _, path := range []string{hostlink.HostMetricsDiscoveryPath, hostlink.HostMetricsPath} {
			if err := d.Manager.AddMetricsServerExtraHandler(path, handler); err != nil {
				return nil, fmt.Errorf("host metrics route %s: %w", path, err)
			}
		}
	}
	d.HostFleet = srv
	return srv, nil
}

// macHostAuth admits a host only when a MacHost with its serial exists, is
// approved and not denied (R-SEC-3).
type macHostAuth struct {
	c  client.Client
	ns string
}

func (a macHostAuth) Allowed(ctx context.Context, serial string) error {
	mh, err := findMacHost(ctx, a.c, a.ns, serial)
	if err != nil {
		return err
	}
	switch {
	case mh == nil:
		return fmt.Errorf("host %s is not registered (cucinactl hosts register/approve)", serial)
	case !mh.Spec.Approved:
		return fmt.Errorf("host %s is not approved (cucinactl hosts approve %s)", serial, serial)
	case mh.Status.Phase == v1alpha1.MacHostDenied:
		return fmt.Errorf("host %s is denied", serial)
	}
	return nil
}

func findMacHost(ctx context.Context, c client.Client, ns, serial string) (*v1alpha1.MacHost, error) {
	want, err := pki.CanonicalSerial(serial)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.MacHostList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if s, err := pki.CanonicalSerial(list.Items[i].Spec.Serial); err == nil && s == want {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// welcome builds the Welcome a host receives at every connect.
func welcome(d *Deps) hostlink.WelcomeProvider {
	cfg := d.Config
	storage, scheduler := cfg.Endpoints.HostStorage, cfg.Endpoints.HostScheduler
	if storage == "" {
		storage = cfg.Endpoints.WorkerStorage
	}
	if scheduler == "" {
		scheduler = cfg.Endpoints.WorkerScheduler
	}
	return func(serial string) *cucinav1.Welcome {
		w := &cucinav1.Welcome{
			Protocol:          &cucinav1.ProtocolVersion{Major: cucinaproto.Major, Minor: cucinaproto.Minor},
			ClusterId:         cfg.ClusterID,
			HeartbeatInterval: durationpb.New(max(cfg.Hosts.StaleAfter.Duration/4, 5*time.Second)),
			Settings: &cucinav1.HostSettings{
				LogLevel:                cfg.Observability.LogLevel,
				CentralEndpoint:         storage,
				SchedulerEndpoint:       scheduler,
				MaximumMessageSizeBytes: cfg.Worker.MaximumMessageSizeBytes,
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if mh, err := findMacHost(ctx, d.Client, cfg.Namespace, serial); err == nil && mh != nil {
			w.DesiredImages = mh.Spec.DesiredImages
			if mh.Spec.Slots != nil {
				w.Slots = uint32(*mh.Spec.Slots)
			}
		}
		return w
	}
}

// registrySecret hands hosts the read-only package credential from a
// kubernetes.io/basic-auth Secret (username, password), re-read per request so
// that a rotated credential needs no restart (R-OPS-7).
type registrySecret struct {
	c              client.Client
	ns, name, host string
	d              *Deps
}

func (r registrySecret) Credentials(ctx context.Context, _, _ string) (*cucinav1.GetRegistryCredentialsResponse, error) {
	var sec corev1.Secret
	if err := r.c.Get(ctx, client.ObjectKey{Namespace: r.ns, Name: r.name}, &sec); err != nil {
		return nil, fmt.Errorf("registry credential Secret %s: %w", r.name, err)
	}
	return hostlink.StaticRegistry{Host: r.host, Username: string(sec.Data[corev1.BasicAuthUsernameKey]),
		Password: string(sec.Data[corev1.BasicAuthPasswordKey]), Clock: r.d.Clock}.Credentials(ctx, "", "")
}
