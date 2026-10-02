// SPDX-License-Identifier: FSL-1.1-ALv2

package fakes

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// ComputeConfig shapes the simulated EC2 region.
type ComputeConfig struct {
	// Subnets maps subnet ID → availability zone.
	Subnets map[string]string
	// PendingMin/Max: pending → running.
	PendingMin, PendingMax time.Duration
	// BootMin/Max: running → guest ready (the OnReady hook; the worker can register).
	BootMin, BootMax time.Duration
	// WindowsBootFast/Slow: running → ready for Windows images with / without a
	// Fast Launch pre-provisioned snapshot.
	WindowsBootFast, WindowsBootSlow time.Duration
	// VisibleMin/Max: delay before a new instance appears in Describe (eventual consistency).
	VisibleMin, VisibleMax time.Duration
	// ShutdownLatency: shutting-down → terminated.
	ShutdownLatency time.Duration
	// TerminatedRetention: how long terminated instances stay visible in Describe.
	TerminatedRetention time.Duration
	// VCPUQuota is the account's running on-demand vCPU limit (0 = unlimited).
	VCPUQuota int
	// Buckets are the API token buckets per operation (RunInstances is "Launch").
	Buckets map[string]BucketSpec
	// Prices are the on-demand prices; DefaultPrices() if nil.
	Prices map[string]ports.InstancePrice
	// FastLaunchEnableLatency/DisableLatency and FastLaunchRefill (time to
	// pre-provision one snapshot).
	FastLaunchEnableLatency, FastLaunchDisableLatency, FastLaunchRefill time.Duration
}

// BucketSpec is a token bucket: Burst requests, refilled at Rate per second.
type BucketSpec struct {
	Burst float64
	Rate  float64
}

// DefaultComputeConfig mirrors measured EC2 behaviour (D3: Linux ready in
// ≈ 35–40 s, Windows ≈ 85 s with Fast Launch, ≈ 4 min without) and the
// documented API rate limits (RunInstances burst 5, refill 2/s).
func DefaultComputeConfig() ComputeConfig {
	return ComputeConfig{
		Subnets:                  map[string]string{"subnet-a": "us-west-1a", "subnet-b": "us-west-1b"},
		PendingMin:               5 * time.Second,
		PendingMax:               12 * time.Second,
		BootMin:                  20 * time.Second,
		BootMax:                  30 * time.Second,
		WindowsBootFast:          75 * time.Second,
		WindowsBootSlow:          4 * time.Minute,
		VisibleMin:               0,
		VisibleMax:               3 * time.Second,
		ShutdownLatency:          30 * time.Second,
		TerminatedRetention:      time.Hour,
		FastLaunchEnableLatency:  10 * time.Minute,
		FastLaunchDisableLatency: 5 * time.Minute,
		FastLaunchRefill:         5 * time.Minute,
		Buckets: map[string]BucketSpec{
			"Launch":    {Burst: 5, Rate: 2},
			"Describe":  {Burst: 100, Rate: 20},
			"Terminate": {Burst: 100, Rate: 5},
		},
	}
}

// DefaultPrices is a small us-west-1 on-demand price table (USD/h; Windows
// prices include the licence). Values are representative, not authoritative.
func DefaultPrices() map[string]ports.InstancePrice {
	return map[string]ports.InstancePrice{
		"c8i.2xlarge":  {Type: "c8i.2xlarge", USDPerHour: 0.4256, VCPU: 8, MemoryGiB: 16},
		"c7i.2xlarge":  {Type: "c7i.2xlarge", USDPerHour: 0.4011, VCPU: 8, MemoryGiB: 16},
		"c7a.2xlarge":  {Type: "c7a.2xlarge", USDPerHour: 0.4637, VCPU: 8, MemoryGiB: 16},
		"m6id.2xlarge": {Type: "m6id.2xlarge", USDPerHour: 0.5436, VCPU: 8, MemoryGiB: 32, NVMeGiB: 474},
		"c8g.2xlarge":  {Type: "c8g.2xlarge", USDPerHour: 0.3626, VCPU: 8, MemoryGiB: 16},
		"c7g.2xlarge":  {Type: "c7g.2xlarge", USDPerHour: 0.3418, VCPU: 8, MemoryGiB: 16},
		"c8i.8xlarge":  {Type: "c8i.8xlarge", USDPerHour: 1.7024, VCPU: 32, MemoryGiB: 64},
		"c7i.8xlarge":  {Type: "c7i.8xlarge", USDPerHour: 1.6044, VCPU: 32, MemoryGiB: 64},
		"c7a.8xlarge":  {Type: "c7a.8xlarge", USDPerHour: 1.8547, VCPU: 32, MemoryGiB: 64},
		"t3.micro":     {Type: "t3.micro", USDPerHour: 0.0124, VCPU: 2, MemoryGiB: 1},
	}
}

