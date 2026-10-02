// SPDX-License-Identifier: FSL-1.1-ALv2

package pki_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

// Guards the controller client certificate's lifetime (found by the kind smoke
// test: the issuer ignored the spec's Lifetime, the chart's 720h, while the
// renewal check honored it, so every Rotator pass and every bootstrap re-issued
// the certificate as "lifetime shortened"): the spec decides the lifetime of
// controller certificates as it does for server ones, and a second pass at the
// same time leaves the Secret alone.
func TestControllerCertificateHonorsSpecLifetime(t *testing.T) {
	ctx := context.Background()
	clock := pkitest.NewClock(pkitest.Epoch)
	caSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cucina-ca"}, Data: pkitest.CAData(t, clock.Now())}
	c := newKube(t, caSecret)
	rot := &pki.Rotator{Client: c, Namespace: ns, CASecret: "cucina-ca", Clock: clock, Specs: []pki.CertSpec{
		{SecretName: "controller-client", Role: pki.RoleController, Lifetime: config.Duration{Duration: 720 * time.Hour}},
	}}
	pass := func() pki.CertResult {
		t.Helper()
		res, err := rot.RunOnce(ctx)
		require.NoError(t, err)
		require.Len(t, res, 1)
		return res[0]
	}

	require.Equal(t, pki.CertCreated, pass().Action)
	leaf := pkitest.ParseChain(t, getSecret(t, c, "controller-client").Data["tls.crt"])[0]
	require.InDelta(t, (720 * time.Hour).Seconds(), leaf.NotAfter.Sub(leaf.NotBefore).Seconds(), (2 * pki.Backdate).Seconds())
	second := pass()
	require.Equal(t, pki.CertUnchanged, second.Action, "re-issued again: %s", second.Reason)

	rot.Specs[0].Lifetime = config.Duration{Duration: 5 * time.Minute}
	_, err := rot.RunOnce(ctx)
	require.Error(t, err, "a lifetime below 10m is a configuration error, not a certificate")
}
