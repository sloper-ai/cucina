// SPDX-License-Identifier: FSL-1.1-ALv2

// Package pkitest holds test helpers for code that consumes internal/pki: a
// manual clock, throwaway CAs and CSR builders. Test-only; never import it from
// production code.
package pkitest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/sloper-ai/cucina/internal/pki"
)

// Epoch is a fixed, arbitrary test time.
var Epoch = time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)

// Clock is a manual ports.Clock: time moves only through Advance.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// NewClock returns a clock at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now implements ports.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After implements ports.Clock; the channel fires once Advance passes now+d.
func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Sleep implements ports.Clock.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.After(d):
		return nil
	}
}

// Advance moves time forward and fires due timers.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}

// Set jumps to t (forward only) and fires due timers.
func (c *Clock) Set(t time.Time) { c.Advance(t.Sub(c.Now())) }

// CAData returns fresh CA Secret data (ca.crt, ca.key) valid at now.
func CAData(t testing.TB, now time.Time) map[string][]byte {
	t.Helper()
	d, err := pki.NewCAMaterial(now, 0, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// NewCA returns a fresh CA valid at now.
func NewCA(t testing.TB, now time.Time) *pki.CA {
	t.Helper()
	ca, err := pki.ParseCA(CAData(t, now))
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// NewIssuer returns an issuer over a fresh CA with the default policy.
func NewIssuer(t testing.TB, clock *Clock) (*pki.Issuer, *pki.CA) {
	t.Helper()
	ca := NewCA(t, clock.Now())
	iss, err := pki.NewIssuer(ca, clock, pki.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	return iss, ca
}

// Key returns a fresh ECDSA P-256 key.
func Key(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// CSR returns a PEM CSR built from tmpl and signed by key.
func CSR(t testing.TB, key crypto.Signer, tmpl *x509.CertificateRequest) []byte {
	t.Helper()
	if tmpl == nil {
		tmpl = &x509.CertificateRequest{}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// CSRFor returns a clean CSR for id (subject CN + the identity URI SAN), the
// shape Cucina's own clients produce.
func CSRFor(t testing.TB, key crypto.Signer, id pki.Identity) []byte {
	t.Helper()
	u, err := url.Parse(id.String())
	if err != nil {
		t.Fatal(err)
	}
	return CSR(t, key, &x509.CertificateRequest{Subject: pkix.Name{CommonName: id.String()}, URIs: []*url.URL{u}})
}

// ParseChain parses a PEM chain (leaf first).
func ParseChain(t testing.TB, pemData []byte) []*x509.Certificate {
	t.Helper()
	var out []*x509.Certificate
	for {
		var b *pem.Block
		b, pemData = pem.Decode(pemData)
		if b == nil {
			return out
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
}
