// SPDX-License-Identifier: FSL-1.1-ALv2

package pools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
)

// Resolution errors; the reconciler maps them to conditions.
var (
	ErrUnknownPlatform  = errors.New("platform not in the catalog")
	ErrProviderMismatch = errors.New("provider does not match the platform")
	ErrUnknownSizeClass = errors.New("size class not declared by the platform")
	ErrInstanceName     = errors.New("instance name not configured")
	ErrSizing           = errors.New("cannot size the pool's VMs")
	ErrInvalidSpec      = errors.New("invalid pool spec")
)

// Env carries the resolution inputs that come neither from the catalog nor
// from the WorkerPool: controller configuration and facts the reconciler looked up.
type Env struct {
	// InstanceNames are the configured Buildbarn instance names; the first is the default.
	InstanceNames []string
	// VCPUsPerVM is the vCPU count of one VM of the pool (EC2: the first, i.e.
	// preferred, instance type; Tart: spec.tart.cpu or the host-derived default).
	VCPUsPerVM int
	// Generation is the image generation (see Generation).
	Generation string
}

// Resolved is a WorkerPool resolved against the catalog: the provider-neutral
// PoolSpec for the autoscaler plus what WorkerSettings need.
type Resolved struct {
	Spec     domain.PoolSpec
	Platform *Platform
	// Runners are the catalog runners in Spec.Runners order (emulator, generic flag).
	Runners []Runner
	// DeriveConcurrency[i] is true when runner i uses vcpusFactor 1.0 without an
	// override: workers then derive the slots from their actual vCPUs, which
	// matters when a fallback instance type has a different size.
	DeriveConcurrency []bool
	BuildDirectory    string // fuse | winfsp | nfsv4 | native
	L1Placement       string // auto | instance-store | ebs | memory | vm-disk
	L1SizeBytes       uint64 // 0 = derive from the machine
	Spot              bool   // EC2 spot capacity (SIGTERM on interruption notice)
}

// DefaultMacL1GiB is the default persistent L1 size of macOS VMs (R-CACHE-2).
const DefaultMacL1GiB = 40

// Resolve merges a WorkerPool with its catalog platform and defaults. It is
// pure: everything that needs I/O (image, vCPUs) arrives through env.
func Resolve(cat *Catalog, wp *v1alpha1.WorkerPool, env Env) (*Resolved, error) {
	s := &wp.Spec
	p, ok := cat.Platform(s.Platform)
	if !ok {
		return nil, fmt.Errorf("%w: %q (add it to values platforms.extra and run helm upgrade)", ErrUnknownPlatform, s.Platform)
	}
	if string(p.Provider) != s.Provider {
		return nil, fmt.Errorf("%w: pool says %q, platform %q is %q", ErrProviderMismatch, s.Provider, p.Name, p.Provider)
	}
	scName := s.SizeClass
	if scName == "" {
		scName = "default"
	}
	sc, ok := p.SizeClass(scName)
	if !ok {
		return nil, fmt.Errorf("%w: %q (platform %q)", ErrUnknownSizeClass, scName, p.Name)
	}
	instances, err := instanceNames(s.InstanceNames, env.InstanceNames)
	if err != nil {
		return nil, err
	}
	if s.Capacity.Max < 0 || s.Capacity.MinRunning < 0 || s.Capacity.MinRunning > s.Capacity.Max {
		return nil, fmt.Errorf("%w: capacity minRunning=%d max=%d", ErrInvalidSpec, s.Capacity.MinRunning, s.Capacity.Max)
	}

	r := &Resolved{Platform: p, L1Placement: p.Defaults.L1Placement}
	for _, cr := range p.Runners {
		slots, derive, err := runnerSlots(cr, s.Worker, env.VCPUsPerVM)
		if err != nil {
			return nil, err
		}
		r.Spec.Runners = append(r.Spec.Runners, domain.Runner{Name: cr.Name, Properties: cr.Properties, Concurrency: slots})
		r.Runners = append(r.Runners, cr)
		r.DeriveConcurrency = append(r.DeriveConcurrency, derive)
	}

	idle, err := timer(s.Timers.IdleTimeout, p.Defaults.IdleTimeout.Duration, "idleTimeout")
	if err != nil {
		return nil, err
	}
	drain, err := timer(s.Timers.DrainTimeout, p.Defaults.DrainTimeout.Duration, "drainTimeout")
	if err != nil {
		return nil, err
	}
	startup, err := timer(s.Timers.StartupTimeout, p.Defaults.StartupTimeout.Duration, "startupTimeout")
	if err != nil {
		return nil, err
	}

	rollout := domain.RolloutLazy
	if s.Rollout == string(domain.RolloutEager) {
		rollout = domain.RolloutEager
	}
	r.Spec.Name = domain.PoolName(wp.Name)
	r.Spec.Provider = p.Provider
	r.Spec.Platform = p.Name
	r.Spec.SizeClass = sc
	r.Spec.InstanceNames = instances
	r.Spec.MinRunning = int(s.Capacity.MinRunning)
	r.Spec.Max = int(s.Capacity.Max)
	r.Spec.IdleTimeout, r.Spec.DrainTimeout, r.Spec.StartupTimeout = idle, drain, startup
	r.Spec.Generation = env.Generation
	r.Spec.Rollout = rollout
	r.Spec.Paused = s.Paused

	r.BuildDirectory = buildDirectory(s.Worker.BuildDirectory, p)
	if pl := s.Worker.L1.Placement; pl != "" && pl != "auto" {
		if pl == "vm-disk" && p.Provider != domain.ProviderTart {
			return nil, fmt.Errorf("%w: worker.l1.placement vm-disk is only valid for tart pools", ErrInvalidSpec)
		}
		r.L1Placement = pl
	}
	switch {
	case s.Worker.L1.SizeGiB != nil && *s.Worker.L1.SizeGiB > 0:
		r.L1SizeBytes = uint64(*s.Worker.L1.SizeGiB) << 30
	case p.Provider == domain.ProviderTart:
		r.L1SizeBytes = DefaultMacL1GiB << 30
	}
	r.Spot = s.EC2 != nil && s.EC2.CapacityType == "spot"
	return r, nil
}

