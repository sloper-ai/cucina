// SPDX-License-Identifier: FSL-1.1-ALv2

package static_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/sloper-ai/cucina/charts/cucina/tests/charttest"
)

// Guards: R-TEST-7 — short configured host/worker lifetimes must not trigger
// infrastructure-age alerts immediately; the public rendered rules stay evaluable.
func TestShortLivedCertificateRules(t *testing.T) {
	promtool := charttest.Tool(t, "PROMTOOL", "promtool")
	var spec map[string]any
	for _, object := range charttest.Objects(t, charttest.Template(t, nil, "monitoring.prometheusRules.enabled=true", "pki.hostCertTTL=1h", "pki.workerCertTTL=3h", "pki.vmCertTTL=2h")) {
		if object.Kind == "PrometheusRule" {
			spec = object.Spec
		}
	}
	if spec == nil {
		t.Fatal("no PrometheusRule rendered")
	}
	dir := t.TempDir()
	b, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules.yaml"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := `rule_files: [rules.yaml]
evaluation_interval: 1m
tests:
  - name: short certificate lifetimes
    interval: 1m
    input_series:
      - series: 'cucina_cert_expiry_seconds{namespace="cucina",role="host"}'
        values: '3599'
      - series: 'cucina_cert_expiry_seconds{namespace="cucina",role="worker"}'
        values: '7199'
    alert_rule_test:
      - eval_time: 0m
        alertname: CucinaCertificateExpiringSoon
        exp_alerts: []
      - eval_time: 0m
        alertname: CucinaCertificateExpiryImminent
        exp_alerts: []
`
	path := filepath.Join(dir, "short.yaml")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(promtool, "test", "rules", path)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("short-lived certificate rules: %v\n%s", err, out)
	}
}

// TestPrometheusRules renders the PrometheusRule (default thresholds), checks it with
// `promtool check rules` and runs the promtool unit tests in tests/rules against it:
// every UC16 alert fires on its condition (R-OBS-3), retention is recorded and alerted
// (R-CP-4), and the ported bb-deployments arithmetic holds (R-OBS-2).
func TestPrometheusRules(t *testing.T) {
	promtool := charttest.Tool(t, "PROMTOOL", "promtool")
	var spec map[string]any
	for _, o := range charttest.Objects(t, charttest.Template(t, nil, "monitoring.prometheusRules.enabled=true")) {
		if o.Kind == "PrometheusRule" {
			spec = o.Spec
		}
	}
	if spec == nil {
		t.Fatal("no PrometheusRule rendered")
	}
	dir := t.TempDir()
	b, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules.yaml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	tests, err := filepath.Glob(filepath.Join(charttest.ChartDir(t), "tests", "rules", "*_test.yaml"))
	if err != nil || len(tests) == 0 {
		t.Fatalf("no promtool tests: %v", err)
	}
	args := []string{"test", "rules"}
	for _, f := range tests {
		dst := filepath.Join(dir, filepath.Base(f))
		if err := os.WriteFile(dst, charttest.ReadFile(t, f), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, dst)
	}
	if out, err := exec.Command(promtool, "check", "rules", filepath.Join(dir, "rules.yaml")).CombinedOutput(); err != nil {
		t.Fatalf("promtool check rules: %v\n%s", err, out)
	}
	if out, err := exec.Command(promtool, args...).CombinedOutput(); err != nil {
		t.Fatalf("promtool test rules: %v\n%s", err, out)
	}
}
