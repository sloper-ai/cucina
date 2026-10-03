// SPDX-License-Identifier: FSL-1.1-ALv2

package envtest_test

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	"github.com/sloper-ai/cucina/internal/envtest"
)

// TestControlPlaneStarts guards R-BUILD-1 ("Kubernetes test assets") and
// R-BUILD-5: the pinned envtest-v1.36.2 assets reach the test through runfiles
// (KUBEBUILDER_ASSETS from cucina_go_test(envtest = True)) and start a
// loopback-only control plane inside the Bazel sandbox. ADR 0112 regression:
// Stop must reap both real children before removing their state, including Windows.
func TestControlPlaneStarts(t *testing.T) {
	// This test owns local children even if a caller uses an existing cluster.
	// An absent kubeconfig makes a mistaken external-cluster selection fail safely.
	t.Setenv("USE_EXISTING_CLUSTER", "true")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-existing-cluster"))
	useExistingCluster := false
	env := &envtest.Environment{UseExistingCluster: &useExistingCluster}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	stopAttempted := false
	t.Cleanup(func() {
		if !stopAttempted {
			if err := env.Stop(); err != nil {
				t.Errorf("envtest stop: %v", err)
			}
		}
	})

	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("HTTP client: %v", err)
	}
	t.Cleanup(httpClient.CloseIdleConnections)
	dc, err := discovery.NewDiscoveryClientForConfigAndClient(cfg, httpClient)
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
	httpClient.CloseIdleConnections()
	apiURL, err := url.Parse(cfg.Host)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := []string{apiURL.Host, env.ControlPlane.Etcd.URL.Host}
	stateDirs := []string{env.ControlPlane.APIServer.CertDir, env.ControlPlane.Etcd.DataDir}
	// A failed Stop stays failed; cleanup must not turn it into a retry.
	stopAttempted = true
	if err := env.Stop(); err != nil {
		t.Fatalf("envtest stop: %v", err)
	}
	for _, endpoint := range endpoints {
		conn, err := net.DialTimeout("tcp", endpoint, time.Second)
		if err == nil {
			_ = conn.Close()
			t.Errorf("owned control-plane child still listens after Stop: %s", endpoint)
		}
	}
	for _, dir := range stateDirs {
		if dir == "" {
			t.Fatal("control-plane temporary directory was not recorded")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("temporary control-plane directory survives successful Stop: %s: %v", dir, err)
		}
	}
}
