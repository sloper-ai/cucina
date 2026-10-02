// SPDX-License-Identifier: FSL-1.1-ALv2

package pki_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"net"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

var (
	oidBasicConstraints = asn1.ObjectIdentifier{2, 5, 29, 19}
	oidKeyUsage         = asn1.ObjectIdentifier{2, 5, 29, 15}
	oidExtKeyUsage      = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidSAN              = asn1.ObjectIdentifier{2, 5, 29, 17}
	oidSKI              = asn1.ObjectIdentifier{2, 5, 29, 14}
	oidAKI              = asn1.ObjectIdentifier{2, 5, 29, 35}
)

// TestIssueProfiles guards R-SEC-2 (short-lived worker/host/VM certificates
// with the documented URI SANs) and the profile table of docs/security.md.
func TestIssueProfiles(t *testing.T) {
	clock := pkitest.NewClock(pkitest.Epoch)
	iss, ca := pkitest.NewIssuer(t, clock)
	key := pkitest.Key(t)
	worker, _ := pki.WorkerIdentity("linux-x86-64", "i-0123456789abcdef0")
	vm, _ := pki.VMIdentity("macos-arm64-xcode27.0", "H4X9K2LM7Q", "vm-1")
	host, _ := pki.HostIdentity("H4X9K2LM7Q")

	for _, tc := range []struct {
		name  string
		issue func() (*pki.Issued, error)
		uri   string
		ttl   time.Duration
		eku   []x509.ExtKeyUsage
		dns   []string
	}{
		{"worker", func() (*pki.Issued, error) {
			return iss.IssueWorker(pkitest.CSRFor(t, key, worker), "linux-x86-64", "i-0123456789abcdef0")
		}, worker.String(), 24 * time.Hour, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil},
		{"vm", func() (*pki.Issued, error) {
			return iss.IssueVM(pkitest.CSRFor(t, key, vm), "macos-arm64-xcode27.0", "H4X9K2LM7Q", "vm-1")
		}, vm.String(), 12 * time.Hour, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil},
		{"host", func() (*pki.Issued, error) { return iss.IssueHost(pkitest.CSR(t, key, nil), "H4X9K2LM7Q") },
			host.String(), 7 * 24 * time.Hour, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil},
		{"controller", func() (*pki.Issued, error) { return iss.IssueController(key.Public()) },
			pki.ControllerURI, 90 * 24 * time.Hour, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil},
		{"server", func() (*pki.Issued, error) {
			return iss.IssueServer(key.Public(), "frontend", []string{"cucina-frontend.ns.svc"}, []net.IP{net.ParseIP("10.0.0.1")}, false, 0)
		}, "spiffe://cucina/server/frontend", 90 * 24 * time.Hour, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"cucina-frontend.ns.svc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.issue()
			require.NoError(t, err)
			leaf := got.Leaf
			require.Len(t, leaf.URIs, 1)
			require.Equal(t, tc.uri, leaf.URIs[0].String())
			require.Equal(t, tc.dns, leaf.DNSNames)
			require.Empty(t, leaf.EmailAddresses)
			require.False(t, leaf.IsCA)
			require.True(t, leaf.BasicConstraintsValid)
			require.Equal(t, tc.eku, leaf.ExtKeyUsage)
			require.Equal(t, x509.KeyUsageDigitalSignature, leaf.KeyUsage)
			require.Equal(t, clock.Now().Add(tc.ttl), leaf.NotAfter)
			require.Equal(t, clock.Now().Add(-pki.Backdate), leaf.NotBefore)
			// The chain the peer presents is leaf + issuing CA and verifies against the bundle.
			chain := pkitest.ParseChain(t, got.ChainPEM)
			require.Len(t, chain, 2)
			require.Equal(t, ca.Certificate().Raw, chain[1].Raw)
			_, err = leaf.Verify(x509.VerifyOptions{Roots: ca.Pool(), CurrentTime: clock.Now(), KeyUsages: tc.eku})
			require.NoError(t, err)
			require.Equal(t, ca.BundlePEM(), got.BundlePEM)
		})
	}

	// Hard maximums are configuration errors (R-SEC-2 "<= 7 days"; VMs <= 12 h).
	for _, p := range []pki.Policy{{WorkerTTL: 8 * 24 * time.Hour}, {HostTTL: 169 * time.Hour}, {VMTTL: 13 * time.Hour}, {WorkerTTL: time.Minute}} {
		_, err := pki.NewIssuer(ca, clock, p)
		require.Error(t, err, "%+v", p)
	}

	// A leaf never outlives its issuing CA.
	clock.Set(ca.Certificate().NotAfter.Add(-time.Hour))
	got, err := iss.IssueWorker(pkitest.CSR(t, key, nil), "p", "i-0123456789abcdef0")
	require.NoError(t, err)
	require.Equal(t, ca.Certificate().NotAfter, got.NotAfter)
	clock.Advance(2 * time.Hour)
	_, err = iss.IssueWorker(pkitest.CSR(t, key, nil), "p", "i-0123456789abcdef0")
	require.ErrorIs(t, err, pki.ErrCAUnavailable)
}

