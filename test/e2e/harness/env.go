// SPDX-License-Identifier: FSL-1.1-ALv2

package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ScopeSmallFunctional is the user-selected .large/max-one-worker campaign.
// Diagnostic timings must not qualify the original four-worker benchmark.
const ScopeSmallFunctional = "small-functional"

// Env is an environment descriptor, read from
// ~/.config/cucina/e2e/<name>.json (0600, never committed: it holds instance
// IDs, endpoints and other environment identifiers, §12). It is generated for
// the AWS campaign from the OpenTofu outputs by `e2e env`.
type Env struct {
	Name string  `json:"name"`
	Kind EnvKind `json:"kind"`
	// MeasurementScope distinguishes functional checks from the original
	// max-four, large-worker performance campaign.
	MeasurementScope string `json:"measurementScope,omitempty"`
	// RunID is the cucina:run tag value of the campaign (§12).
	RunID string `json:"runId"`
	// Capabilities lists the Requirements this environment satisfies.
	Capabilities []Requirement `json:"capabilities"`
	Kubernetes   *KubeEnv      `json:"kubernetes,omitempty"`
	Endpoints    Endpoints     `json:"endpoints"`
	AWS          *AWSEnv       `json:"aws,omitempty"`
	// Clients maps a client name ("linux-client", "windows-client") to its VM.
	Clients map[string]ClientEnv `json:"clients,omitempty"`
	DevMac  *DevMacEnv           `json:"devMac,omitempty"`
	IdP     *IdPEnv              `json:"idp,omitempty"`
	// Cucinactl locates the CLI binaries per client OS ("darwin", "linux",
	// "windows"): local paths on the dev Mac, uploaded to clients on demand.
	Cucinactl map[string]string `json:"cucinactl,omitempty"`
	// Pools names the campaign pools by role ("linux", "linux-arm64",
	// "windows", "macos").
	Pools  map[string]string `json:"pools,omitempty"`
	Abseil AbseilPin         `json:"abseil"`
	Safety Safety            `json:"safety"`
	// Secrets are paths of 0600 files under ~/.config/cucina (never their
	// contents): service-account keys created after the install.
	Secrets Secrets `json:"secrets,omitempty"`
	// ArtifactsDir receives raw artifacts and results (bulk data; no secrets).
	ArtifactsDir string `json:"artifactsDir"`
	// RepoDir is the Cucina checkout on the dev Mac.
	RepoDir string `json:"repoDir,omitempty"`
	// Images maps a pool to its current and next image (T12 rollout).
	Images map[string]ImageVersions `json:"images,omitempty"`
	// Cucina is the commit of this repository the clients clone for T22.
	Cucina *RepoPin `json:"cucina,omitempty"`
	// WorkerSelectors maps a lane (linux, windows, macos) to the PromQL label
	// matchers selecting its bb_worker metrics (default job=~".*worker.*").
	WorkerSelectors map[string]string `json:"workerSelectors,omitempty"`
	// CLI names disposable, isolated T20 resources, never production fixtures.
	CLI *CLIFixtures `json:"cli,omitempty"`
	// CrossInventoryFile pins canonical repository -> component/version/variant
	// and immutable source identity for NFR-X5; never infer versions from names.
	CrossInventoryFile string `json:"crossInventoryFile,omitempty"`
}

// CLIFixtures is the explicit allow-list for T20 destructive CLI coverage.
// The scenario verifies run-scoped names, idle state and operation ownership.
type CLIFixtures struct {
	Pool         string `json:"pool,omitempty"`
	Host         string `json:"host,omitempty"`
	VM           string `json:"vm,omitempty"`
	InvocationID string `json:"invocationId,omitempty"`
	Operation    string `json:"operation,omitempty"`
}

// ImageVersions are a pool's image IDs for T12.
type ImageVersions struct {
	Current string `json:"current"`
	Next    string `json:"next"`
}

// RepoPin is a git repository at a commit.
type RepoPin struct {
	URL    string `json:"url"`
	Commit string `json:"commit"`
}

