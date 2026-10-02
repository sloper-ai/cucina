// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

// MaxCSRBytes bounds the PEM size of a CSR accepted from the network.
const MaxCSRBytes = 16 << 10

// ErrBadCSR is returned for CSRs that are malformed, use a weak key, fail
// proof of possession or request anything beyond the expected identity.
var ErrBadCSR = errors.New("bad certificate signing request")

var (
	oidExtensionRequest     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 14}
	oidExtSubjectAltName    = asn1.ObjectIdentifier{2, 5, 29, 17}
	acceptedCSRSignatureAlg = map[x509.SignatureAlgorithm]bool{
		x509.SHA256WithRSA: true, x509.SHA384WithRSA: true, x509.SHA512WithRSA: true,
		x509.SHA256WithRSAPSS: true, x509.SHA384WithRSAPSS: true, x509.SHA512WithRSAPSS: true,
		x509.ECDSAWithSHA256: true, x509.ECDSAWithSHA384: true, x509.ECDSAWithSHA512: true,
		x509.PureEd25519: true,
	}
)

// ParseCSR validates a PEM PKCS#10 CSR for the identity want and returns its
// public key — the only thing a CSR contributes to an issued certificate.
//
// Accepted keys: ECDSA P-256/P-384, Ed25519, RSA 2048–4096 bits with e=65537.
// The CSR must carry a valid self-signature (proof of possession, no SHA-1/MD5)
// and may request no extension except a subjectAltName holding exactly the
// URI of want; any other requested extension (CA flag, key usages, EKUs, extra
// SANs, …) or attribute rejects the CSR. The subject is ignored.
func ParseCSR(pemData []byte, want Identity) (crypto.PublicKey, error) {
	csr, err := parseCSR(pemData)
	if err != nil {
		return nil, err
	}
	for _, attr := range csr.Attributes { //nolint:staticcheck // Attributes is the only parsed view of non-extension attributes.
		if !attr.Type.Equal(oidExtensionRequest) {
			return nil, fmt.Errorf("%w: unexpected attribute %s", ErrBadCSR, attr.Type)
		}
	}
	for _, ext := range csr.Extensions {
		if !ext.Id.Equal(oidExtSubjectAltName) {
			return nil, fmt.Errorf("%w: requests extension %s; the CA sets every extension itself", ErrBadCSR, ext.Id)
		}
	}
	if len(csr.DNSNames) > 0 || len(csr.EmailAddresses) > 0 || len(csr.IPAddresses) > 0 || len(csr.URIs) > 1 {
		return nil, fmt.Errorf("%w: requests subject alternative names beyond the workload identity", ErrBadCSR)
	}
	if len(csr.URIs) == 1 && csr.URIs[0].String() != want.String() {
		return nil, fmt.Errorf("%w: requests identity %q, expected %q", ErrBadCSR, csr.URIs[0].String(), want.String())
	}
	return csr.PublicKey, nil
}

// CSRKeyHash returns the hex SHA-256 of the CSR's DER SubjectPublicKeyInfo. It is
// stable across re-signed CSRs for the same key (enrollment polling idempotency
// and key binding). The CSR's signature is verified first.
func CSRKeyHash(pemData []byte) (string, error) {
	csr, err := parseCSR(pemData)
	if err != nil {
		return "", err
	}
	return PublicKeyHash(csr.PublicKey)
}

// PublicKeyHash returns the hex SHA-256 of the DER SubjectPublicKeyInfo of pub.
func PublicKeyHash(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

func parseCSR(pemData []byte) (*x509.CertificateRequest, error) {
	if len(pemData) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrBadCSR)
	}
	if len(pemData) > MaxCSRBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrBadCSR, MaxCSRBytes)
	}
	block, rest := pem.Decode(pemData)
	if block == nil || (block.Type != "CERTIFICATE REQUEST" && block.Type != "NEW CERTIFICATE REQUEST") {
		return nil, fmt.Errorf("%w: not a PEM CERTIFICATE REQUEST", ErrBadCSR)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("%w: trailing data after the PEM block", ErrBadCSR)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadCSR, err)
	}
	if !acceptedCSRSignatureAlg[csr.SignatureAlgorithm] {
		return nil, fmt.Errorf("%w: signature algorithm %s not accepted", ErrBadCSR, csr.SignatureAlgorithm)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: proof of possession failed: %w", ErrBadCSR, err)
	}
	if err := checkLeafKey(csr.PublicKey); err != nil {
		return nil, err
	}
	return csr, nil
}

// checkLeafKey enforces the leaf key policy.
func checkLeafKey(pub crypto.PublicKey) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return fmt.Errorf("%w: ECDSA curve %s not accepted (P-256 or P-384)", ErrBadCSR, k.Curve.Params().Name)
		}
	case ed25519.PublicKey:
		if len(k) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: malformed Ed25519 key", ErrBadCSR)
		}
	case *rsa.PublicKey:
		if bits := k.N.BitLen(); bits < 2048 || bits > 4096 {
			return fmt.Errorf("%w: RSA key of %d bits not accepted (2048-4096)", ErrBadCSR, bits)
		}
		if k.E != 65537 {
			return fmt.Errorf("%w: RSA public exponent %d not accepted (65537)", ErrBadCSR, k.E)
		}
	default:
		return fmt.Errorf("%w: key type %T not accepted", ErrBadCSR, pub)
	}
	return nil
}
