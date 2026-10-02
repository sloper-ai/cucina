// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/sloper-ai/cucina/internal/ports"
)

// SchedulerProbe checks that the scheduler's worker endpoint is reachable.
type SchedulerProbe interface {
	// Probe returns nil when the scheduler answered.
	Probe(ctx context.Context) error
}

// GRPCSchedulerProbe dials the scheduler worker endpoint with the worker's
// mTLS identity and calls grpc.health.v1.Health/Check (every Buildbarn gRPC
// server registers it). Any answer from the server counts as contact — also
// a refusal such as PermissionDenied: the switch detects an orphaned worker,
// not authorization problems (those leave the worker idle, which the idle
// limit catches).
type GRPCSchedulerProbe struct {
	conn *grpc.ClientConn
}

// NewGRPCSchedulerProbe builds the probe from the files bootstrap wrote.
func NewGRPCSchedulerProbe(fs ports.FS, endpoint, serverName, certFile, keyFile, caFile string) (*GRPCSchedulerProbe, error) {
	certPEM, err := fs.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := fs.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("worker key pair: %w", err)
	}
	caPEM, err := fs.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA bundle holds no certificate")
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
		ServerName:   serverName,
		Certificates: []tls.Certificate{pair},
	}
	conn, err := grpc.NewClient("dns:///"+endpoint, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return nil, err
	}
	return &GRPCSchedulerProbe{conn: conn}, nil
}

// Probe implements SchedulerProbe.
func (p *GRPCSchedulerProbe) Probe(ctx context.Context) error {
	_, err := healthpb.NewHealthClient(p.conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.Unimplemented, codes.NotFound, codes.PermissionDenied, codes.Unauthenticated:
		return nil // the server is alive and answered
	}
	return err
}

// Close releases the connection.
func (p *GRPCSchedulerProbe) Close() error { return p.conn.Close() }
