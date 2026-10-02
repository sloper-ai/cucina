// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/metrics"
	"github.com/sloper-ai/cucina/internal/ports"
)

const (
	// pullRetryAfter rate-limits repeated pre-pull requests per host and image.
	pullRetryAfter = 10 * time.Minute
	// hostStopTimeout bounds a graceful VM shutdown when a host is removed.
	hostStopTimeout = 2 * time.Minute
)

// MacHostReconciler mirrors hostd reports into MacHost status, pushes the
// operator's cordon to hostd, detects offline hosts (Hosts.StaleAfter), asks
// hosts to pre-pull desired images, and stops a removed host's VMs before its
// finalizer goes (R-MAC-6, R-OPS-3). Leader-only.
type MacHostReconciler struct {
	Client    client.Client
	HostFleet ports.HostFleet
	Config    *config.Controller
	Clock     ports.Clock
	Metrics   *metrics.Metrics
	Log       *slog.Logger
	// PoolImages returns the Tart images of the pools whose host selector
	// matches the given labels (pre-pulled like spec.desiredImages, R-MAC-5).
	PoolImages func(labels map[string]string) []string

	mu     sync.Mutex
	pulled map[string]time.Time // serial|image → last pull request
}

// Reconcile implements reconcile.Reconciler.
func (r *MacHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var mh v1alpha1.MacHost
	if err := r.Client.Get(ctx, req.NamespacedName, &mh); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.exportHostMetrics(ctx, req.Namespace)
		}
		return ctrl.Result{}, err
	}
	deleting := !mh.DeletionTimestamp.IsZero()
	if !deleting && !controllerutil.ContainsFinalizer(&mh, v1alpha1.Finalizer) {
		controllerutil.AddFinalizer(&mh, v1alpha1.Finalizer)
		if err := r.Client.Update(ctx, &mh); err != nil {
			return ctrl.Result{}, err
		}
	}

	host, found, err := r.host(ctx, mh.Spec.Serial)
	if err != nil {
		r.Log.Warn("listing hosts failed", "err", err)
	}
	now := r.Clock.Now()

	if deleting {
		if !controllerutil.ContainsFinalizer(&mh, v1alpha1.Finalizer) {
			return ctrl.Result{}, nil
		}
		if found && r.online(host, now) && host.RunningVMs > 0 {
			for _, vm := range host.VMs {
				if occupiesSlot(vm.State) {
					if err := r.HostFleet.StopVM(ctx, host.Serial, vmName(vm.ID), hostStopTimeout, "host-removed"); err != nil {
						r.Log.Warn("stopping VM of a removed host failed", "serial", host.Serial, "vm", vm.ID, "err", err)
					}
				}
			}
			return ctrl.Result{RequeueAfter: finalizeRequeue}, nil
		}
		// Stopped, or offline: an offline host's VMs power themselves off (R-POOL-7).
		before := mh.DeepCopy()
		controllerutil.RemoveFinalizer(&mh, v1alpha1.Finalizer)
		if err := r.Client.Patch(ctx, &mh, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.exportHostMetrics(ctx, mh.Namespace)
	}

	if found && r.online(host, now) {
		if host.Cordoned != mh.Spec.Cordoned {
			if err := r.HostFleet.SetCordon(ctx, host.Serial, mh.Spec.Cordoned); err != nil {
				r.Log.Warn("setting cordon failed", "serial", host.Serial, "err", err)
			}
		}
		if mh.Spec.Approved && !mh.Spec.Cordoned {
			images := slices.Clone(mh.Spec.DesiredImages)
			if r.PoolImages != nil {
				images = append(images, r.PoolImages(mh.Spec.Labels)...)
			}
			r.prePull(ctx, host, images, now)
		}
	}

	status := HostStatus(&mh, host, found, r.Config.Hosts.StaleAfter.Duration, now)
	if !equality.Semantic.DeepEqual(status, mh.Status) {
		before := mh.DeepCopy()
		mh.Status = status
		patch, err := statusPatch(before, &mh, macHostStatusRequired...)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Client.Status().Patch(ctx, &mh, patch); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	if err := r.exportHostMetrics(ctx, mh.Namespace); err != nil {
		return ctrl.Result{}, err
	}
	// Re-check well within StaleAfter so offline hosts are marked promptly.
	return ctrl.Result{RequeueAfter: max(r.Config.Hosts.StaleAfter.Duration/4, 5*time.Second)}, nil
}

func (r *MacHostReconciler) host(ctx context.Context, serial string) (ports.HostState, bool, error) {
	if r.HostFleet == nil {
		return ports.HostState{}, false, nil
	}
	hosts, err := r.HostFleet.Hosts(ctx)
	if err != nil {
		return ports.HostState{}, false, err
	}
	for _, h := range hosts {
		if h.Serial == serial {
			return h, true, nil
		}
	}
	return ports.HostState{}, false, nil
}

func (r *MacHostReconciler) online(h ports.HostState, now time.Time) bool {
	return h.Online && (h.LastSeen.IsZero() || now.Sub(h.LastSeen) <= r.Config.Hosts.StaleAfter.Duration)
}

func (r *MacHostReconciler) prePull(ctx context.Context, h ports.HostState, images []string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pulled == nil {
		r.pulled = map[string]time.Time{}
	}
	slices.Sort(images)
	for _, img := range slices.Compact(images) {
		if img == "" || slices.Contains(h.Images, img) {
			continue
		}
		key := h.Serial + "|" + img
		if at, ok := r.pulled[key]; ok && now.Sub(at) < pullRetryAfter {
			continue
		}
		r.pulled[key] = now
		if err := r.HostFleet.PullImage(ctx, h.Serial, img); err != nil {
			r.Log.Warn("pre-pull request failed", "serial", h.Serial, "image", img, "err", err)
		}
	}
}

// exportHostMetrics sets cucina_hosts{phase} and cucina_host_heartbeat_age_seconds{serial}.
func (r *MacHostReconciler) exportHostMetrics(ctx context.Context, namespace string) error {
	if r.Metrics == nil {
		return nil
	}
	var list v1alpha1.MacHostList
	if err := r.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return err
	}
	counts := map[string]int{}
	for _, p := range []string{v1alpha1.MacHostPending, v1alpha1.MacHostOnline, v1alpha1.MacHostOffline, v1alpha1.MacHostDraining, v1alpha1.MacHostCordoned, v1alpha1.MacHostDenied} {
		counts[p] = 0
	}
	now := r.Clock.Now()
	r.Metrics.HostHeartbeatAgeSeconds.Reset()
	for _, h := range list.Items {
		if h.Status.Phase != "" {
			counts[h.Status.Phase]++
		}
		if hb := h.Status.LastHeartbeat; hb != nil {
			r.Metrics.HostHeartbeatAgeSeconds.WithLabelValues(h.Spec.Serial).Set(now.Sub(hb.Time).Seconds())
		}
	}
	for p, n := range counts {
		r.Metrics.Hosts.WithLabelValues(p).Set(float64(n))
	}
	return nil
}

