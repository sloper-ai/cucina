// SPDX-License-Identifier: FSL-1.1-ALv2

// Package config defines the runtime configuration of the Cucina binaries as
// typed structs. The Helm chart renders these as JSON (controller.json), the
// binaries parse them strictly at startup (unknown fields are an error, R-TEST-7
// "fail fast on configuration"), and the chart's values.schema.json mirrors them.
package config

import "time"

// Controller is the configuration of cucina-controller (one binary serving the
// fleet reconcilers, the STS, enrollment, host and management gRPC APIs and
// Prometheus HTTP service discovery). Path: /etc/cucina/controller.json.
type Controller struct {
	ClusterID   string `json:"clusterId"`   // tag value cucina:cluster; unique per installation
	Namespace   string `json:"namespace"`   // namespace holding the CRs and Secrets
	ReleaseName string `json:"releaseName"` // Helm release (resource name prefix)
	// InstanceNames are the configured Buildbarn instance names (tenants); first is the default ("main").
	InstanceNames []string `json:"instanceNames"`
	// PlatformsFile is the merged platform catalog (platforms/pools.json + values.platforms.extra).
	PlatformsFile string `json:"platformsFile"`

	LeaderElection LeaderElection `json:"leaderElection"`
	Listeners      Listeners      `json:"listeners"`
	TLS            TLS            `json:"tls"`
	PKI            PKI            `json:"pki"`
	Scheduler      Scheduler      `json:"scheduler"`
	Endpoints      Endpoints      `json:"endpoints"`
	AWS            *AWS           `json:"aws,omitempty"` // nil when no EC2 pools exist
	Auth           Auth           `json:"auth"`
	Hosts          Hosts          `json:"hosts"`
	Worker         WorkerDefaults `json:"worker"`
	Autoscaler     Autoscaler     `json:"autoscaler"`
	Observability  Observability  `json:"observability"`
	Registry       *Registry      `json:"registry,omitempty"`
}

// LeaderElection configures controller-runtime leader election (2 replicas by default).
type LeaderElection struct {
	Enabled       bool     `json:"enabled"`
	ID            string   `json:"id"`
	LeaseDuration Duration `json:"leaseDuration,omitempty"`
	RenewDeadline Duration `json:"renewDeadline,omitempty"`
	RetryPeriod   Duration `json:"retryPeriod,omitempty"`
}

// Listeners are the controller's listen addresses. The STS and the probes/metrics
// listeners are served by every replica; the others by every replica too (stateless
// except for leader-only reconcilers).
type Listeners struct {
	Probes     string `json:"probes"`     // :8081 /-/healthy /-/ready (plain HTTP, pod network)
	Metrics    string `json:"metrics"`    // :9090 Prometheus /metrics and /sd/workers (HTTP SD)
	STS        string `json:"sts"`        // :8443 HTTPS: /token /jwks.json /.well-known/cucina-configuration
	Management string `json:"management"` // :8444 gRPC ManagementService (TLS, Cucina JWT)
	Enrollment string `json:"enrollment"` // :8445 gRPC EnrollmentService (TLS, no client cert)
	Host       string `json:"host"`       // :8446 gRPC HostService (mTLS)
}

// TLS holds the server certificate location (rotated without restart: fsnotify reload).
type TLS struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	CAFile   string `json:"caFile"` // Cucina CA bundle (verifies client certs and scheduler)
}

// PKI configures Cucina's private CA for workers and hosts (R-SEC-2/3).
type PKI struct {
	CASecret      string   `json:"caSecret"`      // Secret with ca.crt and ca.key (chart-generated, or cert-manager)
	WorkerCertTTL Duration `json:"workerCertTTL"` // default 24h (max 168h)
	HostCertTTL   Duration `json:"hostCertTTL"`   // default 168h, renewed before expiry
	VMCertTTL     Duration `json:"vmCertTTL"`     // default 12h
}

// Scheduler locates the Buildbarn scheduler.
type Scheduler struct {
	BuildQueueStateAddress string   `json:"buildQueueStateAddress"` // host:port, mTLS with the controller's client cert
	PollInterval           Duration `json:"pollInterval"`           // 1s
	ClientCertFile         string   `json:"clientCertFile"`
	ClientKeyFile          string   `json:"clientKeyFile"`
	// QueueFailAfter fails queued work of a pool that cannot obtain capacity (R-RE-2/R-SCALE-4).
	QueueFailAfter Duration `json:"queueFailAfter"`
}

// Endpoints are what workers and clients are told to connect to.
type Endpoints struct {
	ClientEndpoint  string `json:"clientEndpoint"`  // grpcs://cucina.example.com:443 (public)
	WorkerScheduler string `json:"workerScheduler"` // host:port workers use for scheduler Synchronize (private)
	WorkerStorage   string `json:"workerStorage"`   // host:port workers use for CAS/AC (private)
	WorkerEnroll    string `json:"workerEnroll"`    // host:port of EnrollmentService (private)
	HostEndpoint    string `json:"hostEndpoint"`    // host:port of HostService (reachable from Mac sites)
	ServerName      string `json:"serverName"`      // TLS server name
	STSURL          string `json:"stsUrl"`          // https://…/ (issuer of Cucina JWTs)
	ManagementURL   string `json:"managementUrl"`   // host:port for cucinactl
}