type capKey struct{ typ, az string }

// Compute is a stateful fake EC2 region implementing ports.Compute.
type Compute struct {
	*Faults
	mu    sync.Mutex
	clock ports.Clock
	rnd   *Rand
	cfg   ComputeConfig

	instances  map[string]*fInstance
	ids        []string                  // sorted keys of instances
	tombstones map[string]ports.Instance // instances aged out of Describe (tokens still resolve)
	tokens     map[string]string         // client token → instance ID
	volumes    map[string]*fResource
	enis       map[string]*fResource
	images     map[string]ports.Image
	fast       map[string]*fFastLaunch
	capacity   map[capKey]int // remaining launches per (type, AZ); absent = unlimited
	buckets    map[string]*bucket
	leakProb   float64
	nextID     uint64
	now        time.Time // last advance

	onReady      []func(ports.Instance)
	onTerminated []func(ports.Instance)
	events       []func()
	launchesByTk map[string]int // ground truth: instances created per token (NoDuplicateLaunchPerToken)
}

type fInstance struct {
	inst         ports.Instance
	params       string
	visibleAt    time.Time
	runningAt    time.Time
	readyAt      time.Time
	ready        bool
	terminatedAt time.Time // zero while alive
	goneAt       time.Time
	interrupted  bool
}

// Resource is a fake EBS volume or ENI.
type Resource struct {
	Kind       ports.OrphanKind
	ID         string
	Tags       map[string]string
	InstanceID string    // attached instance ("" = detached)
	DetachedAt time.Time // when it became orphaned
	Deleted    bool
}

type fResource struct{ Resource }

type fFastLaunch struct {
	status     ports.FastLaunchStatus
	target     int
	readyAt    time.Time // enabling → enabled / disabling → disabled
	nextRefill time.Time
}

type bucket struct {
	spec   BucketSpec
	tokens float64
	last   time.Time
}

