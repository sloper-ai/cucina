// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/enroll"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

type staticCert struct{ c *tls.Certificate }

func (s staticCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.c, nil }

// TestEnrollmentServiceOverTLS is the integration row (localhost only): the
// EnrollmentService behind a TLS listener with server authentication only
// (callers have no certificate yet, UC22) serves a real gRPC client that
// trusts nothing but the CA's SPKI pin; the per-source rate limit applies.
func TestEnrollmentServiceOverTLS(t *testing.T) {
	e := newEnv(t, memoryStores(), func(d *enroll.Deps) { d.Options.HostBurst = 4 })
	ctx := context.Background()

	key := pkitest.Key(t)
	issued, err := e.issuer.IssueServer(key.Public(), "controller", []string{"enroll.cucina.test"}, nil, false, 0)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	cert, err := tls.X509KeyPair(issued.ChainPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, err)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(pki.ServerTLSConfig(staticCert{&cert}))))
	e.srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	tlsCfg, err := pki.PinnedTLSConfig([]string{pki.SPKIPin(e.ca.Certificate())}, "enroll.cucina.test", e.clock.Now)
	require.NoError(t, err)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := cucinav1.NewEnrollmentServiceClient(conn)

	w, err := client.EnrollWorker(ctx, e.workerRequest(e.launch(nil), pkitest.Key(t)))
	require.NoError(t, err)
	require.Equal(t, testPool, w.GetPool())

	_, token := e.createToken("office-1", 3, 0)
	hostKey := pkitest.Key(t)
	resp, err := client.EnrollHost(ctx, e.hostRequest(token, "C02XK0AAJGH6", hostKey))
	require.NoError(t, err)
	require.Equal(t, pending, resp.GetStatus())
	_, err = e.srv.Admin().ApproveHost(ctx, "C02XK0AAJGH6", "admin@test")
	require.NoError(t, err)
	resp, err = client.EnrollHost(ctx, e.hostRequest(token, "C02XK0AAJGH6", hostKey))
	require.NoError(t, err)
	require.Equal(t, approved, resp.GetStatus())

	// The fake clock never refills the bucket: the burst of 4 is spent after two more calls.
	codesSeen := map[codes.Code]int{}
	for range 4 {
		_, err := client.EnrollHost(ctx, e.hostRequest(token, "C02XK0AAJGH6", hostKey))
		codesSeen[status.Code(err)]++
	}
	require.Equal(t, 2, codesSeen[codes.ResourceExhausted], "per-source rate limit")

	_, err = client.EnrollHost(ctx, &cucinav1.EnrollHostRequest{})
	require.Error(t, err)
}
