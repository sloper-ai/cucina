// SPDX-License-Identifier: FSL-1.1-ALv2

package redact_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/hostd/redact"
)

// TestBytes guards the rule that certificates, keys and tokens never leave the
// host in diagnostics or logs (mgmt StreamWorkerLogs passes them through).
func TestBytes(t *testing.T) {
	tests := []struct{ in, secret string }{
		{"-----BEGIN PRIVATE KEY-----\nMIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQg\n-----END PRIVATE KEY-----", "MIGHAgEAMBMG"},
		{"cert: -----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIQ\n-----END CERTIFICATE-----", "MIIBszCCAVmg"},
		{`{"SiteEnrollmentToken":"cuc_et_ab12cd34_s3cr3tvalue"}`, "s3cr3tvalue"},
		{"authorization: Bearer eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl", "eyJzdWIiOiJ4In0"},
		{"TART_REGISTRY_PASSWORD=ghs_abcdefghijklmnopqrstuvwx", "ghs_abcdefghijklmnop"},
		{"pull https://bot:hunter2@ghcr.io/v2/", "hunter2"},
		{"token mysitetoken-123456 configured", "mysitetoken-123456"},
	}
	for _, tc := range tests {
		out := string(redact.Bytes([]byte(tc.in), "mysitetoken-123456"))
		require.NotContains(t, out, tc.secret, tc.in)
		require.Contains(t, out, redact.Marker)
	}
	plain := "2026-10-02T08:00:00Z level=info msg=\"vm ready\" vm=vm-1 ip=192.168.64.5"
	require.Equal(t, plain, string(redact.Bytes([]byte(plain))))
	require.NotContains(t, string(redact.Bytes([]byte("x"), "")), redact.Marker)
}
