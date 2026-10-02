// SPDX-License-Identifier: FSL-1.1-ALv2

// Package agenttest holds the test fixtures of the worker agent's unit and
// integration tests: a throwaway CA that issues certificates the way the
// controller does, fakes for the agent's Host and Poweroff ports, an
// auto-advancing variant of the shared fake clock and valid pool settings.
// Test-only code; nothing here is used by the agent itself.
package agenttest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/workeragent"
)

// Epoch is the start of every fake clock.
var Epoch = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)

// AutoClock is the shared manual fake clock driven by the code under test:
// Sleep and After advance virtual time instead of blocking, which suits the
// single-goroutine retry loops of bootstrap (no real waiting, deterministic).
type AutoClock struct{ *fakes.Clock }

var _ ports.Clock = AutoClock{}

// NewAutoClock returns an AutoClock reading Epoch.
func NewAutoClock() AutoClock { return AutoClock{fakes.NewClock(Epoch)} }

// After implements ports.Clock.
func (c AutoClock) After(d time.Duration) <-chan time.Time {
	ch := c.Clock.After(d)
	c.Advance(d)
	return ch
}

// Sleep implements ports.Clock.
func (c AutoClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.Advance(d)
	return nil
}

// Host is a fake workeragent.Host: a fixed boot, uptime on the fake clock.
type Host struct {
	Clock  ports.Clock
	Boot   time.Time
	ID     string
	Memory uint64
}

var _ workeragent.Host = (*Host)(nil)

// BootID implements workeragent.Host.
func (h *Host) BootID() (string, error) { return h.ID, nil }

// Uptime implements workeragent.Host.
func (h *Host) Uptime() (time.Duration, error) { return h.Clock.Now().Sub(h.Boot), nil }

// MemoryBytes implements workeragent.Host.
func (h *Host) MemoryBytes() (uint64, error) { return h.Memory, nil }

// Power is a fake workeragent.Poweroff recording every request.
type Power struct {
	mu      sync.Mutex
	reasons []string
}

var _ workeragent.Poweroff = (*Power)(nil)

// PowerOff implements workeragent.Poweroff.
func (p *Power) PowerOff(_ context.Context, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reasons = append(p.reasons, reason)
	return nil
}

// Calls returns the reasons of every power-off request so far.
func (p *Power) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.reasons...)
}

// CA is a throwaway certificate authority (never written to the repository).
// It is valid at Epoch (worker certificates are checked on the fake clock) and
// in real time (TLS handshakes check the wall clock).
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	CertPEM []byte
}

var serial atomic.Int64

// NewCA creates a CA named name.
func NewCA(t testing.TB, name string) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial.Add(1)), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Unix(0, 0), NotAfter: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &CA{cert: cert, key: key, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *CA) issue(t testing.TB, pub any, tmpl *x509.Certificate) []byte {
	t.Helper()
	tmpl.SerialNumber = big.NewInt(serial.Add(1))
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// ServerCert returns a TLS server certificate for dnsName, valid in real time.
func (ca *CA) ServerCert(t testing.TB, dnsName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	certPEM := ca.issue(t, &key.PublicKey, &x509.Certificate{
		Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, err)
	return pair
}

// WorkerCert issues a worker certificate for a CSR the way the controller
// does: the CSR contributes only the public key; the URI SAN names the pool
// and node; client authentication only; valid from Epoch to notAfter.
func (ca *CA) WorkerCert(t testing.TB, csrPEM []byte, pool, node string, notAfter time.Time) []byte {
	t.Helper()
	block, _ := pem.Decode(csrPEM)
	require.NotNil(t, block)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	require.NoError(t, err)
	require.NoError(t, csr.CheckSignature())
	uri, err := url.Parse("spiffe://cucina/worker/" + pool + "/" + node)
	require.NoError(t, err)
	return ca.issue(t, csr.PublicKey, &x509.Certificate{
		Subject: pkix.Name{CommonName: node}, URIs: []*url.URL{uri},
		NotBefore: Epoch.Add(-time.Minute), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

// NewCSR returns a CSR for a fresh key nobody else holds.
func NewCSR(t testing.TB) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// Settings returns valid linux-x86-64 pool settings (what internal/pools
// sends at enrollment), without a node.
func Settings() *cucinav1.WorkerSettings {
	return &cucinav1.WorkerSettings{
		Pool:                    "linux-x86-64",
		SchedulerEndpoint:       "scheduler.cucina.test:8983",
		StorageEndpoint:         "storage.cucina.test:8981",
		ServerName:              "cucina.test",
		BuildDirectory:          "fuse",
		L1Placement:             "auto",
		MaximumMessageSizeBytes: 16 << 20,
		SizeClass:               1,
		InstanceNamePrefixes:    []string{"main"},
		MetricsPort:             9980,
		Runners: []*cucinav1.RunnerSettings{{
			Name: "native",
			Platform: []*cucinav1.PlatformProperty{
				{Name: "ISA", Value: "x86-64"}, {Name: "OSFamily", Value: "linux"},
			},
		}},
		Deadman: &cucinav1.DeadmanSettings{
			IdleLimit: durationpb.New(30 * time.Minute), UnreachableLimit: durationpb.New(10 * time.Minute),
			MaxUptime: durationpb.New(12 * time.Hour),
		},
		HandleSpotInterruption: true,
	}
}
