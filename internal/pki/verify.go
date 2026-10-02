// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/sloper-ai/cucina/internal/ports"
)

// ErrUnauthenticated is returned when a peer presents no acceptable certificate.
var ErrUnauthenticated = errors.New("peer not authenticated")

// Verifier verifies workload certificates presented to Cucina's own mTLS
// endpoints (HostService) per docs/security.md §Workload identity, "Verification
// rules for Cucina's own verifiers". The trust bundle is read on every call, so
// a CA rotation takes effect without a restart.
type Verifier struct {
	roots func() *x509.CertPool
	clock ports.Clock
}

// NewVerifier verifies against the trust bundle of src.
func NewVerifier(src CASource, clock ports.Clock) *Verifier {
	return &Verifier{roots: func() *x509.CertPool { return src.Current().Pool() }, clock: clock}
}

// NewVerifierWithRoots verifies against an arbitrary (reloadable) pool, e.g. a Bundle.
func NewVerifierWithRoots(roots func() *x509.CertPool, clock ports.Clock) *Verifier {
	return &Verifier{roots: roots, clock: clock}
}

// Verify checks a presented chain (leaf first) and returns the leaf's identity
// if its role is one of accept.
func (v *Verifier) Verify(chain []*x509.Certificate, accept ...Role) (Identity, error) {
	if len(chain) == 0 {
		return Identity{}, fmt.Errorf("%w: no client certificate", ErrUnauthenticated)
	}
	leaf := chain[0]
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: v.roots(), Intermediates: inter, CurrentTime: v.clock.Now(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	if leaf.IsCA || len(leaf.URIs) != 1 {
		return Identity{}, fmt.Errorf("%w: certificate must carry exactly one URI SAN", ErrUnauthenticated)
	}
	id, err := ParseIdentityURL(leaf.URIs[0])
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	if id.Role != RoleServer && (len(leaf.DNSNames) > 0 || len(leaf.IPAddresses) > 0 || len(leaf.EmailAddresses) > 0) {
		return Identity{}, fmt.Errorf("%w: workload certificate with DNS/IP/e-mail SANs", ErrUnauthenticated)
	}
	if !slices.Contains(accept, id.Role) {
		return Identity{}, fmt.Errorf("%w: identity %s (role %s) is not accepted here", ErrUnauthenticated, id, id.Role)
	}
	return id, nil
}

// VerifyRaw is Verify for DER certificates as handed to tls.Config callbacks.
func (v *Verifier) VerifyRaw(raw [][]byte, accept ...Role) (Identity, error) {
	chain := make([]*x509.Certificate, 0, len(raw))
	for _, der := range raw {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return Identity{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
		}
		chain = append(chain, c)
	}
	return v.Verify(chain, accept...)
}

// PeerIdentity returns the verified identity of a gRPC peer that connected over TLS.
func (v *Verifier) PeerIdentity(ctx context.Context, accept ...Role) (Identity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return Identity{}, fmt.Errorf("%w: no transport security information", ErrUnauthenticated)
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return Identity{}, fmt.Errorf("%w: connection is not TLS", ErrUnauthenticated)
	}
	return v.Verify(info.State.PeerCertificates, accept...)
}

// CertificateSource supplies the serving certificate (KeyPair implements it).
type CertificateSource interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

// ServerTLSConfig returns a TLS 1.3 server configuration with server
// authentication only (EnrollmentService: callers have no certificate yet).
func ServerTLSConfig(cert CertificateSource) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: cert.GetCertificate}
}

// MutualTLSConfig returns a TLS 1.3 server configuration that requires a client
// certificate whose identity has one of the accepted roles (HostService: RoleHost).
// Verification happens in VerifyPeerCertificate against the current bundle, so
// it follows CA rotations without restarting the listener.
func (v *Verifier) MutualTLSConfig(cert CertificateSource, accept ...Role) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: cert.GetCertificate,
		ClientAuth:     tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			_, err := v.VerifyRaw(raw, accept...)
			return err
		},
	}
}
