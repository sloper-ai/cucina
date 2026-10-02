// SPDX-License-Identifier: FSL-1.1-ALv2

package pki

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/sloper-ai/cucina/internal/ports"
)

// Reloader re-reads its files; Watch drives it.
type Reloader interface {
	Reload() error
	Files() []string
}

// KeyPair is a certificate/key pair loaded from files and swapped atomically on
// reload, for tls.Config.GetCertificate / GetClientCertificate (zero-downtime
// rotation: new handshakes use the new pair, established connections keep theirs).
// A reload that fails (half-written files, mismatched pair) keeps the last good pair.
type KeyPair struct {
	certFile, keyFile string
	cur               atomic.Pointer[tls.Certificate]
	onLoad            func(leaf *x509.Certificate)
}

// LoadKeyPair loads the pair once (an error is fatal: fail fast at start-up).
// onLoad, if set, is called with the leaf after every successful (re)load that
// changed the certificate (e.g. to update ExpiryTracker).
func LoadKeyPair(certFile, keyFile string, onLoad func(leaf *x509.Certificate)) (*KeyPair, error) {
	k := &KeyPair{certFile: certFile, keyFile: keyFile, onLoad: onLoad}
	if err := k.Reload(); err != nil {
		return nil, err
	}
	return k, nil
}

// Reload implements Reloader.
func (k *KeyPair) Reload() error {
	c, err := tls.LoadX509KeyPair(k.certFile, k.keyFile)
	if err != nil {
		return fmt.Errorf("pki: loading %s/%s: %w", k.certFile, k.keyFile, err)
	}
	if c.Leaf == nil {
		if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return err
		}
	}
	if old := k.cur.Load(); old != nil && bytes.Equal(old.Certificate[0], c.Certificate[0]) {
		return nil
	}
	k.cur.Store(&c)
	if k.onLoad != nil {
		k.onLoad(c.Leaf)
	}
	return nil
}

// Files implements Reloader.
func (k *KeyPair) Files() []string { return []string{k.certFile, k.keyFile} }

// Leaf returns the current leaf certificate.
func (k *KeyPair) Leaf() *x509.Certificate { return k.cur.Load().Leaf }

// GetCertificate implements tls.Config.GetCertificate.
func (k *KeyPair) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return k.cur.Load(), nil
}

// GetClientCertificate implements tls.Config.GetClientCertificate.
func (k *KeyPair) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return k.cur.Load(), nil
}

// Bundle is a PEM CA bundle file reloaded atomically (RootCAs / client CAs).
type Bundle struct {
	file string
	pool atomic.Pointer[x509.CertPool]
	raw  atomic.Pointer[[]byte]
}

// LoadBundle loads the bundle once (an error is fatal).
func LoadBundle(file string) (*Bundle, error) {
	b := &Bundle{file: file}
	if err := b.Reload(); err != nil {
		return nil, err
	}
	return b, nil
}

// Reload implements Reloader.
func (b *Bundle) Reload() error {
	data, err := os.ReadFile(b.file)
	if err != nil {
		return err
	}
	certs, err := parseCerts(data)
	if err != nil {
		return fmt.Errorf("pki: %s: %w", b.file, err)
	}
	if len(certs) == 0 {
		return fmt.Errorf("pki: %s holds no certificate", b.file)
	}
	if old := b.raw.Load(); old != nil && bytes.Equal(*old, data) {
		return nil
	}
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	b.pool.Store(pool)
	b.raw.Store(&data)
	return nil
}

// Files implements Reloader.
func (b *Bundle) Files() []string { return []string{b.file} }

// Pool returns the current pool.
func (b *Bundle) Pool() *x509.CertPool { return b.pool.Load() }

// PEM returns the current bundle bytes.
func (b *Bundle) PEM() []byte { return append([]byte(nil), *b.raw.Load()...) }

// settleDelay is how long after a file event the watcher reloads once more.
const settleDelay = 500 * time.Millisecond

// Watcher reloads every reloader whenever an entry of one of their directories
// changes (Kubernetes swaps Secret volumes atomically through the ..data
// symlink, so the directory, not the file, is watched) and at least every poll
// interval as a fallback. Reload errors are logged and the last good material is
// kept.
type Watcher struct {
	fs     *fsnotify.Watcher
	rs     []Reloader
	logger *slog.Logger
	clock  ports.Clock
	poll   time.Duration
}

// NewWatcher registers the reloaders' directories (events from this point on are
// delivered to Run).
func NewWatcher(logger *slog.Logger, clock ports.Clock, poll time.Duration, rs ...Reloader) (*Watcher, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if poll <= 0 {
		poll = time.Minute
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dirs := map[string]bool{}
	for _, r := range rs {
		for _, f := range r.Files() {
			dirs[filepath.Dir(f)] = true
		}
	}
	for d := range dirs {
		if err := fw.Add(d); err != nil {
			_ = fw.Close()
			return nil, fmt.Errorf("pki: watching %s: %w", d, err)
		}
	}
	return &Watcher{fs: fw, rs: rs, logger: logger, clock: clock, poll: poll}, nil
}

// Run processes events until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	defer func() { _ = w.fs.Close() }()
	reloadAll := func(trigger string) {
		for _, r := range w.rs {
			if err := r.Reload(); err != nil {
				w.logger.Warn("certificate reload failed; keeping the previous material", "trigger", trigger, "files", r.Files(), "error", err)
			}
		}
	}
	// settle re-runs the reload shortly after an event: the kubelet swaps a
	// Secret volume in several steps (new generation directory, then the ..data
	// rename) and kqueue (macOS) reports only the first one.
	var settle <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.fs.Events:
			if !ok {
				return errors.New("pki: file watcher closed")
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				reloadAll("fsnotify")
				settle = w.clock.After(settleDelay)
			}
		case <-settle:
			settle = nil
			reloadAll("fsnotify-settle")
		case err, ok := <-w.fs.Errors:
			if !ok {
				return errors.New("pki: file watcher closed")
			}
			w.logger.Warn("certificate file watcher error", "error", err)
		case <-w.clock.After(w.poll):
			reloadAll("poll")
		}
	}
}

// Watch is NewWatcher + Run.
func Watch(ctx context.Context, logger *slog.Logger, clock ports.Clock, poll time.Duration, rs ...Reloader) error {
	w, err := NewWatcher(logger, clock, poll, rs...)
	if err != nil {
		return err
	}
	return w.Run(ctx)
}

// ClientTLSConfig is the controller's mTLS client configuration for the
// scheduler's BuildQueueState listener: the client certificate and the server's
// trust bundle are both read per handshake, so rotations need no restart.
func ClientTLSConfig(cert *KeyPair, roots func() *x509.CertPool, serverName string, now func() time.Time) *tls.Config {
	if now == nil {
		now = time.Now
	}
	return &tls.Config{
		MinVersion:           tls.VersionTLS12,
		ServerName:           serverName,
		GetClientCertificate: cert.GetClientCertificate,
		InsecureSkipVerify:   true, //nolint:gosec // verified in VerifyConnection against the reloadable bundle.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("pki: server presented no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots: roots(), Intermediates: inter, DNSName: serverName, CurrentTime: now(),
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return err
		},
	}
}
