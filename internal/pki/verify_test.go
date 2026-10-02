// SPDX-License-Identifier: FSL-1.1-ALv2

package pki_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

// handshake runs a TLS handshake over a loopback TCP connection (kernel
// buffers: either side may abort mid-flight without deadlocking) and returns
// both errors.
func handshake(t *testing.T, server, client *tls.Config) (serverErr, clientErr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		s := tls.Server(conn, server)
		done <- s.Handshake()
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	c := tls.Client(conn, client)
	clientErr = c.Handshake()
	if clientErr == nil {
		// TLS 1.3 clients finish before the server has judged their certificate:
		// read once so a server-side rejection surfaces here too.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Read(make([]byte, 1))
	}
	_ = conn.Close()
	return <-done, clientErr
}

func tlsCert(t *testing.T, issued *pki.Issued, key any) tls.Certificate {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	c, err := tls.X509KeyPair(issued.ChainPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	require.NoError(t, err)
	return c
}

type staticCert struct{ c tls.Certificate }

func (s staticCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &s.c, nil }

// TestHostServiceMutualTLS guards the HostService admission rule (R-SEC-2,
// docs/security.md "Verification rules for Cucina's own verifiers"): only a
// host identity from the current bundle completes the mTLS handshake.
func TestHostServiceMutualTLS(t *testing.T) {
	clock := pkitest.NewClock(pkitest.Epoch)
	iss, ca := pkitest.NewIssuer(t, clock)
	foreign, _ := pkitest.NewIssuer(t, clock)
	key := pkitest.Key(t)
	srv, err := iss.IssueServer(key.Public(), "controller", []string{"hosts.cucina.example"}, nil, false, 0)
	require.NoError(t, err)
	serverCfg := pki.NewVerifier(ca, clock).MutualTLSConfig(staticCert{tlsCert(t, srv, key)}, pki.RoleHost)

	client := func(issued *pki.Issued, err error) *tls.Config {
		require.NoError(t, err)
		return &tls.Config{ServerName: "hosts.cucina.example", RootCAs: ca.Pool(), Certificates: []tls.Certificate{tlsCert(t, issued, key)}, MinVersion: tls.VersionTLS13}
	}
	for _, tc := range []struct {
		name string
		cfg  *tls.Config
		ok   bool
	}{
		{"host", client(iss.IssueHost(pkitest.CSR(t, key, nil), "H4X9K2LM7Q")), true},
		{"worker", client(iss.IssueWorker(pkitest.CSR(t, key, nil), "linux-x86-64", "i-0123456789abcdef0")), false},
		{"vm", client(iss.IssueVM(pkitest.CSR(t, key, nil), "macos-arm64", "H4X9K2LM7Q", "vm-1")), false},
		{"controller", client(iss.IssueController(key.Public())), false},
		{"host of a foreign CA", client(foreign.IssueHost(pkitest.CSR(t, key, nil), "H4X9K2LM7Q")), false},
		{"no certificate", &tls.Config{ServerName: "hosts.cucina.example", RootCAs: ca.Pool(), MinVersion: tls.VersionTLS13}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serr, _ := handshake(t, serverCfg, tc.cfg)
			if tc.ok {
				require.NoError(t, serr)
			} else {
				require.Error(t, serr)
			}
		})
	}
}

// TestPinnedTLSConfig guards first contact without a CA file (R-MAC-10 "CA
// certificate or pin"): a client holding only the CA's SPKI pin accepts the
// enrollment endpoint's chain, and nothing else.
func TestPinnedTLSConfig(t *testing.T) {
	clock := pkitest.NewClock(pkitest.Epoch)
	iss, ca := pkitest.NewIssuer(t, clock)
	_, otherCA := pkitest.NewIssuer(t, clock)
	key := pkitest.Key(t)
	srv, err := iss.IssueServer(key.Public(), "controller", []string{"enroll.cucina.example"}, nil, false, 0)
	require.NoError(t, err)
	full := tlsCert(t, srv, key)
	leafOnly := full
	leafOnly.Certificate = full.Certificate[:1]

	pin := pki.SPKIPin(ca.Certificate())
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, pin)
	for _, tc := range []struct {
		name       string
		cert       tls.Certificate
		pins       []string
		serverName string
		ok         bool
	}{
		{"pinned CA in chain", full, []string{pin}, "enroll.cucina.example", true},
		{"one of two pins (rotation)", full, []string{pki.SPKIPin(otherCA.Certificate()), pin}, "enroll.cucina.example", true},
		{"wrong pin", full, []string{pki.SPKIPin(otherCA.Certificate())}, "enroll.cucina.example", false},
		{"wrong server name", full, []string{pin}, "evil.example", false},
		{"CA missing from chain", leafOnly, []string{pin}, "enroll.cucina.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := pki.PinnedTLSConfig(tc.pins, tc.serverName, clock.Now)
			require.NoError(t, err)
			_, cerr := handshake(t, &tls.Config{Certificates: []tls.Certificate{tc.cert}, MinVersion: tls.VersionTLS13}, cfg)
			if tc.ok {
				require.NoError(t, cerr)
			} else {
				require.Error(t, cerr)
			}
		})
	}
	_, err = pki.PinnedTLSConfig([]string{"sha256:XYZ"}, "x", nil)
	require.Error(t, err)
}
