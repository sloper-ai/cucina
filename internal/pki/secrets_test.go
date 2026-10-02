// SPDX-License-Identifier: FSL-1.1-ALv2

package pki_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

const ns = "cucina"

func newKube(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &s))
	return &s
}

// TestEnsureCANeverOverwrites guards the bootstrap contract (chart pre-install
// and pre-upgrade hook): EnsureCA creates the CA Secret once and never replaces
// an existing Secret, not even an unusable one.
func TestEnsureCANeverOverwrites(t *testing.T) {
	ctx := context.Background()
	c := newKube(t)
	first, err := pki.EnsureCA(ctx, c, ns, "cucina-ca")
	require.NoError(t, err)
	second, err := pki.EnsureCA(ctx, c, ns, "cucina-ca")
	require.NoError(t, err)
	require.Equal(t, first.Certificate().Raw, second.Certificate().Raw)
	require.Equal(t, elliptic.P256(), first.Certificate().PublicKey.(*ecdsa.PublicKey).Curve, "ECDSA P-256 by default")

	broken := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "operator-ca"}, Data: map[string][]byte{"ca.crt": []byte("junk")}}
	c = newKube(t, broken)
	_, err = pki.EnsureCA(ctx, c, ns, "operator-ca")
	require.Error(t, err)
	require.Equal(t, []byte("junk"), getSecret(t, c, "operator-ca").Data["ca.crt"], "never overwritten")
}

// TestRotatorKeepsServerCertificatesFresh guards the bootstrap/Rotator addendum:
// server and controller certificate Secrets are created with the spec's SANs,
// renewed at 2/3 of their lifetime or when SANs or the issuing CA change,
// unmanaged Secrets (existing Secret / cert-manager) are never touched, and
// cucina_cert_expiry_seconds reports the earliest expiry per role.
func TestRotatorKeepsServerCertificatesFresh(t *testing.T) {
	ctx := context.Background()
	clock := pkitest.NewClock(pkitest.Epoch)
	caSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cucina-ca"}, Data: pkitest.CAData(t, clock.Now())}
	unmanaged := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "public-tls"}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": []byte("from cert-manager"), "tls.key": []byte("k")}}
	c := newKube(t, caSecret, unmanaged)
	specs := []pki.CertSpec{
		{SecretName: "frontend-tls", Component: "frontend", DNSNames: []string{"cucina-frontend", "cucina-frontend.cucina.svc"}, IPAddresses: []string{"10.0.0.7"}, ClientAuth: true},
		{SecretName: "scheduler-tls", Component: "scheduler", DNSNames: []string{"cucina-scheduler.cucina.svc"}, Lifetime: config.Duration{Duration: 30 * 24 * time.Hour}},
		{SecretName: "controller-client", Role: pki.RoleController},
		{SecretName: "public-tls", Component: "frontend", DNSNames: []string{"cucina.example.com"}},
	}
	tracker := pki.NewExpiryTracker(clock)
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(tracker))
	rot := &pki.Rotator{Client: c, Namespace: ns, CASecret: "cucina-ca", Specs: specs, Clock: clock, Expiry: tracker}
	actions := func() map[string]pki.CertAction {
		res, err := rot.RunOnce(ctx)
		require.NoError(t, err)
		out := map[string]pki.CertAction{}
		for _, r := range res {
			out[r.SecretName] = r.Action
		}
		return out
	}

	require.Equal(t, map[string]pki.CertAction{"frontend-tls": pki.CertCreated, "scheduler-tls": pki.CertCreated,
		"controller-client": pki.CertCreated, "public-tls": pki.CertUnmanaged}, actions())
	fe := getSecret(t, c, "frontend-tls")
	require.Equal(t, corev1.SecretTypeTLS, fe.Type)
	chain := pkitest.ParseChain(t, fe.Data["tls.crt"])
	require.Len(t, chain, 2, "leaf + issuing CA (SPKI pinning clients find the CA in the handshake)")
	require.ElementsMatch(t, specs[0].DNSNames, chain[0].DNSNames)
	require.Equal(t, "10.0.0.7", chain[0].IPAddresses[0].String())
	require.Equal(t, "spiffe://cucina/server/frontend", chain[0].URIs[0].String())
	require.Equal(t, caSecret.Data["ca.crt"], fe.Data["ca.crt"])
	ctl := pkitest.ParseChain(t, getSecret(t, c, "controller-client").Data["tls.crt"])[0]
	require.Equal(t, pki.ControllerURI, ctl.URIs[0].String())
	require.Equal(t, []byte("from cert-manager"), getSecret(t, c, "public-tls").Data["tls.crt"])
	require.Equal(t, 3, testutil.CollectAndCount(tracker, pki.ExpiryMetricName), "roles ca, server, controller")
	require.InDelta(t, (30 * 24 * time.Hour).Seconds(), expirySeconds(t, reg, "server"), 1, "earliest server certificate")
	problems, err := testutil.CollectAndLint(tracker)
	require.NoError(t, err)
	require.Empty(t, problems, "metric hygiene (docs/contracts.md §6)")

	require.Equal(t, pki.CertUnchanged, actions()["frontend-tls"], "idempotent")

	clock.Advance(20 * 24 * time.Hour) // 2/3 of the scheduler's 30 days
	got := actions()
	require.Equal(t, pki.CertRenewed, got["scheduler-tls"])
	require.Equal(t, pki.CertUnchanged, got["frontend-tls"])

	rot.Specs[0].DNSNames = append(rot.Specs[0].DNSNames, "cucina.example.com")
	require.Equal(t, pki.CertRenewed, actions()["frontend-tls"], "SAN change re-issues")

	sec := getSecret(t, c, "cucina-ca")
	introduced, err := pki.RotateCAData(sec.Data, pki.RotationIntroduce, clock.Now())
	require.NoError(t, err)
	sec.Data = introduced
	require.NoError(t, c.Update(ctx, sec))
	require.Equal(t, pki.CertBundle, actions()["frontend-tls"], "introduce only distributes the new bundle")
	require.Len(t, pkitest.ParseChain(t, getSecret(t, c, "frontend-tls").Data["ca.crt"]), 2)

	sec = getSecret(t, c, "cucina-ca")
	activated, err := pki.RotateCAData(sec.Data, pki.RotationActivate, clock.Now())
	require.NoError(t, err)
	sec.Data = activated
	require.NoError(t, c.Update(ctx, sec))
	got = actions()
	require.Equal(t, pki.CertRenewed, got["frontend-tls"], "activate re-issues under the new CA")
	require.Equal(t, pki.CertRenewed, got["controller-client"])
}

func expirySeconds(t *testing.T, reg *prometheus.Registry, role string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "role" && l.GetValue() == role {
					return m.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("no %s sample for role %q", pki.ExpiryMetricName, role)
	return 0
}
