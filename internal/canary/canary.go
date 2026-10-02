// SPDX-License-Identifier: FSL-1.1-ALv2

// Package canary implements Cucina's synthetic canaries (R-TEST-7): one probe
// library, two cadences, shared with the e2e scenario harness (R-TEST-8d).
//
//   - The cache canary (every 5 min) mints a Cucina JWT at the STS with a
//     service key, verifies it against the published JWKS, then does an
//     AC/CAS round trip through the client endpoint: GetCapabilities,
//     BatchUpdateBlobs, FindMissingBlobs, BatchReadBlobs, UpdateActionResult,
//     GetActionResult. It never starts a worker.
//   - The execution canary (per pool, daily and after each deploy) runs a tiny
//     action with skip_cache_lookup (Bazel's --noremote_accept_cached
//     semantics) and do_not_cache on the pool's exact runner properties, so a
//     pool at zero scales out for it; the queue time it observes is a
//     cold-start sample.
//
// Results are exported as cucina_canary_* metrics (Metrics) either in-process
// (Loop) or, for one-shot runs from a CronJob or `helm test`, pushed to the
// controller's Handler. `cucina-controller canary cache|exec` is Command().
package canary

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Kinds of canary.
const (
	KindCache = "cache"
	KindExec  = "exec"
)

// Step is one timed probe step.
type Step struct {
	Name     string        `json:"name"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}

// Result is one probe run. It is the JSON pushed to the controller and the
// value recorded by e2e scenarios.
type Result struct {
	Kind     string        `json:"kind"`
	Pool     string        `json:"pool,omitempty"`
	Instance string        `json:"instance"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	Success  bool          `json:"success"`
	Error    string        `json:"error,omitempty"`
	Steps    []Step        `json:"steps"`
	// Exec canary observations (from ExecutedActionMetadata).
	QueueTime time.Duration `json:"queueTime,omitempty"`
	Worker    string        `json:"worker,omitempty"`
	// Subject of the minted token (never the token itself).
	Subject string `json:"subject,omitempty"`
	// Compressors the endpoint advertises (R-DATA-3: ZSTD expected).
	Compressors []string `json:"compressors,omitempty"`
}

// Endpoint describes how to reach the client endpoint (the Buildbarn
// frontend) and the STS.
type Endpoint struct {
	// Target is grpcs://host:port (TLS, default), grpc://host:port
	// (plaintext; local tests only) or host:port.
	Target       string
	InstanceName string
	// CAFile is a PEM bundle trusted for the endpoint and the STS (Cucina's
	// private CA); empty means the system roots.
	CAFile string
	// ServerName overrides TLS SNI/verification name.
	ServerName string
}

// TLSConfig returns the client TLS configuration for the endpoint.
func (e Endpoint) TLSConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: e.ServerName}
	if e.CAFile != "" {
		pem, err := os.ReadFile(e.CAFile)
		if err != nil {
			return nil, fmt.Errorf("canary: CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("canary: CA bundle %s holds no certificates", e.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// Dial opens a gRPC client connection to the endpoint with per-RPC bearer
// credentials (empty token: unauthenticated). The e2e security scenarios use
// it for direct REAPI calls.
func (e Endpoint) Dial(token string) (*grpc.ClientConn, error) {
	target, plaintext := e.Target, false
	switch {
	case strings.HasPrefix(target, "grpcs://"):
		target = strings.TrimPrefix(target, "grpcs://")
	case strings.HasPrefix(target, "grpc://"):
		target, plaintext = strings.TrimPrefix(target, "grpc://"), true
	}
	if target == "" {
		return nil, errors.New("canary: empty endpoint")
	}
	opts := []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16 << 20))}
	if plaintext {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		cfg, err := e.TLSConfig()
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer{token: token, secure: !plaintext}))
	}
	return grpc.NewClient(target, opts...)
}

type bearer struct {
	token  string
	secure bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearer) RequireTransportSecurity() bool { return b.secure }

// recorder accumulates steps into a Result.
type recorder struct {
	r   *Result
	now func() time.Time
}

func (rec *recorder) step(name string, fn func() error) error {
	start := rec.now()
	err := fn()
	s := Step{Name: name, Duration: rec.now().Sub(start)}
	if err != nil {
		s.Error = err.Error()
	}
	rec.r.Steps = append(rec.r.Steps, s)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (rec *recorder) finish(start time.Time, err error) Result {
	rec.r.Duration = rec.now().Sub(start)
	rec.r.Success = err == nil
	if err != nil {
		rec.r.Error = err.Error()
	}
	return *rec.r
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
