// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cucinav1 "github.com/sloper-ai/cucina/api/proto/cucina/v1"
	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

func u32[T ~int | ~int32 | ~int64](v T) uint32 {
	switch {
	case v <= 0:
		return 0
	case int64(v) > math.MaxUint32:
		return math.MaxUint32
	}
	return uint32(v)
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func metaTS(t *metav1.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return ts(t.Time)
}

func dur(d time.Duration) *durationpb.Duration {
	if d <= 0 {
		return nil
	}
	return durationpb.New(d)
}

func money(m Micros) *cucinav1.Money { return &cucinav1.Money{Micros: int64(m)} }

// ---------------------------------------------------------------- pools

// readyCondition summarises a pool's conditions as (condition, message): "Ready"
// when Ready=True, else the Ready reason (QueueNotDeclared, NoCapacity, …).
func readyCondition(conds []metav1.Condition) (string, string) {
	for _, c := range conds {
		if c.Type != v1alpha1.ConditionReady {
			continue
		}
		if c.Status == metav1.ConditionTrue {
			return v1alpha1.ConditionReady, c.Message
		}
		if c.Reason != "" {
			return c.Reason, c.Message
		}
		return "NotReady", c.Message
	}
	return "Unknown", ""
}

func conditionStrings(conds []metav1.Condition) []string {
	out := make([]string, 0, len(conds))
	for _, c := range conds {
		s := fmt.Sprintf("%s=%s", c.Type, c.Status)
		if c.Reason != "" {
			s += " " + c.Reason
		}
		if c.Message != "" {
			s += ": " + c.Message
		}
		out = append(out, s)
	}
	return out
}

func poolSummary(p Pool, now time.Time) *cucinav1.PoolSummary {
	r := p.Resource
	minRunning := r.Spec.Capacity.MinRunning
	if f := p.Floor; f != nil && f.ExpiresAt.After(now) && f.MinRunning > minRunning {
		minRunning = f.MinRunning
	}
	cond, msg := readyCondition(r.Status.Conditions)
	provider := r.Spec.Provider
	if provider == "" {
		provider = string(p.Spec.Provider)
	}
	platform := r.Spec.Platform
	if platform == "" {
		platform = p.Spec.Platform
	}
	generation := r.Status.ImageGeneration
	if generation == "" {
		generation = p.Spec.Generation
	}
	return &cucinav1.PoolSummary{
		Name:            p.Name(),
		Platform:        platform,
		Provider:        provider,
		MinRunning:      u32(minRunning),
		Max:             u32(r.Spec.Capacity.Max),
		Desired:         u32(r.Status.Desired),
		Launching:       u32(r.Status.Launching),
		Registered:      u32(r.Status.Registered),
		Busy:            u32(r.Status.Busy),
		Idle:            u32(r.Status.Idle),
		Draining:        u32(r.Status.Draining),
		Stopped:         u32(r.Status.Stopped),
		ImageGeneration: generation,
		Paused:          r.Spec.Paused || p.Spec.Paused,
		Condition:       cond,
		Message:         msg,
	}
}

// poolQueueKeys returns every scheduler queue a pool's workers serve: instance name
// × runner × size class (contracts §3).
func poolQueueKeys(spec domain.PoolSpec, defaultNames []string) []domain.QueueKey {
	names := spec.InstanceNames
	if len(names) == 0 {
		names = defaultNames
	}
	seen := map[domain.QueueKey]bool{}
	var out []domain.QueueKey
	for _, n := range names {
		for _, r := range spec.Runners {
			k := domain.QueueKey{InstanceNamePrefix: n, PlatformKey: domain.PropertiesKey(r.Properties), SizeClass: spec.SizeClass}
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	slices.SortFunc(out, compareQueueKeys)
	return out
}

func compareQueueKeys(a, b domain.QueueKey) int {
	if c := strings.Compare(a.InstanceNamePrefix, b.InstanceNamePrefix); c != 0 {
		return c
	}
	if c := strings.Compare(a.PlatformKey, b.PlatformKey); c != 0 {
		return c
	}
	switch {
	case a.SizeClass < b.SizeClass:
		return -1
	case a.SizeClass > b.SizeClass:
		return 1
	}
	return 0
}

// poolOS returns the OSFamily property of the pool's runners ("linux", "windows", "macos").
func poolOS(spec domain.PoolSpec) string {
	for _, r := range spec.Runners {
		if v := r.Properties["OSFamily"]; v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------- workers

// workerState maps the controller's VM state onto the API's worker states.
func workerState(vm domain.VM) string {
	switch vm.State {
	case domain.VMRegistered:
		if vm.Busy > 0 {
			return "busy"
		}
		return "idle"
	case "":
		return "unknown"
	}
	return string(vm.State)
}

// countsAsInstance reports whether a VM is a standing worker instance (it exists
// and costs money or a host slot).
func countsAsInstance(vm domain.VM) bool {
	switch vm.State {
	case domain.VMStopped, domain.VMTerminated:
		return false
	}
	return true
}

func workerSummary(w Worker, now time.Time) *cucinav1.WorkerSummary {
	st := workerState(w.VM)
	out := &cucinav1.WorkerSummary{
		Node:         w.ID,
		Pool:         string(w.Pool),
		State:        st,
		Generation:   w.Generation,
		InstanceType: w.InstanceType,
		Host:         w.Host,
		Threads:      u32(w.Threads),
		BusyThreads:  u32(w.Busy),
		Drained:      w.Drained,
		Launched:     ts(w.LaunchedAt),
		PrivateIp:    w.PrivateIP,
	}
	if st == "idle" && !w.IdleSince.IsZero() && now.After(w.IdleSince) {
		out.IdleFor = durationpb.New(now.Sub(w.IdleSince))
	}
	return out
}

// ---------------------------------------------------------------- queues

func parsePlatformKey(k string) []*cucinav1.PlatformProperty {
	if k == "" {
		return nil
	}
	var out []*cucinav1.PlatformProperty
	for _, kv := range strings.Split(k, ";") {
		name, value, _ := strings.Cut(kv, "=")
		out = append(out, &cucinav1.PlatformProperty{Name: name, Value: value})
	}
	return out
}

func queueRef(k domain.QueueKey) *cucinav1.QueueRef {
	return &cucinav1.QueueRef{
		InstanceNamePrefix: k.InstanceNamePrefix,
		Platform:           parsePlatformKey(k.PlatformKey),
		SizeClass:          k.SizeClass,
	}
}

// queueKey converts an API queue reference to the scheduler's key.
func queueKey(r *cucinav1.QueueRef) (domain.QueueKey, error) {
	if r == nil {
		return domain.QueueKey{}, invalid("queue is required")
	}
	props := make(map[string]string, len(r.GetPlatform()))
	for _, p := range r.GetPlatform() {
		if p.GetName() == "" {
			return domain.QueueKey{}, invalid("queue platform property without a name")
		}
		if _, dup := props[p.GetName()]; dup {
			return domain.QueueKey{}, invalid("queue platform property %q repeated", p.GetName())
		}
		props[p.GetName()] = p.GetValue()
	}
	return domain.QueueKey{
		InstanceNamePrefix: r.GetInstanceNamePrefix(),
		PlatformKey:        domain.PropertiesKey(props),
		SizeClass:          r.GetSizeClass(),
	}, nil
}

// ---------------------------------------------------------------- hosts

func hostSlots(h v1alpha1.MacHost) uint32 {
	if h.Spec.Slots != nil {
		return u32(*h.Spec.Slots)
	}
	return 2 // Apple licence limit and the default
}

func hostSummary(h v1alpha1.MacHost) *cucinav1.HostSummary {
	return &cucinav1.HostSummary{
		Serial:           h.Spec.Serial,
		Name:             h.Name,
		Site:             h.Spec.Site,
		Phase:            h.Status.Phase,
		RunningVms:       u32(h.Status.RunningVMs),
		Slots:            hostSlots(h),
		L2HitRatio:       h.Status.L2.HitRatio,
		WanBytesReceived: h.Status.L2.WANBytesReceived,
		LastHeartbeat:    metaTS(h.Status.LastHeartbeat),
		AgentVersion:     h.Status.Facts.AgentVersion,
	}
}

func hostDetail(h v1alpha1.MacHost) *cucinav1.HostDetail {
	d := &cucinav1.HostDetail{
		Summary:      hostSummary(h),
		MacosVersion: h.Status.Facts.MacOSVersion,
		TartVersion:  h.Status.Facts.TartVersion,
		Chip:         h.Status.Facts.Chip,
		Cores:        u32(h.Status.Facts.Cores),
		MemoryGib:    u32(h.Status.Facts.MemoryGiB),
		DiskFreeGib:  u32(h.Status.Facts.DiskFreeGiB),
		Images:       slices.Clone(h.Status.Images),
		Cordoned:     h.Spec.Cordoned,
		Approved:     h.Spec.Approved,
		Labels:       h.Spec.Labels,
		CertExpiry:   metaTS(h.Status.CertificateExpiry),
		Filevault:    h.Status.Facts.FileVault,
	}
	for _, vm := range h.Status.VMs {
		d.Vms = append(d.Vms, &cucinav1.HostVM{
			Name: vm.Name, Pool: vm.Pool, State: vm.State, Image: vm.Image,
			Generation: vm.Generation, Registered: vm.Registered,
		})
	}
	return d
}

// ---------------------------------------------------------------- operations

// slashDigest renders the scheduler's "hash-size" digest as the API's "hash/size".
func slashDigest(d string) string {
	if i := strings.LastIndexByte(d, '-'); i > 0 && !strings.Contains(d, "/") {
		return d[:i] + "/" + d[i+1:]
	}
	return d
}

func operationSummary(o ports.Operation) *cucinav1.OperationSummary {
	out := &cucinav1.OperationSummary{
		Name:         o.Name,
		Queue:        queueRef(o.Queue),
		ActionDigest: slashDigest(o.ActionDigest),
		Stage:        o.Stage,
		QueuedAt:     ts(o.QueuedAt),
		TargetId:     o.TargetID,
		InvocationId: o.InvocationID,
		Priority:     o.Priority,
	}
	if o.Worker != nil {
		out.WorkerNode = o.Worker[domain.LabelNode]
		if t, err := strconv.ParseUint(o.Worker["thread"], 10, 32); err == nil {
			out.WorkerThread = uint32(t)
		}
	}
	return out
}

// ---------------------------------------------------------------- cost, alerts

func costSummary(r CostReport) *cucinav1.CostSummary {
	out := &cucinav1.CostSummary{
		Today:            money(r.Today),
		MonthToDate:      money(r.MonthToDate),
		StandingPerMonth: money(r.StandingPerMonth),
	}
	for _, p := range r.Pools {
		out.Pools = append(out.Pools, &cucinav1.PoolCost{
			Pool: p.Pool, InstanceSeconds: p.InstanceSeconds, Compute: money(p.Compute),
			Ebs: money(p.EBS), DataTransfer: money(p.DataTransfer), Standing: money(p.Standing),
		})
	}
	return out
}

func alertProto(a Alert) *cucinav1.Alert {
	return &cucinav1.Alert{Name: a.Name, Severity: a.Severity, Summary: a.Summary, Since: ts(a.Since), Labels: a.Labels}
}

func startProto(s StartLatency) *cucinav1.StartLatency {
	return &cucinav1.StartLatency{
		Pool: s.Pool, Vm: s.VM, Launched: ts(s.Launched), ToRunning: dur(s.ToRunning),
		ToRegistered: dur(s.ToRegistered), ToFirstAction: dur(s.ToFirstAction), Path: s.Path,
	}
}
