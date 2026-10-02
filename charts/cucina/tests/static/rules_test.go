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
