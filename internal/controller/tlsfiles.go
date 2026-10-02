// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/config"
)

// reloadEvery bounds how often certificate files are stat'ed. Secrets mounted
// as directories are swapped atomically by the kubelet; the next handshake
// after the swap (plus at most this delay) uses the new files, so rotation
// needs no restart (R-CP-5, R-SEC-2).
const reloadEvery = 10 * time.Second

func stamp(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// keyPair serves a certificate/key pair from files, reloading on change.
type keyPair struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	stamps  [2]time.Time
	checked time.Time
}

func newKeyPair(certFile, keyFile string) (*keyPair, error) {
	k := &keyPair{certFile: certFile, keyFile: keyFile}
	if _, err := k.get(); err != nil {
		return nil, err
	}
	return k, nil
}

func (k *keyPair) get() (*tls.Certificate, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	if k.cert != nil && now.Sub(k.checked) < reloadEvery {
		return k.cert, nil
	}
	k.checked = now
	c, errC := stamp(k.certFile)
	s, errK := stamp(k.keyFile)
	if err := errors.Join(errC, errK); err != nil {
		if k.cert != nil {
			return k.cert, nil // keep serving the last good pair during a swap
		}
		return nil, fmt.Errorf("tls key pair: %w", err)
	}
	if k.cert != nil && c.Equal(k.stamps[0]) && s.Equal(k.stamps[1]) {
		return k.cert, nil
	}
	pair, err := tls.LoadX509KeyPair(k.certFile, k.keyFile)
	if err != nil {
		if k.cert != nil {
			return k.cert, nil
		}
		return nil, fmt.Errorf("tls key pair %s: %w", k.certFile, err)
	}
	k.cert, k.stamps = &pair, [2]time.Time{c, s}
	return k.cert, nil
}

// caBundle serves a CA pool from a PEM bundle file (two roots during CA rotation).
type caBundle struct {
	file string

	mu      sync.Mutex
	pool    *x509.CertPool
	stamp   time.Time
	checked time.Time
}

func newCABundle(file string) (*caBundle, error) {
	b := &caBundle{file: file}
	if _, err := b.get(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *caBundle) get() (*x509.CertPool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.pool != nil && now.Sub(b.checked) < reloadEvery {
		return b.pool, nil
	}
	b.checked = now
	st, err := stamp(b.file)
	if err != nil {
		if b.pool != nil {
			return b.pool, nil
		}
		return nil, fmt.Errorf("CA bundle: %w", err)
	}
	if b.pool != nil && st.Equal(b.stamp) {
		return b.pool, nil
	}
	pem, err := os.ReadFile(b.file)
	if err != nil {
		return nil, fmt.Errorf("CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		if b.pool != nil {
			return b.pool, nil
		}
		return nil, fmt.Errorf("CA bundle %s: no PEM certificates", b.file)
	}
	b.pool, b.stamp = pool, st
	return pool, nil
}

// verifyAgainst verifies the peer chain and server name against the current CA bundle.
func verifyAgainst(ca *caBundle) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("tls: peer sent no certificate")
		}
		pool, err := ca.get()
		if err != nil {
			return err
		}
		opts := x509.VerifyOptions{Roots: pool, DNSName: cs.ServerName, Intermediates: x509.NewCertPool()}
		for _, c := range cs.PeerCertificates[1:] {
			opts.Intermediates.AddCert(c)
		}
		_, err = cs.PeerCertificates[0].Verify(opts)
		return err
	}
}

// TLSConfigs are the reloadable TLS configurations of the controller's listeners
// and of its scheduler client.
type TLSConfigs struct {
	// Server authenticates the controller only (STS, enrollment, management).
	Server *tls.Config
	// MutualServer additionally requires a client certificate issued by the
	// Cucina CA (HostService, R-SEC-2).
	MutualServer *tls.Config
	// SchedulerClient is the mTLS client identity for BuildQueueState (R-SEC-4);
	// nil when not configured.
	SchedulerClient *tls.Config
}

// NewTLSConfigs loads the configured files once (failing fast on errors) and
// returns configurations that reload them on change. The scheduler client
// identity is loaded only in ModeController: the STS never calls the
// scheduler, and its Pods do not mount the controller's client certificate.
func NewTLSConfigs(c *config.Controller, mode Mode) (*TLSConfigs, error) {
	out := &TLSConfigs{}
	var ca *caBundle
	if c.TLS.CAFile != "" {
		var err error
		if ca, err = newCABundle(c.TLS.CAFile); err != nil {
			return nil, err
		}
	}
	if c.TLS.CertFile != "" {
		kp, err := newKeyPair(c.TLS.CertFile, c.TLS.KeyFile)
		if err != nil {
			return nil, err
		}
		getCert := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return kp.get() }
		out.Server = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: getCert}
		if ca != nil {
			base := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: getCert, ClientAuth: tls.RequireAndVerifyClientCert}
			out.MutualServer = &tls.Config{
				MinVersion:     tls.VersionTLS12,
				GetCertificate: getCert,
				ClientAuth:     tls.RequireAndVerifyClientCert,
				GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
					pool, err := ca.get()
					if err != nil {
						return nil, err
					}
					cfg := base.Clone()
					cfg.ClientCAs = pool
					return cfg, nil
				},
			}
		}
	}
	if mode == ModeController && c.Scheduler.ClientCertFile != "" && ca != nil {
		kp, err := newKeyPair(c.Scheduler.ClientCertFile, c.Scheduler.ClientKeyFile)
		if err != nil {
			return nil, err
		}
		out.SchedulerClient = &tls.Config{
			MinVersion:           tls.VersionTLS12,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return kp.get() },
			// The scheduler's certificate is verified in VerifyConnection against the
			// reloadable CA bundle (two roots during a CA rotation), not a pool frozen
			// at startup.
			InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection
			VerifyConnection:   verifyAgainst(ca),
		}
	}
	return out, nil
}
