// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/config"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// Image re-resolution periods: a tag selector is re-resolved often so that a
// newly published AMI starts a new generation within minutes (R-OPS-2, T12);
// an explicit AMI ID is only re-checked for existence.
const (
	selectorImageTTL = 2 * time.Minute
	explicitImageTTL = 15 * time.Minute
	// defaultTartVCPUs sizes a macOS pool before any host reported facts.
	defaultTartVCPUs  = 4
	defaultTartMaxAge = 7 * 24 * time.Hour
)

// ErrProviderUnavailable means the pool's provider is not configured in this controller.
var ErrProviderUnavailable = errors.New("provider not available")

// RuntimeBuilder turns WorkerPool objects into PoolRuntimes: catalog
// resolution plus the lookups that need I/O (image, vCPUs), with caches so
// that reconciles do not hammer the EC2 API (R-SCALE-6).
type RuntimeBuilder struct {
	Config  *config.Controller
	Catalog *pools.Catalog
	Compute ports.Compute
	Clock   ports.Clock
	// Scaling is the planner configuration (dead-man consistency checks).
	Scaling scaling.Config

	mu     sync.Mutex
	images map[string]imageEntry // by pool name
	vcpus  map[string]int        // by instance type
}

type imageEntry struct {
	key string
	img ports.Image
	err error
	at  time.Time
	ok  bool // img holds a successful resolution for key
}

// Build resolves wp. hosts are the MacHost objects (Tart sizing). A non-nil
// error means the spec cannot be resolved (status: Ready=False, InvalidSpec);
// image errors are not returned but carried in PoolRuntime.ImageErr so that
// the planner can fail queued work fast (R-RE-2).
//
// prevGeneration is the generation the pool ran with so far (last runtime or
// status); it is kept while the image cannot be resolved, so a missing image
// fails new work fast without turning every running VM into an old generation.
func (b *RuntimeBuilder) Build(ctx context.Context, wp *v1alpha1.WorkerPool, hosts []v1alpha1.MacHost, prevGeneration string) (*PoolRuntime, error) {
	s := &wp.Spec
	p, ok := b.Catalog.Platform(s.Platform)
	if !ok {
		return nil, fmt.Errorf("%w: %q", pools.ErrUnknownPlatform, s.Platform)
	}
	rt := &PoolRuntime{Ledger: ReadLedger(wp), InstanceSecondsToday: float64(wp.Status.InstanceSecondsToday)}
	env := pools.Env{InstanceNames: b.Config.InstanceNames}
	var version, identity string

	switch domain.Provider(s.Provider) {
	case domain.ProviderEC2:
		if s.EC2 == nil {
			return nil, fmt.Errorf("%w: spec.ec2 is required for provider ec2", pools.ErrInvalidSpec)
		}
		if b.Compute == nil || b.Config.AWS == nil {
			return nil, fmt.Errorf("%w: EC2 pools need the controller's aws configuration", ErrProviderUnavailable)
		}
		img, err := b.image(ctx, wp)
		rt.ImageErr = err
		rt.ImageRef = img.ID
		rt.ImageSizeGiB = img.SizeGiB
		version, identity = s.Image.Version, img.ID
		if version == "" {
			version = img.Version
		}
		env.VCPUsPerVM = b.instanceVCPUs(ctx, s.EC2.InstanceTypes, p.OS == "windows")
		rt.EC2 = ec2Launch(b.Config, s, img.ID, p.OS == "windows")
	case domain.ProviderTart:
		if s.Tart == nil {
			return nil, fmt.Errorf("%w: spec.tart is required for provider tart", pools.ErrInvalidSpec)
		}
		ref := s.Image.Reference
		if ref == "" {
			rt.ImageErr = fmt.Errorf("%w: spec.image.reference is empty", ports.ErrImageNotFound)
		}
		rt.ImageRef = ref
		version, identity = s.Image.Version, ref
		if version == "" {
			version = refTag(ref)
		}
		env.VCPUsPerVM = tartVCPUs(s.Tart, hosts)
		rt.Tart = tartLaunch(s.Tart, ref)
		if hosts != nil {
			rt.Tart.Hosts = hostPolicies(hosts)
		}
	default:
		return nil, fmt.Errorf("%w: provider %q", pools.ErrInvalidSpec, s.Provider)
	}
	rt.ImageVersion = version
	env.Generation = pools.Generation(version, identity)
	if rt.ImageErr != nil {
		env.Generation = prevGeneration
		if env.Generation == "" {
			env.Generation = "unresolved"
		}
	}

	res, err := pools.Resolve(b.Catalog, wp, env)
	if err != nil {
		return nil, err
	}
	spec := scaling.Spec{
		PoolSpec:  res.Spec,
		ClusterID: b.Config.ClusterID,
		VCPUs:     env.VCPUsPerVM,
		Deleting:  !wp.DeletionTimestamp.IsZero(),
	}
	if s.Worker.Concurrency != nil {
		spec.VCPUs = int(*s.Worker.Concurrency)
	}
	if s.EC2 != nil {
		spec.InstanceTypes = slices.Clone(s.EC2.InstanceTypes)
		spec.SubnetIDs = slices.Clone(s.EC2.SubnetIDs)
	}
	for _, w := range s.FloorSchedule {
		fw, err := scaling.ParseFloorWindow(w.Name, w.Days, w.Start, w.End, int(w.MinRunning))
		if err != nil {
			return nil, fmt.Errorf("%w: floorSchedule: %w", pools.ErrInvalidSpec, err)
		}
		spec.Floors = append(spec.Floors, fw)
	}
	if f := ReadFloorOverride(wp); f != nil && b.Clock.Now().Before(f.ExpiresAt) {
		// A temporary floor from the management API: a window active all day,
		// every day, until it expires (dropped by the next resync after that).
		spec.Floors = append(spec.Floors, scaling.FloorWindow{
			Name: "override", MinRunning: min(int(f.MinRunning), spec.Max),
			Days: []time.Weekday{time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday},
		})
	}
	if s.DailyInstanceHourCap != nil {
		spec.DailyInstanceHourCap = float64(*s.DailyInstanceHourCap)
	}
	if err := scaling.ValidateSpec(spec, b.Scaling); err != nil {
		return nil, fmt.Errorf("%w: %w", pools.ErrInvalidSpec, err)
	}
	rt.Spec = spec
	rt.Resolved = res
	rt.Queues = res.Queues()
	return rt, nil
}

