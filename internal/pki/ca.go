// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // RFC 5280 §4.2.1.2 method (1) key identifier, not a security function.
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"
)

// Keys of the CA Secret (config.PKI.CASecret). A chart-generated or existing CA
// provides ca.crt + ca.key; a cert-manager-issued CA Certificate provides
// tls.crt + tls.key (the issuing, possibly intermediate, CA) and ca.crt (its root).
const (
	SecretCACert    = "ca.crt"   // PEM trust bundle: 1 root, 2 during a rotation
	SecretCAKey     = "ca.key"   // PEM key of the active issuing CA (its certificate is in ca.crt)
	SecretTLSCert   = "tls.crt"  // PEM issuing CA (+ intermediates) when ca.key is absent
	SecretTLSKey    = "tls.key"  // PEM key of tls.crt's first certificate
	SecretNextCAKey = "next.key" // staged key of the next CA between the introduce and activate phases
)

// DefaultCAValidity is the validity of a generated CA certificate.
const DefaultCAValidity = 10 * 365 * 24 * time.Hour

// ErrNoCA is returned when a Secret holds no usable CA material.
var ErrNoCA = errors.New("no usable CA")

// CA is an issuing certificate authority plus the trust bundle that verifiers
// use. It is immutable; reloads produce a new *CA.
type CA struct {
	cert     *x509.Certificate   // active issuing CA
	signer   crypto.Signer       // its key
	chain    []*x509.Certificate // issuing CA first, then intermediates; appended after every leaf
	roots    []*x509.Certificate // trust bundle
	rootsPEM []byte
	pool     *x509.CertPool
}

// CASource yields the current CA. *CA implements it (static); SecretCASource
// reloads the CA Secret.
type CASource interface {
	Current() *CA
}

// Current implements CASource for a static CA.
func (ca *CA) Current() *CA { return ca }

// Certificate returns the active issuing CA certificate.
func (ca *CA) Certificate() *x509.Certificate { return ca.cert }

// Roots returns the trust bundle certificates.
func (ca *CA) Roots() []*x509.Certificate { return append([]*x509.Certificate(nil), ca.roots...) }

// BundlePEM returns the PEM trust bundle (what verifiers and enrollment responses carry).
func (ca *CA) BundlePEM() []byte { return append([]byte(nil), ca.rootsPEM...) }

// Pool returns the trust bundle as a cert pool.
func (ca *CA) Pool() *x509.CertPool { return ca.pool }

// ChainPEM returns the certificates appended after an issued leaf.
func (ca *CA) ChainPEM() []byte { return encodeCerts(ca.chain) }

// NewCAMaterial generates a self-signed ECDSA P-256 root CA and returns the
// CA Secret data (ca.crt, ca.key). The CA may only issue leaves (path length 0).
func NewCAMaterial(now time.Time, validity time.Duration, randSrc io.Reader) (map[string][]byte, error) {
	if randSrc == nil {
		randSrc = rand.Reader
	}
	if validity <= 0 {
		validity = DefaultCAValidity
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), randSrc)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial(randSrc)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Cucina"}, CommonName: "Cucina CA " + now.UTC().Format("2006-01-02T15:04:05Z")},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		SubjectKeyId:          keyID(key.Public()),
	}
	der, err := x509.CreateCertificate(randSrc, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		SecretCACert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		SecretCAKey:  keyPEM,
	}, nil
}

