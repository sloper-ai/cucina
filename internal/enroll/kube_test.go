// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/enroll"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

const kubeNS = "cucina"

func kubeClient(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&v1alpha1.MacHost{}).Build()
}

func kubeStores(c client.Client) stores {
	return stores{
		hosts:  &enroll.MacHostStore{Reader: c, Client: c, Namespace: kubeNS},
		tokens: &enroll.SecretTokens{Reader: c, Client: c, Namespace: kubeNS, Name: "cucina-enroll-tokens"},
		replay: &enroll.LeaseReplay{Reader: c, Client: c, Namespace: kubeNS},
	}
}

// TestHostLifecycleOnKubernetes runs the admission story on the production
// stores (MacHost CR, token Secret, Leases) and checks the object contract
// documented in docs/security.md §Enrollment.
func TestHostLifecycleOnKubernetes(t *testing.T) {
	c := kubeClient(t)
	hostLifecycle(t, kubeStores(c))

	var mh v1alpha1.MacHost
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: kubeNS, Name: "c02xk0aajgh6"}, &mh))
	require.Equal(t, "C02XK0AAJGH6", mh.Spec.Serial)
	require.False(t, mh.Spec.Approved, "re-enrolled after removal: pending again")
	require.Len(t, mh.Annotations[enroll.AnnotationIdentityKey], 64)
	require.NotEmpty(t, mh.Labels[enroll.LabelEnrollToken])
}

// TestReplicasShareEnrollmentState guards the two-replica controller default:
// the first enrollment of an instance launch wins on every replica (Lease
// creation is atomic), and a token's host count is shared.
func TestReplicasShareEnrollmentState(t *testing.T) {
	c := kubeClient(t)
	a := newEnv(t, kubeStores(c), nil)
	b := newEnv(t, kubeStores(c), func(d *enroll.Deps) {
		d.Compute, d.Issuer, d.Clock, d.Launches = a.compute, a.issuer, a.clock, a.launches
	})
	ctx := context.Background()

	inst := a.launch(nil)
	_, err := a.srv.EnrollWorker(ctx, a.workerRequest(inst, pkitest.Key(t)))
	require.NoError(t, err)
	_, err = b.srv.EnrollWorker(ctx, a.workerRequest(inst, pkitest.Key(t)))
	require.Equal(t, codes.PermissionDenied, status.Code(err), "the other replica sees the launch record")
	var leases coordinationv1.LeaseList
	require.NoError(t, c.List(ctx, &leases, client.InNamespace(kubeNS)))
	require.Len(t, leases.Items, 1)

	_, token := a.createToken("office-1", 1, 0)
	require.Equal(t, pending, a.enrollHost(a.hostRequest(token, "AAAAAAAAAA01", pkitest.Key(t))).GetStatus())
	require.Equal(t, invalid, b.enrollHost(b.hostRequest(token, "AAAAAAAAAA02", pkitest.Key(t))).GetStatus(), "host count is shared")
}
