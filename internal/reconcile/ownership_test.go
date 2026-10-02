// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Guards the field ownership of user objects (found by the kind smoke test:
// `helm upgrade`, a server-side apply by Helm 4, failed with `conflict with
// "cucina-controller" ... .spec.tart.maxAge` after the reconciler had added
// its finalizer with a full-object Update that rewrote "168h" as "168h0m0s").
// The reconcilers write only metadata and status, so the user's field manager
// keeps the whole spec and re-applies without conflicts.
func TestReconcilersNeverOwnSpec(t *testing.T) {
	c := apiServer(t)
	ns := namespace(t, c, "ownership")
	h := newHarness(t, 41, kubeOpt{c: c, ns: ns})
	ctx := context.Background()
	const serial = "C02HOST0002"
	h.hosts.AddHost(ports.HostState{Serial: serial, Name: "mini-2", Online: true, Approved: true, Slots: 2, Labels: map[string]string{"site": "lab"}})

	pool := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cucina.sloper.ai/v1alpha1", "kind": "WorkerPool",
		"metadata": map[string]any{"name": "macos", "namespace": ns},
		"spec": map[string]any{
			"platform": "macos-arm64-xcode27.0", "provider": "tart",
			"capacity": map[string]any{"minRunning": int64(0), "max": int64(2)},
			"image":    map[string]any{"reference": "ghcr.io/sloper-ai/cucina-worker-macos:27.0-0.1.0"},
			"tart":     map[string]any{"vmsPerHost": int64(2), "diskGiB": int64(120), "maxAge": "168h"},
		},
	}}
	host := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cucina.sloper.ai/v1alpha1", "kind": "MacHost",
		"metadata": map[string]any{"name": "mini-2", "namespace": ns},
		"spec":     map[string]any{"serial": serial, "approved": true, "labels": map[string]any{"site": "lab"}},
	}}
	// As Helm 4 applies its manifests: server-side apply as "helm", without force.
	apply := func(u *unstructured.Unstructured) error {
		return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u.DeepCopy()), client.FieldOwner("helm"))
	}
	require.NoError(t, apply(pool))
	require.NoError(t, apply(host))

	h.reconcilePool("macos")
	_, err := h.comps.MacHost.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "mini-2"}})
	require.NoError(t, err)

	var wp v1alpha1.WorkerPool
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "macos"}, &wp))
	assert.True(t, controllerutil.ContainsFinalizer(&wp, v1alpha1.Finalizer), "pool finalizer added")
	var mh v1alpha1.MacHost
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "mini-2"}, &mh))
	assert.True(t, controllerutil.ContainsFinalizer(&mh, v1alpha1.Finalizer), "host finalizer added")
	for kind, obj := range map[string]client.Object{"WorkerPool": &wp, "MacHost": &mh} {
		for _, mf := range obj.GetManagedFields() {
			if mf.Manager == "helm" || mf.Subresource == "status" || mf.FieldsV1 == nil {
				continue
			}
			fields := mf.FieldsV1.GetRawString()
			assert.NotContains(t, fields, `"f:spec"`, "%s: manager %q (%s) owns spec fields: %s", kind, mf.Manager, mf.Operation, fields)
		}
	}
	assert.Equal(t, "168h", rawMaxAge(t, c, ns), "the user's spec stays as written")

	require.NoError(t, apply(pool), "re-apply of the pool by its field manager conflicts")
	require.NoError(t, apply(host), "re-apply of the host by its field manager conflicts")
}

func rawMaxAge(t *testing.T, c client.Client, ns string) string {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("cucina.sloper.ai/v1alpha1")
	u.SetKind("WorkerPool")
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "macos"}, u))
	v, _, _ := unstructured.NestedString(u.Object, "spec", "tart", "maxAge")
	return v
}
