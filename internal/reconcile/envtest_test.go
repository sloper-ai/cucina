// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/sloper-ai/cucina/internal/controller"
)

// The integration tests run a real kube-apiserver + etcd (envtest) with the
// CRDs from api/crds. KUBEBUILDER_ASSETS points at the pinned envtest-v1.36.2
// binaries (Bazel: cucina_go_test(tier = "integration") provides them; locally
// see docs/dev/controller.md). Without them the tests skip with a reason.

var (
	envOnce sync.Once
	testEnv *envtest.Environment
	envCl   client.Client
	envErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if testEnv != nil {
		_ = testEnv.Stop()
	}
	os.Exit(code)
}

// apiServer returns a client of the shared test API server.
func apiServer(t *testing.T) client.Client {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set: envtest assets (etcd, kube-apiserver v1.36) are required for integration tests")
	}
	crds := os.Getenv("CUCINA_CRD_DIR") // Bazel runfiles; default: the checked-in CRDs
	if crds == "" {
		crds = "../../api/crds"
	}
	envOnce.Do(func() {
		testEnv = &envtest.Environment{CRDDirectoryPaths: []string{crds}, ErrorIfCRDPathMissing: true, BinaryAssetsDirectory: assets}
		cfg, err := testEnv.Start()
		if err != nil {
			envErr = err
			return
		}
		scheme, err := controller.NewScheme()
		if err != nil {
			envErr = err
			return
		}
		envCl, envErr = client.New(cfg, client.Options{Scheme: scheme})
	})
	require.NoError(t, envErr)
	return envCl
}

var nsSeq atomic.Int64

// namespace creates a fresh namespace for one test run (unique per call, so
// `go test -count=N` against the shared API server stays isolated).
func namespace(t *testing.T, c client.Client, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", prefix, nsSeq.Add(1))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	return name
}