// RetireRuntime is the runtime of a deleted pool that no longer resolves: it
// lets the planner drain and stop whatever the pool still runs (R-OPS-3).
func (b *RuntimeBuilder) RetireRuntime(wp *v1alpha1.WorkerPool, last *PoolRuntime) *PoolRuntime {
	if last != nil {
		rt := *last
		rt.Spec.Deleting = true
		return &rt
	}
	rt := &PoolRuntime{Ledger: ReadLedger(wp)}
	rt.Spec.Name = domain.PoolName(wp.Name)
	rt.Spec.Provider = domain.Provider(wp.Spec.Provider)
	rt.Spec.ClusterID = b.Config.ClusterID
	rt.Spec.Deleting = true
	rt.Spec.DrainTimeout = 30 * time.Minute
	rt.Spec.StartupTimeout = 15 * time.Minute
	if p, ok := b.Catalog.Platform(wp.Spec.Platform); ok {
		rt.Spec.DrainTimeout = p.Defaults.DrainTimeout.Duration
		for _, r := range p.Runners {
			rt.Spec.Runners = append(rt.Spec.Runners, domain.Runner{Name: r.Name, Properties: r.Properties, Concurrency: 1})
		}
		rt.Spec.InstanceNames = b.Config.InstanceNames
		if sc, ok := p.SizeClass(wp.Spec.SizeClass); ok {
			rt.Spec.SizeClass = sc
		}
		rt.Queues = scaling.PoolQueues(rt.Spec.PoolSpec)
	}
	if wp.Spec.Tart != nil {
		rt.Tart = tartLaunch(wp.Spec.Tart, wp.Spec.Image.Reference)
	}
	return rt
}

