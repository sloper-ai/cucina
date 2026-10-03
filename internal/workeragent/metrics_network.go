// SPDX-License-Identifier: FSL-1.1-ALv2

package workeragent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/internal/ports"
)

// EC2MetricsListen selects the VPC-local IPv4 interface from IMDSv2. It is not
// a wildcard address. The fixed port matches /sd/worker-agents and the private
// control-plane-only worker security-group rule.
const EC2MetricsListen = "ec2-private"
const AgentMetricsPort = 9982

// MetricsMetadata is the narrow IMDSv2 read used by private telemetry setup.
type MetricsMetadata interface {
	Get(context.Context, string) ([]byte, error)
}

// ResolveMetricsListen preserves an explicit override/disable. The EC2 default
// fails closed on metadata errors instead of exposing an all-interface listener.
func ResolveMetricsListen(ctx context.Context, requested string, metadata MetricsMetadata) (string, error) {
	if requested != EC2MetricsListen {
		return requested, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ip, err := metricsLocalIPv4(ctx, metadata)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(AgentMetricsPort)), nil
}

func metricsLocalIPv4(ctx context.Context, metadata MetricsMetadata) (netip.Addr, error) {
	if metadata == nil {
		return netip.Addr{}, errors.New("private agent metrics requires IMDSv2")
	}
	data, err := metadata.Get(ctx, "/latest/meta-data/local-ipv4")
	if err != nil {
		return netip.Addr{}, fmt.Errorf("private agent metrics local IPv4: %w", err)
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(string(data)))
	if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsLoopback() {
		return netip.Addr{}, errors.New("private agent metrics: invalid IMDS local IPv4")
	}
	return ip, nil
}

var metricsMAC = regexp.MustCompile(`(?i)^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
var metricsExecutable = regexp.MustCompile(`(?i)^[a-z]:[\\/].*[\\/]cucina-worker-agent\.exe$`)

// PrepareWindowsAgentMetrics runs only from privileged bootstrap after a valid
// worker enrollment (also on same-boot resume), before services are started.
// It changes one explicitly owned rule, never unrelated firewall policy.
func PrepareWindowsAgentMetrics(ctx context.Context, metadata MetricsMetadata, ex ports.Exec, executable string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !metricsExecutable.MatchString(executable) || strings.ContainsAny(executable, "*?[]%\r\n\x00") {
		return errors.New("private agent metrics: exact absolute worker-agent executable required")
	}
	ip, err := metricsLocalIPv4(ctx, metadata)
	if err != nil {
		return err
	}
	mac, err := metadata.Get(ctx, "/latest/meta-data/mac")
	if err != nil {
		return fmt.Errorf("private agent metrics interface: %w", err)
	}
	macText := strings.TrimSpace(string(mac))
	if !metricsMAC.MatchString(macText) {
		return errors.New("private agent metrics: invalid IMDS interface")
	}
	cidr, err := metadata.Get(ctx, "/latest/meta-data/network/interfaces/macs/"+macText+"/vpc-ipv4-cidr-block")
	if err != nil {
		return fmt.Errorf("private agent metrics VPC: %w", err)
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(string(cidr)))
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() == 0 || prefix != prefix.Masked() || !prefix.Contains(ip) {
		return errors.New("private agent metrics: canonical containing VPC IPv4 CIDR required")
	}
	// All values are validated or quoted as literal PowerShell strings. The
	// rule group is an ownership marker; a same-name unowned rule is an error.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := `$ErrorActionPreference = 'Stop'
$name = 'Cucina-WorkerAgent-Metrics'
$group = 'Cucina-Managed-WorkerAgent-Metrics-v1'
$existing = @(Get-NetFirewallRule -Name $name -ErrorAction SilentlyContinue)
if ($existing.Count -gt 1 -or ($existing.Count -eq 1 -and $existing[0].Group -ne $group)) {
  throw 'Refusing to overwrite an unowned worker-agent metrics firewall rule'
}
$policy = @{
  Name = $name; Enabled = 'True'; Direction = 'Inbound'; Action = 'Allow'; Profile = 'Any'
  Protocol = 'TCP'; LocalPort = 9982
  Program = ` + quote(executable) + `
  LocalAddress = ` + quote(ip.String()) + `
  RemoteAddress = ` + quote(prefix.String()) + `
}
if ($existing.Count -eq 1) {
  Set-NetFirewallRule @policy | Out-Null
} else {
  New-NetFirewallRule @policy -DisplayName 'Cucina worker-agent metrics (private VPC)' -Group $group | Out-Null
}
`
	if ex == nil {
		return errors.New("private agent metrics: privileged executor unavailable")
	}
	result, err := ex.Run(ctx, psCommand(script))
	if err != nil {
		return fmt.Errorf("private agent metrics firewall: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("private agent metrics firewall exited %d", result.ExitCode)
	}
	return nil
}