// Queues returns every Buildbarn size class queue the pool serves
// (instance name x runner x size class).
func (r *Resolved) Queues() []domain.QueueKey {
	var qs []domain.QueueKey
	for _, in := range r.Spec.InstanceNames {
		for _, ru := range r.Spec.Runners {
			qs = append(qs, domain.QueueKey{InstanceNamePrefix: in, PlatformKey: domain.PropertiesKey(ru.Properties), SizeClass: r.Spec.SizeClass})
		}
	}
	return qs
}

func instanceNames(want, configured []string) ([]string, error) {
	if len(configured) == 0 {
		return nil, fmt.Errorf("%w: the controller configuration lists no instance names", ErrInstanceName)
	}
	if len(want) == 0 {
		return slices.Clone(configured), nil
	}
	for _, n := range want {
		if !slices.Contains(configured, n) {
			return nil, fmt.Errorf("%w: %q (configured: %v)", ErrInstanceName, n, configured)
		}
	}
	return slices.Clone(want), nil
}

// runnerSlots applies the concurrency rules: runnerConcurrency[name] overrides
// any runner; worker.concurrency overrides every vCPU-based runner (they share
// the VM's CPUs); otherwise the catalog rule applies.
func runnerSlots(r Runner, w v1alpha1.WorkerSpec, vcpus int) (slots int, derive bool, err error) {
	if v, ok := w.RunnerConcurrency[r.Name]; ok {
		if v < 1 {
			return 0, false, fmt.Errorf("%w: worker.runnerConcurrency[%s] must be >= 1", ErrInvalidSpec, r.Name)
		}
		return int(v), false, nil
	}
	if r.Concurrency.Fixed > 0 {
		return r.Concurrency.Fixed, false, nil
	}
	if w.Concurrency != nil {
		if *w.Concurrency < 1 {
			return 0, false, fmt.Errorf("%w: worker.concurrency must be >= 1", ErrInvalidSpec)
		}
		return int(*w.Concurrency), false, nil
	}
	if vcpus <= 0 {
		return 0, false, fmt.Errorf("%w: runner %q derives its slots from vCPUs, but the VM size is unknown (set worker.concurrency or a known instance type)", ErrSizing, r.Name)
	}
	return r.Concurrency.Slots(vcpus), r.Concurrency.VCPUsFactor == 1, nil
}

func timer(override *metav1.Duration, def time.Duration, name string) (time.Duration, error) {
	if override == nil {
		return def, nil
	}
	d := override.Duration
	if d <= 0 {
		return 0, fmt.Errorf("%w: timers.%s must be positive", ErrInvalidSpec, name)
	}
	return d, nil
}

func buildDirectory(mode string, p *Platform) string {
	if mode == "" || mode == "auto" {
		mode = p.Defaults.BuildDirectory
	}
	if mode != "auto" {
		return mode
	}
	switch p.OS {
	case "linux":
		return "fuse"
	case "windows":
		return "winfsp"
	default:
		// macOS: native until the NFSv4-in-Tart measurement (R-CACHE-3) says otherwise.
		return "native"
	}
}

var labelSafe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

// Generation derives a pool generation (R-POOL-8, R-OPS-2) from the image
// version label. Without a usable label it falls back to a short digest of the
// image identity (AMI ID or OCI reference), so a different image still starts
// a new generation. The result is safe as an EC2 tag, metric label and
// Kubernetes label value.
func Generation(version, imageIdentity string) string {
	if labelSafe.MatchString(version) {
		return version
	}
	if version == "" && imageIdentity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(version + "\x00" + imageIdentity))
	return "g" + hex.EncodeToString(sum[:])[:12]
}
