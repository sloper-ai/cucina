// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// bootKey is the per-boot ECDSA P-256 key. It is never logged and never sent
// anywhere; only its CSR leaves the process.
type bootKey struct {
	priv *ecdsa.PrivateKey
}

func newBootKey() (*bootKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return &bootKey{priv: k}, nil
}

// parseBootKey reads a PKCS#8 PEM ECDSA key written by pkcs8PEM.
func parseBootKey(b []byte) (*bootKey, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PKCS#8 PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an ECDSA key")
	}
	return &bootKey{priv: ec}, nil
}

// String keeps the key out of logs and fmt output.
func (k *bootKey) String() string { return "ecdsa-p256(redacted)" }

// LogValue keeps the key out of structured logs.
func (k *bootKey) LogValue() slog.Value { return slog.StringValue(k.String()) }

// csrPEM returns a PEM PKCS#10 request. The issuer decides every certificate
// field (contracts §5.2); the CSR contributes the public key, and the common
// name only helps humans reading the request.
func (k *bootKey) csrPEM(commonName string) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: commonName},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, k.priv)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// pkcs8PEM returns the private key for the 0600 key file bb_worker reads.
func (k *bootKey) pkcs8PEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k.priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func (k *bootKey) public() crypto.PublicKey { return k.priv.Public() }

// issued is a validated enrollment result.
type issued struct {
	leaf     *x509.Certificate
	certPEM  []byte
	caPEM    []byte
	notAfter time.Time
}

// validateIssued checks that the controller returned a usable identity for our
// key: the leaf parses, carries our public key, is currently valid for at
// least minValidity and chains to the returned CA bundle for client auth.
func validateIssued(certPEM, caPEM []byte, key *bootKey, now time.Time, minValidity time.Duration) (*issued, error) {
	certs, err := parseCerts(certPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	leaf := certs[0]
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(key.public()) {
		return nil, errors.New("certificate does not carry this boot's public key")
	}
	if now.Before(leaf.NotBefore.Add(-5 * time.Minute)) {
		return nil, fmt.Errorf("certificate not valid before %s (clock skew?)", leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if leaf.NotAfter.Sub(now) < minValidity {
		return nil, fmt.Errorf("certificate expires at %s, less than %s from now", leaf.NotAfter.UTC().Format(time.RFC3339), minValidity)
	}
	cas, err := parseCerts(caPEM)
	if err != nil {
		return nil, fmt.Errorf("CA bundle: %w", err)
	}
	roots := x509.NewCertPool()
	for _, c := range cas {
		roots.AddCert(c)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, fmt.Errorf("certificate does not verify against the returned CA bundle: %w", err)
	}
	return &issued{leaf: leaf, certPEM: certPEM, caPEM: caPEM, notAfter: leaf.NotAfter}, nil
}

// parseCerts parses a PEM bundle of one or more certificates (and nothing else).
func parseCerts(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := b
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("trailing non-PEM data")
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate")
	}
	return out, nil
}
