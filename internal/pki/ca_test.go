// SPDX-License-Identifier: FSL-1.1-ALv2

package pki_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

// TestCARotationTwoRootBundle walks the documented two-root rotation
// (docs/security.md §PKI, "Rotation"): after introduce both roots are trusted
// and the old CA still signs; after activate the new CA signs and old leaves
// still verify; after retire only the new root is trusted.
func TestCARotationTwoRootBundle(t *testing.T) {
	clock := pkitest.NewClock(pkitest.Epoch)
	data := pkitest.CAData(t, clock.Now())
	key := pkitest.Key(t)
	issueWith := func(data map[string][]byte) (*pki.CA, *x509.Certificate) {
		ca, err := pki.ParseCA(data)
		require.NoError(t, err)
		iss, err := pki.NewIssuer(ca, clock, pki.Policy{})
		require.NoError(t, err)
		got, err := iss.IssueHost(pkitest.CSR(t, key, nil), "H4X9K2LM7Q")
		require.NoError(t, err)
		return ca, got.Leaf
	}
	verifies := func(ca *pki.CA, leaf *x509.Certificate) bool {
		_, err := pki.NewVerifier(ca, clock).Verify([]*x509.Certificate{leaf}, pki.RoleHost)
		return err == nil
	}

	ca1, leaf1 := issueWith(data)
	require.Len(t, ca1.Roots(), 1)

	introduced, err := pki.RotateCAData(data, pki.RotationIntroduce, clock.Now())
	require.NoError(t, err)
	caI, leafI := issueWith(introduced)
	require.Len(t, caI.Roots(), 2, "both roots are trusted")
	require.Equal(t, ca1.Certificate().Raw, caI.Certificate().Raw, "the current CA keeps signing")
	require.True(t, verifies(caI, leaf1) && verifies(caI, leafI))
	_, err = pki.RotateCAData(introduced, pki.RotationIntroduce, clock.Now())
	require.Error(t, err, "only one next CA at a time")

	activated, err := pki.RotateCAData(introduced, pki.RotationActivate, clock.Now())
	require.NoError(t, err)
	caA, leafA := issueWith(activated)
	require.Len(t, caA.Roots(), 2)
	require.NotEqual(t, ca1.Certificate().Raw, caA.Certificate().Raw, "the next CA signs")
	require.True(t, verifies(caA, leaf1), "leaves of the old CA stay valid until retire")
	require.True(t, verifies(caA, leafA))
	require.False(t, verifies(ca1, leafA), "a verifier that never got the new root rejects new leaves (why introduce comes first)")

	retired, err := pki.RotateCAData(activated, pki.RotationRetire, clock.Now())
	require.NoError(t, err)
	caR, err := pki.ParseCA(retired)
	require.NoError(t, err)
	require.Len(t, caR.Roots(), 1)
	require.False(t, verifies(caR, leaf1), "old leaves are rejected after retire")
	require.True(t, verifies(caR, leafA))
}

// TestParseCAIntermediateFromCertManager covers the cert-manager CA source: the
// issuing CA is tls.crt/tls.key (an intermediate) and ca.crt is its root; issued
// chains carry the intermediate so peers verify against the root bundle.
func TestParseCAIntermediateFromCertManager(t *testing.T) {
	now := pkitest.Epoch
	rootKey := pkitest.Key(t)
	rootTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"}, NotBefore: now.Add(-time.Hour),
		NotAfter: now.Add(24 * time.Hour * 365), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, rootKey.Public(), rootKey)
	require.NoError(t, err)
	root, _ := x509.ParseCertificate(rootDER)
	interKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	interTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "cucina issuing"}, NotBefore: now.Add(-time.Hour),
		NotAfter: now.Add(24 * time.Hour * 90), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}
	interDER, err := x509.CreateCertificate(rand.Reader, interTmpl, root, interKey.Public(), rootKey)
	require.NoError(t, err)
	interKeyDER, err := x509.MarshalECPrivateKey(interKey)
	require.NoError(t, err)

	ca, err := pki.ParseCA(map[string][]byte{
		pki.SecretCACert:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}),
		pki.SecretTLSCert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: interDER}),
		pki.SecretTLSKey:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: interKeyDER}),
	})
	require.NoError(t, err)
	clock := pkitest.NewClock(now)
	iss, err := pki.NewIssuer(ca, clock, pki.Policy{})
	require.NoError(t, err)
	got, err := iss.IssueWorker(pkitest.CSR(t, pkitest.Key(t), nil), "linux-x86-64", "i-0123456789abcdef0")
	require.NoError(t, err)
	chain := pkitest.ParseChain(t, got.ChainPEM)
	require.Len(t, chain, 2, "leaf + intermediate")
	id, err := pki.NewVerifier(ca, clock).Verify(chain, pki.RoleWorker)
	require.NoError(t, err)
	require.Equal(t, "i-0123456789abcdef0", id.Node())
	require.Equal(t, root.Raw, ca.Roots()[0].Raw)

	// A Secret whose key matches no certificate is not a CA (never silently replaced).
	_, err = pki.ParseCA(map[string][]byte{pki.SecretCACert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}),
		pki.SecretCAKey: pkitest.CAData(t, now)[pki.SecretCAKey]})
	require.ErrorIs(t, err, pki.ErrNoCA)
}
