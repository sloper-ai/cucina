// SPDX-License-Identifier: FSL-1.1-ALv2

// Package l2 supervises the host-level bb_storage cache (R-CACHE-4): it keeps a
// host-local CA and the L2's VM-facing server certificate, renders the
// configuration through a Renderer (internal/bbconfig.RenderHostL2), starts
// bb_storage, restarts it on crash or configuration change, and scrapes its
// Prometheus endpoint for the hit ratio and WAN bytes (R-OBS-1, R-DATA-7).
package l2

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// ServerName is the name in the L2's VM-facing certificate (Machine.StorageServerName).
const ServerName = "cucina-host-l2"

// CAKeyName is the SecretStore key of the host-local L2 CA.
const CAKeyName = "host-l2-ca-key"

// Settings are the L2 inputs hostd knows (bbconfig.HostL2Settings subset).
type Settings struct {
	ListenAddress           string // 127.0.0.1:8991 (reached by VMs through the relay)
	UpstreamAddress         string // central worker endpoint
	UpstreamServerName      string
	CABundlePEM             []byte // Cucina CA bundle
	HostSerial              string
	CacheDir                string
	CacheSizeBytes          uint64
	MaximumMessageSizeBytes uint64
	MetricsListenAddress    string // 127.0.0.1:9991
	// Paths written by the supervisor.
	ServerCertPath, ServerKeyPath string
	ClientCertPath, ClientKeyPath string
}

// Renderer renders the bb_storage configuration (bbconfig.RenderHostL2 adapter).
type Renderer interface {
	HostL2(s Settings) ([]byte, error)
}

// Supervisor runs bb_storage.
type Supervisor struct {
	Exec    ports.Exec
	FS      ports.FS
	Secrets ports.SecretStore
	Clock   ports.Clock
	Render  Renderer
	Binary  string
	Dir     string // state dir for config and certificates (0700)
	Log     *slog.Logger

	mu       sync.Mutex
	settings *Settings
	hostCert []byte
	hostKey  []byte
	changed  chan struct{}
	stats    Stats
	restarts int
}

// Stats are the scraped L2 counters.
type Stats struct {
	Hits, Misses         uint64
	WANReceived, WANSent uint64
	SizeBytes            uint64
	Up                   bool
}

// Stats returns the latest scraped counters.
func (s *Supervisor) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Restarts is the number of bb_storage (re)starts.
func (s *Supervisor) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// Configure sets the inputs; bb_storage is (re)started when they change.
func (s *Supervisor) Configure(st Settings, hostCertPEM, hostKeyPEM []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{}, 1)
	}
	s.settings = &st
	s.hostCert, s.hostKey = hostCertPEM, hostKeyPEM
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// HostCAPEM returns the host-local CA certificate VMs must trust for the L2.
func (s *Supervisor) HostCAPEM(ctx context.Context) ([]byte, error) {
	_, certPEM, err := s.ca(ctx)
	return certPEM, err
}

// ca loads or creates the host-local CA. The certificate is derived
// deterministically from the key (fixed serial and validity), so it survives
// restarts without being stored.
func (s *Supervisor) ca(ctx context.Context) (*ecdsa.PrivateKey, []byte, error) {
	der, err := s.Secrets.Get(ctx, CAKeyName)
	var key *ecdsa.PrivateKey
	switch {
	case err == nil:
		k, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, nil, fmt.Errorf("l2 CA key: %w", err)
		}
		var ok bool
		if key, ok = k.(*ecdsa.PrivateKey); !ok {
			return nil, nil, errors.New("l2 CA key is not ECDSA")
		}
	case errors.Is(err, ports.ErrNotFound):
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, nil, err
		}
		if der, err = x509.MarshalPKCS8PrivateKey(key); err != nil {
			return nil, nil, err
		}
		if err := s.Secrets.Put(ctx, CAKeyName, der); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, err
	}
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sum := sha256.Sum256(pub)
	tmpl := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(sum[:16]),
		Subject:               pkix.Name{CommonName: "Cucina host L2 CA", Organization: []string{"Cucina"}},
		NotBefore:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2046, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), nil
}

// serverCert issues the L2's VM-facing certificate (30 days) from the host CA.
func (s *Supervisor) serverCert(ctx context.Context) (certPEM, keyPEM []byte, err error) {
	caKey, caPEM, err := s.ca(ctx)
	if err != nil {
		return nil, nil, err
	}
	b, _ := pem.Decode(caPEM)
	caCert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	now := s.Clock.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: ServerName},
		DNSNames:     []string{ServerName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), nil
}

// Run supervises bb_storage until ctx is done. It waits for Configure.
func (s *Supervisor) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.changed == nil {
		s.changed = make(chan struct{}, 1)
	}
	changed := s.changed
	s.mu.Unlock()
	if s.Log == nil {
		s.Log = slog.New(slog.DiscardHandler)
	}
	var proc ports.Process
	var exited chan struct{}
	failures := 0
	stop := func() {
		if proc != nil {
			_ = proc.Signal("TERM")
			select {
			case <-exited:
			case <-s.Clock.After(30 * time.Second):
				_ = proc.Signal("KILL")
				<-exited
			}
			proc = nil
		}
	}
	defer stop()
	start := func() {
		p, ex, err := s.start(ctx)
		if err != nil {
			failures++
			s.Log.Error("l2: start failed", "err", err)
			return
		}
		proc, exited = p, ex
	}
	scrape := s.Clock.After(15 * time.Second)
	retry := (<-chan time.Time)(nil)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
			stop()
			start()
			if proc == nil {
				retry = s.Clock.After(backoff(failures))
			}
		case <-exited:
			proc, exited = nil, nil
			failures++
			s.Log.Warn("l2: bb_storage exited; restarting", "failures", failures)
			retry = s.Clock.After(backoff(failures))
		case <-retry:
			retry = nil
			if proc == nil {
				start()
				if proc == nil {
					retry = s.Clock.After(backoff(failures))
				}
			}
		case <-scrape:
			s.scrape(ctx)
			scrape = s.Clock.After(15 * time.Second)
		}
	}
}

