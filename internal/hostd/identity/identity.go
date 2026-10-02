// SPDX-License-Identifier: FSL-1.1-ALv2

// Package identity manages the host's mTLS identity (R-SEC-2/-3): the private
// key in the SecretStore (System keychain), the one-time site-token enrollment
// with polling while pending, certificate renewal before expiry, and the TLS
// configurations for the enrollment endpoint (CA or pin verification) and the
// HostService (mTLS).
package identity

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v7"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/hostd/config"
	"github.com/sloper-ai/cucina/internal/ports"
	cproto "github.com/sloper-ai/cucina/internal/proto"
)

// KeyName is the SecretStore key of the host identity private key (PKCS#8 DER).
const KeyName = "host-identity-key"

// StateFile is the (non-secret) enrollment state under the state directory.
const StateFile = "identity.json"

// Errors that end enrollment permanently (hostd exits with code 3).
var (
	ErrDenied       = errors.New("enrollment denied: the serial is revoked or already enrolled (an admin must run `cucinactl hosts remove` and approve again)")
	ErrTokenInvalid = errors.New("site enrollment token rejected (expired, revoked, wrong site or host limit reached)")
	ErrNoToken      = errors.New("not enrolled and no SiteEnrollmentToken configured")
	ErrExpired      = errors.New("host certificate expired; an admin must re-admit this host (cucinactl hosts remove + approve) and its identity state must be reset")
)

// State is the persisted enrollment result. It holds no secret.
type State struct {
	Serial       string    `json:"serial"`
	CertPEM      []byte    `json:"certPem"`
	CAPEM        []byte    `json:"caPem"`
	HostEndpoint string    `json:"hostEndpoint"`
	ExpiresAt    time.Time `json:"expiresAt"`
	EnrolledAt   time.Time `json:"enrolledAt"`
}

// Store persists the key and state.
type Store struct {
	Secrets  ports.SecretStore
	FS       ports.FS
	StateDir string
}

// Key loads the identity key or creates (and stores) a new ECDSA P-256 key.
func (s Store) Key(ctx context.Context) (*ecdsa.PrivateKey, error) { return s.KeyNamed(ctx, KeyName) }

// KeyNamed loads or creates the ECDSA P-256 key stored under name.
func (s Store) KeyNamed(ctx context.Context, name string) (*ecdsa.PrivateKey, error) {
	der, err := s.Secrets.Get(ctx, name)
	if err == nil {
		k, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, fmt.Errorf("identity key in secret store is corrupt: %w", err)
		}
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("identity key in secret store is not ECDSA")
		}
		return ek, nil
	}
	if !errors.Is(err, ports.ErrNotFound) {
		return nil, fmt.Errorf("reading identity key: %w", err)
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err = x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := s.Secrets.Put(ctx, name, der); err != nil {
		return nil, fmt.Errorf("storing identity key: %w", err)
	}
	return k, nil
}