func (b *bucket) take(now time.Time) bool {
	if now.After(b.last) {
		b.tokens = min(b.spec.Burst, b.tokens+now.Sub(b.last).Seconds()*b.spec.Rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

var _ ports.Compute = (*Compute)(nil)

// NewCompute returns a fake region.
func NewCompute(clock ports.Clock, rnd *Rand, cfg ComputeConfig) *Compute {
	if cfg.Prices == nil {
		cfg.Prices = DefaultPrices()
	}
	if cfg.Subnets == nil {
		cfg.Subnets = DefaultComputeConfig().Subnets
	}
	c := &Compute{Faults: newFaults(clock, rnd.Child("compute-faults")), clock: clock, rnd: rnd.Child("compute"), cfg: cfg,
		instances: map[string]*fInstance{}, tombstones: map[string]ports.Instance{}, tokens: map[string]string{}, volumes: map[string]*fResource{}, enis: map[string]*fResource{},
		images: map[string]ports.Image{}, fast: map[string]*fFastLaunch{}, capacity: map[capKey]int{}, buckets: map[string]*bucket{},
		now: clock.Now(), launchesByTk: map[string]int{}}
	for op, b := range cfg.Buckets {
		c.buckets[op] = &bucket{spec: b, tokens: b.Burst, last: c.now}
	}
	return c
}

// ------------------------------------------------------------- knobs

// OnReady registers a hook called when an instance's guest is ready (the
// worker agent would now register with the scheduler).
func (c *Compute) OnReady(f func(ports.Instance)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onReady = append(c.onReady, f)
}

// OnTerminated registers a hook called when an instance starts shutting down
// (terminate, spot interruption, dead-man) — its worker disappears.
func (c *Compute) OnTerminated(f func(ports.Instance)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onTerminated = append(c.onTerminated, f)
}

// SetCapacity limits launches of type in AZ to n more (0 = insufficient
// capacity, n < 0 = unlimited).
func (c *Compute) SetCapacity(typ, az string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 0 {
		delete(c.capacity, capKey{typ, az})
		return
	}
	c.capacity[capKey{typ, az}] = n
}

// SetVCPUQuota sets the running vCPU quota (0 = unlimited).
func (c *Compute) SetVCPUQuota(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg.VCPUQuota = n
}

// SetBucket replaces the token bucket of op.
func (c *Compute) SetBucket(op string, burst, ratePerSecond float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buckets[op] = &bucket{spec: BucketSpec{Burst: burst, Rate: ratePerSecond}, tokens: burst, last: c.clock.Now()}
}

// SetLeakOnTerminate makes each volume of a terminated instance survive
// (orphaned, e.g. a DeleteOnTermination race after a crash) with probability p.
func (c *Compute) SetLeakOnTerminate(p float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leakProb = p
}

// AddImage registers an AMI. With no image registered every ImageID is accepted.
func (c *Compute) AddImage(img ports.Image) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if img.CreatedAt.IsZero() {
		img.CreatedAt = c.clock.Now()
	}
	c.images[img.ID] = img
}

// RemoveImage deregisters an AMI.
func (c *Compute) RemoveImage(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.images, id)
}

// InjectOrphan creates a detached pool-tagged volume or ENI (as left behind by a crash).
func (c *Compute) InjectOrphan(kind ports.OrphanKind, tags map[string]string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	r := &fResource{Resource{Kind: kind, Tags: maps.Clone(tags), DetachedAt: now}}
	if kind == ports.OrphanENI {
		r.ID = c.newID("eni-")
		c.enis[r.ID] = r
	} else {
		r.ID = c.newID("vol-")
		c.volumes[r.ID] = r
	}
	return r.ID
}

// Interrupt terminates an instance from the outside (spot interruption,
// dead-man switch, operator): it shuts down without a controller call.
func (c *Compute) Interrupt(id string) error {
	c.mu.Lock()
	in, ok := c.instances[id]
	if !ok || !in.terminatedAt.IsZero() {
		c.mu.Unlock()
		return ports.ErrNotFound
	}
	c.advanceLocked(c.clock.Now())
	in.interrupted = true
	c.terminateLocked(in, c.clock.Now())
	ev := c.takeEvents()
	c.mu.Unlock()
	runEvents(ev)
	return nil
}

// Tick advances the fake to the clock's time and fires due hooks.
func (c *Compute) Tick() {
	c.mu.Lock()
	c.advanceLocked(c.clock.Now())
	ev := c.takeEvents()
	c.mu.Unlock()
	runEvents(ev)
}

// ------------------------------------------------------------ ground truth

// All returns every instance ever launched that is not yet gone from the
// region (including ones not yet visible to Describe), sorted by ID.
func (c *Compute) All() []ports.Instance {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advanceLocked(c.clock.Now())
	out := make([]ports.Instance, 0, len(c.ids))
	for _, id := range c.ids {
		out = append(out, cloneInstance(c.instances[id].inst))
	}
	return out
}

// Resources returns every volume and ENI (including deleted ones), sorted.
func (c *Compute) Resources() []Resource {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Resource
	for _, m := range []map[string]*fResource{c.volumes, c.enis} {
		for _, r := range m {
			rr := r.Resource
			rr.Tags = maps.Clone(r.Tags)
			out = append(out, rr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// LaunchesPerToken returns how many instances each client token created
// (ground truth for NoDuplicateLaunchPerToken; a correct EC2 never exceeds 1).
func (c *Compute) LaunchesPerToken() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.launchesByTk)
}

// InstanceSeconds returns billed instance-seconds per pool (60 s minimum per
// launch, R-OBS-5) and the estimated cost in USD from the price table.
func (c *Compute) InstanceSeconds() (seconds map[domain.PoolName]float64, usd map[domain.PoolName]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	c.advanceLocked(now)
	seconds, usd = map[domain.PoolName]float64{}, map[domain.PoolName]float64{}
	for _, in := range c.instances {
		end := now
		if !in.terminatedAt.IsZero() {
			end = in.terminatedAt
		}
		s := max(end.Sub(in.inst.LaunchTime).Seconds(), 60)
		seconds[in.inst.Pool] += s
		usd[in.inst.Pool] += s / 3600 * c.cfg.Prices[in.inst.Type].USDPerHour
	}
	return seconds, usd
}

// ------------------------------------------------------------- port methods

// Launch implements ports.Compute.
func (c *Compute) Launch(ctx context.Context, req ports.LaunchRequest) (ports.Instance, error) {
	if err := c.enter(ctx, "Launch"); err != nil {
		return ports.Instance{}, err
	}
	c.mu.Lock()
	now := c.clock.Now()
	c.advanceLocked(now)
	inst, err := c.launchLocked(req, now)
	ev := c.takeEvents()
	c.mu.Unlock()
	runEvents(ev)
	return inst, err
}

func launchParams(req ports.LaunchRequest) string {
	return fmt.Sprintf("%s|%s|%s|%v|%v|%s", req.Pool, req.Generation, req.ImageID, req.InstanceTypes, req.SubnetIDs, req.CapacityType)
}

func (c *Compute) launchLocked(req ports.LaunchRequest, now time.Time) (ports.Instance, error) {
	if b := c.buckets["Launch"]; b != nil && !b.take(now) {
		return ports.Instance{}, fmt.Errorf("RequestLimitExceeded: %w", ports.ErrThrottled)
	}
	if req.Token != "" {
		// Like the adapter (client-token lookup first) and EC2: a known token
		// returns the instance it created, whatever its state and parameters.
		if id, ok := c.tokens[req.Token]; ok {
			if in := c.instances[id]; in != nil {
				return launchView(in.inst), nil
			}
			return launchView(c.tombstones[id]), nil
		}
	}
	if len(req.InstanceTypes) == 0 || len(req.SubnetIDs) == 0 || req.Pool == "" {
		return ports.Instance{}, fmt.Errorf("launch needs pool, instance types and subnets: %w", ports.ErrInvalid)
	}
	var img ports.Image
	if len(c.images) > 0 {
		var ok bool
		if img, ok = c.images[req.ImageID]; !ok {
			return ports.Instance{}, fmt.Errorf("InvalidAMIID.NotFound %s: %w", req.ImageID, ports.ErrImageNotFound)
		}
	}
	quotaHit := false
	for _, typ := range req.InstanceTypes {
		price, known := c.cfg.Prices[typ]
		if !known {
			return ports.Instance{}, fmt.Errorf("InvalidInstanceType %s: %w", typ, ports.ErrInvalid)
		}
		for _, subnet := range req.SubnetIDs {
			az, ok := c.cfg.Subnets[subnet]
			if !ok {
				return ports.Instance{}, fmt.Errorf("InvalidSubnetID.NotFound %s: %w", subnet, ports.ErrInvalid)
			}
			if n, limited := c.capacity[capKey{typ, az}]; limited && n <= 0 {
				continue // InsufficientInstanceCapacity for this (type, AZ): try the next one
			}
			if c.cfg.VCPUQuota > 0 && c.runningVCPUs()+price.VCPU > c.cfg.VCPUQuota {
				quotaHit = true
				continue
			}
			if n, limited := c.capacity[capKey{typ, az}]; limited {
				c.capacity[capKey{typ, az}] = n - 1
			}
			return c.createLocked(req, typ, subnet, az, img, now), nil
		}
	}
	if quotaHit {
		return ports.Instance{}, fmt.Errorf("VcpuLimitExceeded: %w", ports.ErrQuotaExceeded)
	}
	return ports.Instance{}, fmt.Errorf("InsufficientInstanceCapacity for %v in %v: %w", req.InstanceTypes, req.SubnetIDs, ports.ErrInsufficientCapacity)
}

func (c *Compute) runningVCPUs() int {
	n := 0
	for _, in := range c.instances {
		if in.terminatedAt.IsZero() {
			n += c.cfg.Prices[in.inst.Type].VCPU
		}
	}
	return n
}

func (c *Compute) newID(prefix string) string {
	c.nextID++
	return fmt.Sprintf("%s%017x", prefix, uint64(c.rnd.Int63n(1<<40))<<20|c.nextID&0xfffff)
}

func (c *Compute) createLocked(req ports.LaunchRequest, typ, subnet, az string, img ports.Image, now time.Time) ports.Instance {
	id := c.newID("i-")
	tags := maps.Clone(req.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	in := &fInstance{params: launchParams(req)}
	in.inst = ports.Instance{
		ID: id, Pool: req.Pool, Generation: req.Generation, State: ports.InstancePending, Type: typ, SubnetID: subnet, AZ: az,
		PrivateIP: netip.AddrFrom4([4]byte{10, 0, byte(c.nextID >> 8), byte(c.nextID)}), LaunchTime: now,
		CapacityType: req.CapacityType, ImageID: req.ImageID, Tags: tags,
	}
	if in.inst.CapacityType == "" {
		in.inst.CapacityType = ports.OnDemand
	}
	in.runningAt = now.Add(c.rnd.Duration(c.cfg.PendingMin, c.cfg.PendingMax))
	boot := c.rnd.Duration(c.cfg.BootMin, c.cfg.BootMax)
	if img.Platform == "windows" {
		boot = c.cfg.WindowsBootSlow
		if fl := c.fast[img.ID]; fl != nil && fl.status.State == "enabled" && fl.status.Snapshots > 0 {
			fl.status.Snapshots--
			if fl.nextRefill.IsZero() || fl.nextRefill.Before(now) {
				fl.nextRefill = now.Add(c.cfg.FastLaunchRefill)
			}
			boot = c.cfg.WindowsBootFast
		}
	}
	in.readyAt = in.runningAt.Add(boot)
	in.visibleAt = now.Add(c.rnd.Duration(c.cfg.VisibleMin, c.cfg.VisibleMax))
	vols := append([]ports.VolumeSpec{req.RootVolume}, req.ExtraVolumes...)
	for range vols {
		v := &fResource{Resource{Kind: ports.OrphanVolume, ID: c.newID("vol-"), Tags: maps.Clone(tags), InstanceID: id}}
		c.volumes[v.ID] = v
		in.inst.VolumeIDs = append(in.inst.VolumeIDs, v.ID)
	}
	eni := &fResource{Resource{Kind: ports.OrphanENI, ID: c.newID("eni-"), Tags: maps.Clone(tags), InstanceID: id}}
	c.enis[eni.ID] = eni
	in.inst.ENIIDs = []string{eni.ID}
	c.instances[id] = in
	i, _ := slices.BinarySearch(c.ids, id)
	c.ids = slices.Insert(c.ids, i, id)
	if req.Token != "" {
		c.tokens[req.Token] = id
		c.launchesByTk[req.Token]++
	}
	return launchView(in.inst)
}

// launchView is what a RunInstances response carries: no block-device
// mappings yet (volumes appear in Describe a second or two later).
func launchView(in ports.Instance) ports.Instance {
	in = cloneInstance(in)
	in.VolumeIDs = nil
	return in
}

// Describe implements ports.Compute (eventually consistent, tag-filtered).
func (c *Compute) Describe(ctx context.Context, f ports.InstanceFilter) ([]ports.Instance, error) {
	if err := c.enter(ctx, "Describe"); err != nil {
		return nil, err
	}
	if f.Cluster == "" {
		return nil, fmt.Errorf("describe without cluster tag filter: %w", ports.ErrInvalid)
	}
	c.mu.Lock()
	now := c.clock.Now()
	c.advanceLocked(now)
	defer func() {
		ev := c.takeEvents()
		c.mu.Unlock()
		runEvents(ev)
	}()
	if b := c.buckets["Describe"]; b != nil && !b.take(now) {
		return nil, fmt.Errorf("RequestLimitExceeded: %w", ports.ErrThrottled)
	}
	states := f.States
	if len(states) == 0 {
		states = []ports.InstanceState{ports.InstancePending, ports.InstanceRunning, ports.InstanceShuttingDown}
	}
	var out []ports.Instance
	for _, id := range c.ids {
		in := c.instances[id]
		if now.Before(in.visibleAt) || in.inst.Tags[domain.TagCluster] != f.Cluster {
			continue
		}
		if f.Pool != "" && in.inst.Pool != f.Pool {
			continue
		}
		if len(f.IDs) > 0 && !slices.Contains(f.IDs, id) {
			continue
		}
		if !slices.Contains(states, in.inst.State) {
			continue
		}
		out = append(out, cloneInstance(in.inst))
	}
	return out, nil
}

// Terminate implements ports.Compute.
func (c *Compute) Terminate(ctx context.Context, cluster string, ids []string) (map[string]error, error) {
	if err := c.enter(ctx, "Terminate"); err != nil {
		return nil, err
	}
	c.mu.Lock()
	now := c.clock.Now()
	c.advanceLocked(now)
	if b := c.buckets["Terminate"]; b != nil && !b.take(now) {
		c.mu.Unlock()
		return nil, fmt.Errorf("RequestLimitExceeded: %w", ports.ErrThrottled)
	}
	errs := map[string]error{}
	for _, id := range ids {
		in, ok := c.instances[id]
		switch {
		case !ok:
			errs[id] = fmt.Errorf("InvalidInstanceID.NotFound %s: %w", id, ports.ErrNotFound)
		case in.inst.Tags[domain.TagCluster] != cluster || in.inst.Tags[domain.TagManagedBy] != domain.ManagedByValue:
			errs[id] = fmt.Errorf("instance %s: %w", id, ports.ErrNotOwned)
		case in.terminatedAt.IsZero():
			c.terminateLocked(in, now)
		}
	}
	ev := c.takeEvents()
	c.mu.Unlock()
	runEvents(ev)
	return errs, nil
}

func (c *Compute) terminateLocked(in *fInstance, now time.Time) {
	in.inst.State = ports.InstanceShuttingDown
	in.terminatedAt = now
	in.goneAt = now.Add(c.cfg.ShutdownLatency + c.cfg.TerminatedRetention)
	inst := cloneInstance(in.inst)
	for _, h := range c.onTerminated {
		h := h
		c.events = append(c.events, func() { h(inst) })
	}
}

// ListOrphans implements ports.Compute.
func (c *Compute) ListOrphans(ctx context.Context, cluster string) ([]ports.Orphan, error) {
	if err := c.enter(ctx, "ListOrphans"); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	c.advanceLocked(now)
	var out []ports.Orphan
	for _, m := range []map[string]*fResource{c.volumes, c.enis} {
		for _, r := range m {
			if r.Deleted || r.InstanceID != "" || r.Tags[domain.TagCluster] != cluster || r.Tags[domain.TagPool] == "" {
				continue
			}
			out = append(out, ports.Orphan{Kind: r.Kind, ID: r.ID, Pool: domain.PoolName(r.Tags[domain.TagPool]), Age: now.Sub(r.DetachedAt)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// DeleteOrphans implements ports.Compute.
func (c *Compute) DeleteOrphans(ctx context.Context, cluster string, orphans []ports.Orphan) (map[string]error, error) {
	if err := c.enter(ctx, "DeleteOrphans"); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	errs := map[string]error{}
	for _, o := range orphans {
		m := c.volumes
		if o.Kind == ports.OrphanENI {
			m = c.enis
		}
		r, ok := m[o.ID]
		switch {
		case !ok:
			errs[o.ID] = ports.ErrNotFound
		case r.Deleted:
			// already deleted: success (idempotent, like the adapter)
		case r.Tags[domain.TagCluster] != cluster:
			errs[o.ID] = ports.ErrNotOwned
		case r.InstanceID != "":
			errs[o.ID] = fmt.Errorf("%s is attached to %s: %w", o.ID, r.InstanceID, ports.ErrInvalid)
		default:
			r.Deleted = true
		}
	}
	return errs, nil
}

// ResolveImage implements ports.Compute.
func (c *Compute) ResolveImage(ctx context.Context, sel ports.ImageSelector) (ports.Image, error) {
	if err := c.enter(ctx, "ResolveImage"); err != nil {
		return ports.Image{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if sel.ID != "" {
		if img, ok := c.images[sel.ID]; ok {
			return img, nil
		}
		return ports.Image{}, fmt.Errorf("image %s: %w", sel.ID, ports.ErrImageNotFound)
	}
	var best *ports.Image
	for _, id := range sortedKeys(c.images) {
		img := c.images[id]
		match := len(sel.Tags) > 0
		for k, v := range sel.Tags {
			if imageTag(img, k) != v {
				match = false
			}
		}
		if match && (best == nil || img.CreatedAt.After(best.CreatedAt)) {
			best = &img
		}
	}
	if best == nil {
		return ports.Image{}, fmt.Errorf("no image matches %v: %w", sel.Tags, ports.ErrImageNotFound)
	}
	return *best, nil
}

func imageTag(img ports.Image, k string) string {
	switch k {
	case domain.TagImageVersion:
		return img.Version
	case domain.TagGeneration:
		return img.Generation
	case "Name":
		return img.Name
	}
	return ""
}

// FastLaunch implements ports.Compute. Like the adapter, "enable" and
// "disable" block (on the clock, bounded by ctx) until the image reaches
// enabled/disabled; "describe" returns the current state.
func (c *Compute) FastLaunch(ctx context.Context, op ports.FastLaunchOp) (ports.FastLaunchStatus, error) {
	if err := c.enter(ctx, "FastLaunch"); err != nil {
		return ports.FastLaunchStatus{}, err
	}
	c.mu.Lock()
	now := c.clock.Now()
	c.advanceLocked(now)
	if _, ok := c.images[op.ImageID]; !ok && len(c.images) > 0 {
		c.mu.Unlock()
		return ports.FastLaunchStatus{}, fmt.Errorf("image %s: %w", op.ImageID, ports.ErrImageNotFound)
	}
	fl := c.fast[op.ImageID]
	if fl == nil {
		fl = &fFastLaunch{status: ports.FastLaunchStatus{ImageID: op.ImageID, State: "disabled"}}
		c.fast[op.ImageID] = fl
	}
	want := ""
	switch op.Action {
	case "enable":
		if op.TargetCount <= 0 {
			c.mu.Unlock()
			return fl.status, fmt.Errorf("fast launch target count %d: %w", op.TargetCount, ports.ErrInvalid)
		}
		fl.target, want = op.TargetCount, "enabled"
		if fl.status.State != "enabled" {
			fl.status.State = "enabling"
			fl.readyAt = now.Add(c.cfg.FastLaunchEnableLatency)
		}
	case "disable":
		want = "disabled"
		if fl.status.State != "disabled" {
			fl.status.State = "disabling"
			fl.readyAt = now.Add(c.cfg.FastLaunchDisableLatency)
		}
	case "describe", "":
	default:
		c.mu.Unlock()
		return fl.status, fmt.Errorf("fast launch action %q: %w", op.Action, ports.ErrInvalid)
	}
	st, wait := fl.status, fl.readyAt.Sub(now)
	c.mu.Unlock()
	if want == "" || st.State == want {
		return st, nil
	}
	if err := c.clock.Sleep(ctx, wait); err != nil {
		return st, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advanceLocked(c.clock.Now())
	return c.fast[op.ImageID].status, nil
}

// PreProvisionFastLaunch puts an image straight into the enabled state with
// target pre-provisioned snapshots (test setup; no waiting).
func (c *Compute) PreProvisionFastLaunch(imageID string, target int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	c.fast[imageID] = &fFastLaunch{status: ports.FastLaunchStatus{ImageID: imageID, State: "enabled", Snapshots: target},
		target: target, nextRefill: now}
}

// InstancePrices implements ports.Compute.
func (c *Compute) InstancePrices(ctx context.Context, types []string, windows bool) (map[string]ports.InstancePrice, error) {
	if err := c.enter(ctx, "InstancePrices"); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]ports.InstancePrice{}
	var missing []string
	for _, t := range types {
		p, ok := c.cfg.Prices[t]
		if !ok {
			missing = append(missing, t)
			continue
		}
		if windows {
			p.Windows = true
			p.USDPerHour += 0.046 * float64(p.VCPU) // licence surcharge per vCPU-hour
		}
		out[t] = p
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("no price for %v: %w", missing, ports.ErrNotFound)
	}
	return out, nil
}

// ------------------------------------------------------------- time

// advanceLocked applies every state change due at or before now, in time order.
func (c *Compute) advanceLocked(now time.Time) {
	if now.Before(c.now) {
		now = c.now
	}
	c.now = now
	var gone []string
	defer func() {
		if len(gone) > 0 {
			c.ids = slices.DeleteFunc(c.ids, func(id string) bool { return slices.Contains(gone, id) })
		}
	}()
	for _, id := range c.ids {
		in := c.instances[id]
		if in.terminatedAt.IsZero() {
			if in.inst.State == ports.InstancePending && !now.Before(in.runningAt) {
				in.inst.State = ports.InstanceRunning
			}
			if !in.ready && in.inst.State == ports.InstanceRunning && !now.Before(in.readyAt) {
				in.ready = true
				inst := cloneInstance(in.inst)
				for _, h := range c.onReady {
					h := h
					c.events = append(c.events, func() { h(inst) })
				}
			}
			continue
		}
		if in.inst.State == ports.InstanceShuttingDown && !now.Before(in.terminatedAt.Add(c.cfg.ShutdownLatency)) {
			in.inst.State = ports.InstanceTerminated
			c.releaseResourcesLocked(in, in.terminatedAt.Add(c.cfg.ShutdownLatency))
		}
		if !in.goneAt.IsZero() && !now.Before(in.goneAt) {
			c.tombstones[id] = cloneInstance(in.inst)
			delete(c.instances, id)
			gone = append(gone, id)
		}
	}
	for _, img := range sortedKeys(c.fast) {
		fl := c.fast[img]
		switch fl.status.State {
		case "enabling":
			if !now.Before(fl.readyAt) {
				fl.status.State = "enabled"
				fl.nextRefill = fl.readyAt
			}
		case "disabling":
			if !now.Before(fl.readyAt) {
				fl.status.State, fl.status.Snapshots = "disabled", 0
			}
		}
		if fl.status.State == "enabled" && c.cfg.FastLaunchRefill > 0 {
			for fl.status.Snapshots < fl.target && !fl.nextRefill.After(now) {
				fl.status.Snapshots++
				fl.nextRefill = fl.nextRefill.Add(c.cfg.FastLaunchRefill)
			}
			if fl.status.Snapshots >= fl.target && fl.nextRefill.Before(now) {
				fl.nextRefill = now
			}
		}
	}
}

func (c *Compute) releaseResourcesLocked(in *fInstance, at time.Time) {
	for _, vid := range in.inst.VolumeIDs {
		if v := c.volumes[vid]; v != nil && v.InstanceID == in.inst.ID {
			v.InstanceID = ""
			v.DetachedAt = at
			if c.leakProb > 0 && c.rnd.Float64() < c.leakProb {
				continue // orphaned
			}
			v.Deleted = true
		}
	}
	for _, eid := range in.inst.ENIIDs {
		if e := c.enis[eid]; e != nil && e.InstanceID == in.inst.ID {
			e.InstanceID, e.DetachedAt, e.Deleted = "", at, true
		}
	}
}

func (c *Compute) takeEvents() []func() {
	ev := c.events
	c.events = nil
	return ev
}

func runEvents(ev []func()) {
	for _, f := range ev {
		f()
	}
}

func cloneInstance(in ports.Instance) ports.Instance {
	in.Tags = maps.Clone(in.Tags)
	in.VolumeIDs = slices.Clone(in.VolumeIDs)
	in.ENIIDs = slices.Clone(in.ENIIDs)
	return in
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// IsCapacityError reports whether err is one of the capacity sentinels.
func IsCapacityError(err error) bool {
	return errors.Is(err, ports.ErrInsufficientCapacity) || errors.Is(err, ports.ErrQuotaExceeded)
}
