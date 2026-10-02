// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// CRDFieldManager owns the fields `crds apply` sets on Cucina's CRDs.
const CRDFieldManager = "cucina-controller"

const (
	crdAPIVersion = "apiextensions.k8s.io/v1"
	crdKind       = "CustomResourceDefinition"
)

// ParseCRDs reads every *.yaml in fsys (api/crds.FS: the CRDs this binary was
// built against) and returns the CustomResourceDefinitions sorted by name. Any
// other kind, an unnamed object or an empty bundle is an error.
func ParseCRDs(fsys fs.FS) ([]*unstructured.Unstructured, error) {
	files, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		dec := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(b)))
		for {
			doc, err := dec.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			if len(bytes.TrimSpace(doc)) == 0 || isOnlyComments(doc) {
				continue
			}
			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(doc, &u.Object); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			if u.GetAPIVersion() != crdAPIVersion || u.GetKind() != crdKind {
				return nil, fmt.Errorf("%s: %s %s is not a %s/%s", f, u.GetAPIVersion(), u.GetKind(), crdAPIVersion, crdKind)
			}
			if u.GetName() == "" {
				return nil, fmt.Errorf("%s: CustomResourceDefinition without metadata.name", f)
			}
			// Server-side apply takes only intent: no status, no server-set metadata.
			unstructured.RemoveNestedField(u.Object, "status")
			unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no CustomResourceDefinitions in the embedded bundle")
	}
	slices.SortFunc(out, func(a, b *unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out, nil
}

// CRDNames lists the objects' names in order.
func CRDNames(objs []*unstructured.Unstructured) []string {
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.GetName())
	}
	return names
}

// CRDApplyRules is the cluster-scoped RBAC `crds apply` needs: get (to wait
// for Established), create (server-side apply of a missing CRD) and patch, on
// exactly the named CRDs. The chart's CRD-hook ClusterRole must equal it.
func CRDApplyRules(names []string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		APIGroups:     []string{"apiextensions.k8s.io"},
		Resources:     []string{"customresourcedefinitions"},
		ResourceNames: slices.Clone(names),
		Verbs:         []string{"get", "create", "patch"},
	}}
}

// ApplyCRDs server-side applies objs as CRDFieldManager, forcing ownership of
// fields another manager set (Helm creates crds/ on first install), then waits
// until every CRD is Established so that objects of their kinds can be created
// right after. The chart runs it before every install and upgrade
// (`cucina-controller crds apply`, ADR 0406) because Helm never upgrades crds/.
func ApplyCRDs(ctx context.Context, c client.Client, objs []*unstructured.Unstructured, log *slog.Logger) error {
	for _, o := range objs {
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), client.FieldOwner(CRDFieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply CustomResourceDefinition %s: %w", o.GetName(), err)
		}
		log.Info("applied CustomResourceDefinition", "name", o.GetName())
	}
	for _, o := range objs {
		if err := waitEstablished(ctx, c, o.GetName()); err != nil {
			return err
		}
		log.Info("CustomResourceDefinition established", "name", o.GetName())
	}
	return nil
}

func waitEstablished(ctx context.Context, c client.Client, name string) error {
	last := "not observed yet"
	for {
		got := &unstructured.Unstructured{}
		got.SetAPIVersion(crdAPIVersion)
		got.SetKind(crdKind)
		err := c.Get(ctx, client.ObjectKey{Name: name}, got)
		if err == nil {
			conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
			var parts []string
			for _, raw := range conds {
				m, _ := raw.(map[string]any)
				if m["type"] == "Established" && m["status"] == "True" {
					return nil
				}
				parts = append(parts, fmt.Sprintf("%v=%v (%v)", m["type"], m["status"], m["message"]))
			}
			if len(parts) > 0 {
				last = strings.Join(parts, "; ")
			}
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("CustomResourceDefinition %s is not Established: %s: %w", name, last, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func isOnlyComments(doc []byte) bool {
	for line := range strings.SplitSeq(string(doc), "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			return false
		}
	}
	return true
}