// HostStatus computes a MacHost status from hostd's report. Fields written by
// other components (facts other than the agent version, L2, certificate
// expiry) are preserved.
func HostStatus(mh *v1alpha1.MacHost, h ports.HostState, found bool, staleAfter time.Duration, now time.Time) v1alpha1.MacHostStatus {
	st := *mh.Status.DeepCopy()
	if found {
		if !h.LastSeen.IsZero() {
			hb := metav1.NewTime(h.LastSeen)
			st.LastHeartbeat = &hb
		}
		if h.AgentVersion != "" {
			st.Facts.AgentVersion = h.AgentVersion
		}
		st.RunningVMs = int32(h.RunningVMs)
		st.VMs = nil
		for _, vm := range h.VMs {
			st.VMs = append(st.VMs, v1alpha1.HostVMStatus{
				Name: vmName(vm.ID), Pool: string(vm.Pool), State: hostVMState(vm.State),
				Generation: vm.Generation, Registered: !vm.RegisteredAt.IsZero() && vm.State != domain.VMStopped,
			})
		}
		sort.Slice(st.VMs, func(i, j int) bool { return st.VMs[i].Name < st.VMs[j].Name })
		st.Images = slices.Sorted(slices.Values(h.Images))
	}
	online := found && h.Online && (h.LastSeen.IsZero() || now.Sub(h.LastSeen) <= staleAfter)
	switch {
	case !mh.Spec.Approved:
		st.Phase = v1alpha1.MacHostPending
	case !found:
		st.Phase = v1alpha1.MacHostPending
	case !online:
		st.Phase = v1alpha1.MacHostOffline
	case mh.Spec.Cordoned && h.RunningVMs > 0:
		st.Phase = v1alpha1.MacHostDraining
	case mh.Spec.Cordoned:
		st.Phase = v1alpha1.MacHostCordoned
	default:
		st.Phase = v1alpha1.MacHostOnline
	}
	ready := st.Phase == v1alpha1.MacHostOnline
	msg := map[string]string{
		v1alpha1.MacHostPending:  "waiting for approval (cucinactl hosts approve) or for hostd to connect",
		v1alpha1.MacHostOffline:  fmt.Sprintf("no heartbeat for more than %s: unavailable, in-flight actions are retried elsewhere", staleAfter),
		v1alpha1.MacHostDraining: "cordoned: VMs shut down when idle",
		v1alpha1.MacHostCordoned: "cordoned and idle (maintenance)",
		v1alpha1.MacHostOnline:   fmt.Sprintf("%d VM(s) running", h.RunningVMs),
	}[st.Phase]
	s := metav1.ConditionFalse
	if ready {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: v1alpha1.ConditionReady, Status: s, Reason: st.Phase, Message: msg, ObservedGeneration: mh.Generation, LastTransitionTime: metav1.NewTime(now)})
	return st
}

func hostVMState(s domain.VMState) string {
	switch s {
	case domain.VMLaunching:
		return "starting"
	case domain.VMRegistered, domain.VMDraining:
		return "running"
	case domain.VMStopping:
		return "stopping"
	case domain.VMStopped:
		return "stopped"
	case domain.VMFailed:
		return "failed"
	default:
		return string(s)
	}
}
