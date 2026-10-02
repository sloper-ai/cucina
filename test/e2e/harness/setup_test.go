// SPDX-License-Identifier: FSL-1.1-ALv2

package harness_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/harness"
)

// Guards R-TEST-8d/§12: usable private-address AWS descriptors from the real
// output contract, safe opt-ins, tag/region validation and typed overrides.
func TestNewAWSEnv(t *testing.T) {
	base := `{"region":{"value":"us-west-1"},"sg_workers":{"value":"sg-example"},"sg_isolation":{"value":"sg-isolation"},"tags":{"value":{"cucina:env":"e2e","cucina:run":"test-run","cucina:expires":"2026-10-09T00:00:00Z"}}}`
	out := `{"region":"us-west-1","k3s_public_ip":"203.0.113.7","k3s_private_ip":"192.0.2.9","k3s_instance_id":"i-example","linux_client_instance_id":"i-linux","windows_client_instance_id":"i-windows","tags":{"cucina:env":"e2e","cucina:run":"test-run","cucina:expires":"2026-10-09T00:00:00Z"}}`
	opts := harness.AWSSetup{Name: "aws-test", RepoDir: "/src/cucina", StateDir: "/private/cucina/aws-e2e", ArtifactsDir: "/data/e2e", Bazel: "/tools/bazelisk", Cucinactl: map[string]string{"darwin": "/tools/cucinactl", "linux": "/tools/cucinactl-linux"}}
	e, err := harness.NewAWSEnv([]byte(base), []byte(out), nil, opts)
	require.NoError(t, err)
	require.Equal(t, harness.ScopeSmallFunctional, e.MeasurementScope)
	require.Equal(t, "test-run", e.RunID)
	require.Equal(t, "https://192.0.2.9:8443", e.Clients["linux-client"].STS)
	require.Equal(t, "grpcs://192.0.2.9:443", e.Clients["windows-client"].RemoteExecution)
	require.Equal(t, "192.0.2.9:8444", e.Clients["windows-client"].Management)
	require.Equal(t, "https://203.0.113.7:8445", e.Endpoints.Enrollment)
	require.Equal(t, "sg-isolation", e.AWS.IsolationSecurityGroup)
	require.True(t, e.Has(harness.RequiresCrossMatrix))
	require.True(t, e.Has(harness.RequiresWindowsClient))
	require.False(t, e.Has(harness.RequiresMacHost), "a Mac client is not an enrolled host")
	require.False(t, e.Has(harness.RequiresIdP), "mock deployment must be opted into")
	require.False(t, e.Has(harness.RequiresDestructive))
	patch := `{"devMac":{"hostdBinary":"/tools/hostd","hostdConfig":"/private/hostd.json"},"kubernetes":{"namespace":"custom","valuesFiles":["/private/custom.json"]},"workerSelectors":{"linux":"job=\"cucina-workers\",pool=\"linux-x86-64\""}}`
	opts.WithIDP, opts.AllowDestructive = true, true
	e, err = harness.NewAWSEnv([]byte(base), []byte(out), []byte(patch), opts)
	require.NoError(t, err)
	require.True(t, e.Has(harness.RequiresMacHost))
	require.True(t, e.Has(harness.RequiresDestructive))
	require.Equal(t, "/tools/bazelisk", e.DevMac.Bazel, "nested overrides preserve defaults")
	require.Equal(t, []string{"/private/custom.json"}, e.Kubernetes.ValuesFiles)
	require.Equal(t, "https://mock-oauth2-server.cucina-e2e.svc.cluster.local:30443", e.IdP.MockOAuth2URL)
	require.Equal(t, "127.0.0.1:18443", e.IdP.LocalAddr)
	for name, tc := range map[string]struct{ base, out, patch string }{
		"wrong region":     {base, strings.ReplaceAll(out, "us-west-1", "us-east-1"), ""},
		"different run":    {base, strings.ReplaceAll(out, "test-run", "other-run"), ""},
		"missing outputs":  {base, `{}`, ""},
		"unknown settings": {base, out, `{"secrtes":{}}`},
		"scope override":   {base, out, `{"aws":{"region":"us-east-1"}}`},
		"run override":     {base, out, `{"runId":"other-run","aws":{"tags":{"cucina:run":"other-run"}}}`},
		"path traversal":   {base, out, `{"name":"../other"}`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := harness.NewAWSEnv([]byte(tc.base), []byte(tc.out), []byte(tc.patch), opts)
			require.Error(t, err)
		})
	}
}
