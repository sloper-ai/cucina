// SPDX-License-Identifier: FSL-1.1-ALv2

package report

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sloper-ai/cucina/test/e2e/harness"
	"github.com/sloper-ai/cucina/test/e2e/nfr"
)

// Guards §12 "redact environment identifiers from committed reports": every
// identifier class is replaced; look-alikes that are not identifiers survive.
func TestRedactionPatterns(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"node i-0abc123def4567890 launched", "node <redacted:aws-resource-id> launched"},
		{"vol-0123456789abcdef0, eni-0a1b2c3d4e5f60718, sg-0fedcba9876543210", "<redacted:aws-resource-id>, <redacted:aws-resource-id>, <redacted:aws-resource-id>"},
		{"ami-0123456789abcdef0 snap-0123456789abcdef0 subnet-0123456789abcdef0 vpc-0123456789abcdef0", "<redacted:aws-resource-id> <redacted:aws-resource-id> <redacted:aws-resource-id> <redacted:aws-resource-id>"},
		{"arn:aws:iam::123456789012:role/cucina", "arn:aws:iam::<redacted:aws-account>:role/cucina"},
		{"account id: 123456789012", "account id: <redacted:aws-account>"},
		{"123456789012.dkr.ecr.us-west-1.amazonaws.com/cucina", "<redacted:ecr-registry>/cucina"},
		{"endpoint 203.0.113.7:443 and 10.0.1.23", "endpoint 203.0.113.7:443 and <redacted:ipv4>"},
		{"2600:1f1c:abc:de00::12 and 2600:1f1c::5", "<redacted:ipv6> and <redacted:ipv6>"},
		{"ip-10-0-1-23.us-west-1.compute.internal", "<redacted:ec2-hostname>"},
		{"ec2-203-0-113-7.us-west-1.compute.amazonaws.com", "<redacted:ec2-hostname>"},
		{"built on Someones-MacBook-Pro.local", "built on <redacted:local-hostname>"},
		{"/Users/alice/Library and /home/bob/x", "/Users/<redacted:user>/Library and /home/<redacted:user>/x"},
		{"mail me@corp.dev", "mail <redacted:email>"},
		{"https://b.s3.amazonaws.com/p.pkg?X-Amz-Algorithm=AWS4&X-Amz-Credential=AKIA/x&X-Amz-Signature=ab", "https://b.s3.amazonaws.com/p.pkg?<redacted:presigned-query>"},
		{"Bearer eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ4In0x.c2lnbmF0dXJlMTIz", "Bearer <redacted:jwt>"},
		{"key cuc_sk_12_Zm9vYmFy", "key <redacted:service-key>"},
		// Not identifiers: unchanged.
		{"Windows SDK 10.0.26100.0, MSVC 14.50.35717", "Windows SDK 10.0.26100.0, MSVC 14.50.35717"},
		{"loopback 127.0.0.1 and 0.0.0.0, docs 192.0.2.10", "loopback 127.0.0.1 and 0.0.0.0, docs 192.0.2.10"},
		{"at 10:15:30 took 1h2m3s", "at 10:15:30 took 1h2m3s"},
		{"digest 52a8f3bec5916e47e37ac583d0064dc880a7ba06cf74aead5011e9d66a92368a/145", "digest 52a8f3bec5916e47e37ac583d0064dc880a7ba06cf74aead5011e9d66a92368a/145"},
		{"admin@example.com, /home/ubuntu/e2e, svc.cluster.local", "admin@example.com, /home/ubuntu/e2e, svc.cluster.local"},
		{"worker-12345678 and default-1234abcd", "worker-12345678 and default-1234abcd"},
	} {
		got, _ := Redactor{}.Apply(tc.in)
		require.Equal(t, tc.want, got, tc.in)
		require.Empty(t, Check(got), "a redacted text passes the gate: %s", got)
	}
	got, found := Redactor{Literals: map[string]string{"cucina-e2e-mini.corp": "host"}}.Apply("host cucina-e2e-mini.corp up")
	require.Equal(t, "host <redacted:host> up", got)
	require.Len(t, found, 1)
	// A descriptor-supplied endpoint is redacted even when a test deployment
	// happens to use a documentation address; the generic pattern preserves it.
	got, _ = (Redactor{Literals: map[string]string{"203.0.113.7": "endpoint"}}).Apply("endpoint 203.0.113.7:443")
	require.Equal(t, "endpoint <redacted:endpoint>:443", got)
	require.NotEmpty(t, Check("left behind: i-0abc123def4567890"))
}

// Guards the report contract (e2e task deliverable 6): every §8 NFR row,
// per-scenario numbers, cost against the budget, limitations for skips and
// unmeasured NFRs, and the tag-sweep block.
func TestRender(t *testing.T) {
	results := []*harness.Result{
		{ID: "T1", Title: "Linux cold", Status: harness.StatusPass, Duration: 42 * time.Minute, CostClass: harness.CostHigh,
			Cost:    harness.CostRecord{EstimateUSD: 12, MeasuredUSD: 7.5, Items: map[string]float64{"compute:c8i.8xlarge": 7.4, "ebs:gp3": 0.1}},
			NFRs:    nfr.ColdStart("linux", []time.Duration{40 * time.Second, 70 * time.Second}),
			Metrics: map[string]harness.Metric{"linux.cold-build.wall_seconds": {Value: 1234, Unit: "s"}},
			Queries: []harness.QueryRecord{{Query: "max(cucina:queue_time_seconds:p95_5m)", Result: "{} 0.4"}}},
		{ID: "T4", Title: "Windows cold", Status: harness.StatusSkip, SkipReason: "requires windows-client"},
		{ID: "T15", Title: "Teardown", Status: harness.StatusPass, Values: map[string]any{"sweep": map[string]any{"output": "instances: 0\nvolumes: 0", "clean": true}}},
	}
	md := Render(Input{Date: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), RunID: "e2e-test", Results: results, BudgetUSD: 300,
		ExtraSpend: map[string]float64{"standing environment": 20}, ADRs: []string{"[1001](../adr/1001-x.md) — x"}, Issues: "# Issues\n- fixed a thing"})
	require.Contains(t, md, "# Cucina acceptance campaign — 2026-10-05")
	for _, d := range nfr.Catalog {
		require.Contains(t, md, "| "+d.ID+" |", d.ID)
	}
	require.Contains(t, md, "| NFR-P1 |")
	require.Contains(t, md, "spend **$27.50** of $300")
	require.Contains(t, md, "T4 skipped: requires windows-client")
	require.Contains(t, md, "NFR-X5 not measured")
	require.Contains(t, md, "instances: 0\nvolumes: 0\n# clean: true")
	require.Contains(t, md, "- fixed a thing")
	require.Contains(t, md, "`max(cucina:queue_time_seconds:p95_5m)`")
	require.Equal(t, 1, strings.Count(md, "### T1 — Linux cold"))
}
