// SPDX-License-Identifier: FSL-1.1-ALv2

package bbtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PKI is a throwaway certificate authority for one test: ECDSA P-256 keys in
// t.TempDir(), never production PKI (that is internal/pki) and never committed.
type PKI struct {
	Dir    string
	CAPEM  string
	Pool   *x509.CertPool
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

// KeyPair is an issued leaf certificate on disk and in memory.
type KeyPair struct {
	CertPath string
	KeyPath  string
	CertPEM  []byte
	KeyPEM   []byte
	TLS      tls.Certificate
}

// LeafOptions describe a leaf certificate.
type LeafOptions struct {
	CommonName string
	URIs       []string // SPIFFE-style identities (docs/security.md)
	DNSNames   []string
	IPs        []net.IP
	Server     bool // serverAuth EKU
	Client     bool // clientAuth EKU
}

// NewPKI creates a CA valid for one day.
func NewPKI(t testing.TB) *PKI {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "bbtest CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	p := &PKI{Dir: t.TempDir(), caCert: cert, caKey: key, Pool: x509.NewCertPool()}
	p.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	p.Pool.AddCert(cert)
	if err := os.WriteFile(filepath.Join(p.Dir, "ca.crt"), []byte(p.CAPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Issue signs a leaf and writes <name>.crt and <name>.key into p.Dir.
func (p *PKI) Issue(t testing.TB, name string, o LeafOptions) KeyPair {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: o.CommonName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(12 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames:     o.DNSNames,
		IPAddresses:  o.IPs,
	}
	for _, u := range o.URIs {
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, parsed)
	}
	if o.Server {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if o.Client {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	kp := KeyPair{
		CertPath: filepath.Join(p.Dir, name+".crt"),
		KeyPath:  filepath.Join(p.Dir, name+".key"),
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
	if err := os.WriteFile(kp.CertPath, kp.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp.KeyPath, kp.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if kp.TLS, err = tls.X509KeyPair(kp.CertPEM, kp.KeyPEM); err != nil {
		t.Fatal(err)
	}
	return kp
}

// LoopbackServer issues a server certificate for localhost, 127.0.0.1 and ::1
// (also usable as a client certificate, like Cucina's in-cluster servers).
func (p *PKI) LoopbackServer(t testing.TB, name, uri string) KeyPair {
	t.Helper()
	o := LeafOptions{
		CommonName: name,
		DNSNames:   []string{"localhost"},
		IPs:        []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		Server:     true,
		Client:     true,
	}
	if uri != "" {
		o.URIs = []string{uri}
	}
	return p.Issue(t, name, o)
}

// Workload issues a client certificate carrying exactly one URI SAN, like the
// workload identities of docs/security.md (spiffe://cucina/worker/<pool>/<node>, …).
func (p *PKI) Workload(t testing.TB, name, uri string) KeyPair {
	t.Helper()
	return p.Issue(t, name, LeafOptions{CommonName: name, URIs: []string{uri}, Client: true})
}

// ClientTLS returns a client tls.Config trusting the CA, presenting kp (when
// non-nil) and verifying serverName ("" means the dialled host).
func (p *PKI) ClientTLS(kp *KeyPair, serverName string) *tls.Config {
	c := &tls.Config{RootCAs: p.Pool, ServerName: serverName, MinVersion: tls.VersionTLS12}
	if kp != nil {
		c.Certificates = []tls.Certificate{kp.TLS}
	}
	return c
}

// CopyKeyPair writes kp to dir/certName and dir/keyName (for example a worker's
// PKIDir/worker.crt and worker.key).
func CopyKeyPair(t testing.TB, kp KeyPair, dir, certName, keyName string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, certName), kp.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyName), kp.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
