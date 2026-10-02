// SPDX-License-Identifier: FSL-1.1-ALv2

package controller

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/sloper-ai/cucina/internal/config"
)

// Mode selects which parts of the configuration a subcommand needs.
type Mode string

const (
	// ModeController runs the manager, reconcilers and every server.
	ModeController Mode = "controller"
	// ModeSTS runs only the STS HTTP server (its own leaderless Deployment).
	ModeSTS Mode = "sts"
	// ModeTool covers one-shot subcommands (bootstrap, uninstall-prep): only the
	// identity of the installation is required.
	ModeTool Mode = "tool"
)

// Defaults applied to zero values before validation. The chart renders every
// field; the defaults keep hand-written configurations (development, tests) short.
const (
	DefaultPollInterval            = time.Second
	DefaultQueueFailAfter          = 10 * time.Minute // < platformQueueWithNoWorkersTimeout (900 s), so clients see Cucina's message first
	DefaultWorkerCertTTL           = 24 * time.Hour
	DefaultHostCertTTL             = 7 * 24 * time.Hour
	DefaultVMCertTTL               = 12 * time.Hour
	MaxCertTTL                     = 7 * 24 * time.Hour // R-SEC-2: <= 7 days
	DefaultTokenTTL                = 15 * time.Minute
	DefaultKeyRotationPublishLead  = 10 * time.Minute
	DefaultHostStaleAfter          = 2 * time.Minute
	DefaultHostTokenTTL            = 30 * 24 * time.Hour
	DefaultSweepInterval           = 5 * time.Minute
	DefaultRunInstancesBurst       = 5 // R-SCALE-6
	DefaultRunInstancesRefill      = 2.0
	DefaultDeadmanIdleLimit        = 30 * time.Minute // R-POOL-7
	DefaultDeadmanUnreachableLimit = 10 * time.Minute
	DefaultDeadmanMaxUptime        = 12 * time.Hour
	DefaultICEBackoffMin           = 15 * time.Second
	DefaultICEBackoffMax           = 5 * time.Minute
	DefaultMaxMessageSizeBytes     = 16 << 20
	DefaultWorkerMetricsPort       = 9987
	DefaultLeaseDuration           = 15 * time.Second
	DefaultRenewDeadline           = 10 * time.Second
	DefaultRetryPeriod             = 2 * time.Second
)

// LoadConfig reads, strictly parses, defaults and validates the controller
// configuration (R-TEST-7 "parse into types at startup and exit non-zero with a
// precise message").
func LoadConfig(path string, mode Mode) (*config.Controller, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("controller configuration: %w", err)
	}
	c, err := ParseConfig(b, mode)
	if err != nil {
		return nil, fmt.Errorf("controller configuration %s: %w", path, err)
	}
	return c, nil
}