func ec2Launch(cfg *config.Controller, s *v1alpha1.WorkerPoolSpec, imageID string, windows bool) *EC2Launch {
	e := s.EC2
	l := &EC2Launch{
		ImageID:           imageID,
		CapacityType:      ports.OnDemand,
		FallbackOnDemand:  e.SpotFallback,
		SecurityGroupIDs:  slices.Clone(e.SecurityGroupIDs),
		InstanceProfile:   e.InstanceProfile,
		AssociatePublicIP: e.AssociatePublicIP,
		RootVolume:        volume(e.RootVolume, ""),
		Windows:           windows,
		Tags:              map[string]string{},
	}
	if e.CapacityType == string(ports.Spot) {
		l.CapacityType = ports.Spot
	}
	if e.DataVolume != nil {
		dev := "/dev/sdf"
		if windows {
			dev = "xvdf"
		}
		l.ExtraVolumes = []ports.VolumeSpec{volume(*e.DataVolume, dev)}
	}
	if cfg.AWS != nil {
		maps.Copy(l.Tags, cfg.AWS.ExtraTags)
	}
	maps.Copy(l.Tags, e.ExtraTags)
	for k := range l.Tags {
		if strings.HasPrefix(k, "cucina:") && slices.Contains(reservedTags, k) {
			delete(l.Tags, k) // the controller's own tags cannot be overridden
		}
	}
	return l
}

var reservedTags = []string{domain.TagManagedBy, domain.TagCluster, domain.TagPool, domain.TagGeneration, domain.TagImageVersion, domain.TagLaunchToken, domain.TagRole}

func volume(v v1alpha1.VolumeSpec, device string) ports.VolumeSpec {
	t := v.Type
	if t == "" {
		t = "gp3"
	}
	return ports.VolumeSpec{DeviceName: device, SizeGiB: int(v.SizeGiB), Type: t, IOPS: int(v.IOPS), Throughput: int(v.ThroughputMiBps), InitializationRate: int(v.InitializationRateMiBps)}
}

func tartLaunch(t *v1alpha1.TartSpec, ref string) *TartLaunch {
	l := &TartLaunch{Image: ref, HostSelector: maps.Clone(t.HostSelector), VMsPerHost: int(t.VMsPerHost), MaxAge: defaultTartMaxAge}
	if l.VMsPerHost <= 0 || l.VMsPerHost > MaxVMsPerHost {
		l.VMsPerHost = MaxVMsPerHost
	}
	if t.CPU != nil {
		l.CPU = int(*t.CPU)
	}
	if t.MemoryGiB != nil {
		l.MemoryGiB = int(*t.MemoryGiB)
	}
	if t.DiskGiB != nil {
		l.DiskGiB = int(*t.DiskGiB)
	}
	if t.MaxAge != nil && t.MaxAge.Duration > 0 {
		l.MaxAge = t.MaxAge.Duration
	}
	return l
}

// tartVCPUs sizes a macOS VM: spec.tart.cpu, else the smallest (cores−2)/slots
// over eligible hosts that reported facts (R-MAC-3), else a small default.
func tartVCPUs(t *v1alpha1.TartSpec, hosts []v1alpha1.MacHost) int {
	if t.CPU != nil && *t.CPU > 0 {
		return int(*t.CPU)
	}
	per := int(t.VMsPerHost)
	if per <= 0 || per > MaxVMsPerHost {
		per = MaxVMsPerHost
	}
	best := 0
	for _, h := range hosts {
		if !h.Spec.Approved || h.Status.Facts.Cores <= 2 || !labelsMatch(t.HostSelector, h.Spec.Labels) {
			continue
		}
		slots := per
		if h.Spec.Slots != nil && int(*h.Spec.Slots) < slots {
			slots = max(int(*h.Spec.Slots), 1)
		}
		v := max((int(h.Status.Facts.Cores)-2)/slots, 1)
		if best == 0 || v < best {
			best = v
		}
	}
	if best == 0 {
		return defaultTartVCPUs
	}
	return best
}

