// SPDX-License-Identifier: FSL-1.1-ALv2

package controller_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/controller"
)

// Guards the STS Deployment's startup (found by the kind smoke test, ADR 0406):
// the STS never calls the scheduler, so it starts without the controller's
// scheduler client certificate, which the chart mounts only into the
// controller Pods; the controller still fails fast when that file is missing.
func TestTLSConfigsLoadSchedulerClientOnlyForTheController(t *testing.T) {
	dir := t.TempDir()
	crt, key := writeSelfSigned(t, dir)
	cfg := &config.Controller{}
	cfg.TLS = config.TLS{CertFile: crt, KeyFile: key, CAFile: crt}
	cfg.Scheduler.ClientCertFile = filepath.Join(dir, "controller-client", "tls.crt")
	cfg.Scheduler.ClientKeyFile = filepath.Join(dir, "controller-client", "tls.key")

	sts, err := controller.NewTLSConfigs(cfg, controller.ModeSTS)
	require.NoError(t, err, "the STS must not need the scheduler client certificate")
	assert.NotNil(t, sts.Server)
	assert.Nil(t, sts.SchedulerClient)

	_, err = controller.NewTLSConfigs(cfg, controller.ModeController)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "controller-client")

	cfg.Scheduler.ClientCertFile, cfg.Scheduler.ClientKeyFile = crt, key
	ctl, err := controller.NewTLSConfigs(cfg, controller.ModeController)
	require.NoError(t, err)
	assert.NotNil(t, ctl.SchedulerClient)
}

// writeSelfSigned writes a self-signed ECDSA certificate and key (usable as
// both the CA bundle and a key pair) and returns their paths.
func writeSelfSigned(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cucina-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:              []string{"cucina-test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certFile, keyFile
}
