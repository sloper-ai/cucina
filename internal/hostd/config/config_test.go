// SPDX-License-Identifier: FSL-1.1-ALv2

package config_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"howett.net/plist"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/internal/hostd/config"
)

func testCert(t *testing.T, isCA bool) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}

const pinHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// A managed-preferences file as an MDM custom-settings payload writes it (XML plist).
const managedXML = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>ControllerURL</key><string>https://cucina.example.com:8445</string>
	<key>CAPinSHA256</key><array><string>` + pinHex + `</string></array>
	<key>SiteEnrollmentToken</key><string>cst_s3cr3t-token-value</string>
	<key>Site</key><string>office-1</string>
	<key>Labels</key><dict><key>rack</key><string>a2</string></dict>
	<key>VMSlots</key><integer>1</integer>
	<key>L2SizeGiB</key><integer>300</integer>
	<key>LogLevel</key><string>debug</string>
	<key>PayloadUUID</key><string>5D0F7B5E-0000-0000-0000-000000000000</string>
</dict>
</plist>`

func marshal(t *testing.T, m map[string]any, format int) []byte {
	t.Helper()
	b, err := plist.Marshal(m, format)
	require.NoError(t, err)
	return b
}

// TestLoadManagedPreferences guards R-MAC-10 (schema, strict validation with a
// message naming the key) and R-SEC-3 (the token is never rendered).
func TestLoadManagedPreferences(t *testing.T) {
	ca := testCert(t, true)
	leaf := testCert(t, false)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))
	base := func() map[string]any {
		return map[string]any{"ControllerURL": "https://cucina.example.com:8445", "CACertificate": caPEM}
	}
	with := func(k string, v any) map[string]any { m := base(); m[k] = v; return m }
	without := func(k string) map[string]any { m := base(); delete(m, k); return m }

	tests := []struct {
		name    string
		data    []byte
		wantErr string // substring naming the key; "" = valid
		check   func(t *testing.T, c config.Config)
	}{
		{name: "mdm xml payload", data: []byte(managedXML), check: func(t *testing.T, c config.Config) {
			require.Equal(t, "cucina.example.com:8445", c.EnrollAddress())
			require.Equal(t, "cucina.example.com", c.ServerName())
			require.Len(t, c.CAPins, 1)
			require.Equal(t, pinHex, hex.EncodeToString(c.CAPins[0][:]))
			require.Equal(t, "cst_s3cr3t-token-value", c.SiteEnrollmentToken)
			require.Equal(t, map[string]string{"rack": "a2"}, c.Labels)
			require.Equal(t, 1, c.VMSlots)
			require.Equal(t, 300, c.L2SizeGiB)
			require.Equal(t, "debug", c.LogLevel)
			require.Equal(t, config.DefaultTartPath, c.TartPath)
			require.Equal(t, 7*24*time.Hour, c.VMMaxAge)
			require.True(t, c.Forced["VMSlots"] && c.Forced["SiteEnrollmentToken"])
			require.NotContains(t, c.String(), "s3cr3t")
		}},
		{name: "binary plist with DER CA as data", data: marshal(t, map[string]any{
			"ControllerURL": "https://10.0.0.1:8445", "CACertificate": ca.Raw, "VMCPUCount": 6, "VMMemoryGiB": 24,
		}, plist.BinaryFormat), check: func(t *testing.T, c config.Config) {
			require.Len(t, c.CACertificates, 1)
			require.Equal(t, 6, c.VMCPUCount)
			require.Equal(t, 24, c.VMMemoryGiB)
		}},
		{name: "unknown key", data: marshal(t, with("VMSlot", 2), plist.XMLFormat), wantErr: `key "VMSlot"`},
		{name: "wrong type", data: marshal(t, with("VMSlots", "2"), plist.XMLFormat), wantErr: `key "VMSlots"`},
		{name: "three slots exceed the Apple limit", data: marshal(t, with("VMSlots", 3), plist.XMLFormat), wantErr: `key "VMSlots"`},
		{name: "controller url required", data: marshal(t, without("ControllerURL"), plist.XMLFormat), wantErr: `key "ControllerURL"`},
		{name: "plain http refused", data: marshal(t, with("ControllerURL", "http://cucina:8445"), plist.XMLFormat), wantErr: `key "ControllerURL"`},
		{name: "ca or pin required", data: marshal(t, without("CACertificate"), plist.XMLFormat), wantErr: `key "CACertificate"`},
		{name: "leaf is not a CA", data: marshal(t, with("CACertificate", leaf.Raw), plist.XMLFormat), wantErr: `key "CACertificate"`},
		{name: "bad pin", data: marshal(t, with("CAPinSHA256", "abc"), plist.XMLFormat), wantErr: `key "CAPinSHA256"`},
		{name: "root tart user refused", data: marshal(t, with("RunAsUser", "root"), plist.XMLFormat), wantErr: `key "RunAsUser"`},
		{name: "relative tart path refused", data: marshal(t, with("TartPath", "tart"), plist.XMLFormat), wantErr: `key "TartPath"`},
		{name: "not a plist", data: []byte("{ not: plist"), wantErr: "plist"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src, err := config.ParsePlist(tc.data, true)
			var c config.Config
			if err == nil {
				c, err = config.Load(src)
			}
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			tc.check(t, c)
		})
	}
}

// TestManagedValuesTakePrecedence guards R-MAC-10 "profile values take
// precedence": a forced managed value beats a locally written preference.
func TestManagedValuesTakePrecedence(t *testing.T) {
	managed, err := config.ParsePlist([]byte(managedXML), true)
	require.NoError(t, err)
	local, err := config.ParsePlist(marshal(t, map[string]any{"VMSlots": 2, "MetricsListen": "0.0.0.0:9470"}, plist.XMLFormat), false)
	require.NoError(t, err)

	c, err := config.Load(config.Layered{managed, local})
	require.NoError(t, err)
	require.Equal(t, 1, c.VMSlots)
	require.True(t, c.Forced["VMSlots"])
	require.Equal(t, "0.0.0.0:9470", c.MetricsListen)
	require.False(t, c.Forced["MetricsListen"])
}

// TestControllerOverrides guards the precedence of controller HostSettings over
// preferences for tunables and the hard 1..2 slot range (R-MAC-3).
func TestControllerOverrides(t *testing.T) {
	src, err := config.ParsePlist([]byte(managedXML), true)
	require.NoError(t, err)
	c, err := config.Load(src)
	require.NoError(t, err)

	tests := []struct {
		name  string
		slots uint32
		hs    *cucinav1.HostSettings
		want  config.Tunables
	}{
		{"preferences only", 0, nil, config.Tunables{Slots: 1, L2SizeGiB: 300, LogLevel: "debug"}},
		{"controller overrides", 2, &cucinav1.HostSettings{VmCpu: 6, VmMemoryGib: 20, L2SizeGib: 100, LogLevel: "warn",
			CentralEndpoint: "storage:8981", SchedulerEndpoint: "sched:8983"},
			config.Tunables{Slots: 2, VMCPU: 6, VMMemoryGiB: 20, L2SizeGiB: 100, LogLevel: "warn",
				CentralEndpoint: "storage:8981", SchedulerEndpoint: "sched:8983"}},
		{"slots clamped to the Apple limit", 5, &cucinav1.HostSettings{LogLevel: "bogus"},
			config.Tunables{Slots: 2, L2SizeGiB: 300, LogLevel: "debug"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, c.Tunables(tc.slots, tc.hs))
		})
	}
	require.False(t, strings.Contains(c.String(), c.SiteEnrollmentToken))
}
