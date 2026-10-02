// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Default and maximum certificate lifetimes (R-SEC-2: workers and hosts <= 7 days).
const (
	DefaultWorkerTTL     = 24 * time.Hour
	MaxWorkerTTL         = 7 * 24 * time.Hour
	DefaultHostTTL       = 7 * 24 * time.Hour
	MaxHostTTL           = 7 * 24 * time.Hour
	DefaultVMTTL         = 12 * time.Hour
	MaxVMTTL             = 12 * time.Hour
	DefaultServerTTL     = 90 * 24 * time.Hour
	MaxServerTTL         = 397 * 24 * time.Hour
	DefaultControllerTTL = DefaultServerTTL
	MaxControllerTTL     = MaxServerTTL
	// Backdate is subtracted from NotBefore to tolerate clock skew between the
	// controller and the verifying peers.
	Backdate = 5 * time.Minute
)

// ErrCAUnavailable is returned when the CA cannot sign (expired or not yet valid).
var ErrCAUnavailable = errors.New("CA unavailable")

// Policy holds the leaf lifetimes. Zero values mean the defaults; values above
// the hard maximum are a configuration error (fail fast at start-up).
type Policy struct {
	WorkerTTL     time.Duration
	HostTTL       time.Duration
	VMTTL         time.Duration
	ServerTTL     time.Duration
	ControllerTTL time.Duration
}

// PolicyFromConfig maps config.PKI onto a Policy (server/controller lifetimes
// come from the rotator configuration and use the defaults here).
func PolicyFromConfig(c config.PKI) Policy {
	return Policy{WorkerTTL: c.WorkerCertTTL.Duration, HostTTL: c.HostCertTTL.Duration, VMTTL: c.VMCertTTL.Duration}
}

func (p Policy) withDefaults() (Policy, error) {
	type field struct {
		name     string
		v        *time.Duration
		def, max time.Duration
	}
	var errs []error
	for _, f := range []field{
		{"workerCertTTL", &p.WorkerTTL, DefaultWorkerTTL, MaxWorkerTTL},
		{"hostCertTTL", &p.HostTTL, DefaultHostTTL, MaxHostTTL},
		{"vmCertTTL", &p.VMTTL, DefaultVMTTL, MaxVMTTL},
		{"serverCertTTL", &p.ServerTTL, DefaultServerTTL, MaxServerTTL},
		{"controllerCertTTL", &p.ControllerTTL, DefaultControllerTTL, MaxControllerTTL},
	} {
		switch {
		case *f.v == 0:
			*f.v = f.def
		case *f.v < 10*time.Minute || *f.v > f.max:
			errs = append(errs, fmt.Errorf("pki.%s %s out of range [10m, %s]", f.name, *f.v, f.max))
		}
	}
	return p, errors.Join(errs...)
}

// Issued is a freshly issued certificate.
type Issued struct {
	Identity  Identity
	Leaf      *x509.Certificate
	ChainPEM  []byte // the leaf followed by the issuing CA chain (what a peer presents)
	BundlePEM []byte // the trust bundle (what a peer verifies others with)
	NotAfter  time.Time
}

// Issuer issues leaf certificates with strict profiles: the issuer decides every
// field (serial, validity, subject, key usage, EKU, basic constraints, SANs, key
// identifiers); a CSR contributes only its public key.
type Issuer struct {
	src    CASource
	clock  ports.Clock
	policy Policy
	rand   io.Reader
}

// NewIssuer returns an Issuer. clock is required; pol zero values take defaults.
func NewIssuer(src CASource, clock ports.Clock, pol Policy) (*Issuer, error) {
	if src == nil || clock == nil {
		return nil, errors.New("pki: NewIssuer needs a CA source and a clock")
	}
	p, err := pol.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Issuer{src: src, clock: clock, policy: p, rand: rand.Reader}, nil
}

// Policy returns the effective policy.
func (i *Issuer) Policy() Policy { return i.policy }

// CA returns the current CA.
func (i *Issuer) CA() *CA { return i.src.Current() }

// IssueWorker issues an EC2 worker certificate (spiffe://cucina/worker/<pool>/<instance-id>).
func (i *Issuer) IssueWorker(csrPEM []byte, pool, instanceID string) (*Issued, error) {
	id, err := WorkerIdentity(pool, instanceID)
	if err != nil {
		return nil, err
	}
	return i.issueFromCSR(csrPEM, id, i.policy.WorkerTTL)
}

// IssueVM issues a macOS VM worker certificate
// (spiffe://cucina/worker/<pool>/<serial>/<vm>). The serial must come from the
// authenticated host identity, never from the request (HostService.IssueVMIdentity).
func (i *Issuer) IssueVM(csrPEM []byte, pool, serial, vm string) (*Issued, error) {
	id, err := VMIdentity(pool, serial, vm)
	if err != nil {
		return nil, err
	}
	return i.issueFromCSR(csrPEM, id, i.policy.VMTTL)
}

