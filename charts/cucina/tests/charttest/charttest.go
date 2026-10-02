// SPDX-License-Identifier: FSL-1.1-ALv2

// Package charttest holds the helpers of the Helm chart's Go tests
// (charts/cucina/tests/static and charts/cucina/tests/boot): locating the chart,
// running the pinned helm binary, extracting rendered objects, and minting Cucina
// JWTs with a throwaway key. Test-only; never imported by production code.
package charttest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// RepoRoot locates the repository: $CUCINA_REPO_ROOT, the Bazel runfiles root
// (the chart files are data dependencies), or the source tree of this file.
func RepoRoot(t testing.TB) string {
	t.Helper()
	if r := os.Getenv("CUCINA_REPO_ROOT"); r != "" {
		return r
	}
	if src := os.Getenv("TEST_SRCDIR"); src != "" {
		ws := os.Getenv("TEST_WORKSPACE")
		if ws == "" {
			ws = "_main"
		}
		return filepath.Join(src, ws)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}

// ChartDir is charts/cucina.
func ChartDir(t testing.TB) string { return filepath.Join(RepoRoot(t), "charts", "cucina") }

// WritableChartFile is the source-tree path of a chart file that an -update run
// rewrites (never Bazel's read-only runfiles).
func WritableChartFile(t testing.TB, rel string) string {
	t.Helper()
	if ws := os.Getenv("BUILD_WORKSPACE_DIRECTORY"); ws != "" {
		return filepath.Join(ws, "charts", "cucina", rel)
	}
	if os.Getenv("TEST_SRCDIR") != "" {
		t.Fatal("-update must run from the source tree (go test) or `bazel run`, not `bazel test`")
	}
	return filepath.Join(ChartDir(t), rel)
}

// Tool returns the binary named by env (Bazel passes the pinned tools) or name on
// PATH; without either the test is skipped outside Bazel and fails under Bazel.
func Tool(t testing.TB, env, name string) string {
	t.Helper()
	if p := os.Getenv(env); p != "" {
		if filepath.IsAbs(p) {
			return p
		}
		// Bazel passes $(rlocationpath …): relative to the runfiles root.
		for _, root := range []string{os.Getenv("RUNFILES_DIR"), os.Getenv("TEST_SRCDIR"), "."} {
			if root == "" {
				continue
			}
			if c, err := filepath.Abs(filepath.Join(root, p)); err == nil {
				if _, err := os.Stat(c); err == nil {
					return c
				}
			}
		}
		t.Fatalf("%s=%q does not name an existing file", env, p)
	}
	p, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("TEST_SRCDIR") != "" {
			t.Fatalf("%s is not set; the Bazel target must provide %s", env, name)
		}
		t.Skipf("%s not found: install it or set %s", name, env)
	}
	return p
}

// Helm runs the helm binary ($HELM or PATH) with isolated cache/config dirs.
func Helm(t testing.TB, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(Tool(t, "HELM", "helm"), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+filepath.Join(home, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(home, "config"), "HELM_DATA_HOME="+filepath.Join(home, "data"))
	out, err := cmd.Output()
	if err != nil {
		return out, &CommandError{Args: args, Err: err, Stderr: stderr.String()}
	}
	return out, nil
}

// CommandError carries the stderr of a failed helm run.
type CommandError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *CommandError) Error() string {
	return "helm " + strings.Join(e.Args, " ") + ": " + e.Err.Error() + "\n" + e.Stderr
}

// Template renders the chart (release "cucina", namespace "cucina").
func Template(t testing.TB, valuesFiles []string, sets ...string) []byte {
	t.Helper()
	out, err := Helm(t, TemplateArgs(t, valuesFiles, sets...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TemplateArgs are the helm arguments of Template.
func TemplateArgs(t testing.TB, valuesFiles []string, sets ...string) []string {
	args := []string{"template", "cucina", ChartDir(t), "--namespace", "cucina"}
	for _, f := range valuesFiles {
		args = append(args, "-f", f)
	}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	return args
}

// Object is one rendered Kubernetes object.
type Object struct {
	Kind     string                `json:"kind"`
	Metadata struct{ Name string } `json:"metadata"`
	Data     map[string]string     `json:"data"`
	Spec     map[string]any        `json:"spec"`
	Raw      []byte                `json:"-"`
}

// Objects splits rendered manifests into objects.
func Objects(t testing.TB, manifests []byte) []Object {
	t.Helper()
	var out []Object
	for _, doc := range strings.Split(string(manifests), "\n---\n") {
		if strings.TrimSpace(doc) == "" || strings.TrimSpace(doc) == "---" {
			continue
		}
		var o Object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("rendered manifest is not YAML: %v\n%s", err, doc)
		}
		if o.Kind == "" {
			continue
		}
		o.Raw = []byte(doc)
		out = append(out, o)
	}
	return out
}

// ReadFile reads a file or fails the test.
func ReadFile(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// MarshalJSON is json.MarshalIndent without HTML escaping, with a final newline.
func MarshalJSON(t testing.TB, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// SortedKeys returns the keys of m in order.
func SortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
