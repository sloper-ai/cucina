// SPDX-License-Identifier: FSL-1.1-ALv2

package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sloper-ai/cucina/test/e2e/harness"
)

// Component label values of the chart's workloads
// (app.kubernetes.io/component).
const (
	ComponentFrontend   = "frontend"
	ComponentStorage    = "storage"
	ComponentScheduler  = "scheduler"
	ComponentController = "controller"
	ComponentSTS        = "sts"
)

// Kube runs kubectl and helm against the campaign release.
type Kube struct {
	K harness.KubeEnv
}

func (k *Kube) base() []string {
	args := []string{"--kubeconfig", k.K.Kubeconfig}
	if k.K.Context != "" {
		args = append(args, "--context", k.K.Context)
	}
	return args
}

// Kubectl runs kubectl in the release namespace.
func (k *Kube) Kubectl(ctx context.Context, args ...string) ([]byte, error) {
	return run(ctx, nil, "kubectl", append(append(k.base(), "-n", k.K.Namespace), args...)...)
}

// Helm runs helm in the release namespace.
func (k *Kube) Helm(ctx context.Context, args ...string) ([]byte, error) {
	hargs := []string{"--kubeconfig", k.K.Kubeconfig, "-n", k.K.Namespace}
	if k.K.Context != "" {
		hargs = append(hargs, "--kube-context", k.K.Context)
	}
	return run(ctx, nil, "helm", append(hargs, args...)...)
}

func (k *Kube) valuesArgs(extra []string) []string {
	var out []string
	for _, v := range k.K.ValuesFiles {
		out = append(out, "-f", v)
	}
	return append(out, extra...)
}

// Install performs a fresh `helm install --wait` of the packaged chart (T0).
func (k *Kube) Install(ctx context.Context, timeout time.Duration, extra ...string) ([]byte, error) {
	args := append([]string{"install", k.K.Release, k.K.Chart, "--create-namespace", "--wait", "--timeout", timeout.String()}, k.valuesArgs(extra)...)
	return k.Helm(ctx, args...)
}

// Upgrade performs `helm upgrade --wait` with extra values (T11).
func (k *Kube) Upgrade(ctx context.Context, timeout time.Duration, extra ...string) ([]byte, error) {
	args := append([]string{"upgrade", k.K.Release, k.K.Chart, "--wait", "--timeout", timeout.String()}, k.valuesArgs(extra)...)
	return k.Helm(ctx, args...)
}

// Rollback rolls back to a revision (0 = previous).
func (k *Kube) Rollback(ctx context.Context, revision int, timeout time.Duration) ([]byte, error) {
	args := []string{"rollback", k.K.Release}
	if revision > 0 {
		args = append(args, strconv.Itoa(revision))
	}
	return k.Helm(ctx, append(args, "--wait", "--timeout", timeout.String())...)
}

// Test runs `helm test`.
func (k *Kube) Test(ctx context.Context, timeout time.Duration) ([]byte, error) {
	return k.Helm(ctx, "test", k.K.Release, "--timeout", timeout.String(), "--logs")
}

// Uninstall runs `helm uninstall --wait` (the pre-delete hook drains pools).
func (k *Kube) Uninstall(ctx context.Context, timeout time.Duration) ([]byte, error) {
	return k.Helm(ctx, "uninstall", k.K.Release, "--wait", "--timeout", timeout.String())
}

// Revision returns the release's current revision.
func (k *Kube) Revision(ctx context.Context) (int, error) {
	out, err := k.Helm(ctx, "status", k.K.Release, "-o", "json")
	if err != nil {
		return 0, err
	}
	var st struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return 0, err
	}
	return st.Version, nil
}

// Pod is the part of a pod the scenarios look at.
type Pod struct {
	Name      string
	Component string
	Phase     string
	Ready     bool
	Restarts  int
	Node      string
	Started   time.Time
}

// Pods lists the namespace's pods.
func (k *Kube) Pods(ctx context.Context, selector string) ([]Pod, error) {
	args := []string{"get", "pods", "-o", "json"}
	if selector != "" {
		args = append(args, "-l", selector)
	}
	out, err := k.Kubectl(ctx, args...)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Phase      string    `json:"phase"`
				StartTime  time.Time `json:"startTime"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				ContainerStatuses []struct {
					RestartCount int `json:"restartCount"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	var pods []Pod
	for _, it := range list.Items {
		p := Pod{Name: it.Metadata.Name, Component: it.Metadata.Labels["app.kubernetes.io/component"], Phase: it.Status.Phase, Node: it.Spec.NodeName, Started: it.Status.StartTime}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				p.Ready = true
			}
		}
		for _, cs := range it.Status.ContainerStatuses {
			p.Restarts += cs.RestartCount
		}
		pods = append(pods, p)
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods, nil
}

// NotReady lists running pods that are not Ready (completed hook/test pods
// excluded).
func NotReady(pods []Pod) []string {
	var out []string
	for _, p := range pods {
		if p.Phase == "Succeeded" {
			continue
		}
		if !p.Ready {
			out = append(out, p.Name+"("+p.Phase+")")
		}
	}
	return out
}

// DeletePods deletes the pods of a component (T9b scheduler, T9c storage).
func (k *Kube) DeletePods(ctx context.Context, component string, grace time.Duration) ([]string, error) {
	pods, err := k.Pods(ctx, "app.kubernetes.io/component="+component)
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("no %s pods", component)
	}
	var names []string
	for _, p := range pods {
		names = append(names, p.Name)
	}
	_, err = k.Kubectl(ctx, append([]string{"delete", "pod", "--wait=false", fmt.Sprintf("--grace-period=%d", int(grace.Seconds()))}, names...)...)
	return names, err
}

// RolloutRestart restarts a deployment/statefulset and waits (T9d).
func (k *Kube) RolloutRestart(ctx context.Context, kind, name string, timeout time.Duration) error {
	if _, err := k.Kubectl(ctx, "rollout", "restart", kind+"/"+name); err != nil {
		return err
	}
	_, err := k.Kubectl(ctx, "rollout", "status", kind+"/"+name, "--timeout", timeout.String())
	return err
}

// WaitReady waits until every pod of the namespace is Ready.
func (k *Kube) WaitReady(ctx context.Context, timeout time.Duration) error {
	_, err := k.Kubectl(ctx, "wait", "--for=condition=Ready", "pod", "--all", "--timeout", timeout.String(),
		"--field-selector=status.phase!=Succeeded")
	return err
}

// Event is a Kubernetes event (controller timeline: launch, register, drain…).
type Event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
}

// Events returns the namespace's events since t for objects of kind (e.g.
// WorkerPool).
func (k *Kube) Events(ctx context.Context, kind string, since time.Time) ([]Event, error) {
	out, err := k.Kubectl(ctx, "get", "events", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
			Reason        string    `json:"reason"`
			Message       string    `json:"message"`
			LastTimestamp time.Time `json:"lastTimestamp"`
			EventTime     time.Time `json:"eventTime"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	var evs []Event
	for _, it := range list.Items {
		t := it.LastTimestamp
		if t.IsZero() {
			t = it.EventTime
		}
		if (kind != "" && !strings.EqualFold(it.InvolvedObject.Kind, kind)) || t.Before(since) {
			continue
		}
		evs = append(evs, Event{Time: t, Kind: it.InvolvedObject.Kind, Name: it.InvolvedObject.Name, Reason: it.Reason, Message: it.Message})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Time.Before(evs[j].Time) })
	return evs, nil
}
