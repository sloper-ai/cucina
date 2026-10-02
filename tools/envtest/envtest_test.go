// SPDX-License-Identifier: FSL-1.1-ALv2

package envtest_test

import (
	"testing"

	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestControlPlaneStarts guards R-BUILD-1 ("Kubernetes test assets") and
// R-BUILD-5: the pinned envtest-v1.36.2 assets reach the test through runfiles
// (KUBEBUILDER_ASSETS from cucina_go_test(envtest = True)) and start a
// loopback-only control plane inside the Bazel sandbox.
func TestControlPlaneStarts(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("envtest stop: %v", err)
		}
	})

	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatalf("discovery client: %v", err)
	}
	v, err := dc.ServerVersion()
	if err != nil {
		t.Fatalf("server version: %v", err)
	}
	if v.GitVersion != "v1.36.2" {
		t.Errorf("kube-apiserver version = %s, want v1.36.2 (envtest-v1.36.2 pin)", v.GitVersion)
	}
}