// IssueHost issues a Mac host certificate (spiffe://cucina/host/<serial>); used by
// EnrollHost and HostService.RenewCertificate.
func (i *Issuer) IssueHost(csrPEM []byte, serial string) (*Issued, error) {
	id, err := HostIdentity(serial)
	if err != nil {
		return nil, err
	}
	return i.issueFromCSR(csrPEM, id, i.policy.HostTTL)
}

// IssueController issues the controller's BuildQueueState client certificate
// for a key the controller generated itself.
func (i *Issuer) IssueController(pub crypto.PublicKey) (*Issued, error) {
	if err := checkLeafKey(pub); err != nil {
		return nil, err
	}
	return i.issue(pub, ControllerIdentity(), i.policy.ControllerTTL, nil, nil, false)
}

// IssueServer issues a server certificate for an in-cluster component with the
// given DNS names and IP addresses (at least one of them). clientAuth adds the
// clientAuth EKU for components that also dial mTLS (frontend -> storage).
func (i *Issuer) IssueServer(pub crypto.PublicKey, component string, dnsNames []string, ips []net.IP, clientAuth bool, ttl time.Duration) (*Issued, error) {
	id, err := ServerIdentity(component)
	if err != nil {
		return nil, err
	}
	if err := checkLeafKey(pub); err != nil {
		return nil, err
	}
	if len(dnsNames) == 0 && len(ips) == 0 {
		return nil, fmt.Errorf("pki: server certificate for %q needs at least one DNS name or IP address", component)
	}
	if ttl == 0 {
		ttl = i.policy.ServerTTL
	}
	if ttl < 10*time.Minute || ttl > MaxServerTTL {
		return nil, fmt.Errorf("pki: server certificate lifetime %s out of range [10m, %s]", ttl, MaxServerTTL)
	}
	return i.issue(pub, id, ttl, dnsNames, ips, clientAuth)
}

func (i *Issuer) issueFromCSR(csrPEM []byte, id Identity, ttl time.Duration) (*Issued, error) {
	pub, err := ParseCSR(csrPEM, id)
	if err != nil {
		return nil, err
	}
	return i.issue(pub, id, ttl, nil, nil, false)
}

func (i *Issuer) issue(pub crypto.PublicKey, id Identity, ttl time.Duration, dnsNames []string, ips []net.IP, clientAuth bool) (*Issued, error) {
	ca := i.src.Current()
	if ca == nil {
		return nil, fmt.Errorf("%w: no CA loaded", ErrCAUnavailable)
	}
	if publicKeysEqual(ca.cert.PublicKey, pub) {
		return nil, fmt.Errorf("%w: the CSR carries the CA's own public key", ErrBadCSR)
	}
	now := i.clock.Now()
	if now.Before(ca.cert.NotBefore) || !now.Before(ca.cert.NotAfter) {
		return nil, fmt.Errorf("%w: issuing CA is valid %s..%s", ErrCAUnavailable, ca.cert.NotBefore.Format(time.RFC3339), ca.cert.NotAfter.Format(time.RFC3339))
	}
	notAfter := now.Add(ttl)
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter // never outlive the issuer
	}
	serial, err := randomSerial(i.rand)
	if err != nil {
		return nil, err
	}
	usage := x509.KeyUsageDigitalSignature
	if _, isRSA := pub.(*rsa.PublicKey); isRSA {
		usage |= x509.KeyUsageKeyEncipherment
	}
	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if id.Role == RoleServer {
		eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		if clientAuth {
			eku = append(eku, x509.ExtKeyUsageClientAuth)
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization:       []string{"Cucina"},
			OrganizationalUnit: []string{string(id.Role)},
			CommonName:         commonName(id),
		},
		NotBefore:             now.Add(-Backdate),
		NotAfter:              notAfter,
		KeyUsage:              usage,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
		IsCA:                  false,
		URIs:                  []*url.URL{id.URL()},
		DNSNames:              dnsNames,
		IPAddresses:           ips,
		SubjectKeyId:          keyID(pub),
		AuthorityKeyId:        ca.cert.SubjectKeyId,
	}
	der, err := x509.CreateCertificate(i.rand, tmpl, ca.cert, pub, ca.signer)
	if err != nil {
		return nil, fmt.Errorf("pki: signing %s: %w", id, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), ca.ChainPEM()...)
	return &Issued{Identity: id, Leaf: leaf, ChainPEM: chain, BundlePEM: ca.BundlePEM(), NotAfter: leaf.NotAfter}, nil
}

// commonName is a short human-readable label (identity is only the URI SAN).
func commonName(id Identity) string {
	switch id.Role {
	case RoleWorker:
		return id.InstanceID
	case RoleVM:
		return id.VM
	case RoleHost:
		return id.Serial
	case RoleServer:
		return id.Component
	}
	return string(id.Role)
}