// Load returns the persisted state, or nil when the host is not enrolled.
func (s Store) Load() (*State, error) {
	b, err := s.FS.ReadFile(filepath.Join(s.StateDir, StateFile))
	if err != nil {
		if ok, _ := s.FS.Exists(filepath.Join(s.StateDir, StateFile)); !ok {
			return nil, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("identity state is corrupt: %w", err)
	}
	return &st, nil
}

// Save persists the state atomically.
func (s Store) Save(st *State) error {
	if err := s.FS.MkdirAll(s.StateDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return s.FS.WriteFileAtomic(filepath.Join(s.StateDir, StateFile), b, 0o600)
}

// CSR builds a PKCS#10 request for key. The issuer decides every certificate
// field; the CSR only contributes the public key (CN is informational).
func CSR(key crypto.Signer, cn string) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// NewVMKey generates a VM worker key (generated on the host, R-MAC-4) and returns it with its PKCS#8 PEM.
func NewVMKey() (*ecdsa.PrivateKey, []byte, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, nil, err
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseLeaf parses the first certificate of a PEM bundle.
func ParseLeaf(certPEM []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

// NeedsRenewal is true from two thirds of the certificate's lifetime on.
func NeedsRenewal(cert *x509.Certificate, now time.Time) bool {
	life := cert.NotAfter.Sub(cert.NotBefore)
	return !now.Before(cert.NotBefore.Add(life * 2 / 3))
}

// EnrollParams are the inputs of the one-time enrollment.
type EnrollParams struct {
	Token    string
	Serial   string
	Hostname string
	CSRPEM   []byte
	Facts    *cucinav1.HostFacts
}

// Enroll exchanges the site token once (R-SEC-3) and polls while the serial is
// pending approval, honouring retry_after. Transient RPC errors are retried
// with exponential backoff; DENIED and TOKEN_INVALID are permanent.
func Enroll(ctx context.Context, client cucinav1.EnrollmentServiceClient, p EnrollParams, clock ports.Clock, log *slog.Logger) (*cucinav1.EnrollHostResponse, error) {
	if p.Token == "" {
		return nil, ErrNoToken
	}
	req := &cucinav1.EnrollHostRequest{
		Protocol:     &cucinav1.ProtocolVersion{Major: cproto.Major, Minor: cproto.Minor},
		SiteToken:    p.Token,
		SerialNumber: p.Serial,
		Hostname:     p.Hostname,
		CsrPem:       p.CSRPEM,
		Facts:        p.Facts,
	}
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 2 * time.Second
	bo.MaxInterval = 2 * time.Minute
	for {
		resp, err := client.EnrollHost(ctx, req)
		if err != nil {
			if !Transient(err) {
				return nil, fmt.Errorf("enrollment refused: %w", err)
			}
			wait := bo.NextBackOff()
			log.Warn("enrollment endpoint unavailable; retrying", "err", status.Code(err).String(), "retry_in", wait)
			if err := clock.Sleep(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		bo.Reset()
		switch resp.GetStatus() {
		case cucinav1.EnrollHostResponse_STATUS_APPROVED:
			if len(resp.GetCertificatePem()) == 0 || len(resp.GetCaPem()) == 0 {
				return nil, errors.New("enrollment approved without certificate")
			}
			return resp, nil
		case cucinav1.EnrollHostResponse_STATUS_PENDING:
			wait := resp.GetRetryAfter().AsDuration()
			if wait < 5*time.Second {
				wait = 30 * time.Second
			}
			if wait > 10*time.Minute {
				wait = 10 * time.Minute
			}
			log.Info("enrollment pending approval (cucinactl hosts approve <serial>)", "serial", p.Serial, "retry_in", wait)
			if err := clock.Sleep(ctx, wait); err != nil {
				return nil, err
			}
		case cucinav1.EnrollHostResponse_STATUS_DENIED:
			return nil, fmt.Errorf("%w: %s", ErrDenied, resp.GetMessage())
		case cucinav1.EnrollHostResponse_STATUS_TOKEN_INVALID:
			return nil, fmt.Errorf("%w: %s", ErrTokenInvalid, resp.GetMessage())
		default:
			return nil, fmt.Errorf("enrollment: unexpected status %v", resp.GetStatus())
		}
	}
}

// Transient reports whether a gRPC error is worth retrying.
func Transient(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted, codes.Internal, codes.Unknown:
		return true
	}
	return false
}

// EnrollTLS verifies the controller with the configured CA certificates, or
// with SPKI pins (any certificate of the presented chain must match a pin and
// the leaf must chain to it and match the server name).
func EnrollTLS(cfg config.Config) *tls.Config {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName()}
	if len(cfg.CACertificates) > 0 {
		pool := x509.NewCertPool()
		for _, c := range cfg.CACertificates {
			pool.AddCert(c)
		}
		tc.RootCAs = pool
		return tc
	}
	pins := cfg.CAPins
	name := cfg.ServerName()
	tc.InsecureSkipVerify = true // replaced by the pin verification below
	tc.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
		certs := make([]*x509.Certificate, 0, len(raw))
		for _, r := range raw {
			c, err := x509.ParseCertificate(r)
			if err != nil {
				return err
			}
			certs = append(certs, c)
		}
		if len(certs) == 0 {
			return errors.New("no server certificate")
		}
		roots := x509.NewCertPool()
		inter := x509.NewCertPool()
		matched := false
		for _, c := range certs {
			pin := config.SPKIPin(c)
			for _, p := range pins {
				if p == pin {
					matched = true
					roots.AddCert(c)
				}
			}
			inter.AddCert(c)
		}
		if !matched {
			return errors.New("controller certificate chain matches no configured CAPinSHA256")
		}
		_, err := certs[0].Verify(x509.VerifyOptions{DNSName: name, Roots: roots, Intermediates: inter})
		if err != nil && pinMatchesLeaf(certs[0], pins) {
			// A pinned leaf is accepted as is (self-signed dev controllers).
			return certs[0].VerifyHostname(name)
		}
		return err
	}
	return tc
}

func pinMatchesLeaf(c *x509.Certificate, pins [][32]byte) bool {
	pin := config.SPKIPin(c)
	for _, p := range pins {
		if p == pin {
			return true
		}
	}
	return false
}

// Holder serves the current client certificate to TLS handshakes so renewals
// apply to the next (re)connection without restarting hostd.
type Holder struct {
	mu   sync.RWMutex
	cert *tls.Certificate
	ca   *x509.CertPool
	leaf *x509.Certificate
	// reload, when set, re-reads the certificate at every handshake (an
	// MDM-issued keychain identity that MDM renews).
	reload func() (*tls.Certificate, error)
}

// SetKeychain serves a keychain identity (re-read at every handshake) with caPEM.
func (h *Holder) SetKeychain(load func() (*tls.Certificate, error), caPEM []byte) error {
	c, err := load()
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("CA bundle has no certificate")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cert, h.ca, h.leaf, h.reload = c, pool, c.Leaf, load
	return nil
}

// Set installs a new certificate (PEM chain) for key and the CA bundle.
func (h *Holder) Set(certPEM, caPEM []byte, key crypto.Signer) error {
	var chain [][]byte
	rest := certPEM
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type == "CERTIFICATE" {
			chain = append(chain, b.Bytes)
		}
	}
	if len(chain) == 0 {
		return errors.New("certificate PEM has no certificate")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("CA bundle has no certificate")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cert = &tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}
	h.ca = pool
	h.leaf = leaf
	return nil
}

// Leaf returns the current leaf certificate.
func (h *Holder) Leaf() *x509.Certificate {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.leaf
}

// ClientTLS is the mTLS configuration for HostService.
func (h *Holder) ClientTLS(serverName string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			h.mu.RLock()
			reload := h.reload
			h.mu.RUnlock()
			if reload != nil {
				if c, err := reload(); err == nil {
					h.mu.Lock()
					h.cert, h.leaf = c, c.Leaf
					h.mu.Unlock()
				}
			}
			h.mu.RLock()
			defer h.mu.RUnlock()
			if h.cert == nil {
				return nil, errors.New("no host certificate")
			}
			return h.cert, nil
		},
		// The CA pool can change on renewal; verify against the current pool.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			h.mu.RLock()
			pool := h.ca
			h.mu.RUnlock()
			if pool == nil {
				return errors.New("no CA bundle")
			}
			certs := make([]*x509.Certificate, 0, len(raw))
			for _, r := range raw {
				c, err := x509.ParseCertificate(r)
				if err != nil {
					return err
				}
				certs = append(certs, c)
			}
			if len(certs) == 0 {
				return errors.New("no server certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range certs[1:] {
				inter.AddCert(c)
			}
			_, err := certs[0].Verify(x509.VerifyOptions{DNSName: serverName, Roots: pool, Intermediates: inter})
			return err
		},
	}
}