// ParseConfig parses b strictly: unknown members, wrong types, duplicate names
// and trailing data are errors whose message carries the JSON pointer of the
// offending member (encoding/json/v2).
func ParseConfig(b []byte, mode Mode) (*config.Controller, error) {
	var c config.Controller
	if err := json.Unmarshal(b, &c, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	ApplyDefaults(&c)
	if err := Validate(&c, mode); err != nil {
		return nil, err
	}
	return &c, nil
}

func setDur(d *config.Duration, v time.Duration) {
	if d.Duration == 0 {
		d.Duration = v
	}
}

// ApplyDefaults fills zero values with the documented defaults.
func ApplyDefaults(c *config.Controller) {
	if c.Observability.LogLevel == "" {
		c.Observability.LogLevel = "info"
	}
	le := &c.LeaderElection
	setDur(&le.LeaseDuration, DefaultLeaseDuration)
	setDur(&le.RenewDeadline, DefaultRenewDeadline)
	setDur(&le.RetryPeriod, DefaultRetryPeriod)
	if le.ID == "" && c.ReleaseName != "" {
		le.ID = c.ReleaseName + "-controller"
	}
	setDur(&c.Scheduler.PollInterval, DefaultPollInterval)
	setDur(&c.Scheduler.QueueFailAfter, DefaultQueueFailAfter)
	setDur(&c.PKI.WorkerCertTTL, DefaultWorkerCertTTL)
	setDur(&c.PKI.HostCertTTL, DefaultHostCertTTL)
	setDur(&c.PKI.VMCertTTL, DefaultVMCertTTL)
	setDur(&c.Auth.TokenTTL, DefaultTokenTTL)
	setDur(&c.Auth.KeyRotationPublishLead, DefaultKeyRotationPublishLead)
	if c.Auth.Audience == "" {
		c.Auth.Audience = "buildbarn"
	}
	setDur(&c.Hosts.StaleAfter, DefaultHostStaleAfter)
	setDur(&c.Hosts.DefaultTokenTTL, DefaultHostTokenTTL)
	if c.AWS != nil {
		setDur(&c.AWS.SweepInterval, DefaultSweepInterval)
		if c.AWS.PricingRegionCode == "" {
			c.AWS.PricingRegionCode = c.AWS.Region
		}
	}
	a := &c.Autoscaler
	if a.RunInstancesBurst == 0 {
		a.RunInstancesBurst = DefaultRunInstancesBurst
	}
	if a.RunInstancesRefill == 0 {
		a.RunInstancesRefill = DefaultRunInstancesRefill
	}
	setDur(&a.DeadmanIdleLimit, DefaultDeadmanIdleLimit)
	setDur(&a.DeadmanUnreachableLimit, DefaultDeadmanUnreachableLimit)
	setDur(&a.DeadmanMaxUptime, DefaultDeadmanMaxUptime)
	setDur(&a.ICEBackoffMin, DefaultICEBackoffMin)
	setDur(&a.ICEBackoffMax, DefaultICEBackoffMax)
	if c.Worker.MaximumMessageSizeBytes == 0 {
		c.Worker.MaximumMessageSizeBytes = DefaultMaxMessageSizeBytes
	}
	if c.Worker.MetricsPort == 0 {
		c.Worker.MetricsPort = DefaultWorkerMetricsPort
	}
}

var (
	clusterIDRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	accountRE   = regexp.MustCompile(`^[0-9]{12}$`)
)

// Validate reports every problem of c for mode at once; each message starts
// with the JSON pointer of the field.
func Validate(c *config.Controller, mode Mode) error {
	v := &validator{}
	v.check(clusterIDRE.MatchString(c.ClusterID), "/clusterId", "must be a lowercase DNS label of at most 63 characters (it is the cucina:cluster tag value)")
	v.check(c.Namespace != "", "/namespace", "must be set")
	v.check(c.ReleaseName != "", "/releaseName", "must be set")
	v.check(len(c.InstanceNames) > 0, "/instanceNames", "must list at least one instance name (the first is the default)")
	for i, n := range c.InstanceNames {
		v.check(n != "" && !slices.Contains(c.InstanceNames[:i], n), fmt.Sprintf("/instanceNames/%d", i), "must be non-empty and unique")
	}
	v.check(slices.Contains([]string{"debug", "info", "warn", "error"}, c.Observability.LogLevel), "/observability/logLevel", "must be one of debug, info, warn, error")
	v.addr(c.Listeners.Probes, "/listeners/probes", true)
	v.addr(c.Listeners.Metrics, "/listeners/metrics", mode != ModeTool)

	switch mode {
	case ModeController:
		v.check(c.PlatformsFile != "", "/platformsFile", "must name the merged platform catalog")
		if c.LeaderElection.Enabled {
			v.check(c.LeaderElection.ID != "", "/leaderElection/id", "must be set when leader election is enabled")
			le := c.LeaderElection
			v.check(le.LeaseDuration.Duration > le.RenewDeadline.Duration && le.RenewDeadline.Duration > le.RetryPeriod.Duration && le.RetryPeriod.Duration > 0,
				"/leaderElection", "needs leaseDuration > renewDeadline > retryPeriod > 0")
		}
		v.addr(c.Listeners.Management, "/listeners/management", false)
		v.addr(c.Listeners.Enrollment, "/listeners/enrollment", false)
		v.addr(c.Listeners.Host, "/listeners/host", false)
		v.addr(c.Listeners.STS, "/listeners/sts", false)
		if c.Listeners.Management != "" || c.Listeners.Enrollment != "" || c.Listeners.Host != "" || c.Listeners.STS != "" {
			v.tls(c)
		}
		v.addr(c.Scheduler.BuildQueueStateAddress, "/scheduler/buildQueueStateAddress", true)
		v.check(c.Scheduler.ClientCertFile != "" && c.Scheduler.ClientKeyFile != "", "/scheduler/clientCertFile",
			"and clientKeyFile must be set: BuildQueueState is reachable only over mTLS with the controller's identity (R-SEC-4)")
		v.check(c.Scheduler.PollInterval.Duration >= time.Second && c.Scheduler.PollInterval.Duration <= 2*time.Second,
			"/scheduler/pollInterval", "must be between 1s and 2s (R-SCALE-1)")
		v.check(c.Scheduler.QueueFailAfter.Duration > 0 && c.Scheduler.QueueFailAfter.Duration < 15*time.Minute,
			"/scheduler/queueFailAfter", "must be positive and below the scheduler's platformQueueWithNoWorkersTimeout (900s)")
		v.check(c.Endpoints.WorkerScheduler != "", "/endpoints/workerScheduler", "must be set (workers' scheduler address)")
		v.check(c.Endpoints.WorkerStorage != "", "/endpoints/workerStorage", "must be set (workers' CAS/AC address)")
		v.check(c.Endpoints.WorkerEnroll != "", "/endpoints/workerEnroll", "must be set (EnrollmentService address handed to workers)")
		v.check(c.Endpoints.ServerName != "", "/endpoints/serverName", "must be set")
		v.check(c.PKI.CASecret != "", "/pki/caSecret", "must be set")
		for name, d := range map[string]time.Duration{"workerCertTTL": c.PKI.WorkerCertTTL.Duration, "hostCertTTL": c.PKI.HostCertTTL.Duration, "vmCertTTL": c.PKI.VMCertTTL.Duration} {
			v.check(d > 0 && d <= MaxCertTTL, "/pki/"+name, "must be positive and at most 168h (R-SEC-2)")
		}
		v.check(c.Hosts.StaleAfter.Duration > 0, "/hosts/staleAfter", "must be positive")
		if c.AWS != nil {
			v.check(c.AWS.Region != "", "/aws/region", "must be set")
			v.check(accountRE.MatchString(c.AWS.AccountID), "/aws/accountId", "must be the 12-digit AWS account ID")
			v.check(c.AWS.SweepInterval.Duration >= time.Minute, "/aws/sweepInterval", "must be at least 1m")
		}
		a := c.Autoscaler
		v.check(a.RunInstancesBurst > 0 && a.RunInstancesRefill > 0, "/autoscaler/runInstancesBurst", "and runInstancesRefillPerSecond must be positive (R-SCALE-6)")
		v.check(a.ICEBackoffMin.Duration > 0 && a.ICEBackoffMin.Duration <= a.ICEBackoffMax.Duration, "/autoscaler/iceBackoffMin", "must be positive and <= iceBackoffMax")
		v.check(a.DeadmanIdleLimit.Duration > 0 && a.DeadmanUnreachableLimit.Duration > 0 && a.DeadmanMaxUptime.Duration > 0,
			"/autoscaler/deadmanIdleLimit", "and the other dead-man limits must be positive (R-POOL-7)")
		v.authCommon(c)
	case ModeSTS:
		v.addr(c.Listeners.STS, "/listeners/sts", true)
		v.tls(c)
		v.authCommon(c)
	}
	return v.err()
}

type validator struct{ errs []error }

func (v *validator) check(ok bool, path, msg string) {
	if !ok {
		v.errs = append(v.errs, fmt.Errorf("%s %s", path, msg))
	}
}

func (v *validator) addr(a, path string, required bool) {
	if a == "" {
		v.check(!required, path, "must be set (host:port)")
		return
	}
	_, port, err := net.SplitHostPort(a)
	v.check(err == nil && port != "", path, fmt.Sprintf("%q must be host:port or :port", a))
}

func (v *validator) tls(c *config.Controller) {
	v.check(c.TLS.CertFile != "" && c.TLS.KeyFile != "", "/tls/certFile", "and keyFile must be set for the TLS listeners")
	v.check(c.TLS.CAFile != "", "/tls/caFile", "must be set (Cucina CA bundle)")
}

func (v *validator) authCommon(c *config.Controller) {
	v.check(c.Auth.SigningKeySecret != "", "/auth/signingKeySecret", "must be set")
	v.check(c.Auth.JWKSConfigMap != "", "/auth/jwksConfigMap", "must be set")
	v.check(c.Auth.DenyListConfigMap != "", "/auth/denyListConfigMap", "must be set")
	v.check(c.Auth.TokenTTL.Duration > 0 && c.Auth.TokenTTL.Duration <= 15*time.Minute, "/auth/tokenTTL", "must be positive and at most 15m (R-AUTH)")
	v.check(c.Auth.KeyRotationPublishLead.Duration >= 10*time.Minute, "/auth/keyRotationPublishLead", "must be at least 10m")
	if c.Endpoints.STSURL != "" {
		u, err := url.Parse(c.Endpoints.STSURL)
		v.check(err == nil && u.Scheme == "https" && u.Host != "", "/endpoints/stsUrl", "must be an https:// URL")
	} else {
		v.check(false, "/endpoints/stsUrl", "must be set (issuer of Cucina JWTs)")
	}
}

func (v *validator) err() error { return errors.Join(v.errs...) }
