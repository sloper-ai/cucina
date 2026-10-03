// SPDX-License-Identifier: FSL-1.1-ALv2

package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/internal/fakes"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/workeragent"
	"github.com/sloper-ai/cucina/internal/workeragent/imds"
	"github.com/sloper-ai/cucina/internal/workeragent/imds/imdsfake"
)

func decodePowerShell(t *testing.T, encoded string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	require.Zero(t, len(b)%2)
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(units))
}

// Guards: R-OBS-1 / R-SEC-2 — private agent telemetry is scoped to the IMDS
// interface, exact executable and containing VPC; no failed lookup becomes Any.
func TestPrivateAgentMetricsBoundary(t *testing.T) {
	const executable = `C:\cucina\bin\cucina-worker-agent.exe`
	const mac = "02:00:00:00:00:01"
	for _, tc := range []struct {
		name, ip, cidr        string
		metadataDown, unowned bool
		wantError             bool
	}{
		{name: "private VPC", ip: "10.0.1.12", cidr: "10.0.0.0/16"},
		{name: "shared address VPC", ip: "100.64.1.12", cidr: "100.64.0.0/16"},
		{name: "metadata unavailable", ip: "10.0.1.12", cidr: "10.0.0.0/16", metadataDown: true, wantError: true},
		{name: "wildcard interface", ip: "0.0.0.0", cidr: "10.0.0.0/16", wantError: true},
		{name: "loopback interface", ip: "127.0.0.1", cidr: "127.0.0.0/8", wantError: true},
		{name: "multicast interface", ip: "224.0.0.1", cidr: "224.0.0.0/8", wantError: true},
		{name: "global CIDR", ip: "10.0.1.12", cidr: "0.0.0.0/0", wantError: true},
		{name: "different VPC", ip: "10.0.1.12", cidr: "10.1.0.0/16", wantError: true},
		{name: "malformed CIDR", ip: "10.0.1.12", cidr: "Any", wantError: true},
		{name: "noncanonical CIDR", ip: "10.0.1.12", cidr: "10.0.1.2/16", wantError: true},
		{name: "unowned rule", ip: "10.0.1.12", cidr: "10.0.0.0/16", unowned: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := imdsfake.New(t)
			md.SetMetadata("/latest/meta-data/local-ipv4", []byte(tc.ip))
			md.SetMetadata("/latest/meta-data/mac", []byte(mac))
			md.SetMetadata("/latest/meta-data/network/interfaces/macs/"+mac+"/vpc-ipv4-cidr-block", []byte(tc.cidr))
			if tc.metadataDown {
				md.FailNext("/latest/meta-data/local-ipv4", http.StatusForbidden, http.StatusForbidden)
			}
			client := imds.New(md.URL)
			clock := fakes.NewClock(time.Unix(0, 0))
			ex := fakes.NewExec(clock, fakes.NewRand(1))
			policyInstalled := false
			ex.Handle("powershell.exe", func(_ context.Context, command ports.Command) (ports.ExecResult, error) {
				i := slices.Index(command.Args, "-EncodedCommand")
				require.GreaterOrEqual(t, i, 0)
				script := decodePowerShell(t, command.Args[i+1])
				// Public command boundary: exact allow-rule constraints, no global
				// firewall toggles or deletion of unrelated policy.
				for _, required := range []string{
					"Program = '" + executable + "'", "LocalAddress = '" + tc.ip + "'", "RemoteAddress = '" + tc.cidr + "'", "LocalPort = 9982", "Protocol = 'TCP'",
					"$existing[0].Group -ne $group", "throw 'Refusing to overwrite", "Set-NetFirewallRule @policy", "New-NetFirewallRule @policy",
				} {
					require.Contains(t, script, required)
				}
				require.NotContains(t, script, "Set-NetFirewallProfile")
				require.NotContains(t, script, "Remove-NetFirewallRule")
				require.NotContains(t, script, "RemoteAddress = 'Any'")
				if tc.unowned {
					return ports.ExecResult{ExitCode: 1}, nil
				}
				policyInstalled = true
				return ports.ExecResult{}, nil
			})
			address, listenErr := workeragent.ResolveMetricsListen(t.Context(), workeragent.EC2MetricsListen, client)
			err := workeragent.PrepareWindowsAgentMetrics(t.Context(), client, ex, executable)
			if tc.wantError {
				require.Error(t, err)
				require.False(t, policyInstalled, "invalid metadata/unowned rule must not install an allow rule")
				if tc.metadataDown || strings.Contains(tc.name, "interface") {
					require.Error(t, listenErr)
					require.Empty(t, address)
				}
				return
			}
			require.NoError(t, listenErr)
			require.Equal(t, tc.ip+":9982", address)
			require.NoError(t, err)
			require.True(t, policyInstalled)
			require.NoError(t, workeragent.PrepareWindowsAgentMetrics(t.Context(), client, ex, executable), "owned rule setup is idempotent")
		})
	}
	for _, explicit := range []string{"", "127.0.0.1:9982"} {
		address, err := workeragent.ResolveMetricsListen(t.Context(), explicit, nil)
		require.NoError(t, err)
		require.Equal(t, explicit, address, "explicit override/disable performs no metadata lookup")
	}
}
