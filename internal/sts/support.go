// SPDX-License-Identifier: FSL-1.1-ALv2

package sts

import (
	"crypto/tls"
	"os"
	"sync"
	"time"

	"github.com/maypok86/otter/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sloper-ai/cucina/internal/ports"
)

// metrics are the STS metrics (contracts §6).
type metrics struct {
	exchanges *prometheus.CounterVec
	ttl       prometheus.Histogram
}

func newMetrics(reg prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		exchanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cucina_sts_exchanges_total",
			Help: "Token exchanges at the Cucina STS by issuer (configured issuer URL, service-account or unknown) and result (ok or the OAuth error code).",
		}, []string{"issuer", "result"}),
		ttl: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "cucina_sts_token_ttl_seconds",
			Help:    "Lifetime of the Cucina JWTs issued by the STS.",
			Buckets: []float64{60, 120, 300, 600, 900},
		}),
	}
	for _, c := range []prometheus.Collector{m.exchanges, m.ttl} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// limiter is a token bucket per key (client IP or subject): `perMinute` requests per
// minute with an equal burst. Buckets live in a bounded cache (least recently used keys
// are dropped, which only ever resets a bucket to full).
type limiter struct {
	mu      sync.Mutex
	clock   ports.Clock
	rate    float64 // tokens per second
	burst   float64
	buckets *otter.Cache[string, *bucket]
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(clock ports.Clock, perMinute, maxKeys int) *limiter {
	return &limiter{
		clock: clock, rate: float64(perMinute) / 60, burst: float64(perMinute),
		buckets: otter.Must(&otter.Options[string, *bucket]{MaximumSize: maxKeys}),
	}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	b, ok := l.buckets.GetIfPresent(key)
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets.Set(key, b)
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// CertReloader serves the TLS certificate from files and re-reads them when their
// modification time or size changes (checked at most every 10 s, on handshakes), so a
// rotated Secret is picked up without a restart.
type CertReloader struct {
	certFile, keyFile string
	clock             ports.Clock
	mu                sync.Mutex
	cert              *tls.Certificate
	stamp             [2]fileStamp
	checked           time.Time
}

type fileStamp struct {
	mod  time.Time
	size int64
}

func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size()}, nil
}

// NewCertReloader loads the pair once (an error is fatal at start-up).
func NewCertReloader(certFile, keyFile string, clock ports.Clock) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile, clock: clock}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *CertReloader) load() error {
	cs, err := stampOf(r.certFile)
	if err != nil {
		return err
	}
	ks, err := stampOf(r.keyFile)
	if err != nil {
		return err
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	r.cert, r.stamp = &c, [2]fileStamp{cs, ks}
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. A failed reload keeps serving the
// previous certificate.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock.Now()
	if now.Sub(r.checked) >= 10*time.Second {
		r.checked = now
		cs, err1 := stampOf(r.certFile)
		ks, err2 := stampOf(r.keyFile)
		if err1 == nil && err2 == nil && (cs != r.stamp[0] || ks != r.stamp[1]) {
			_ = r.load()
		}
	}
	return r.cert, nil
}
