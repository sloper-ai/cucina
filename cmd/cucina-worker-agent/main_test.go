// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Guards: the `render` CLI contract hostd uses through `tart exec` (flags,
// strict inputs, output files, plan on stdout) and fail-fast exit codes.
func TestRenderCommand(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, IsCA: true,
		BasicConstraintsValid: true, NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1<<33, 0)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	settings := `{"pool":"macos-arm64-xcode27.0","node":"mini-01/vm-1","runners":[{"name":"xcode","platform":[` +
		`{"name":"OSFamily","value":"macos"},{"name":"ISA","value":"arm-a64"},{"name":"xcode-version","value":"27.0"}]}],` +
		`"schedulerEndpoint":"192.0.2.1:8983","storageEndpoint":"192.0.2.1:8981","serverName":"cucina.test",` +
		`"buildDirectory":"native","l1Placement":"vm-disk","maximumMessageSizeBytes":"16777216","instanceNamePrefixes":["main"]}`
	caFile := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(caFile, []byte(ca), 0o644))
	machine, err := json.Marshal(map[string]any{
		"os": "darwin", "arch": "arm64", "vcpus": 6, "memoryBytes": 16 << 30, "stateRoot": "/var/db/cucina",
		"stateRootBytes": 200 << 30, "buildRoot": "/Volumes/CucinaBuild", "runDir": "/var/run/cucina-runner",
		"pkiDir": "/var/db/cucina/pki", "caBundleFile": caFile, "storageIsHostL2": true,
	})
	require.NoError(t, err)
	sf, mf, out := filepath.Join(dir, "settings.json"), filepath.Join(dir, "machine.json"), filepath.Join(dir, "out")
	require.NoError(t, os.WriteFile(sf, []byte(settings), 0o644))
	require.NoError(t, os.WriteFile(mf, machine, 0o644))

	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, run([]string{"render", "--settings", sf, "--machine", mf, "--out", out}, &stdout, &stderr), stderr.String())
	for _, f := range []string{"worker.json", "runner.json", "env"} {
		_, err := os.Stat(filepath.Join(out, f))
		require.NoError(t, err, f)
	}
	var plan struct {
		Directories []struct{ Path string }
		Notes       []string
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &plan))
	require.NotEmpty(t, plan.Directories, "the caller learns which directories to create")

	require.NoError(t, os.WriteFile(sf, []byte(`{"pool":"x","bogus":1}`), 0o644))
	require.NotEqual(t, 0, run([]string{"render", "--settings", sf, "--machine", mf, "--out", out}, &stdout, &stderr),
		"unknown settings fields fail")
	require.NotEqual(t, 0, run([]string{"render", "--settings", sf}, &stdout, &stderr), "missing flags fail")
}
