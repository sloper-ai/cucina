// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/enroll"
	"github.com/sloper-ai/cucina/internal/hostlink"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/mgmt"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/reconcile"
)

// SharedCost is the key of the cost model (reconcile.CostModel).
const SharedCost = "reconcile.cost"

// ManagementService (agent `mgmt`) for cucinactl, with the controller-side
// views and actions it needs: pools and their history from the autoscaler,
// workers, the scheduler, MacHosts, enrollment, keys and revocations.
// Built after the reconcilers (it reads the Fleet).
func init() {
	RegisterComponent(Factory{Name: "mgmt", Modes: []Mode{ModeController}, Listener: ListenerManagement, Order: 10, New: newMgmt})
}

func newMgmt(ctx context.Context, d *Deps) (any, error) {
	cfg := d.Config
	if cfg.Listeners.Management == "" {
		return nil, nil
	}
	km, ok := Shared[*keys.Manager](d, SharedKeys)
	if !ok {
		return nil, errors.New("the management API needs the key manager (Cucina JWT verification)")
	}
	fleet, ok := d.Fleet.(*reconcile.Fleet)
	if !ok {
		return nil, errors.New("the management API needs the autoscaler runtime")
	}
	views := &mgmtViews{d: d, fleet: fleet}
	deps := mgmt.Deps{
		Authenticator: mgmt.NewTokenAuthenticator(km.Verifier),
		Auditor:       mgmt.NewSlogAuditor(d.Log.With("log", "audit", "component", "mgmt")),
		Pools:         views,
		Workers:       views,
		Scheduler:     d.BuildQueue,
		PoolAdmin:     views,
		Hosts:         views,
		Keys:          &mgmt.KeysAdapter{Store: km},
		Revocations:   &mgmt.KeysAdapter{Store: km},
	}
	if d.Compute != nil {
		deps.Orphans = d.Compute
	}
	if d.HostFleet != nil {
		deps.HostAdmin = views
	}
	if srv, ok := Shared[*enroll.Server](d, SharedEnroll); ok {
		deps.Enrollment = &mgmt.EnrollAdapter{Admin: srv.Admin()}
	}
	if cm, ok := Shared[*reconcile.CostModel](d, SharedCost); ok {
		deps.Cost = costSource{m: cm, f: fleet}
	}
	ring := mgmt.NewLogRing(1 << 20)
	LogTee.Set(ring)
	deps.LogTail = ring
	deps.Support = views
	deps.Images = views
	deps.Components = views
	redacted := *cfg // secret names only; the configuration holds no secret values
	return mgmt.New(deps, mgmt.Options{
		ClusterID:             cfg.ClusterID,
		Version:               Version,
		InstanceNames:         cfg.InstanceNames,
		Config:                redacted,
		DefaultEnrollTokenTTL: cfg.Hosts.DefaultTokenTTL.Duration,
		Logger:                d.Log.With("component", "mgmt"),
	})
}

// costSource prices the leader's recorded usage with the current price table.
type costSource struct {
	m *reconcile.CostModel
	f *reconcile.Fleet
}

func (c costSource) Cost(ctx context.Context, q mgmt.CostQuery) (mgmt.CostReport, error) {
	return (&mgmt.CostAdapter{Model: c.m.Model(), Usage: c.f.Usage}).Cost(ctx, q)
}

// Images implements mgmt.ImageSource: the image each pool launches now (from
// the WorkerPool status) and whether EC2 Fast Launch is enabled on it.
func (v *mgmtViews) Images(ctx context.Context) ([]mgmt.Image, error) {
	var list v1alpha1.WorkerPoolList
	if err := v.d.Client.List(ctx, &list, client.InNamespace(v.d.Config.Namespace)); err != nil {
		return nil, err
	}
	var out []mgmt.Image
	for _, wp := range list.Items {
		if wp.Status.ResolvedImage == "" {
			continue
		}
		fl := "n/a"
		if wp.Spec.EC2 != nil && wp.Spec.EC2.FastLaunch != nil {
			fl = "disabled"
			for _, img := range splitCSV(wp.Annotations[reconcile.AnnFastLaunchImages]) {
				if img == wp.Status.ResolvedImage {
					fl = "enabled"
				}
			}
		}
		out = append(out, mgmt.Image{Pool: wp.Name, Reference: wp.Status.ResolvedImage, Version: wp.Spec.Image.Version,
			Generation: wp.Status.ImageGeneration, Current: true, FastLaunch: fl})
	}
	return out, nil
}

