// SPDX-License-Identifier: FSL-1.1-ALv2

package bootdata_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/sloper-ai/cucina/internal/workeragent/bootdata"
)

func testCAPEM(t testing.TB) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Unix(1<<32, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// Guards: R-POOL-3 (controller → agent boot data codec): whatever the
// controller can encode, the agent decodes to the same value.
func TestRoundTrip(t *testing.T) {
	ca := testCAPEM(t)
	ident := rapid.StringMatching(`[a-z0-9][a-z0-9.-]{0,40}`)
	rapid.Check(t, func(rt *rapid.T) {
		in := bootdata.BootData{
			EnrollEndpoint: ident.Draw(rt, "host") + ":" + strconv.Itoa(rapid.IntRange(1, 65535).Draw(rt, "port")),
			CAPEM:          ca,
			Cluster:        ident.Draw(rt, "cluster"),
			Pool:           ident.Draw(rt, "pool"),
			Generation:     ident.Draw(rt, "generation"),
		}
		if rapid.Bool().Draw(rt, "serverName") {
			in.ServerName = ident.Draw(rt, "sn")
		}
		enc, err := bootdata.Encode(in)
		require.NoError(rt, err)
		require.LessOrEqual(rt, len(enc), bootdata.MaxSize)
		out, err := bootdata.Decode(enc)
		require.NoError(rt, err)
		in.Version = bootdata.Version
		require.Equal(rt, in, out)
	})
}

// Guards: R-POOL-3 / R-SEC-3 boot data limits — ≤ 4 KiB, versioned, no secrets.
func TestRejects(t *testing.T) {
	ca := testCAPEM(t)
	keyPEM := "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIA==\n-----END EC PRIVATE KEY-----\n"
	valid := bootdata.BootData{EnrollEndpoint: "enroll.cucina.internal:8445", CAPEM: ca, Pool: "linux-x86-64"}
	cases := map[string]struct {
		encode *bootdata.BootData
		decode string
	}{
		"oversize":              {encode: &bootdata.BootData{EnrollEndpoint: valid.EnrollEndpoint, CAPEM: ca, Pool: strings.Repeat("p", bootdata.MaxSize)}},
		"oversize input":        {decode: `{"cucinaBootData":1,"pool":"` + strings.Repeat("p", bootdata.MaxSize) + `"}`},
		"private key in caPem":  {encode: &bootdata.BootData{EnrollEndpoint: valid.EnrollEndpoint, CAPEM: ca + keyPEM}},
		"no certificate":        {encode: &bootdata.BootData{EnrollEndpoint: valid.EnrollEndpoint, CAPEM: ""}},
		"endpoint without port": {encode: &bootdata.BootData{EnrollEndpoint: "enroll.cucina.internal", CAPEM: ca}},
		"future version":        {encode: &bootdata.BootData{Version: 2, EnrollEndpoint: valid.EnrollEndpoint, CAPEM: ca}},
		"missing version":       {decode: `{"enrollEndpoint":"a:1","caPem":""}`},
		"not json (script)":     {decode: "#!/bin/sh\necho hi\n"},
		"trailing content":      {decode: `{"cucinaBootData":1} {}`},
		"empty user data":       {decode: "  \n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.encode != nil {
				_, err := bootdata.Encode(*tc.encode)
				require.Error(t, err)
				return
			}
			_, err := bootdata.Decode([]byte(tc.decode))
			require.Error(t, err)
		})
	}
	enc, err := bootdata.Encode(valid)
	require.NoError(t, err)
	got, err := bootdata.Decode(append(append([]byte("\n"), enc...), '\n'))
	require.NoError(t, err)
	require.Equal(t, "enroll.cucina.internal", got.TLSServerName())
}