// KubeEnv locates the cluster and the Helm release.
type KubeEnv struct {
	Kubeconfig  string   `json:"kubeconfig"`
	Context     string   `json:"context,omitempty"`
	Namespace   string   `json:"namespace"`
	Release     string   `json:"release"`
	Chart       string   `json:"chart"` // packaged chart (.tgz) or directory
	ValuesFiles []string `json:"valuesFiles,omitempty"`
	// UpgradeValuesFile carries T11's configuration change (add a pool,
	// change a cache size).
	UpgradeValuesFile string `json:"upgradeValuesFile,omitempty"`
	// BootstrapCLI optionally runs a lead-owned private script after T0's
	// install/test to export the CA/admin key and configure the local profile.
	// Paths/arguments only, never inline credentials. Empty requires an
	// already configured profile that is verified against the descriptor.
	BootstrapCLI []string `json:"bootstrapCLI,omitempty"`
	// RotateSigningKey is the argv that starts a signing-key rotation
	// (T10e), e.g. ["kubectl","exec","deploy/cucina-controller","--",
	// "cucina-controller","keys","rotate"].
	RotateSigningKey []string `json:"rotateSigningKey,omitempty"`
	// MockOAuth2Manifest deploys navikt/mock-oauth2-server for T10.
	MockOAuth2Manifest string `json:"mockOAuth2Manifest,omitempty"`
}

// Endpoints are the client-facing endpoints of the deployment.
type Endpoints struct {
	RemoteExecution string `json:"remoteExecution,omitempty"` // grpcs://host:port
	InstanceName    string `json:"instanceName,omitempty"`
	STS             string `json:"sts,omitempty"`        // https://host:port
	Management      string `json:"management,omitempty"` // host:port
	// Prometheus is the in-cluster Prometheus API base URL as reachable from
	// the dev Mac (a kubectl port-forward the campaign script opens).
	Prometheus string `json:"prometheus,omitempty"`
	CAFile     string `json:"caFile,omitempty"`
	// PublicHost is the endpoint host name/IP (the k3s Elastic IP), used by
	// the external port scan (T10i).
	PublicHost string `json:"publicHost,omitempty"`
	// Host is the HostService endpoint (host:port, mTLS) Mac hosts dial (T10h).
	Host string `json:"host,omitempty"`
	// Enrollment is TLS (not mTLS); clients enroll before they have a certificate.
	Enrollment string `json:"enrollment,omitempty"`
	// WorkerListener is the private mTLS worker endpoint (host:port) as seen
	// from the Linux client (T10h).
	WorkerListener string `json:"workerListener,omitempty"`
}

// AWSEnv scopes every AWS call (§12): profile, region and the campaign tags.
type AWSEnv struct {
	Profile string            `json:"profile"`
	Region  string            `json:"region"`
	Tags    map[string]string `json:"tags"` // cucina:env, cucina:run, cucina:expires
	// ClusterTag is the cucina:cluster value the controller stamps on workers.
	ClusterTag string `json:"clusterTag,omitempty"`
	// K3sInstanceID is the control-plane node (for T9 pod faults via SSM).
	K3sInstanceID string `json:"k3sInstanceId,omitempty"`
	// WorkerSecurityGroup is swapped out for T9e (network cut).
	WorkerSecurityGroup    string `json:"workerSecurityGroup,omitempty"`
	IsolationSecurityGroup string `json:"isolationSecurityGroup,omitempty"`
	SweepScript            string `json:"sweepScript,omitempty"`
}

// ClientEnv is a client VM reached through SSM (no inbound ports).
type ClientEnv struct {
	OS         string `json:"os"` // linux | windows
	InstanceID string `json:"instanceId"`
	User       string `json:"user,omitempty"`    // non-root user for Bazel on Linux
	WorkDir    string `json:"workDir,omitempty"` // e.g. /home/ubuntu/e2e or C:\e2e
	// STS overrides endpoints.sts for this client: in-VPC clients reach the
	// control plane by its private address (R-DATA-4, no public-IP hairpin).
	STS string `json:"sts,omitempty"`
	// Discovery may advertise the public endpoint. Override the profile for
	// in-VPC clients so REAPI and management never hairpin over the EIP.
	RemoteExecution string `json:"remoteExecution,omitempty"`
	Management      string `json:"management,omitempty"`
}

// DevMacEnv describes the dev Mac acting as host and macOS client (T13).
type DevMacEnv struct {
	WorkDir string `json:"workDir"`
	// Bazel is the Bazelisk binary on the dev Mac (the mise shim on PATH only
	// resolves inside directories with a mise.toml).
	Bazel       string `json:"bazel,omitempty"`
	HostdBinary string `json:"hostdBinary,omitempty"`
	HostdConfig string `json:"hostdConfig,omitempty"`
	TartHome    string `json:"tartHome,omitempty"`
	BaseImage   string `json:"baseImage,omitempty"`
	PkgPath     string `json:"pkgPath,omitempty"`
	// UpgradePkgPath is a newer build of the pkg for T14's upgrade in place.
	UpgradePkgPath string `json:"upgradePkgPath,omitempty"`
	// SignerCert is the pkg signing certificate (PEM) the MDM simulation trusts.
	SignerCert string `json:"signerCert,omitempty"`
	MDMKit     string `json:"mdmKit,omitempty"` // macos/pkg/scripts (simulate-mdm.sh, publish-s3-temp.sh)
}

