// SPDX-License-Identifier: FSL-1.1-ALv2

package pki_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/pki"
	"github.com/sloper-ai/cucina/internal/pki/pkitest"
)

// secretVolume mimics the kubelet's atomic Secret volume layout:
// dir/..<gen>/{tls.crt,tls.key}, dir/..data -> ..<gen>, dir/tls.crt -> ..data/tls.crt.
type secretVolume struct {
	t   *testing.T
	dir string
	gen int
}

func (v *secretVolume) publish(files map[string][]byte) {
	v.t.Helper()
	v.gen++
	genDir := filepath.Join(v.dir, "..gen"+string(rune('0'+v.gen)))
	require.NoError(v.t, os.Mkdir(genDir, 0o755))
	for name, data := range files {
		require.NoError(v.t, os.WriteFile(filepath.Join(genDir, name), data, 0o600))
	}
	tmp := filepath.Join(v.dir, "..data_tmp")
	require.NoError(v.t, os.Symlink(filepath.Base(genDir), tmp))
	require.NoError(v.t, os.Rename(tmp, filepath.Join(v.dir, "..data")))
	for name := range files {
		link := filepath.Join(v.dir, name)
		if _, err := os.Lstat(link); os.IsNotExist(err) {
			require.NoError(v.t, os.Symlink(filepath.Join("..data", name), link))
		}
	}
}

// TestKeyPairFollowsSecretVolumeSwap guards zero-downtime rotation (R-CP-5,
// R-SEC-2): a kubelet-style Secret swap is picked up by GetCertificate without a
// restart, and a broken pair never replaces the last good one.
func TestKeyPairFollowsSecretVolumeSwap(t *testing.T) {
	clock := pkitest.NewClock(pkitest.Epoch)
	iss, _ := pkitest.NewIssuer(t, clock)
	pair := func() map[string][]byte {
		key := pkitest.Key(t)
		got, err := iss.IssueServer(key.Public(), "controller", []string{"cucina-controller"}, nil, false, 0)
		require.NoError(t, err)
		der, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		return map[string][]byte{"tls.crt": got.ChainPEM, "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}
	}
	vol := &secretVolume{t: t, dir: t.TempDir()}
	first := pair()
	vol.publish(first)

	loaded := make(chan *x509.Certificate, 8)
	kp, err := pki.LoadKeyPair(filepath.Join(vol.dir, "tls.crt"), filepath.Join(vol.dir, "tls.key"), func(l *x509.Certificate) { loaded <- l })
	require.NoError(t, err)
	<-loaded
	w, err := pki.NewWatcher(nil, wallClock{}, time.Hour, kp)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	second := pair()
	vol.publish(second)
	select {
	case leaf := <-loaded:
		require.Equal(t, pkitest.ParseChain(t, second["tls.crt"])[0].SerialNumber, leaf.SerialNumber)
	case <-time.After(20 * time.Second):
		t.Fatal("the swapped Secret was not reloaded")
	}
	c, err := kp.GetCertificate(nil)
	require.NoError(t, err)
	require.Equal(t, pkitest.ParseChain(t, second["tls.crt"])[0].Raw, c.Certificate[0])

	// A mismatched pair (certificate of one key, another key) is rejected by Reload.
	broken := map[string][]byte{"tls.crt": pair()["tls.crt"], "tls.key": first["tls.key"]}
	vol.publish(broken)
	require.Error(t, kp.Reload())
	c, err = kp.GetCertificate(nil)
	require.NoError(t, err)
	require.Equal(t, pkitest.ParseChain(t, second["tls.crt"])[0].Raw, c.Certificate[0], "last good pair kept")
}

// wallClock drives the watcher's settle timer in real time (the hourly poll never fires).
type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (wallClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
