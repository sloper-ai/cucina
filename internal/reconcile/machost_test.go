// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Guards R-MAC-6 and R-OPS-3 for hosts: MacHost status mirrors hostd's
// reports, the operator's cordon reaches the host (drain → cordoned), a host
// without heartbeat for Hosts.StaleAfter is marked Offline, and removing a host
// stops its VMs before the finalizer goes.
func TestMacHostReconcile(t *testing.T) {
	c := apiServer(t)
	ns := namespace(t, c, "machost")
	h := newHarness(t, 31, kubeOpt{c: c, ns: ns})
	ctx := context.Background()
	const serial = "C02HOST0001"
	h.hosts.AddHost(ports.HostState{Serial: serial, Name: "mini-1", Online: true, Approved: true, Slots: 2, Labels: map[string]string{"site": "lab"}})

	mh := &v1alpha1.MacHost{ObjectMeta: metav1.ObjectMeta{Name: "mini-1", Namespace: ns}, Spec: v1alpha1.MacHostSpec{Serial: serial, Approved: true, Labels: map[string]string{"site": "lab"}}}
	require.NoError(t, c.Create(ctx, mh))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "mini-1"}}
	reconcileHost := func() {
		t.Helper()
		_, err := h.comps.MacHost.Reconcile(ctx, req)
		require.NoError(t, err)
	}
	phase := func() string {
		t.Helper()
		var got v1alpha1.MacHost
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(mh), &got))
		return got.Status.Phase
	}

	reconcileHost()
	assert.Equal(t, v1alpha1.MacHostOnline, phase())

	// A VM of some pool runs on the host; cordon → hostd is told, host drains.
	require.NoError(t, h.hosts.StartVM(ctx, serial, ports.StartVMRequest{Pool: "macos", Generation: "27.0-1", Image: "ghcr.io/x/macos:27.0-1", VMName: "macos-1"}))
	var got v1alpha1.MacHost
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(mh), &got))
	got.Spec.Cordoned = true
	require.NoError(t, c.Update(ctx, &got))
	reconcileHost()
	hosts, err := h.hosts.Hosts(ctx)
	require.NoError(t, err)
	assert.True(t, hosts[0].Cordoned, "cordon propagated to hostd")
	assert.Equal(t, v1alpha1.MacHostDraining, phase())

	// The host disconnects; after StaleAfter it is Offline.
	h.hosts.SetOnline(serial, false)
	h.clock.Advance(h.cfg.Hosts.StaleAfter.Duration + time.Second)
	reconcileHost()
	assert.Equal(t, v1alpha1.MacHostOffline, phase())
	assert.Equal(t, 1.0, testutil.ToFloat64(h.metrics.Hosts.WithLabelValues(v1alpha1.MacHostOffline)))

	// Back online and removed: its VMs are stopped before the finalizer goes.
	h.hosts.SetOnline(serial, true)
	require.NoError(t, c.Delete(ctx, mh))
	for i := 0; i < 300; i++ {
		h.clock.Advance(time.Second)
		h.hosts.Tick()
		reconcileHost()
		if err := c.Get(ctx, client.ObjectKeyFromObject(mh), &got); apierrors.IsNotFound(err) {
			break
		}
	}
	err = c.Get(ctx, client.ObjectKeyFromObject(mh), &got)
	assert.True(t, apierrors.IsNotFound(err), "finalizer removed, got %v", err)
	hosts, err = h.hosts.Hosts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, hosts[0].RunningVMs, "VMs stopped before the host was released")
}
