// SPDX-License-Identifier: FSL-1.1-ALv2

package controller_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/sloper-ai/cucina/api/crds"
	"github.com/sloper-ai/cucina/internal/controller"
)

var wantCRDs = []string{"machosts.cucina.sloper.ai", "trustpolicies.cucina.sloper.ai", "workerpools.cucina.sloper.ai"}

// Guards R-OPS-1 "helm upgrade upgrades the CRDs" (ADR 0406, found by the kind
// smoke test): the embedded api/crds parse strictly into exactly Cucina's CRDs,
// and anything else in the bundle is an error, not silently applied.
func TestParseCRDs(t *testing.T) {
	objs, err := controller.ParseCRDs(crds.FS)
	require.NoError(t, err)
	assert.Equal(t, wantCRDs, controller.CRDNames(objs))

	_, err = controller.ParseCRDs(fstest.MapFS{"x.yaml": {Data: []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n")}})
	require.ErrorContains(t, err, "x.yaml")
	_, err = controller.ParseCRDs(fstest.MapFS{"x.yaml": {Data: []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: ''}\n")}})
	require.ErrorContains(t, err, "name")
	_, err = controller.ParseCRDs(fstest.MapFS{})
	require.Error(t, err, "an empty bundle must not pass as success")
}

// Guards R-OPS-1 and the chart's CRD hook (ADR 0406): `crds apply`, running as
// the hook ServiceAccount with only controller.CRDApplyRules, upgrades a CRD
// that Helm created from an older crds/ (forcing ownership of Helm's fields),
// creates missing ones, waits until they are Established, is idempotent, and
// cannot touch anyone else's CRD.
func TestApplyCRDs(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set: needs envtest (etcd, kube-apiserver v1.36)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	restCfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })
	admin, err := client.New(restCfg, client.Options{})
	require.NoError(t, err)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	objs, err := controller.ParseCRDs(crds.FS)
	require.NoError(t, err)

	// What `helm install` left behind from an older chart: the WorkerPool CRD
	// with a different schema description, created (not applied) by "helm".
	var old *unstructured.Unstructured
	for _, o := range objs {
		if o.GetName() == "workerpools.cucina.sloper.ai" {
			old = o.DeepCopy()
		}
	}
	require.NotNil(t, old)
	versions, _, _ := unstructured.NestedSlice(old.Object, "spec", "versions")
	require.NoError(t, unstructured.SetNestedField(versions[0].(map[string]any), "older schema", "schema", "openAPIV3Schema", "description"))
	require.NoError(t, unstructured.SetNestedSlice(old.Object, versions, "spec", "versions"))
	require.NoError(t, admin.Create(ctx, old, client.FieldOwner("helm")))

	const sa = "system:serviceaccount:cucina:cucina-hooks"
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "cucina-crds-cucina"}, Rules: controller.CRDApplyRules(controller.CRDNames(objs))}
	require.NoError(t, admin.Create(ctx, role))
	require.NoError(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cucina-crds-cucina"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: sa}},
	}))
	user, err := env.AddUser(envtest.User{Name: sa}, nil)
	require.NoError(t, err)
	hook, err := client.New(user.Config(), client.Options{})
	require.NoError(t, err)
	// RBAC takes effect once the API server's authorizer has synced the binding.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		ssar := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb: "patch", Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions", Name: "workerpools.cucina.sloper.ai"}}}
		if assert.NoError(c, hook.Create(ctx, ssar)) {
			assert.True(c, ssar.Status.Allowed)
		}
	}, 15*time.Second, 100*time.Millisecond)

	for range 2 { // the second run is the idempotent `helm upgrade` case
		objs, err := controller.ParseCRDs(crds.FS) // ApplyCRDs may mutate its input
		require.NoError(t, err)
		require.NoError(t, controller.ApplyCRDs(ctx, hook, objs, log))
		for _, name := range wantCRDs {
			got := &unstructured.Unstructured{}
			got.SetAPIVersion("apiextensions.k8s.io/v1")
			got.SetKind("CustomResourceDefinition")
			require.NoError(t, admin.Get(ctx, client.ObjectKey{Name: name}, got))
			conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
			established := false
			for _, c := range conds {
				m := c.(map[string]any)
				established = established || (m["type"] == "Established" && m["status"] == "True")
			}
			assert.True(t, established, "%s is not Established", name)
			if name == "workerpools.cucina.sloper.ai" {
				vs, _, _ := unstructured.NestedSlice(got.Object, "spec", "versions")
				desc, _, _ := unstructured.NestedString(vs[0].(map[string]any), "schema", "openAPIV3Schema", "description")
				assert.NotEqual(t, "older schema", desc, "the CRD Helm created must be upgraded")
			}
		}
	}

	foreign := &unstructured.Unstructured{}
	foreign.SetAPIVersion("apiextensions.k8s.io/v1")
	foreign.SetKind("CustomResourceDefinition")
	foreign.SetName("widgets.example.com")
	err = hook.Apply(ctx, client.ApplyConfigurationFromUnstructured(foreign), client.FieldOwner("cucina-controller"), client.ForceOwnership)
	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err), "the hook must not be able to touch other CRDs: %v", err)
}
