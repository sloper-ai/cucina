// SPDX-License-Identifier: FSL-1.1-ALv2

// Package sts is the Cucina security token service (R-AUTH-1): a stateless HTTPS
// service in the controller binary that exchanges external identities (OIDC ID tokens,
// GitHub Actions JWTs, service-account keys) for 15-minute Cucina JWTs per RFC 8693.
//
//	GET  /.well-known/cucina-configuration   discovery (contracts §5.1)
//	POST /token                              token exchange
//	GET  /jwks.json                          public signing keys
//	GET  /-/healthy, /-/ready                probes
//
// Every exchange is audited (subject, issuer, policies, granted scopes, result; never
// token values) and counted in cucina_sts_exchanges_total{issuer,result}. Requests are
// rate limited per client IP and per subject.
package sts

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/keys"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Deps are the STS dependencies.
type Deps struct {
	Config config.Controller
	Engine *auth.Engine
	Keys   *keys.Manager
	Clock  ports.Clock
	// Log is the operational log; Audit receives one JSON record per exchange.
	Log   *slog.Logger
	Audit *slog.Logger
	// Registerer receives the STS metrics (nil: a private registry).
	Registerer prometheus.Registerer
	// TrustedProxies are the peers whose X-Forwarded-For is believed for rate limiting.
	TrustedProxies []netip.Prefix
}

// Server serves the STS.
type Server struct {
	d          Deps
	issuer     string
	ipLimit    *limiter
	subLimit   *limiter
	metrics    *metrics
	instances  map[string]bool
	mux        *http.ServeMux
	shutdownTO time.Duration
}

// New validates the configuration and builds the server.
func New(d Deps) (*Server, error) {
	if d.Engine == nil || d.Keys == nil || d.Clock == nil {
		return nil, errors.New("sts: Engine, Keys and Clock are required")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Audit == nil {
		d.Audit = d.Log.With("log", "audit")
	}
	issuer := strings.TrimRight(d.Config.Endpoints.STSURL, "/")
	if !strings.HasPrefix(issuer, "https://") {
		return nil, errors.New("sts: endpoints.stsUrl must be an https:// URL")
	}
	if len(d.Config.InstanceNames) == 0 {
		return nil, errors.New("sts: at least one instance name is required")
	}
	perMin := d.Config.Auth.RateLimitPerMinute
	if perMin <= 0 {
		perMin = 60
	}
	reg := d.Registerer
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m, err := newMetrics(reg)
	if err != nil {
		return nil, err
	}
	s := &Server{
		d: d, issuer: issuer, metrics: m,
		ipLimit:    newLimiter(d.Clock, perMin, 100_000),
		subLimit:   newLimiter(d.Clock, perMin, 100_000),
		instances:  map[string]bool{},
		shutdownTO: 10 * time.Second,
	}
	for _, n := range d.Config.InstanceNames {
		s.instances[n] = true
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/.well-known/cucina-configuration", s.handleDiscovery)
	s.mux.HandleFunc("/token", s.handleToken)
	s.mux.HandleFunc("/jwks.json", s.handleJWKS)
	s.mux.HandleFunc("/-/healthy", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	s.mux.HandleFunc("/-/ready", s.handleReady)
	return s, nil
}

// Issuer is the `iss` of minted tokens.
func (s *Server) Issuer() string { return s.issuer }

// Handler returns the HTTP handler (tests and alternative listeners).
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		s.mux.ServeHTTP(w, r)
	})
}

// Ready reports whether the server can mint tokens (signing keys loaded).
func (s *Server) Ready() bool {
	ks := s.d.Keys.Ring.KeySet()
	if ks == nil {
		return false
	}
	_, _, ok := ks.Signing()
	return ok
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !s.Ready() {
		http.Error(w, "signing keys not loaded", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// discoveryDoc is GET /.well-known/cucina-configuration (contracts §5.1).
type discoveryDoc struct {
	Version           int                  `json:"version"`
	Issuer            string               `json:"issuer"`
	TokenEndpoint     string               `json:"token_endpoint"`
	JWKSURI           string               `json:"jwks_uri"`
	Endpoints         discoveryEndpoints   `json:"endpoints"`
	IdentityProviders []auth.LoginProvider `json:"identity_providers"`
}

type discoveryEndpoints struct {
	RemoteExecution string `json:"remote_execution"`
	InstanceName    string `json:"instance_name"`
	Management      string `json:"management"`
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	idps := s.d.Engine.LoginProviders()
	if idps == nil {
		idps = []auth.LoginProvider{}
	}
	doc := discoveryDoc{
		Version: 1, Issuer: s.issuer, TokenEndpoint: s.issuer + "/token", JWKSURI: s.issuer + "/jwks.json",
		Endpoints: discoveryEndpoints{
			RemoteExecution: s.d.Config.Endpoints.ClientEndpoint,
			InstanceName:    s.d.Config.InstanceNames[0],
			Management:      s.d.Config.Endpoints.ManagementURL,
		},
		IdentityProviders: idps,
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	ks := s.d.Keys.Ring.KeySet()
	if ks == nil {
		http.Error(w, "signing keys not loaded", http.StatusServiceUnavailable)
		return
	}
	doc, err := ks.JWKSJSON()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_, _ = w.Write(doc)
}

func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method || (method == http.MethodGet && r.Method == http.MethodHead) {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// Run serves HTTPS (HTTP/2 and HTTP/1.1, TLS 1.2+) on listeners.sts with the
// certificate from tls.certFile/keyFile, re-read when the files change, until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	reloader, err := NewCertReloader(s.d.Config.TLS.CertFile, s.d.Config.TLS.KeyFile, s.d.Clock)
	if err != nil {
		return fmt.Errorf("sts: loading TLS certificate: %w", err)
	}
	ln, err := net.Listen("tcp", s.d.Config.Listeners.STS)
	if err != nil {
		return fmt.Errorf("sts: listen: %w", err)
	}
	return s.Serve(ctx, ln, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: reloader.GetCertificate})
}

// Serve runs the server on an existing listener with the given TLS configuration.
func (s *Server) Serve(ctx context.Context, ln net.Listener, tlsCfg *tls.Config) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(s.d.Log.Handler(), slog.LevelWarn),
	}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetHTTP2(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), s.shutdownTO)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.ServeTLS(ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}
