// SPDX-License-Identifier: FSL-1.1-ALv2

package static_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"github.com/sloper-ai/cucina/charts/cucina/tests/charttest"
	"github.com/sloper-ai/cucina/internal/controller"
)

// TestRenderedRBACMatchesBinaries keeps the chart's RBAC equal to what the binaries
// need: the controller and STS Roles grant controller.PolicyRules (what each mode
// checks with SelfSubjectAccessReviews at startup, R-TEST-7), and the CRD hook's
// ClusterRole grants exactly controller.CRDApplyRules for the CRDs in crds/ (ADR 0406;
// `helm install` on kind failed while the CRDs were templates).
func TestRenderedRBACMatchesBinaries(t *testing.T) {
	objs, err := controller.ParseCRDs(os.DirFS(filepath.Join(charttest.ChartDir(t), "crds")))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]rbacv1.PolicyRule{
		"Role/cucina-controller":         controller.PolicyRules(controller.ModeController),
		"Role/cucina-sts":                controller.PolicyRules(controller.ModeSTS),
		"ClusterRole/cucina-crds-cucina": controller.CRDApplyRules(controller.CRDNames(objs)),
	}
	// Grants beyond the self-check, each with its reason.
	extra := map[string][]string{
		// The STS's event recorder may use either Events API, as the controller's does.
		"Role/cucina-sts": {"events.k8s.io/events/*/create", "events.k8s.io/events/*/patch"},
	}
	seen := map[string]bool{}
	for _, o := range charttest.Objects(t, charttest.Template(t, nil)) {
		key := o.Kind + "/" + o.Metadata.Name
		rules, ok := want[key]
		if !ok {
			continue
		}
		seen[key] = true
		var got struct {
			Rules []rbacv1.PolicyRule `json:"rules"`
		}
		if err := yaml.Unmarshal(o.Raw, &got); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		g, w := grants(got.Rules), grants(rules)
		for _, p := range w {
			if !slices.Contains(g, p) {
				t.Errorf("%s does not grant %s", key, p)
			}
		}
		for _, p := range g {
			if !slices.Contains(w, p) && !slices.Contains(extra[key], p) {
				t.Errorf("%s grants %s, which the binary does not need", key, p)
			}
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("%s is not rendered", key)
		}
	}
}

// grants flattens rules into sorted "group/resource/name/verb" permissions.
func grants(rules []rbacv1.PolicyRule) []string {
	var out []string
	for _, r := range rules {
		names := r.ResourceNames
		if len(names) == 0 {
			names = []string{"*"}
		}
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				for _, n := range names {
					for _, v := range r.Verbs {
						out = append(out, g+"/"+res+"/"+n+"/"+v)
					}
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
