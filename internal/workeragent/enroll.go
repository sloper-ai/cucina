// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/workeragent/bootdata"
)

// Enroller is the agent's view of EnrollmentService.EnrollWorker.
type Enroller interface {
	EnrollWorker(ctx context.Context, req *cucinav1.EnrollWorkerRequest) (*cucinav1.EnrollWorkerResponse, error)
}

// ErrServerCertificate marks a controller certificate that does not verify
// against the boot data CA. It is never retried (no trust on first use).
var ErrServerCertificate = errors.New("enrollment server certificate rejected")

// GRPCEnroller calls EnrollmentService over TLS, trusting only the CA bundle
// from the boot data and verifying the boot data server name.
type GRPCEnroller struct {
	conn   *grpc.ClientConn
	client cucinav1.EnrollmentServiceClient

	mu        sync.Mutex
	verifyErr error // last server certificate verification failure
}

// DialEnroller prepares a client for bd.EnrollEndpoint (the connection is
// established lazily by the first call).
func DialEnroller(bd bootdata.BootData) (*GRPCEnroller, error) {
	roots, err := bd.CertPool()
	if err != nil {
		return nil, err
	}
	e := &GRPCEnroller{}
	serverName := bd.TLSServerName()
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
		// Standard verification happens in VerifyConnection (same checks as the
		// default verifier) so that a mismatch is recorded and classified as a
		// permanent refusal instead of an opaque "Unavailable".
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			err := verifyServer(cs, roots, serverName)
			e.mu.Lock()
			e.verifyErr = err
			e.mu.Unlock()
			return err
		},
	}
	conn, err := grpc.NewClient("dns:///"+bd.EnrollEndpoint, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return nil, fmt.Errorf("enrollment client: %w", err)
	}
	e.conn = conn
	e.client = cucinav1.NewEnrollmentServiceClient(conn)
	return e, nil
}

func verifyServer(cs tls.ConnectionState, roots *x509.CertPool, serverName string) error {
	if len(cs.PeerCertificates) == 0 {
		return fmt.Errorf("%w: no certificate presented", ErrServerCertificate)
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, DNSName: serverName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrServerCertificate, err)
	}
	return nil
}

// EnrollWorker implements Enroller. A server certificate verification failure
// is returned as ErrServerCertificate.
func (e *GRPCEnroller) EnrollWorker(ctx context.Context, req *cucinav1.EnrollWorkerRequest) (*cucinav1.EnrollWorkerResponse, error) {
	// verifyErr holds the result of the most recent handshake (verification of
	// the same server against the same roots is deterministic), so it also
	// classifies calls that failed fast on a connection in TRANSIENT_FAILURE.
	resp, err := e.client.EnrollWorker(ctx, req)
	if err != nil {
		e.mu.Lock()
		verr := e.verifyErr
		e.mu.Unlock()
		if verr != nil {
			return nil, verr
		}
		return nil, err
	}
	return resp, nil
}

// Close releases the connection.
func (e *GRPCEnroller) Close() error { return e.conn.Close() }

// enrollmentRefused reports whether an EnrollWorker error is a definitive
// refusal (power off immediately) rather than a transient failure (retry with
// backoff until the bootstrap deadline). The controller must use Unavailable
// (or ResourceExhausted) for "try again" conditions such as an instance that
// is not yet visible through DescribeInstances.
func enrollmentRefused(err error) bool {
	if errors.Is(err, ErrServerCertificate) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return false // network-level error before any gRPC status
	}
	switch st.Code() {
	case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.PermissionDenied,
		codes.Unauthenticated, codes.FailedPrecondition, codes.OutOfRange, codes.Unimplemented:
		return true
	default: // Unavailable, DeadlineExceeded, ResourceExhausted, Aborted, Internal, Unknown, DataLoss, Canceled
		return false
	}
}
