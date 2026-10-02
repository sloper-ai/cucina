// SPDX-License-Identifier: FSL-1.1-ALv2

package static_test

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sloper-ai/cucina/charts/cucina/tests/charttest"
	"github.com/sloper-ai/cucina/internal/controller"
	"github.com/sloper-ai/cucina/internal/pki"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite generated files (values.schema.json definitions, README values table) instead of comparing")

// TestChartCopiesMatchSources guards the copies the chart must carry because Helm
// cannot read files outside the chart directory: the platform catalog (ADR 0002,
// the scheduler's predeclared queues) and the CRDs (R-OPS-1).
func TestChartCopiesMatchSources(t *testing.T) {
	root := charttest.RepoRoot(t)
	pairs := map[string]string{
		"platforms/pools.json": "charts/cucina/files/platforms.json",
	}
	crds, err := filepath.Glob(filepath.Join(root, "api", "crds", "*.yaml"))
	if err != nil || len(crds) == 0 {
		t.Fatalf("no CRDs under api/crds: %v", err)
	}
	for _, c := range crds {
		pairs[filepath.Join("api", "crds", filepath.Base(c))] = filepath.Join("charts", "cucina", "files", "crds", filepath.Base(c))
	}
	copies, _ := filepath.Glob(filepath.Join(root, "charts", "cucina", "files", "crds", "*.yaml"))
	if len(copies) != len(crds) {
		t.Errorf("charts/cucina/files/crds has %d files, api/crds %d: run `make -C charts/cucina sync`", len(copies), len(crds))
	}
	for src, dst := range pairs {
		if !bytes.Equal(charttest.ReadFile(t, filepath.Join(root, src)), charttest.ReadFile(t, filepath.Join(root, dst))) {
			t.Errorf("%s differs from %s: run `make -C charts/cucina sync`", dst, src)
		}
	}
}

