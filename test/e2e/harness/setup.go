// SPDX-License-Identifier: FSL-1.1-ALv2

package harness

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
)

// AWSSetup supplies local inputs that OpenTofu cannot know. It contains
// paths, never credentials. Generating a descriptor calls no cloud APIs.
type AWSSetup struct {
	Name, RunID, Expires, RepoDir, StateDir, ArtifactsDir, Bazel string
	Cucinactl                                                    map[string]string
	ValuesFiles                                                  []string
	AllowDestructive                                             bool
	WithIDP                                                      bool
}

// NewAWSEnv creates the campaign descriptor from flattened or `tofu output
// -json` output documents. A settings document is a partial Env: objects
// merge recursively, arrays replace. ParseEnv rejects unknown settings and
// Validate rejects region/tag drift. Nothing is installed or provisioned.
func NewAWSEnv(baseJSON, envJSON, settings []byte, o AWSSetup) (*Env, error) {
	decode := func(b []byte) (map[string]any, error) {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		for k, v := range m {
			if obj, ok := v.(map[string]any); ok {
				if value, ok := obj["value"]; ok {
					m[k] = value
				}
			}
		}
		return m, nil
	}
	base, err := decode(baseJSON)
	if err != nil {
		return nil, fmt.Errorf("base outputs: %w", err)
	}
	out, err := decode(envJSON)
	if err != nil {
		return nil, fmt.Errorf("env outputs: %w", err)
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	for layer, m := range map[string]map[string]any{"base": base, "env": out} {
		if str(m, "region") != "us-west-1" {
			return nil, fmt.Errorf("%s outputs: region must be us-west-1", layer)
		}
		tags, ok := m["tags"].(map[string]any)
		if !ok || str(tags, "cucina:env") != "e2e" || str(tags, "cucina:run") == "" || str(tags, "cucina:expires") == "" {
			return nil, fmt.Errorf("%s outputs: missing campaign tags", layer)
		}
		if o.RunID == "" {
			o.RunID = str(tags, "cucina:run")
		}
		if o.Expires == "" {
			o.Expires = str(tags, "cucina:expires")
		}
		if str(tags, "cucina:run") != o.RunID || str(tags, "cucina:expires") != o.Expires {
			return nil, fmt.Errorf("%s outputs: campaign run/expiry tags differ from requested descriptor", layer)
		}
	}
	public, private := str(out, "k3s_public_ip"), str(out, "k3s_private_ip")
	if public == "" || private == "" || str(out, "k3s_instance_id") == "" || str(out, "linux_client_instance_id") == "" {
		return nil, fmt.Errorf("env outputs need k3s_public_ip, k3s_private_ip, k3s_instance_id and linux_client_instance_id")
	}
	if o.Name == "" || o.RepoDir == "" || o.StateDir == "" || o.ArtifactsDir == "" {
		return nil, fmt.Errorf("descriptor name, repository, state and artifact directories are required")
	}
	if len(o.ValuesFiles) == 0 {
		o.ValuesFiles = []string{filepath.Join(o.StateDir, "values-endpoints.json"), filepath.Join(o.StateDir, "values-campaign.yaml")}
	}
	d := Env{
		Name: o.Name, Kind: EnvAWS, MeasurementScope: ScopeSmallFunctional, RunID: o.RunID, RepoDir: o.RepoDir, ArtifactsDir: o.ArtifactsDir,
		Kubernetes: &KubeEnv{Kubeconfig: filepath.Join(o.StateDir, "kubeconfig"), Namespace: "cucina", Release: "cucina", Chart: filepath.Join(o.RepoDir, "charts/cucina"), ValuesFiles: o.ValuesFiles},
		Endpoints: Endpoints{RemoteExecution: "grpcs://" + net.JoinHostPort(public, "443"), STS: "https://" + net.JoinHostPort(public, "8443"), Management: net.JoinHostPort(public, "8444"), Host: net.JoinHostPort(public, "8446"), Enrollment: "https://" + net.JoinHostPort(public, "8445"), PublicHost: public,
			WorkerListener: net.JoinHostPort(private, "8981"), InstanceName: "main", CAFile: filepath.Join(o.StateDir, "ca.pem"), Prometheus: "http://127.0.0.1:9090"},
		AWS:     &AWSEnv{Profile: "default", Region: "us-west-1", Tags: map[string]string{"cucina:env": "e2e", "cucina:run": o.RunID, "cucina:expires": o.Expires}, ClusterTag: "cucina", K3sInstanceID: str(out, "k3s_instance_id"), WorkerSecurityGroup: str(base, "sg_workers"), IsolationSecurityGroup: str(base, "sg_isolation"), SweepScript: filepath.Join(o.RepoDir, "deploy/aws-e2e/scripts/sweep.sh")},
		Clients: map[string]ClientEnv{}, Cucinactl: o.Cucinactl,
		DevMac:  &DevMacEnv{WorkDir: filepath.Join(o.ArtifactsDir, o.RunID, "dev-mac"), Bazel: o.Bazel, MDMKit: filepath.Join(o.RepoDir, "macos/pkg/scripts")},
		Pools:   map[string]string{"linux": "linux-x86-64", "linux-arm64": "linux-aarch64", "windows": "windows-x86-64", "macos": "macos-arm64-xcode27.0"},
		Abseil:  AbseilPin{Tag: "20260817.0", Commit: "2065f4ded0558c6f89fee67c8e5228feb4eb960e"},
		Secrets: Secrets{ServiceKeyFile: filepath.Join(o.StateDir, "e2e-writer.key"), ReadOnlyKeyFile: filepath.Join(o.StateDir, "e2e-readonly.key"), AdminKeyFile: filepath.Join(o.StateDir, "break-glass.key")},
		Safety:  Safety{MaxSpendUSD: DefaultBudgetUSD, MaxInstances: 16, AllowDestructive: o.AllowDestructive},
	}
	for _, name := range []string{"linux", "windows"} {
		if id := str(out, name+"_client_instance_id"); id != "" {
			cl := ClientEnv{OS: name, InstanceID: id, STS: "https://" + net.JoinHostPort(private, "8443"), RemoteExecution: "grpcs://" + net.JoinHostPort(private, "443"), Management: net.JoinHostPort(private, "8444"), WorkDir: `C:\e2e`}
			if name == "linux" {
				cl.User, cl.WorkDir = "ubuntu", "/home/ubuntu/e2e"
			}
			d.Clients[name+"-client"] = cl
		}
	}
	if o.WithIDP {
		d.IdP = &IdPEnv{MockOAuth2URL: "https://mock-oauth2-server.cucina-e2e.svc.cluster.local:30443", LocalAddr: "127.0.0.1:18443", CAFile: filepath.Join(o.StateDir, "mock-idp/ca.pem"), ClientID: "cucina-e2e", HostedDomain: "example.com"}
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var merged map[string]any
	if err := json.Unmarshal(encoded, &merged); err != nil {
		return nil, err
	}
	if len(settings) > 0 {
		var patch map[string]any
		if err := json.Unmarshal(settings, &patch); err != nil {
			return nil, fmt.Errorf("settings: %w", err)
		}
		mergeSettings(merged, patch)
	}
	encoded, err = json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	// Derive capabilities only when the corresponding configuration is
	// present; the offline check reports configured-but-missing files.
	parsed, err := ParseEnv(encoded)
	if err != nil {
		return nil, err
	}
	if parsed.AWS == nil || parsed.RunID != o.RunID || parsed.AWS.Tags["cucina:run"] != o.RunID || parsed.AWS.Tags["cucina:expires"] != o.Expires {
		return nil, fmt.Errorf("settings cannot change the campaign identity from the OpenTofu outputs")
	}
	parsed.Capabilities = []Requirement{RequiresAWS, RequiresKubernetes, RequiresPrometheus}
	for name := range parsed.Clients {
		parsed.Capabilities = append(parsed.Capabilities, Requirement(name))
	}
	if len(parsed.Cucinactl) > 0 {
		parsed.Capabilities = append(parsed.Capabilities, RequiresCucinactl)
	}
	if parsed.RepoDir != "" {
		parsed.Capabilities = append(parsed.Capabilities, RequiresCrossMatrix)
	}
	if parsed.Safety.AllowDestructive {
		parsed.Capabilities = append(parsed.Capabilities, RequiresDestructive)
	}
	if parsed.IdP != nil {
		parsed.Capabilities = append(parsed.Capabilities, RequiresIdP)
	}
	if mac := parsed.DevMac; mac != nil {
		if mac.HostdBinary != "" && mac.HostdConfig != "" {
			parsed.Capabilities = append(parsed.Capabilities, RequiresMacHost)
		}
		if mac.PkgPath != "" && mac.MDMKit != "" {
			parsed.Capabilities = append(parsed.Capabilities, RequiresHostdPkg)
		}
	}
	return parsed, parsed.Validate()
}

func mergeSettings(dst, src map[string]any) {
	for k, v := range src {
		if obj, ok := v.(map[string]any); ok {
			if prev, ok := dst[k].(map[string]any); ok {
				mergeSettings(prev, obj)
				continue
			}
		}
		dst[k] = v
	}
}

// DescriptorName accepts only a single safe filename component.
func DescriptorName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.ContainsAny(name, "\n\r\x00")
}