// TestCSRRejected is the CSR row set of the enrollment trust boundary
// (R-SEC-3): weak keys, CA flags, extra SANs or EKUs and broken proofs of
// possession never yield a certificate.
func TestCSRRejected(t *testing.T) {
	clock := pkitest.NewClock(pkitest.Epoch)
	iss, _ := pkitest.NewIssuer(t, clock)
	caData := pkitest.CAData(t, clock.Now())
	caFromData, err := pki.ParseCA(caData)
	require.NoError(t, err)
	issOwnCA, err := pki.NewIssuer(caFromData, clock, pki.Policy{})
	require.NoError(t, err)
	caKeyBlock, _ := pem.Decode(caData[pki.SecretCAKey])
	caKey, err := x509.ParsePKCS8PrivateKey(caKeyBlock.Bytes)
	require.NoError(t, err)

	key := pkitest.Key(t)
	rsa1024, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	p224, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	require.NoError(t, err)
	worker, _ := pki.WorkerIdentity("linux-x86-64", "i-0123456789abcdef0")
	other, _ := pki.WorkerIdentity("linux-x86-64", "i-0fffffffffffffff0")
	ext := func(id asn1.ObjectIdentifier, critical bool, v any) pkix.Extension {
		b, err := asn1.Marshal(v)
		require.NoError(t, err)
		return pkix.Extension{Id: id, Critical: critical, Value: b}
	}
	type basicConstraints struct {
		IsCA       bool `asn1:"optional"`
		MaxPathLen int  `asn1:"optional,default:-1"`
	}
	good := pkitest.CSRFor(t, key, worker)
	tampered := append([]byte(nil), good...)
	{ // flip one bit of the signature value (the last byte of the DER)
		block, _ := pem.Decode(tampered)
		block.Bytes[len(block.Bytes)-1] ^= 0x01
		tampered = pem.EncodeToMemory(block)
	}
	u, _ := url.Parse("spiffe://cucina/controller")
	for _, tc := range []struct {
		name string
		csr  []byte
		iss  *pki.Issuer
	}{
		{"CA flag requested", pkitest.CSR(t, key, &x509.CertificateRequest{ExtraExtensions: []pkix.Extension{ext(oidBasicConstraints, true, basicConstraints{IsCA: true, MaxPathLen: 0})}}), iss},
		{"extra DNS SAN", pkitest.CSR(t, key, &x509.CertificateRequest{DNSNames: []string{"scheduler.cucina.svc"}, URIs: []*url.URL{worker.URL()}}), iss},
		{"extra IP SAN", pkitest.CSR(t, key, &x509.CertificateRequest{IPAddresses: []net.IP{net.ParseIP("10.0.0.1")}}), iss},
		{"e-mail SAN", pkitest.CSR(t, key, &x509.CertificateRequest{EmailAddresses: []string{"root@example.com"}}), iss},
		{"other identity", pkitest.CSRFor(t, key, other), iss},
		{"controller identity smuggled", pkitest.CSR(t, key, &x509.CertificateRequest{URIs: []*url.URL{u}}), iss},
		{"two URIs", pkitest.CSR(t, key, &x509.CertificateRequest{URIs: []*url.URL{worker.URL(), u}}), iss},
		{"EKU requested", pkitest.CSR(t, key, &x509.CertificateRequest{ExtraExtensions: []pkix.Extension{ext(oidExtKeyUsage, false, []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 1}})}}), iss},
		{"key usage requested", pkitest.CSR(t, key, &x509.CertificateRequest{ExtraExtensions: []pkix.Extension{ext(oidKeyUsage, true, asn1.BitString{Bytes: []byte{0x06}, BitLength: 7})}}), iss},
		{"weak RSA key", pkitest.CSR(t, rsa1024, nil), iss},
		{"weak curve", pkitest.CSR(t, p224, nil), iss},
		{"broken proof of possession", tampered, iss},
		{"not PEM", []byte("MIIB…"), iss},
		{"wrong PEM type", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}), iss},
		{"two PEM blocks", append(append([]byte(nil), good...), good...), iss},
		{"oversized", append(append([]byte(nil), good...), make([]byte, pki.MaxCSRBytes)...), iss},
		{"empty", nil, iss},
		{"CA's own key", pkitest.CSR(t, caKey.(crypto.Signer), nil), issOwnCA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.iss.IssueWorker(tc.csr, "linux-x86-64", "i-0123456789abcdef0")
			require.ErrorIs(t, err, pki.ErrBadCSR)
		})
	}

	// Accepted key types (the host keychain may produce RSA; agents use ECDSA).
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	rsa2048, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	for _, k := range []crypto.Signer{edKey, rsa2048, p384, key} {
		got, err := iss.IssueWorker(pkitest.CSR(t, k, nil), "linux-x86-64", "i-0123456789abcdef0")
		require.NoError(t, err)
		if _, isRSA := k.Public().(*rsa.PublicKey); isRSA {
			require.Equal(t, x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment, got.Leaf.KeyUsage)
		}
	}
}