// hostPolicies indexes the MacHost objects by canonical (upper-case) serial.
func hostPolicies(hosts []v1alpha1.MacHost) map[string]HostPolicy {
	out := make(map[string]HostPolicy, len(hosts))
	for _, h := range hosts {
		p := HostPolicy{Approved: h.Spec.Approved && h.Status.Phase != v1alpha1.MacHostDenied, Cordoned: h.Spec.Cordoned, Labels: maps.Clone(h.Spec.Labels)}
		if h.Spec.Slots != nil {
			p.Slots = int(*h.Spec.Slots)
		}
		out[strings.ToUpper(strings.TrimSpace(h.Spec.Serial))] = p
	}
	return out
}

func labelsMatch(sel, labels map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// refTag returns the tag of an OCI reference ("ghcr.io/o/r:27.0-0.1.0" → "27.0-0.1.0").
func refTag(ref string) string {
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') && i >= 0 {
		return ref[i+1:]
	}
	return ""
}

func (b *RuntimeBuilder) image(ctx context.Context, wp *v1alpha1.WorkerPool) (ports.Image, error) {
	s := wp.Spec.Image
	sel := ports.ImageSelector{ID: s.AMI, Tags: s.AMISelector}
	if sel.ID == "" && len(sel.Tags) == 0 {
		return ports.Image{}, fmt.Errorf("%w: spec.image needs ami or amiSelector", ports.ErrImageNotFound)
	}
	keys := make([]string, 0, len(sel.Tags))
	for k, v := range sel.Tags {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	key := sel.ID + "|" + strings.Join(keys, ",")
	ttl := selectorImageTTL
	if sel.ID != "" {
		ttl = explicitImageTTL
	}
	now := b.Clock.Now()
	b.mu.Lock()
	if b.images == nil {
		b.images = map[string]imageEntry{}
	}
	e, ok := b.images[wp.Name]
	b.mu.Unlock()
	if ok && e.key == key && now.Sub(e.at) < ttl {
		return e.img, e.err
	}
	img, err := b.Compute.ResolveImage(ctx, sel)
	switch {
	case err == nil:
		e = imageEntry{key: key, img: img, at: now, ok: true}
	case ok && e.ok && e.key == key && !errors.Is(err, ports.ErrImageNotFound):
		// Transient failure (throttling, timeout): keep the last good image.
		e.at = now
		e.err = nil
	default:
		e = imageEntry{key: key, err: err, at: now}
	}
	b.mu.Lock()
	b.images[wp.Name] = e
	b.mu.Unlock()
	return e.img, e.err
}

// instanceVCPUs returns the vCPUs of the preferred (first) instance type, from
// the provider's instance-type data, falling back to the size suffix.
func (b *RuntimeBuilder) instanceVCPUs(ctx context.Context, types []string, windows bool) int {
	if len(types) == 0 {
		return 0
	}
	t := types[0]
	b.mu.Lock()
	if b.vcpus == nil {
		b.vcpus = map[string]int{}
	}
	v, ok := b.vcpus[t]
	b.mu.Unlock()
	if ok {
		return v
	}
	if prices, err := b.Compute.InstancePrices(ctx, []string{t}, windows); err == nil && prices[t].VCPU > 0 {
		v = prices[t].VCPU
		b.mu.Lock()
		b.vcpus[t] = v
		b.mu.Unlock()
		return v
	}
	return vcpusFromSize(t)
}

// vcpusFromSize estimates vCPUs from an EC2 size suffix (current x86 and
// Graviton families: large=2, xlarge=4, Nxlarge=4N). 0 when unknown (metal).
func vcpusFromSize(instanceType string) int {
	_, size, ok := strings.Cut(instanceType, ".")
	if !ok {
		return 0
	}
	switch size {
	case "medium":
		return 1
	case "large":
		return 2
	case "xlarge":
		return 4
	}
	if n, ok := strings.CutSuffix(size, "xlarge"); ok {
		if k, err := strconv.Atoi(n); err == nil && k > 0 {
			return 4 * k
		}
	}
	return 0
}
