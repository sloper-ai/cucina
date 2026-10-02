// SPDX-License-Identifier: FSL-1.1-ALv2

package static_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sloper-ai/cucina/charts/cucina/tests/charttest"
	"sigs.k8s.io/yaml"
)

// TestManifestsValidateStrictly runs kubeconform -strict over the chart rendered with
// the three test profiles and the samples (R-TEST-6 "Chart"), offline: Kubernetes
// v1.36.5 schemas vendored in tools/kubeconform, third-party custom resources in
// tests/kubeconform/schemas, and the chart's own CRDs converted from api/crds (so a
// rendered WorkerPool/TrustPolicy must match the CRD the cluster will enforce).
func TestManifestsValidateStrictly(t *testing.T) {
	kubeconform := charttest.Tool(t, "KUBECONFORM", "kubeconform")
	root := charttest.RepoRoot(t)
	chart := charttest.ChartDir(t)
	crdSchemas := t.TempDir()
	writeCRDSchemas(t, filepath.Join(root, "api", "crds"), crdSchemas)

	profiles := map[string][]string{
		"small":  {filepath.Join(chart, "ci", "values-small.yaml")},
		"medium": {filepath.Join(chart, "ci", "values-medium.yaml"), filepath.Join(chart, "samples", "trust-policies.yaml")},
		"large":  {filepath.Join(chart, "ci", "values-large.yaml"), filepath.Join(chart, "samples", "pools.yaml")},
	}
	for name, files := range profiles {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			manifests := charttest.Template(t, files)
			cmd := exec.Command(kubeconform, "-strict", "-summary", "-output", "text",
				"-schema-location", filepath.Join(root, "tools", "kubeconform", "schemas", "v1.36.5-standalone-strict", "{{ .ResourceKind }}{{ .KindSuffix }}.json"),
				"-schema-location", filepath.Join(chart, "tests", "kubeconform", "schemas", "{{ .Group }}", "{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json"),
				"-schema-location", filepath.Join(crdSchemas, "{{ .Group }}", "{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json"),
				// No upstream strict CRD schema; CRDs are exercised by envtest. ScrapeConfig and
				// GRPCRoute schemas are too large to vendor for one object each.
				"-skip", "CustomResourceDefinition,ScrapeConfig,GRPCRoute",
				"-")
			cmd.Stdin = bytes.NewReader(manifests)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("kubeconform: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "Invalid: 0, Errors: 0") {
				t.Fatalf("kubeconform found problems:\n%s", out)
			}
		})
	}
}

// writeCRDSchemas converts each CRD version's openAPIV3Schema into a kubeconform
// schema (<group>/<kind>_<version>.json), strict: unknown members are rejected.
func writeCRDSchemas(t *testing.T, crdDir, out string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs in %s: %v", crdDir, err)
	}
	for _, f := range files {
		var crd struct {
			Spec struct {
				Group    string
				Names    struct{ Kind string }
				Versions []struct {
					Name   string
					Schema struct {
						OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
					}
				}
			}
		}
		if err := yaml.Unmarshal(charttest.ReadFile(t, f), &crd); err != nil {
			t.Fatal(err)
		}
		for _, v := range crd.Spec.Versions {
			schema := strictify(v.Schema.OpenAPIV3Schema).(map[string]any)
			props := schema["properties"].(map[string]any)
			props["apiVersion"] = map[string]any{"type": "string", "enum": []any{crd.Spec.Group + "/" + v.Name}}
			props["kind"] = map[string]any{"type": "string", "enum": []any{crd.Spec.Names.Kind}}
			props["metadata"] = map[string]any{"type": "object"} // validated by the API server
			dir := filepath.Join(out, crd.Spec.Group)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(schema)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, strings.ToLower(crd.Spec.Names.Kind)+"_"+v.Name+".json"), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