// TestIssuedCertificatesCarryOnlyProfileFields is the property of R-SEC-3 /
// docs/security.md §PKI: whatever an accepted CSR contains (subject, a matching
// SAN, key type), the certificate's fields are exactly the profile's — the CSR
// contributes nothing but the public key.
func TestIssuedCertificatesCarryOnlyProfileFields(t *testing.T) {
	clock := pkitest.NewClock(pkitest.Epoch)
	iss, ca := pkitest.NewIssuer(t, clock)
	keys := []crypto.Signer{pkitest.Key(t)}
	if k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader); err == nil {
		keys = append(keys, k)
	}
	if _, k, err := ed25519.GenerateKey(rand.Reader); err == nil {
		keys = append(keys, k)
	}
	text := rapid.StringMatching(`[A-Za-z0-9 .,'=+/-]{0,40}`)
	allowed := []asn1.ObjectIdentifier{oidBasicConstraints, oidKeyUsage, oidExtKeyUsage, oidSAN, oidSKI, oidAKI}

	rapid.Check(t, func(rt *rapid.T) {
		key := rapid.SampledFrom(keys).Draw(rt, "key")
		subject := pkix.Name{
			CommonName:         text.Draw(rt, "cn"),
			Organization:       []string{text.Draw(rt, "o")},
			OrganizationalUnit: []string{text.Draw(rt, "ou")},
			Country:            []string{text.Draw(rt, "c")},
			SerialNumber:       text.Draw(rt, "sn"),
		}
		role := rapid.SampledFrom([]pki.Role{pki.RoleWorker, pki.RoleVM, pki.RoleHost}).Draw(rt, "role")
		var id pki.Identity
		switch role {
		case pki.RoleWorker:
			id, _ = pki.WorkerIdentity("linux-x86-64", "i-0123456789abcdef0")
		case pki.RoleVM:
			id, _ = pki.VMIdentity("macos-arm64", "H4X9K2LM7Q", "vm-2")
		case pki.RoleHost:
			id, _ = pki.HostIdentity("H4X9K2LM7Q")
		}
		tmpl := &x509.CertificateRequest{Subject: subject}
		if rapid.Bool().Draw(rt, "withSAN") {
			tmpl.URIs = []*url.URL{id.URL()}
		}
		csr := pkitest.CSR(t, key, tmpl)
		var got *pki.Issued
		var err error
		switch role {
		case pki.RoleWorker:
			got, err = iss.IssueWorker(csr, id.Pool, id.InstanceID)
		case pki.RoleVM:
			got, err = iss.IssueVM(csr, id.Pool, id.Serial, id.VM)
		case pki.RoleHost:
			got, err = iss.IssueHost(csr, id.Serial)
		}
		if err != nil {
			rt.Fatalf("accepted-shape CSR rejected: %v", err)
		}
		leaf := got.Leaf
		wantCN := map[pki.Role]string{pki.RoleWorker: id.InstanceID, pki.RoleVM: id.VM, pki.RoleHost: id.Serial}[role]
		want := pkix.Name{Organization: []string{"Cucina"}, OrganizationalUnit: []string{string(role)}, CommonName: wantCN}
		if leaf.Subject.String() != want.String() || len(leaf.Subject.Names) != 3 {
			rt.Fatalf("subject %q is not the profile's %q (CSR asked for %q)", leaf.Subject, want, subject)
		}
		for _, e := range leaf.Extensions {
			if !slices.ContainsFunc(allowed, e.Id.Equal) {
				rt.Fatalf("unexpected extension %s", e.Id)
			}
		}
		if len(leaf.URIs) != 1 || leaf.URIs[0].String() != id.String() || len(leaf.DNSNames)+len(leaf.IPAddresses)+len(leaf.EmailAddresses) != 0 {
			rt.Fatalf("SANs %v %v %v %v", leaf.URIs, leaf.DNSNames, leaf.IPAddresses, leaf.EmailAddresses)
		}
		if leaf.IsCA || !slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}) || len(leaf.UnknownExtKeyUsage) != 0 {
			rt.Fatalf("usage IsCA=%v EKU=%v", leaf.IsCA, leaf.ExtKeyUsage)
		}
		if leaf.CheckSignatureFrom(ca.Certificate()) != nil || !pubEqual(leaf.PublicKey, key.Public()) {
			rt.Fatalf("not issued by the CA for the CSR key")
		}
	})
}

func pubEqual(a, b crypto.PublicKey) bool {
	return a.(interface{ Equal(crypto.PublicKey) bool }).Equal(b)
}
