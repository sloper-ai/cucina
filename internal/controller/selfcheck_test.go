// SPDX-License-Identifier: FSL-1.1-ALv2

package controller_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/controller"
	"github.com/sloper-ai/cucina/internal/fakes"
)

// Guards R-TEST-7 "AWS/RBAC permission self-checks at startup": the controller
// refuses to start with an actionable message when EC2 rejects the
// tag-filtered Describe or when its ServiceAccount lacks a permission, and the
// documented Role (PolicyRules, docs/dev/controller.md) is exactly sufficient.
func TestSelfChecksFailFast(t *testing.T) {
	ctx := context.Background()
	clock := fakes.NewClock(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	compute := fakes.NewCompute(clock, fakes.NewRand(1), fakes.DefaultComputeConfig())
	cfg := &config.Controller{ClusterID: "test", AWS: &config.AWS{Region: "us-west-1"}}
	require.NoError(t, controller.CheckAWS(ctx, compute, cfg))
	compute.FailNext("Describe", errors.New("UnauthorizedOperation: not authorized to perform ec2:DescribeInstances"))
	err := controller.CheckAWS(ctx, compute, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cucina:cluster=test")
	assert.Contains(t, err.Error(), "IAM policy")

	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set: the RBAC half needs envtest (etcd, kube-apiserver v1.36)")
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	restCfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })
	scheme, err := controller.NewScheme()
	require.NoError(t, err)
	admin, err := client.New(restCfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	const ns = "cucina"
	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	user, err := env.AddUser(envtest.User{Name: "system:serviceaccount:cucina:cucina-controller"}, nil)
	require.NoError(t, err)
	uc := user.Config()
	uc.QPS, uc.Burst = 50, 100 // as the controller's self-check client
	sa, err := client.New(uc, client.Options{Scheme: scheme})
	require.NoError(t, err)

	rules := controller.RequiredRules(controller.ModeController)
	err = controller.CheckRBAC(ctx, sa, ns, rules)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "update cucina.sloper.ai/workerpools/finalizers")

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "cucina-controller", Namespace: ns}, Rules: controller.PolicyRules(controller.ModeController)}
	require.NoError(t, admin.Create(ctx, role))
	require.NoError(t, admin.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cucina-controller", Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "cucina-controller"},
		Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "system:serviceaccount:cucina:cucina-controller"}},
	}))
	assert.NoError(t, controller.CheckRBAC(ctx, sa, ns, rules), "the documented Role grants everything the controller checks")
}