// AWS configures the EC2 provider. Credentials come from the default chain (IRSA,
// Pod Identity, or the node instance profile for the temporary k3s cluster).
type AWS struct {
	Region    string `json:"region"`
	AccountID string `json:"accountId"` // instance identity documents must carry this account
	// ExtraTags are applied to every resource the controller creates (campaign tags).
	ExtraTags map[string]string `json:"extraTags,omitempty"`
	// PricingRegionCode for the Price List API (default: region).
	PricingRegionCode string `json:"pricingRegionCode,omitempty"`
	// SweepInterval for orphaned volumes/ENIs (default 5m).
	SweepInterval Duration `json:"sweepInterval,omitempty"`
}

// Auth configures token issuance and revocation (R-AUTH).
type Auth struct {
	// SigningKeySecret holds the ES256 keys (kid-named entries) used to mint Cucina JWTs.
	SigningKeySecret string `json:"signingKeySecret"`
	// JWKSConfigMap is the ConfigMap (mounted as a directory in Buildbarn pods) the controller writes.
	JWKSConfigMap string `json:"jwksConfigMap"`
	// DenyListConfigMap holds the sid/sub deny-list file read by every JMESPath authorizer.
	DenyListConfigMap string   `json:"denyListConfigMap"`
	TokenTTL          Duration `json:"tokenTTL"` // 15m
	Audience          string   `json:"audience"` // "buildbarn"
	// KeyRotationPublishLead is how long a new key is published before signing starts (>= 10m).
	KeyRotationPublishLead Duration `json:"keyRotationPublishLead"`
	// BreakGlassKeySecret is the Helm-generated bootstrap admin service-account key (R-AUTH-12).
	BreakGlassKeySecret string `json:"breakGlassKeySecret"`
	// ServiceKeysSecret stores hashed service-account keys.
	ServiceKeysSecret  string       `json:"serviceKeysSecret"`
	RateLimitPerMinute int          `json:"rateLimitPerMinute"`
	GroupLookup        *GroupLookup `json:"groupLookup,omitempty"`
}

// GroupLookup configures optional Cloud Identity group resolution.
type GroupLookup struct {
	ServiceAccountSecret string   `json:"serviceAccountSecret"`
	CacheTTL             Duration `json:"cacheTtl"`
}

// Hosts configures macOS host enrollment and L2/WAN defaults.
type Hosts struct {
	DefaultTokenTTL Duration `json:"defaultTokenTtl"`
	// StaleAfter marks a host Offline when no heartbeat arrived for this long.
	StaleAfter Duration `json:"staleAfter"`
	// RegistrySecret holds the read-only GHCR package credential handed to hostd at pull time.
	RegistrySecret string `json:"registrySecret"`
}

// WorkerDefaults feed WorkerSettings.
type WorkerDefaults struct {
	MaximumMessageSizeBytes uint64 `json:"maximumMessageSizeBytes"`
	MetricsPort             uint32 `json:"metricsPort"`
	PushgatewayURL          string `json:"pushgatewayUrl,omitempty"`
	WANCompressionForHosts  bool   `json:"wanCompressionForHosts"`
}

// Autoscaler tunes the decision loop (R-SCALE).
type Autoscaler struct {
	// Shadow runs decisions without acting and exports the diff (R-TEST-7 shadow mode).
	Shadow bool `json:"shadow"`
	// EC2 API token buckets (R-SCALE-6): RunInstances burst 5, refill 2/s by default.
	RunInstancesBurst  int     `json:"runInstancesBurst"`
	RunInstancesRefill float64 `json:"runInstancesRefillPerSecond"`
	// DeadmanIdleLimit etc. are the defaults pushed to workers (R-POOL-7).
	DeadmanIdleLimit        Duration `json:"deadmanIdleLimit"`
	DeadmanUnreachableLimit Duration `json:"deadmanUnreachableLimit"`
	DeadmanMaxUptime        Duration `json:"deadmanMaxUptime"`
	// ICEBackoffMin/Max bound the jittered capacity backoff.
	ICEBackoffMin Duration `json:"iceBackoffMin"`
	ICEBackoffMax Duration `json:"iceBackoffMax"`
}

// Observability configures logs, metrics and tracing.
type Observability struct {
	LogLevel     string `json:"logLevel"`     // debug|info|warn|error
	OTLPEndpoint string `json:"otlpEndpoint"` // optional
	// RetentionMetric names the storage retention metric used by alerts.
	CostEnabled bool `json:"costEnabled"`
}

// Registry configures Tart image registry access (private GHCR package).
type Registry struct {
	Host string `json:"host"` // ghcr.io
	// Credential mode: "static-readonly" (a dedicated read:packages token in HostsRegistrySecret)
	// or "github-app" (installation tokens). The choice is recorded in an ADR.
	Mode string `json:"mode"`
}

// Duration is a time.Duration that (un)marshals as a Go duration string ("5m").
type Duration struct{ time.Duration }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.Duration.String() + `"`), nil
}

// UnmarshalJSON implements json.Unmarshaler; it accepts "5m" strings only.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return &durationError{string(b)}
	}
	v, err := time.ParseDuration(string(b[1 : len(b)-1]))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type durationError struct{ s string }

func (e *durationError) Error() string { return "duration must be a string like \"5m\": " + e.s }
