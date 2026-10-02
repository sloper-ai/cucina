// SPDX-License-Identifier: FSL-1.1-ALv2

package boottest_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sloper-ai/cucina/internal/bbconfig"
	"github.com/sloper-ai/cucina/internal/bbtest"
)

// TestHostL2 boots the pinned bb_storage with the rendered host L2 (NEW
// schema) between a VM and an upstream bb_storage standing in for the central
// worker endpoint (R-CACHE-4, R-DATA-3, R-SEC-2):
//   - only this host's VM certificates are admitted;
//   - writes pass through to the central storage (with the host identity);
//   - reads are cached on the host: once fetched, a blob is served with the
//     WAN link down, also after the L2 restarts (persistent state).
func TestHostL2(t *testing.T) {
	pki := bbtest.NewPKI(t)
	central := pki.LoopbackServer(t, "central", "spiffe://cucina/server/frontend")
	l2Server := pki.LoopbackServer(t, "l2", "")
	host := pki.Workload(t, "host", hostURI)
	vm := pki.Workload(t, "vm", workerURI)
	foreignVM := pki.Workload(t, "foreign-vm", "spiffe://cucina/worker/macos/OTHERHOST01/vm-1")

	upClient, upWorker := bbtest.FreeAddr(t), bbtest.FreeAddr(t)
	upstream := bbtest.BootStorage(t, bbtest.StorageConfig(bbtest.StorageOptions{
		ClientListen: upClient, WorkerListen: upWorker, ServerKeyPair: &central, CAPEM: pki.CAPEM, Compression: true,
	}), bbtest.AllReady(bbtest.GRPCReady(upClient, nil), bbtest.GRPCReady(upWorker, pki.ClientTLS(nil, "localhost"))))

	cacheDir := filepath.Join(t.TempDir(), "l2")
	l2Addr := bbtest.FreeAddr(t)
	settings := bbconfig.HostL2Settings{
		ListenAddresses:         []string{l2Addr},
		ServerCertificatePath:   l2Server.CertPath,
		ServerPrivateKeyPath:    l2Server.KeyPath,
		CABundlePEM:             pki.CAPEM,
		HostSerial:              "TESTSERIAL01",
		UpstreamAddress:         upWorker,
		UpstreamServerName:      "localhost",
		ClientCertificatePath:   host.CertPath,
		ClientPrivateKeyPath:    host.KeyPath,
		CacheDir:                cacheDir,
		CacheSizeBytes:          64 * mib,
		MaximumMessageSizeBytes: 16 << 20,
		MetricsListenAddress:    bbtest.FreeAddr(t),
	}
	for _, d := range bbconfig.HostL2Directories(settings) {
		require.NoError(t, os.MkdirAll(d.Path, d.Mode))
	}
	config, err := bbconfig.RenderHostL2(settings)
	require.NoError(t, err)
	vmTLS := pki.ClientTLS(&vm, "localhost")
	l2 := bbtest.BootStorage(t, config, bbtest.GRPCReady(l2Addr, vmTLS))

	client := func(kp *bbtest.KeyPair, addr string) *bbtest.REClient {
		conn, err := bbtest.Dial(addr, pki.ClientTLS(kp, "localhost"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return bbtest.NewREClient(conn, "main")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	viaL2 := client(&vm, l2Addr)
	conn, err := bbtest.Dial(upClient, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	direct := bbtest.NewREClient(conn, "main")

	// A VM's write goes through to the central storage.
	written := []byte("written by a VM through the host L2")
	_, err = viaL2.Upload(ctx, bbtest.Action{Args: []string{"true"}, Inputs: map[string][]byte{"f": written}})
	require.NoError(t, err)
	got, err := direct.Read(ctx, bbtest.DigestOf(written))
	require.NoError(t, err)
	assert.Equal(t, written, got)

	// A blob that only the central storage has is fetched once and then
	// served by the host, even with the WAN link down and after a restart.
	central2 := []byte("uploaded by another client to the central storage")
	_, err = direct.Upload(ctx, bbtest.Action{Args: []string{"true"}, Inputs: map[string][]byte{"g": central2}})
	require.NoError(t, err)
	got, err = viaL2.Read(ctx, bbtest.DigestOf(central2))
	require.NoError(t, err)
	assert.Equal(t, central2, got)

	upstream.Stop()
	got, err = viaL2.Read(ctx, bbtest.DigestOf(central2))
	require.NoError(t, err, "a cached blob must not need the WAN")
	assert.Equal(t, central2, got)

	l2.Stop()
	bbtest.BootStorage(t, config, bbtest.GRPCReady(l2Addr, vmTLS))
	afterRestart := client(&vm, l2Addr)
	got, err = afterRestart.Read(ctx, bbtest.DigestOf(central2))
	require.NoError(t, err, "the L2 cache must survive a restart")
	assert.Equal(t, central2, got)

	// VMs of other hosts and non-worker identities are refused (mTLS +
	// URI SAN rules of docs/security.md).
	for _, kp := range []*bbtest.KeyPair{&foreignVM, &host} {
		_, err = client(kp, l2Addr).Read(ctx, bbtest.DigestOf(central2))
		assert.Equal(t, codes.Unauthenticated, status.Code(err), "%v", err)
	}
}