// Components implements mgmt.ComponentSource: readiness of the release's
// Deployments and StatefulSets by app.kubernetes.io/component.
func (v *mgmtViews) Components(ctx context.Context) ([]mgmt.Component, error) {
	sel := client.MatchingLabels{"app.kubernetes.io/instance": v.d.Config.ReleaseName}
	ns := client.InNamespace(v.d.Config.Namespace)
	var out []mgmt.Component
	add := func(name string, ready, desired int32) {
		state := "ready"
		switch {
		case ready == 0 && desired > 0:
			state = "down"
		case ready < desired:
			state = "degraded"
		}
		out = append(out, mgmt.Component{Name: name, State: state, Ready: ready, Desired: desired,
			Message: fmt.Sprintf("%d/%d ready", ready, desired)})
	}
	var deps appsv1.DeploymentList
	if err := v.d.Client.List(ctx, &deps, ns, sel); err != nil {
		return nil, err
	}
	for _, x := range deps.Items {
		desired := int32(1)
		if x.Spec.Replicas != nil {
			desired = *x.Spec.Replicas
		}
		add(componentName(x.Labels, x.Name), x.Status.ReadyReplicas, desired)
	}
	var sts appsv1.StatefulSetList
	if err := v.d.Client.List(ctx, &sts, ns, sel); err != nil {
		return nil, err
	}
	for _, x := range sts.Items {
		desired := int32(1)
		if x.Spec.Replicas != nil {
			desired = *x.Spec.Replicas
		}
		add(componentName(x.Labels, x.Name), x.Status.ReadyReplicas, desired)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func componentName(labels map[string]string, fallback string) string {
	if c := labels["app.kubernetes.io/component"]; c != "" {
		return c
	}
	return fallback
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TrustPolicies implements mgmt.SupportSource.
func (v *mgmtViews) TrustPolicies(ctx context.Context) ([]v1alpha1.TrustPolicy, error) {
	var list v1alpha1.TrustPolicyList
	if err := v.d.Client.List(ctx, &list, client.InNamespace(v.d.Config.Namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// Metrics implements mgmt.SupportSource: the controller registry in the
// Prometheus text format.
func (v *mgmtViews) Metrics(context.Context) ([]byte, error) {
	g, ok := v.d.Registry.(prometheus.Gatherer)
	if !ok {
		return nil, errors.New("metrics registry is not gatherable")
	}
	mfs, err := g.Gather()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, mf := range mfs {
		if _, err := expfmt.MetricFamilyToText(&b, mf); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

// mgmtViews adapts the controller state to the management API's interfaces.
type mgmtViews struct {
	d     *Deps
	fleet *reconcile.Fleet
}

// Pools implements mgmt.PoolView.
func (v *mgmtViews) Pools(ctx context.Context) ([]mgmt.Pool, error) {
	var list v1alpha1.WorkerPoolList
	if err := v.d.Client.List(ctx, &list, client.InNamespace(v.d.Config.Namespace)); err != nil {
		return nil, err
	}
	out := make([]mgmt.Pool, 0, len(list.Items))
	for _, wp := range list.Items {
		p := mgmt.Pool{Resource: wp}
		if r, ok := v.d.Pools.Get(domain.PoolName(wp.Name)); ok {
			p.Spec = r.Spec
		} else {
			p.Spec = domain.PoolSpec{Name: domain.PoolName(wp.Name), Provider: domain.Provider(wp.Spec.Provider), Platform: wp.Spec.Platform,
				MinRunning: int(wp.Spec.Capacity.MinRunning), Max: int(wp.Spec.Capacity.Max), Paused: wp.Spec.Paused}
		}
		if f := reconcile.ReadFloorOverride(&wp); f != nil && v.d.Clock.Now().Before(f.ExpiresAt) {
			p.Floor = &mgmt.FloorOverride{MinRunning: f.MinRunning, ExpiresAt: f.ExpiresAt, SetBy: f.SetBy}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// History implements mgmt.PoolView.
func (v *mgmtViews) History(_ context.Context, pool string, limit int) ([]mgmt.PoolEvent, []mgmt.StartLatency, error) {
	evs, sts := v.fleet.History(domain.PoolName(pool), limit)
	out := make([]mgmt.PoolEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, mgmt.PoolEvent{Time: e.Time, Type: e.Type, Subject: e.Subject, Message: e.Message})
	}
	lat := make([]mgmt.StartLatency, 0, len(sts))
	for _, s := range sts {
		lat = append(lat, mgmt.StartLatency{Pool: string(s.Pool), VM: s.VM, Launched: s.Launched, ToRunning: s.ToRunning,
			ToRegistered: s.ToRegistered, ToFirstAction: s.ToFirstAction, Path: s.Path})
	}
	return out, lat, nil
}

// Workers implements mgmt.WorkerView.
func (v *mgmtViews) Workers(context.Context) ([]mgmt.Worker, error) {
	ws := v.fleet.Workers()
	out := make([]mgmt.Worker, 0, len(ws))
	for _, w := range ws {
		out = append(out, mgmt.Worker{VM: w.VM, PrivateIP: w.PrivateIP})
	}
	return out, nil
}

// SetFloor implements mgmt.PoolAdmin (annotation read by the WorkerPool reconciler).
func (v *mgmtViews) SetFloor(ctx context.Context, pool string, f mgmt.FloorOverride) error {
	return reconcile.SetFloorOverride(ctx, v.d.Client, v.d.Config.Namespace, pool,
		reconcile.FloorOverride{MinRunning: f.MinRunning, ExpiresAt: f.ExpiresAt, SetBy: f.SetBy})
}

// SetPaused implements mgmt.PoolAdmin (spec.paused).
func (v *mgmtViews) SetPaused(ctx context.Context, pool string, paused bool) error {
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"paused": paused}})
	wp := &v1alpha1.WorkerPool{}
	wp.Name, wp.Namespace = pool, v.d.Config.Namespace
	return v.d.Client.Patch(ctx, wp, client.RawPatch(types.MergePatchType, patch))
}

// Hosts implements mgmt.HostView.
func (v *mgmtViews) Hosts(ctx context.Context) ([]v1alpha1.MacHost, error) {
	var list v1alpha1.MacHostList
	if err := v.d.Client.List(ctx, &list, client.InNamespace(v.d.Config.Namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// SetCordon implements mgmt.HostAdmin: the operator's intent goes into the
// MacHost (authoritative for placement at once) and is pushed to hostd now
// (the MacHost reconciler re-applies it if the host was offline).
func (v *mgmtViews) SetCordon(ctx context.Context, serial string, cordoned bool) error {
	mh, err := findMacHost(ctx, v.d.Client, v.d.Config.Namespace, serial)
	if err != nil {
		return err
	}
	if mh == nil {
		return fmt.Errorf("host %s is unknown: %w", serial, ports.ErrNotFound)
	}
	before := mh.DeepCopy()
	mh.Spec.Cordoned = cordoned
	if err := v.d.Client.Patch(ctx, mh, client.MergeFrom(before)); err != nil {
		return err
	}
	if err := v.d.HostFleet.SetCordon(ctx, mh.Spec.Serial, cordoned); err != nil {
		v.d.Log.Info("host cordon recorded; hostd will apply it when it reconnects", "serial", mh.Spec.Serial, "err", err)
	}
	return nil
}

// Reimage implements mgmt.HostAdmin: re-clone a VM ("" = every VM of the
// host) from its pool's current image.
func (v *mgmtViews) Reimage(ctx context.Context, serial, vm string) error {
	hosts, err := v.d.HostFleet.Hosts(ctx)
	if err != nil {
		return err
	}
	images := map[domain.PoolName]string{}
	var list v1alpha1.WorkerPoolList
	if err := v.d.Client.List(ctx, &list, client.InNamespace(v.d.Config.Namespace)); err != nil {
		return err
	}
	for _, wp := range list.Items {
		images[domain.PoolName(wp.Name)] = wp.Spec.Image.Reference
	}
	var errs []error
	found := false
	for _, h := range hosts {
		if h.Serial != serial {
			continue
		}
		for _, m := range h.VMs {
			name := reconcileVMName(m.ID)
			if vm != "" && name != vm {
				continue
			}
			found = true
			if err := v.d.HostFleet.ReimageVM(ctx, h.Serial, name, images[m.Pool]); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if !found {
		return fmt.Errorf("no VM %q on host %s", vm, serial)
	}
	return errors.Join(errs...)
}

// Diagnostics implements mgmt.HostAdmin through the host stream.
func (v *mgmtViews) Diagnostics(ctx context.Context, serial string, req mgmt.DiagnosticsRequest) (io.ReadCloser, error) {
	hl, ok := v.d.HostFleet.(*hostlink.Server)
	if !ok {
		return nil, errors.New("host diagnostics need the host stream")
	}
	b, err := hl.CollectDiagnostics(ctx, serial, req.IncludeVMLogs)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func reconcileVMName(node string) string {
	for i := len(node) - 1; i >= 0; i-- {
		if node[i] == '/' {
			return node[i+1:]
		}
	}
	return node
}