func backoff(n int) time.Duration {
	d := time.Second << min(n, 6)
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

func (s *Supervisor) start(ctx context.Context) (ports.Process, chan struct{}, error) {
	s.mu.Lock()
	st := s.settings
	hostCert, hostKey := s.hostCert, s.hostKey
	s.mu.Unlock()
	if st == nil {
		return nil, nil, errors.New("not configured")
	}
	cfg := *st
	if err := s.FS.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, nil, err
	}
	if err := s.FS.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, nil, err
	}
	if err := s.FS.MkdirAll(filepath.Join(cfg.CacheDir, "state"), 0o700); err != nil {
		return nil, nil, err
	}
	certPEM, keyPEM, err := s.serverCert(ctx)
	if err != nil {
		return nil, nil, err
	}
	cfg.ServerCertPath, cfg.ServerKeyPath = filepath.Join(s.Dir, "server.crt"), filepath.Join(s.Dir, "server.key")
	cfg.ClientCertPath, cfg.ClientKeyPath = filepath.Join(s.Dir, "host.crt"), filepath.Join(s.Dir, "host.key")
	for _, f := range []struct {
		path string
		data []byte
		mode uint32
	}{{cfg.ServerCertPath, certPEM, 0o644}, {cfg.ServerKeyPath, keyPEM, 0o600}, {cfg.ClientCertPath, hostCert, 0o644}, {cfg.ClientKeyPath, hostKey, 0o600}} {
		if err := s.FS.WriteFileAtomic(f.path, f.data, f.mode); err != nil {
			return nil, nil, err
		}
	}
	conf, err := s.Render.HostL2(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("rendering bb_storage config: %w", err)
	}
	confPath := filepath.Join(s.Dir, "bb_storage.json")
	if err := s.FS.WriteFileAtomic(confPath, conf, 0o600); err != nil {
		return nil, nil, err
	}
	p, err := s.Exec.Start(ctx, ports.Command{Path: s.Binary, Args: []string{confPath}})
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	s.restarts++
	s.mu.Unlock()
	exited := make(chan struct{})
	go func() {
		res, err := p.Wait(context.Background())
		s.Log.Info("l2: bb_storage exited", "exit_code", res.ExitCode, "err", err, "stderr_tail", tailString(res.Stderr, 400))
		close(exited)
	}()
	s.Log.Info("l2: bb_storage started", "pid", p.PID(), "listen", cfg.ListenAddress, "upstream", cfg.UpstreamAddress)
	return p, exited, nil
}

func tailString(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

func (s *Supervisor) scrape(ctx context.Context) {
	s.mu.Lock()
	st := s.settings
	s.mu.Unlock()
	if st == nil || st.MetricsListenAddress == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+st.MetricsListenAddress+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.mu.Lock()
		s.stats.Up = false
		s.mu.Unlock()
		return
	}
	defer func() { _ = resp.Body.Close() }()
	stats := ParseMetrics(io.LimitReader(resp.Body, 8<<20))
	stats.Up = true
	stats.SizeBytes = st.CacheSizeBytes
	s.mu.Lock()
	s.stats = stats
	s.mu.Unlock()
}

// ParseMetrics derives L2 counters from bb_storage's Prometheus text output:
// requests served = read_caching Get count; misses = grpc (upstream) Get count;
// WAN bytes = blob sizes fetched from / written to the grpc backend.
func ParseMetrics(r io.Reader) Stats {
	var st Stats
	var cachingGets, upstreamGets uint64
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "buildbarn_blob_access_operations_blob_size_bytes_") {
			continue
		}
		name, labels, value, ok := splitSample(line)
		if !ok || labels["storage_type"] != "CAS" {
			continue
		}
		switch {
		case name == "buildbarn_blob_access_operations_blob_size_bytes_count" && labels["backend_type"] == "read_caching" && labels["operation"] == "Get":
			cachingGets += uint64(value)
		case name == "buildbarn_blob_access_operations_blob_size_bytes_count" && labels["backend_type"] == "grpc" && labels["operation"] == "Get":
			upstreamGets += uint64(value)
		case name == "buildbarn_blob_access_operations_blob_size_bytes_sum" && labels["backend_type"] == "grpc" && labels["operation"] == "Get":
			st.WANReceived += uint64(value)
		case name == "buildbarn_blob_access_operations_blob_size_bytes_sum" && labels["backend_type"] == "grpc" && labels["operation"] == "Put":
			st.WANSent += uint64(value)
		}
	}
	st.Misses = upstreamGets
	if cachingGets > upstreamGets {
		st.Hits = cachingGets - upstreamGets
	}
	return st
}

// splitSample parses `name{a="b",c="d"} value`.
func splitSample(line string) (name string, labels map[string]string, value float64, ok bool) {
	labels = map[string]string{}
	var rest string
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", nil, 0, false
		}
		name = line[:i]
		for _, kv := range strings.Split(line[i+1:j], ",") {
			k, v, found := strings.Cut(kv, "=")
			if found {
				labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		rest = line[j+1:]
	} else {
		f := strings.Fields(line)
		if len(f) < 2 {
			return "", nil, 0, false
		}
		name, rest = f[0], strings.Join(f[1:], " ")
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return "", nil, 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return "", nil, 0, false
	}
	return name, labels, v, true
}