// TestSchemaDefinitionsMatchCRDs keeps values.pools / values.trustPolicies validation
// identical to the CRD schemas (fail fast at `helm install`, R-TEST-7), made strict
// (unknown fields are errors) and with the CRDs' CEL one-of rules as JSON Schema.
func TestSchemaDefinitionsMatchCRDs(t *testing.T) {
	path := filepath.Join(charttest.ChartDir(t), "values.schema.json")
	var schema map[string]any
	if err := json.Unmarshal(charttest.ReadFile(t, path), &schema); err != nil {
		t.Fatal(err)
	}
	defs := schema["definitions"].(map[string]any)
	want := generatedDefinitions(t)
	stale := false
	for name, def := range want {
		if !reflect.DeepEqual(roundTrip(t, defs[name]), roundTrip(t, def)) {
			stale = true
			defs[name] = def
		}
	}
	if !stale {
		return
	}
	if *update {
		if err := os.WriteFile(charttest.WritableChartFile(t, "values.schema.json"), charttest.MarshalJSON(t, schema), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Error("values.schema.json definitions are stale: run `go test ./charts/cucina/tests/static -run TestSchemaDefinitionsMatchCRDs -update`")
}

// TestReadmeValuesTableUpToDate keeps the README's values reference generated from
// the schema descriptions.
func TestReadmeValuesTableUpToDate(t *testing.T) {
	const begin, end = "<!-- BEGIN VALUES TABLE (generated from values.schema.json) -->", "<!-- END VALUES TABLE -->"
	var schema map[string]any
	if err := json.Unmarshal(charttest.ReadFile(t, filepath.Join(charttest.ChartDir(t), "values.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	readme := string(charttest.ReadFile(t, filepath.Join(charttest.ChartDir(t), "README.md")))
	i, j := strings.Index(readme, begin), strings.Index(readme, end)
	if i < 0 || j < i {
		t.Fatalf("README.md lacks the %q … %q markers", begin, end)
	}
	table := valuesTable(schema)
	got := readme[i+len(begin) : j]
	if got == table {
		return
	}
	if *update {
		out := readme[:i+len(begin)] + table + readme[j:]
		if err := os.WriteFile(charttest.WritableChartFile(t, "README.md"), []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Error("README.md values table is stale: run `go test ./charts/cucina/tests/static -run TestReadmeValuesTableUpToDate -update`")
}

func roundTrip(t testing.TB, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// generatedDefinitions converts the CRD spec schemas into the chart's definitions.
func generatedDefinitions(t testing.TB) map[string]any {
	t.Helper()
	wp := crdSpecSchema(t, "cucina.sloper.ai_workerpools.yaml")
	tp := crdSpecSchema(t, "cucina.sloper.ai_trustpolicies.yaml")

	// CEL rules of the CRDs, as JSON Schema (CEL itself is not evaluated by Helm).
	providerRules := []any{
		ifThen("provider", "ec2", map[string]any{"required": []any{"ec2"}, "not": map[string]any{"required": []any{"tart"}}}),
		ifThen("provider", "tart", map[string]any{"required": []any{"tart"}, "not": map[string]any{"required": []any{"ec2"}}}),
	}
	wpSpec := deepCopy(t, wp).(map[string]any)
	wpSpec["allOf"] = providerRules

	pool := deepCopy(t, wp).(map[string]any)
	props := pool["properties"].(map[string]any)
	props["name"] = map[string]any{"$ref": "#/definitions/dnsLabel", "description": "WorkerPool name (also the pool label, cucina:pool tag and SPIFFE path segment)."}
	props["enabled"] = map[string]any{"type": "boolean", "description": "Render this pool (default true)."}
	pool["required"] = append([]any{"name"}, pool["required"].([]any)...)
	pool["allOf"] = providerRules
	pool["description"] = "A WorkerPool: name, enabled, and the WorkerPool spec fields (api/v1alpha1)."

	tpSpec := deepCopy(t, tp).(map[string]any)
	tpSpec["allOf"] = []any{
		ifThen("type", "oidc", map[string]any{"required": []any{"issuer", "claimMappings"}}),
		ifThen("type", "serviceAccount", map[string]any{"required": []any{"serviceAccount"}}),
	}
	return map[string]any{"WorkerPoolSpec": wpSpec, "pool": pool, "TrustPolicySpec": tpSpec}
}

func ifThen(prop, value string, then map[string]any) map[string]any {
	return map[string]any{
		"if":   map[string]any{"properties": map[string]any{prop: map[string]any{"const": value}}, "required": []any{prop}},
		"then": then,
	}
}

func deepCopy(t testing.TB, v any) any { return roundTrip(t, v) }

// crdSpecSchema returns spec of the first served version, made strict: every
// object with properties rejects unknown members; Kubernetes extensions (CEL rules,
// list types) are dropped because JSON Schema validators do not evaluate them.
func crdSpecSchema(t testing.TB, file string) map[string]any {
	t.Helper()
	var crd map[string]any
	if err := yaml.Unmarshal(charttest.ReadFile(t, filepath.Join(charttest.RepoRoot(t), "api", "crds", file)), &crd); err != nil {
		t.Fatal(err)
	}
	versions := crd["spec"].(map[string]any)["versions"].([]any)
	schema := versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	spec := schema["properties"].(map[string]any)["spec"].(map[string]any)
	return strictify(spec).(map[string]any)
}

func strictify(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range n {
			if strings.HasPrefix(k, "x-kubernetes-") {
				continue
			}
			out[k] = strictify(val)
		}
		if _, hasProps := out["properties"]; hasProps {
			if _, set := out["additionalProperties"]; !set {
				out["additionalProperties"] = false
			}
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i := range n {
			out[i] = strictify(n[i])
		}
		return out
	default:
		return v
	}
}

// valuesTable renders the README values table: one row per leaf (or documented
// object) of the schema, with type and description.
func valuesTable(schema map[string]any) string {
	defs := schema["definitions"].(map[string]any)
	var rows []string
	var walk func(prefix string, node map[string]any, depth int)
	resolve := func(node map[string]any) map[string]any {
		for {
			ref, ok := node["$ref"].(string)
			if !ok {
				return node
			}
			target := defs[strings.TrimPrefix(ref, "#/definitions/")].(map[string]any)
			merged := map[string]any{}
			for k, v := range target {
				merged[k] = v
			}
			for k, v := range node {
				if k != "$ref" {
					merged[k] = v
				}
			}
			node = merged
		}
	}
	walk = func(prefix string, node map[string]any, depth int) {
		node = resolve(node)
		props, _ := node["properties"].(map[string]any)
		desc, _ := node["description"].(string)
		if len(props) == 0 || depth >= 3 || strings.HasPrefix(prefix, "pools") || strings.HasPrefix(prefix, "trustPolicies") || strings.HasPrefix(prefix, "platforms.extra") {
			rows = append(rows, fmt.Sprintf("| `%s` | %s | %s |", prefix, typeOf(node), strings.ReplaceAll(desc, "|", "\\|")))
			return
		}
		if desc != "" && prefix != "" {
			rows = append(rows, fmt.Sprintf("| `%s` | object | %s |", prefix, strings.ReplaceAll(desc, "|", "\\|")))
		}
		keys := make([]string, 0, len(props))
		for k := range props {
			if k == "global" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			child, ok := props[k].(map[string]any)
			if !ok {
				continue
			}
			walk(p, child, depth+1)
		}
	}
	walk("", schema, 0)
	return "\n| Key | Type | Description |\n| --- | --- | --- |\n" + strings.Join(rows, "\n") + "\n"
}

func typeOf(node map[string]any) string {
	switch t := node["type"].(type) {
	case string:
		if e, ok := node["enum"].([]any); ok {
			parts := make([]string, len(e))
			for i, v := range e {
				parts[i] = fmt.Sprint(v)
			}
			return t + " (" + strings.Join(parts, ", ") + ")"
		}
		return t
	case []any:
		parts := make([]string, len(t))
		for i, v := range t {
			parts[i] = fmt.Sprint(v)
		}
		return strings.Join(parts, " or ")
	}
	if _, ok := node["oneOf"]; ok {
		return "see schema"
	}
	return "object"
}

// TestSampleTrustPoliciesMatchAuthTestdata keeps the shipped TrustPolicy samples (Google
// Workspace, GitHub Actions, this repository's policy, a service account; R-AUTH-7,
// R-AUTH-12) identical to the specs the trust-policy engine is tested with, so every
// sample is known to compile and to accept and reject as documented.
func TestSampleTrustPoliciesMatchAuthTestdata(t *testing.T) {
	var samples struct {
		TrustPolicies []struct {
			Name string         `json:"name"`
			Spec map[string]any `json:"spec"`
		} `json:"trustPolicies"`
	}
	if err := yaml.Unmarshal(charttest.ReadFile(t, filepath.Join(charttest.ChartDir(t), "samples", "trust-policies.yaml")), &samples); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]map[string]any{}
	files, _ := filepath.Glob(filepath.Join(charttest.RepoRoot(t), "internal", "auth", "testdata", "policies", "*.yaml"))
	if len(files) == 0 && os.Getenv("TEST_SRCDIR") != "" {
		// Under Bazel the fixtures are runfiles of //internal/auth:sample_policies, whose
		// visibility does not include this package yet; `go test` covers this check.
		t.Skip("internal/auth/testdata/policies is not in the runfiles")
	}
	for _, f := range files {
		var tp struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     map[string]any        `json:"spec"`
		}
		if err := yaml.Unmarshal(charttest.ReadFile(t, f), &tp); err != nil {
			t.Fatal(err)
		}
		fixtures[tp.Metadata.Name] = tp.Spec
	}
	if len(samples.TrustPolicies) == 0 {
		t.Fatal("samples/trust-policies.yaml has no policies")
	}
	for _, s := range samples.TrustPolicies {
		want, ok := fixtures[s.Name]
		if !ok {
			t.Errorf("sample %q has no counterpart in internal/auth/testdata/policies", s.Name)
			continue
		}
		if !reflect.DeepEqual(s.Spec, want) {
			t.Errorf("sample %q differs from internal/auth/testdata/policies", s.Name)
		}
	}
}

// TestRenderedControllerConfigParses checks controller.json the way the binary does at
// startup (internal/controller.ParseConfig: strict JSON, defaults, per-mode validation)
// for every test profile and every mode that reads it (controller, STS, hooks).
func TestRenderedControllerConfigParses(t *testing.T) {
	chart := charttest.ChartDir(t)
	for _, profile := range []string{"small", "medium", "large"} {
		var configJSON string
		for _, o := range charttest.Objects(t, charttest.Template(t, []string{filepath.Join(chart, "ci", "values-"+profile+".yaml")})) {
			if o.Kind == "ConfigMap" && o.Metadata.Name == "cucina-controller" {
				configJSON = o.Data["controller.json"]
			}
		}
		if configJSON == "" {
			t.Fatalf("%s: no controller.json rendered", profile)
		}
		for _, mode := range []controller.Mode{controller.ModeController, controller.ModeSTS, controller.ModeTool} {
			if _, err := controller.ParseConfig([]byte(configJSON), mode); err != nil {
				t.Errorf("%s profile, %s mode: %v", profile, mode, err)
			}
		}
	}
}

// TestRenderedCertificateListParses checks certs.json (the --certs list of the bootstrap
// hook and the controller, ADR 0552/0403) strictly as []pki.CertSpec: one server
// certificate per TLS consumer of the chart-generated groups with a valid component
// identity and SANs, and the controller's client certificate without SANs.
func TestRenderedCertificateListParses(t *testing.T) {
	chart := charttest.ChartDir(t)
	cases := map[string]struct {
		values  []string
		servers int
	}{
		"generated (small)":        {[]string{filepath.Join(chart, "ci", "values-small.yaml")}, 5},
		"cert-manager (medium)":    {[]string{filepath.Join(chart, "ci", "values-medium.yaml")}, 0},
		"existing Secrets (large)": {[]string{filepath.Join(chart, "ci", "values-large.yaml")}, 0},
	}
	for name, c := range cases {
		var certs string
		for _, o := range charttest.Objects(t, charttest.Template(t, c.values)) {
			if o.Kind == "ConfigMap" && o.Metadata.Name == "cucina-controller" {
				certs = o.Data["certs.json"]
			}
		}
		var specs []pki.CertSpec
		if err := jsonv2.Unmarshal([]byte(certs), &specs, jsonv2.RejectUnknownMembers(true)); err != nil {
			t.Fatalf("%s: certs.json: %v", name, err)
		}
		servers, controllers := 0, 0
		for _, s := range specs {
			switch s.Role {
			case pki.RoleServer:
				servers++
				if _, err := pki.ServerIdentity(s.Component); err != nil {
					t.Errorf("%s: %s: %v", name, s.SecretName, err)
				}
				if len(s.DNSNames) == 0 && len(s.IPAddresses) == 0 {
					t.Errorf("%s: %s has no SANs", name, s.SecretName)
				}
				for _, ip := range s.IPAddresses {
					if net.ParseIP(ip) == nil {
						t.Errorf("%s: %s: invalid IP %q", name, s.SecretName, ip)
					}
				}
			case pki.RoleController:
				controllers++
				if len(s.DNSNames)+len(s.IPAddresses) > 0 || s.Component != "" {
					t.Errorf("%s: controller client certificate %s carries SANs or a component", name, s.SecretName)
				}
			default:
				t.Errorf("%s: %s has role %q", name, s.SecretName, s.Role)
			}
		}
		if servers != c.servers || controllers != 1 {
			t.Errorf("%s: %d server and %d controller certificates, want %d and 1", name, servers, controllers, c.servers)
		}
	}
}