// IdPEnv locates the mock OIDC issuers (T10, ADR 1003). The issuers live at
// the in-cluster HTTPS URL (the STS requires HTTPS and checks that the
// discovery document's issuer equals the TrustPolicy's), which the dev Mac
// reaches through a kubectl port-forward on LocalAddr.
type IdPEnv struct {
	// MockOAuth2URL is the issuers' base URL as the STS sees it, e.g.
	// https://mock-oauth2-server.cucina-e2e.svc.cluster.local:30443 (issuers at
	// /google, /google-short, /github).
	MockOAuth2URL string `json:"mockOAuth2Url"`
	// LocalAddr is the dev Mac's port-forward to the mock (127.0.0.1:18443).
	LocalAddr string `json:"localAddr,omitempty"`
	// CAFile is the throwaway CA that signed the mock's certificate.
	CAFile       string `json:"caFile,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	HostedDomain string `json:"hostedDomain,omitempty"`
}

// Secrets locates credentials the campaign uses. They are moved to client
// VMs only over the SSM port-forwarding path (remote.PutPrivate), never
// inline in a Run Command, and are never logged.
type Secrets struct {
	// ServiceKeyFile: the campaign's writer key (cas/ac write, execute) the
	// clients log in with (`cucinactl login --key`).
	ServiceKeyFile string `json:"serviceKeyFile,omitempty"`
	// ReadOnlyKeyFile: a principal without ac-write (T10f).
	ReadOnlyKeyFile string `json:"readOnlyKeyFile,omitempty"`
	// AdminKeyFile: the Helm-generated break-glass admin key.
	AdminKeyFile string `json:"adminKeyFile,omitempty"`
	// Valid mTLS identities for T10h's positive controls, stored privately.
	HostCertFile   string `json:"hostCertFile,omitempty"`
	HostKeyFile    string `json:"hostKeyFile,omitempty"`
	WorkerCertFile string `json:"workerCertFile,omitempty"`
	WorkerKeyFile  string `json:"workerKeyFile,omitempty"`
}

// AbseilPin is the build under test (§10.2).
type AbseilPin struct {
	Tag    string `json:"tag"`
	Commit string `json:"commit"`
	Repo   string `json:"repo,omitempty"`
}

// Safety limits (R-TEST-8d, §12).
type Safety struct {
	MaxSpendUSD      float64 `json:"maxSpendUSD"`
	MaxInstances     int     `json:"maxInstances"`
	AllowDestructive bool    `json:"allowDestructive"`
	// AllowOverBudget is set only after the user approved a projected
	// overrun (§12: "stop non-essential resources and ask").
	AllowOverBudget bool `json:"allowOverBudget"`
}

// DescriptorPath returns ~/.config/cucina/e2e/<name>.json.
func DescriptorPath(name string) (string, error) {
	if !DescriptorName(name) {
		return "", fmt.Errorf("invalid descriptor name %q", name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "cucina", "e2e", name+".json"), nil
}

// LoadEnv reads a descriptor by name (or by path if name contains a slash or
// ends in .json). Unknown fields are rejected so typos fail fast.
func LoadEnv(nameOrPath string) (*Env, error) {
	path := nameOrPath
	if !strings.Contains(nameOrPath, string(filepath.Separator)) && !strings.HasSuffix(nameOrPath, ".json") {
		p, err := DescriptorPath(nameOrPath)
		if err != nil {
			return nil, err
		}
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("environment descriptor: %w", err)
	}
	return ParseEnv(b)
}

// ParseEnv decodes and validates a descriptor.
func ParseEnv(b []byte) (*Env, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var e Env
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("environment descriptor: %w", err)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	e.expandPaths()
	return &e, nil
}

// Validate checks the descriptor's internal consistency.
func (e *Env) Validate() error {
	switch e.Kind {
	case EnvKindCluster, EnvAWS, EnvProdSmoke, EnvCanary:
	default:
		return fmt.Errorf("environment %q: unknown kind %q", e.Name, e.Kind)
	}
	if !DescriptorName(e.Name) || !DescriptorName(e.RunID) {
		return fmt.Errorf("environment: name and runId must be nonempty single path components")
	}
	if e.AWS != nil || e.Has(RequiresAWS) {
		if e.AWS == nil || e.AWS.Region == "" || e.AWS.Tags["cucina:run"] == "" || e.AWS.Tags["cucina:env"] == "" {
			return fmt.Errorf("environment %q: capability aws needs aws.region and the cucina:env/cucina:run tags (§12)", e.Name)
		}
		if e.AWS.Profile != "default" || e.AWS.Region != "us-west-1" || e.AWS.Tags["cucina:env"] != "e2e" || e.AWS.Tags["cucina:run"] != e.RunID {
			return fmt.Errorf("environment %q: AWS scope must be profile default, region us-west-1, cucina:env=e2e and matching runId", e.Name)
		}
		if _, err := time.Parse(time.RFC3339, e.AWS.Tags["cucina:expires"]); err != nil {
			return fmt.Errorf("environment %q: cucina:expires must be an RFC3339 timestamp", e.Name)
		}
		if e.Safety.MaxSpendUSD > DefaultBudgetUSD && !e.Safety.AllowOverBudget {
			return fmt.Errorf("environment %q: budget above $300 requires explicit user-approved allowOverBudget", e.Name)
		}
		if e.Safety.MaxSpendUSD <= 0 || e.Safety.MaxInstances <= 0 {
			return fmt.Errorf("environment %q: capability aws needs safety.maxSpendUSD and safety.maxInstances", e.Name)
		}
	}
	if e.Kind == EnvProdSmoke && e.Safety.AllowDestructive {
		return fmt.Errorf("environment %q: prod-smoke can never allow destructive scenarios", e.Name)
	}
	for name, c := range e.Clients {
		if c.OS != "linux" && c.OS != "windows" {
			return fmt.Errorf("environment %q: client %s: os must be linux or windows", e.Name, name)
		}
		if c.InstanceID == "" {
			return fmt.Errorf("environment %q: client %s: missing instanceId", e.Name, name)
		}
	}
	return nil
}

// Has reports whether the environment offers a capability. Client and
// destructive capabilities are also derived from the descriptor's content so
// a stale capability list cannot claim what is not configured.
func (e *Env) Has(r Requirement) bool {
	listed := false
	for _, c := range e.Capabilities {
		if c == r {
			listed = true
		}
	}
	if !listed {
		return false
	}
	switch r {
	case RequiresDestructive:
		return e.Safety.AllowDestructive && e.Kind != EnvProdSmoke
	case RequiresLinuxClient:
		_, ok := e.Clients["linux-client"]
		return ok
	case RequiresWindowsClient:
		_, ok := e.Clients["windows-client"]
		return ok
	case RequiresMacHost:
		return e.DevMac != nil && e.DevMac.HostdConfig != "" && e.DevMac.HostdBinary != ""
	case RequiresHostdPkg:
		return e.DevMac != nil && e.DevMac.PkgPath != "" && e.DevMac.MDMKit != ""
	case RequiresCucinactl:
		return len(e.Cucinactl) > 0
	case RequiresIdP:
		return e.IdP != nil && e.IdP.MockOAuth2URL != ""
	case RequiresKubernetes:
		return e.Kubernetes != nil
	case RequiresPrometheus:
		return e.Endpoints.Prometheus != ""
	}
	return true
}

func (e *Env) expandPaths() {
	home, _ := os.UserHomeDir()
	exp := func(p *string) {
		if strings.HasPrefix(*p, "~/") && home != "" {
			*p = filepath.Join(home, (*p)[2:])
		}
	}
	exp(&e.ArtifactsDir)
	exp(&e.RepoDir)
	exp(&e.CrossInventoryFile)
	exp(&e.Endpoints.CAFile)
	exp(&e.Secrets.ServiceKeyFile)
	exp(&e.Secrets.ReadOnlyKeyFile)
	exp(&e.Secrets.AdminKeyFile)
	exp(&e.Secrets.HostCertFile)
	exp(&e.Secrets.HostKeyFile)
	exp(&e.Secrets.WorkerCertFile)
	exp(&e.Secrets.WorkerKeyFile)
	if e.IdP != nil {
		exp(&e.IdP.CAFile)
	}
	if e.Kubernetes != nil {
		exp(&e.Kubernetes.Kubeconfig)
		exp(&e.Kubernetes.Chart)
		exp(&e.Kubernetes.UpgradeValuesFile)
		exp(&e.Kubernetes.MockOAuth2Manifest)
		for i := range e.Kubernetes.ValuesFiles {
			exp(&e.Kubernetes.ValuesFiles[i])
		}
	}
	if e.DevMac != nil {
		exp(&e.DevMac.WorkDir)
		exp(&e.DevMac.Bazel)
		exp(&e.DevMac.HostdBinary)
		exp(&e.DevMac.HostdConfig)
		exp(&e.DevMac.TartHome)
		exp(&e.DevMac.PkgPath)
		exp(&e.DevMac.UpgradePkgPath)
		exp(&e.DevMac.SignerCert)
		exp(&e.DevMac.MDMKit)
	}
	for k, v := range e.Cucinactl {
		exp(&v)
		e.Cucinactl[k] = v
	}
}
