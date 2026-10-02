// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/api/v1alpha1"
)

func i32p(v int32) *int32 { return &v }

// Guards R-TEST-7 (CRD CEL validation fails fast) and R-POOL-1: the API
// server itself rejects inconsistent WorkerPool/MacHost objects with a precise
// message, including the provider/ec2/tart rules and immutable fields.
func TestCRDValidation(t *testing.T) {
	c := apiServer(t)
	ns := namespace(t, c, "crd-validation")
	ctx := context.Background()

	tartPool := func(name string) *v1alpha1.WorkerPool {
		return &v1alpha1.WorkerPool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.WorkerPoolSpec{
			Platform: "macos-arm64-xcode27.0", Provider: "tart", Capacity: v1alpha1.CapacitySpec{Max: 2},
			Image: v1alpha1.ImageSpec{Reference: "ghcr.io/sloper-ai/cucina-worker-macos:27.0-0.1.0"},
			Tart:  &v1alpha1.TartSpec{VMsPerHost: 2},
		}}
	}
	ec2Pool := func(name string) *v1alpha1.WorkerPool {
		wp := linuxPool(name, 4)
		wp.Namespace, wp.Generation = ns, 0
		return wp
	}
	cases := []struct {
		name string
		obj  client.Object
		want string // "" = accepted
	}{
		{"valid ec2 pool", ec2Pool("ok-ec2"), ""},
		{"valid tart pool", tartPool("ok-tart"), ""},
		{"ec2 without ec2 settings", func() client.Object { p := ec2Pool("no-ec2"); p.Spec.EC2 = nil; return p }(), "spec.ec2 is required for provider ec2"},
		{"tart without tart settings", func() client.Object { p := tartPool("no-tart"); p.Spec.Tart = nil; return p }(), "spec.tart is required for provider tart"},
		{"ec2 with tart settings", func() client.Object { p := ec2Pool("both"); p.Spec.Tart = &v1alpha1.TartSpec{VMsPerHost: 1}; return p }(), "spec.tart is only valid for provider tart"},
		{"tart with ec2 settings", func() client.Object { p := tartPool("both2"); p.Spec.EC2 = ec2Pool("x").Spec.EC2; return p }(), "spec.ec2 is only valid for provider ec2"},
		{"minRunning above max", func() client.Object { p := ec2Pool("floor"); p.Spec.Capacity.MinRunning = 5; return p }(), "capacity.minRunning must not exceed capacity.max"},
		{"unknown provider", func() client.Object { p := ec2Pool("gce"); p.Spec.Provider = "gce"; return p }(), `Unsupported value: "gce"`},
		{"three VMs per host", func() client.Object { p := tartPool("three"); p.Spec.Tart.VMsPerHost = 3; return p }(), "spec.tart.vmsPerHost"},
		{"no instance types", func() client.Object { p := ec2Pool("notypes"); p.Spec.EC2.InstanceTypes = nil; return p }(), "spec.ec2.instanceTypes"},
		{"host with three slots", &v1alpha1.MacHost{ObjectMeta: metav1.ObjectMeta{Name: "h3", Namespace: ns}, Spec: v1alpha1.MacHostSpec{Serial: "C02XYZ123", Slots: i32p(3)}}, "spec.slots"},
		{"host serial too short", &v1alpha1.MacHost{ObjectMeta: metav1.ObjectMeta{Name: "h4", Namespace: ns}, Spec: v1alpha1.MacHostSpec{Serial: "C02"}}, "spec.serial"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Create(ctx, tc.obj)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	// Immutable fields (CEL transition rules).
	immutable := []struct {
		name   string
		mutate func(*v1alpha1.WorkerPool)
	}{
		{"platform", func(p *v1alpha1.WorkerPool) { p.Spec.Platform = "linux-aarch64" }},
		{"sizeClass", func(p *v1alpha1.WorkerPool) { p.Spec.SizeClass = "large" }},
	}
	for _, tc := range immutable {
		t.Run("immutable "+tc.name, func(t *testing.T) {
			var p v1alpha1.WorkerPool
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "ok-ec2"}, &p))
			tc.mutate(&p)
			err := c.Update(ctx, &p)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "platform, provider and sizeClass are immutable")
		})
	}
	t.Run("mutable max", func(t *testing.T) {
		var p v1alpha1.WorkerPool
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "ok-ec2"}, &p))
		p.Spec.Capacity.Max = 8
		require.NoError(t, c.Update(ctx, &p), "UC11: pool changes apply without recreating it")
	})
	t.Run("immutable serial", func(t *testing.T) {
		h := &v1alpha1.MacHost{ObjectMeta: metav1.ObjectMeta{Name: "h1", Namespace: ns}, Spec: v1alpha1.MacHostSpec{Serial: "C02ABC123"}}
		require.NoError(t, c.Create(ctx, h))
		h.Spec.Serial = "C02DEF456"
		err := c.Update(ctx, h)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "serial is immutable")
	})
}
