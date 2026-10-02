// SPDX-License-Identifier: FSL-1.1-ALv2

package enroll_test

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/bbtest"
	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

func keyPairFiles(t *testing.T, dir, name string, chainPEM []byte, key crypto.Signer) bbtest.KeyPair {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	kp := bbtest.KeyPair{CertPath: filepath.Join(dir, name+".crt"), KeyPath: filepath.Join(dir, name+".key"), CertPEM: chainPEM,
		KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}
	require.NoError(t, os.WriteFile(kp.CertPath, kp.CertPEM, 0o600))
	require.NoError(t, os.WriteFile(kp.KeyPath, kp.KeyPEM, 0o600))
	kp.TLS, err = tls.X509KeyPair(kp.CertPEM, kp.KeyPEM)
	require.NoError(t, err)
	return kp
}

// TestT10hSchedulerRejectsWorkersWithoutValidCertificate is campaign case
// T10(h) at the integration tier: the pinned bb_scheduler, configured like the
// chart (worker listener: tlsClientCertificate with Cucina's CA bundle and the
// documented validation expression, synchronize authorizer), lets a worker
// register with the certificate EnrollWorker issued and a VM with the one its
// host obtained, and refuses a host certificate, a certificate of another CA
// and a caller without certificate (R-SEC-2, UC22).
func TestT10hSchedulerRejectsWorkersWithoutValidCertificate(t *testing.T) {
	_ = bbtest.Binary(t, bbtest.EnvScheduler) // skip early under `go test` without the pinned binary
	e := newEnvAt(t, memoryStores(), time.Now().UTC().Truncate(time.Second), nil)
	foreign := newEnvAt(t, memoryStores(), e.clock.Now(), nil)
	ctx := context.Background()
	dir := t.TempDir()

	// Certificates from the real enrollment flows.
	workerKey := pkitest.Key(t)
	w, err := e.srv.EnrollWorker(ctx, e.workerRequest(e.launch(nil), workerKey))
	require.NoError(t, err)
	_, err = e.srv.Admin().RegisterSerials(ctx, &cucinav1.RegisterHostSerialsRequest{Serials: []string{"H4X9K2LM7Q"}}, "admin@test")
	require.NoError(t, err)
	_, token := e.createToken("office-1", 1, 0)
	hostKey := pkitest.Key(t)
	h := e.enrollHost(e.hostRequest(token, "H4X9K2LM7Q", hostKey))
	require.Equal(t, approved, h.GetStatus())
	vmKey := pkitest.Key(t)
	vm, err := e.srv.IssueVMIdentity(ctx, hostIdentity(t, "H4X9K2LM7Q"), &cucinav1.IssueVMIdentityRequest{VmName: "vm-1", Pool: "macos-arm64", CsrPem: pkitest.CSR(t, vmKey, nil)})
	require.NoError(t, err)
	foreignKey := pkitest.Key(t)
	fw, err := foreign.srv.EnrollWorker(ctx, foreign.workerRequest(foreign.launch(nil), foreignKey))
	require.NoError(t, err)

	srvKey := pkitest.Key(t)
	srv, err := e.issuer.IssueServer(srvKey.Public(), "scheduler", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, false, 0)
	require.NoError(t, err)
	clientAddr, workerAddr, bqsAddr := bbtest.FreeAddr(t), bbtest.FreeAddr(t), bbtest.FreeAddr(t)
	cfg := bbtest.SchedulerConfig(bbtest.SchedulerOptions{
		ClientListen: clientAddr, WorkerListen: workerAddr, BuildQueueStateListen: bqsAddr,
		ServerKeyPair: ptr(keyPairFiles(t, dir, "scheduler", srv.ChainPEM, srvKey)), CAPEM: string(e.ca.BundlePEM()),
		StorageAddress: bbtest.FreeAddr(t),
		Queues:         []bbtest.Queue{{InstanceNamePrefix: "main", Properties: map[string]string{"OSFamily": "linux", "ISA": "x86-64"}, SizeClasses: []uint32{0}}},
	})
	require.Contains(t, string(cfg), pki.BuildbarnSchedulerWorkerValidation, "bbtest renders the documented expression")
	bbtest.BootScheduler(t, cfg, bbtest.GRPCReady(clientAddr, nil), bbtest.WithDir(dir))

	register := func(cert *tls.Certificate) error {
		tlsCfg := &tls.Config{RootCAs: e.ca.Pool(), ServerName: "localhost", MinVersion: tls.VersionTLS12}
		if cert != nil {
			tlsCfg.Certificates = []tls.Certificate{*cert}
		}
		conn, err := bbtest.Dial(workerAddr, tlsCfg)
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return bbtest.NewFakeWorker(conn, map[string]string{"pool": "p", "node": "n", "thread": "0"}, "main",
			map[string]string{"OSFamily": "linux", "ISA": "x86-64"}, 0).Register(cctx)
	}
	pair := func(chain []byte, key crypto.Signer) *tls.Certificate {
		kp := keyPairFiles(t, dir, "client", chain, key)
		return &kp.TLS
	}
	require.NoError(t, register(pair(w.GetCertificatePem(), workerKey)), "EC2 worker certificate from EnrollWorker")
	require.NoError(t, register(pair(vm.GetCertificatePem(), vmKey)), "VM certificate from IssueVMIdentity")
	for name, cert := range map[string]*tls.Certificate{
		"host certificate":           pair(h.GetCertificatePem(), hostKey),
		"worker of another CA":       pair(fw.GetCertificatePem(), foreignKey),
		"no certificate (anonymous)": nil,
	} {
		err := register(cert)
		require.Error(t, err, name)
		require.Equal(t, codes.Unauthenticated, status.Code(err), "%s: %v", name, err)
	}
}

func ptr[T any](v T) *T { return &v }