// ParseCA builds a CA from CA Secret data. With ca.key, the issuing CA is the
// certificate in ca.crt whose public key matches ca.key (so the bundle order
// does not matter during a rotation). Without ca.key, tls.crt/tls.key name the
// issuing CA, which must verify against ca.crt.
func ParseCA(data map[string][]byte) (*CA, error) {
	rootsPEM := data[SecretCACert]
	roots, err := parseCerts(rootsPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNoCA, SecretCACert, err)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%w: %s holds no certificate", ErrNoCA, SecretCACert)
	}
	pool := x509.NewCertPool()
	for _, r := range roots {
		if !r.IsCA || !r.BasicConstraintsValid {
			return nil, fmt.Errorf("%w: %s holds a non-CA certificate (%s)", ErrNoCA, SecretCACert, r.Subject)
		}
		pool.AddCert(r)
	}
	ca := &CA{roots: roots, rootsPEM: encodeCerts(roots), pool: pool}

	switch {
	case len(data[SecretCAKey]) > 0:
		key, err := parseKey(data[SecretCAKey])
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrNoCA, SecretCAKey, err)
		}
		for _, r := range roots {
			if publicKeysEqual(r.PublicKey, key.Public()) {
				ca.cert, ca.signer, ca.chain = r, key, []*x509.Certificate{r}
				break
			}
		}
		if ca.cert == nil {
			return nil, fmt.Errorf("%w: %s matches no certificate in %s", ErrNoCA, SecretCAKey, SecretCACert)
		}
	case len(data[SecretTLSKey]) > 0 && len(data[SecretTLSCert]) > 0:
		key, err := parseKey(data[SecretTLSKey])
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrNoCA, SecretTLSKey, err)
		}
		chain, err := parseCerts(data[SecretTLSCert])
		if err != nil || len(chain) == 0 {
			return nil, fmt.Errorf("%w: %s holds no certificate", ErrNoCA, SecretTLSCert)
		}
		if !publicKeysEqual(chain[0].PublicKey, key.Public()) {
			return nil, fmt.Errorf("%w: %s does not match the first certificate of %s", ErrNoCA, SecretTLSKey, SecretTLSCert)
		}
		inter := x509.NewCertPool()
		for _, c := range chain[1:] {
			inter.AddCert(c)
		}
		if _, err := chain[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, CurrentTime: chain[0].NotBefore.Add(time.Second),
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			return nil, fmt.Errorf("%w: issuing CA in %s does not chain to %s: %w", ErrNoCA, SecretTLSCert, SecretCACert, err)
		}
		ca.cert, ca.signer, ca.chain = chain[0], key, chain
	default:
		return nil, fmt.Errorf("%w: neither %s nor %s/%s present", ErrNoCA, SecretCAKey, SecretTLSCert, SecretTLSKey)
	}
	if !ca.cert.IsCA || ca.cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, fmt.Errorf("%w: issuing certificate %s is not allowed to sign certificates", ErrNoCA, ca.cert.Subject)
	}
	return ca, nil
}

// ---------------------------------------------------------------- helpers

func randomSerial(r io.Reader) (*big.Int, error) {
	// 128 random bits, positive and non-zero (RFC 5280 §4.1.2.2: <= 20 octets).
	b := make([]byte, 16)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	b[0] &= 0x7f
	b[0] |= 0x40
	return new(big.Int).SetBytes(b), nil
}

// keyID is the RFC 5280 §4.2.1.2 method (1) key identifier.
func keyID(pub crypto.PublicKey) []byte {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(der, &spki); err != nil {
		return nil
	}
	sum := sha1.Sum(spki.PublicKey.Bytes) //nolint:gosec // identifier only
	return sum[:]
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	ea, ok := a.(equaler)
	return ok && ea.Equal(b)
}

func parseCerts(pemData []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := bytes.TrimSpace(pemData)
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("trailing data that is not PEM")
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		rest = bytes.TrimSpace(rest)
	}
	return out, nil
}

func encodeCerts(certs []*x509.Certificate) []byte {
	var b bytes.Buffer
	for _, c := range certs {
		_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	return b.Bytes()
}

// parseKey accepts PKCS#8 ("PRIVATE KEY"), SEC 1 ("EC PRIVATE KEY") and PKCS#1
// ("RSA PRIVATE KEY") PEM, as written by Cucina, cert-manager and openssl.
func parseKey(pemData []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("more than one PEM block")
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
	}
	if err != nil {
		return nil, err
	}
	switch k := key.(type) {
	case *ecdsa.PrivateKey, *rsa.PrivateKey, ed25519.PrivateKey:
		return k.(crypto.Signer), nil
	}
	return nil, fmt.Errorf("unsupported key type %T", key)
}

func encodeKey(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
