// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// pinPrefix prefixes an SPKI pin: "sha256:<64 lower-case hex>".
const pinPrefix = "sha256:"

// SPKIPin returns the pin of a (CA) certificate: SHA-256 over its DER
// SubjectPublicKeyInfo, as "sha256:<hex>" (docs/security.md, "SPKI pin format").
func SPKIPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return pinPrefix + hex.EncodeToString(sum[:])
}

// ParsePin validates a pin string and returns its digest.
func ParsePin(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, pinPrefix) {
		return nil, fmt.Errorf("pin %q must start with %q", s, pinPrefix)
	}
	h := strings.TrimPrefix(s, pinPrefix)
	if len(h) != 64 || strings.ToLower(h) != h {
		return nil, fmt.Errorf("pin %q must be %s followed by 64 lower-case hex digits", s, pinPrefix)
	}
	return hex.DecodeString(h)
}

// PinnedTLSConfig returns a client TLS configuration for a peer that knows only
// SPKI pins of Cucina's CA (first contact of hostd/worker agents before they hold
// the CA bundle). The server must present its issuing CA in the chain (Cucina's
// server Secrets do); the leaf must verify against a pinned CA for serverName.
func PinnedTLSConfig(pins []string, serverName string, now func() time.Time) (*tls.Config, error) {
	if len(pins) == 0 {
		return nil, errors.New("pki: no pins")
	}
	digests := make([][]byte, 0, len(pins))
	for _, p := range pins {
		d, err := ParsePin(p)
		if err != nil {
			return nil, err
		}
		digests = append(digests, d)
	}
	if now == nil {
		now = time.Now
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         serverName,
		InsecureSkipVerify: true, //nolint:gosec // verification happens in VerifyConnection against the pinned CA.
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyPinnedChain(cs.PeerCertificates, digests, serverName, now())
		},
	}, nil
}

func verifyPinnedChain(certs []*x509.Certificate, digests [][]byte, serverName string, now time.Time) error {
	if len(certs) == 0 {
		return errors.New("pki: server presented no certificate")
	}
	roots := x509.NewCertPool()
	found := false
	for _, c := range certs[1:] {
		if !c.IsCA {
			continue
		}
		sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
		for _, d := range digests {
			if subtle.ConstantTimeCompare(sum[:], d) == 1 {
				roots.AddCert(c)
				found = true
			}
		}
	}
	if !found {
		return errors.New("pki: no CA certificate in the server chain matches a pinned key")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, DNSName: serverName, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}
