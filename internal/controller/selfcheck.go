// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/ports"
)

// Rule is one RBAC permission the controller needs (docs/dev/controller.md
// mirrors this table for the chart's Role/ClusterRole).
type Rule struct {
	Group       string
	Resource    string // "workerpools", "workerpools/status", …
	Verbs       []string
	ClusterWide bool
}

const cucinaGroup = "cucina.sloper.ai"

// RequiredRules lists the permissions each mode needs. The controller checks
// them at startup with SelfSubjectAccessReviews and refuses to start with the
// list of what is missing (R-TEST-7 "AWS/RBAC permission self-checks").
func RequiredRules(mode Mode) []Rule {
	rw := []string{"get", "list", "watch", "create", "update", "patch"}
	rwd := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	ro := []string{"get", "list", "watch"}
	switch mode {
	case ModeSTS:
		return []Rule{
			{cucinaGroup, "trustpolicies", ro, false},
			{"", "secrets", ro, false},
			{"", "configmaps", ro, false},
			{"", "events", []string{"create", "patch"}, false},
		}
	case ModeController:
		return []Rule{
			{cucinaGroup, "workerpools", []string{"get", "list", "watch", "update", "patch"}, false},
			{cucinaGroup, "workerpools/status", []string{"get", "update", "patch"}, false},
			{cucinaGroup, "workerpools/finalizers", []string{"update"}, false},
			{cucinaGroup, "machosts", rwd, false},
			{cucinaGroup, "machosts/status", []string{"get", "update", "patch"}, false},
			{cucinaGroup, "machosts/finalizers", []string{"update"}, false},
			{cucinaGroup, "trustpolicies", ro, false},
			{cucinaGroup, "trustpolicies/status", []string{"get", "update", "patch"}, false},
			{"coordination.k8s.io", "leases", []string{"get", "list", "create", "update", "delete"}, false},
			{"", "events", []string{"create", "patch"}, false},
			{"events.k8s.io", "events", []string{"create", "patch"}, false},
			{"", "secrets", rw, false},
			{"", "configmaps", rw, false},
			{"apps", "deployments", []string{"get", "list", "watch", "patch"}, false},
			{"apps", "statefulsets", ro, false},
			{"", "pods", []string{"get", "list", "watch", "patch"}, false},
		}
	}
	return nil
}

// CheckRBAC asks the API server whether this identity has every required
// permission in namespace and returns one error listing all missing ones. The
// SelfSubjectAccessReviews run concurrently (one per resource and verb).
func CheckRBAC(ctx context.Context, c client.Client, namespace string, rules []Rule) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	type check struct {
		rule Rule
		verb string
	}
	var checks []check
	for _, r := range rules {
		for _, v := range r.Verbs {
			checks = append(checks, check{r, v})
		}
	}
	missing := make([]string, len(checks))
	errs := make([]error, len(checks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, ch := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, sub, _ := strings.Cut(ch.rule.Resource, "/")
			ns := namespace
			if ch.rule.ClusterWide {
				ns = ""
			}
			ssar := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Verb: ch.verb, Group: ch.rule.Group, Resource: res, Subresource: sub},
			}}
			if err := c.Create(ctx, ssar); err != nil {
				errs[i] = err
				return
			}
			if !ssar.Status.Allowed {
				g := ch.rule.Group
				if g == "" {
					g = "core"
				}
				missing[i] = fmt.Sprintf("%s %s/%s in %q", ch.verb, g, ch.rule.Resource, ns)
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("RBAC self-check: SelfSubjectAccessReview failed (is the API server reachable?): %w", err)
	}
	missing = slices.DeleteFunc(missing, func(s string) bool { return s == "" })
	if len(missing) > 0 {
		return fmt.Errorf("RBAC self-check: the controller's ServiceAccount lacks %d permission(s): %s (the chart's Role must grant docs/dev/controller.md#rbac)",
			len(missing), strings.Join(missing, "; "))
	}
	return nil
}

// CheckAWS performs a tag-filtered Describe with the cluster tag (R-SCALE-6)
// to prove that credentials, region and the controller's IAM policy work.
func CheckAWS(ctx context.Context, compute ports.Compute, cfg *config.Controller) error {
	if cfg.AWS == nil {
		return nil
	}
	if compute == nil {
		return errors.New("AWS self-check: the configuration has an aws section but no EC2 adapter is compiled in (internal/controller/components_ec2.go)")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := compute.Describe(ctx, ports.InstanceFilter{Cluster: cfg.ClusterID}); err != nil {
		return fmt.Errorf("AWS self-check: DescribeInstances filtered by tag cucina:cluster=%s in %s failed: %w "+
			"(check the controller's AWS credentials — IRSA/EKS Pod Identity or the node instance profile with IMDSv2 hop limit 2 — "+
			"and that its IAM policy allows ec2:DescribeInstances)", cfg.ClusterID, cfg.AWS.Region, err)
	}
	return nil
}

// PolicyRules renders RequiredRules as RBAC policy rules (one rule per API
// group and verb set) — what the chart's namespaced Role must grant.
func PolicyRules(mode Mode) []rbacv1.PolicyRule {
	var out []rbacv1.PolicyRule
	for _, r := range RequiredRules(mode) {
		if r.ClusterWide {
			continue
		}
		merged := false
		for i := range out {
			if out[i].APIGroups[0] == r.Group && slices.Equal(out[i].Verbs, r.Verbs) {
				out[i].Resources = append(out[i].Resources, r.Resource)
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, rbacv1.PolicyRule{APIGroups: []string{r.Group}, Resources: []string{r.Resource}, Verbs: slices.Clone(r.Verbs)})
		}
	}
	return out
}
