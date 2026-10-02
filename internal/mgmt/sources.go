// SPDX-License-Identifier: FSL-1.1-ALv2

package mgmt

import (
	"context"
	"io"
	"time"

	"github.com/sloper-ai/cucina/api/v1alpha1"
	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/ports"
)

// This file declares the consumer-owned interfaces through which the management
// API reads and changes the fleet. cucina-controller provides the adapters
// (docs/dev/mgmt.md). Read methods are called on every request and for every
// overview snapshot, so implementations must answer from controller memory
// (informer caches, reconciler state) and never call AWS synchronously.
//
// Errors: return ports.ErrNotFound / ports.ErrInvalid (wrapped is fine) or a gRPC
// status error to choose the status code; anything else becomes UNAVAILABLE.

// ---------------------------------------------------------------- Pools

// Pool is one WorkerPool as the controller sees it.
type Pool struct {
	// Resource is the WorkerPool object (spec and status) from the informer cache.
	Resource v1alpha1.WorkerPool
	// Spec is the resolved, provider-neutral spec (catalog + resource + defaults).
	Spec domain.PoolSpec
	// Floor is the active temporary floor override (SetPoolFloor), if any.
	Floor *FloorOverride
}

// Name returns the pool name.
func (p Pool) Name() string {
	if p.Resource.Name != "" {
		return p.Resource.Name
	}
	return string(p.Spec.Name)
}

// FloorOverride is a temporary minRunning override. It is standing cost, so it
// always expires (R-SCALE-7, R-OBS-5).
type FloorOverride struct {
	MinRunning int32
	ExpiresAt  time.Time
	SetBy      string // principal subject
}

// PoolEvent is one entry of a pool's scale timeline.
type PoolEvent struct {
	Time    time.Time
	Type    string // launch | register | drain-acknowledged | terminate | fail | ice | scale | rollout; legacy drain is intent only
	Subject string // VM id ("node" label value)
	Message string
}

// StartLatency is one VM start: API call → running → registered → first action.
type StartLatency struct {
	Pool          string
	VM            string
	Launched      time.Time
	ToRunning     time.Duration
	ToRegistered  time.Duration
	ToFirstAction time.Duration
	Path          string // "fast-launch" | "slow" | "tart"
}

// PoolView reads pools from controller memory.
type PoolView interface {
	// Pools returns every WorkerPool, sorted by name.
	Pools(ctx context.Context) ([]Pool, error)
	// History returns a pool's most recent scale events and VM starts, newest first,
	// at most limit of each. pool "" means every pool.
	History(ctx context.Context, pool string, limit int) ([]PoolEvent, []StartLatency, error)
}

// PoolAdmin changes pools.
type PoolAdmin interface {
	// SetFloor stores a temporary floor override (MinRunning 0 clears it). The
	// reconciler treats it as an active floor window until ExpiresAt.
	SetFloor(ctx context.Context, pool string, floor FloorOverride) error
	// SetPaused cordons (true) or uncordons (false) the pool (spec.paused): no new
	// launches; running workers finish and idle out.
	SetPaused(ctx context.Context, pool string, paused bool) error
}

// OrphanSweeper lists and deletes orphaned, pool-tagged volumes and ENIs. It is the
// subset of ports.Compute that GarbageCollectPool uses (ports.Compute satisfies it).
type OrphanSweeper interface {
	ListOrphans(ctx context.Context, cluster string) ([]ports.Orphan, error)
	DeleteOrphans(ctx context.Context, cluster string, orphans []ports.Orphan) (map[string]error, error)
}

// ---------------------------------------------------------------- Workers

// Worker is one worker VM tracked by the controller.
type Worker struct {
	domain.VM
	PrivateIP string
}

// WorkerView lists worker VMs from controller memory.
type WorkerView interface {
	// Workers returns every worker VM of every pool, including stopped Tart VMs.
	Workers(ctx context.Context) ([]Worker, error)
}

// Scheduler is the subset of the scheduler's BuildQueueState API the management API
// uses. ports.BuildQueue satisfies it, so the real adapter is internal/buildqueue.
type Scheduler interface {
	ListPlatformQueues(ctx context.Context) ([]domain.QueueObservation, error)
	AddDrain(ctx context.Context, queue domain.QueueKey, pattern ports.WorkerID) error
	RemoveDrain(ctx context.Context, queue domain.QueueKey, pattern ports.WorkerID) error
	KillOperations(ctx context.Context, f ports.KillFilter, code int32, message string) error
	ListOperations(ctx context.Context, f ports.OperationFilter) ([]ports.Operation, error)
	GetOperation(ctx context.Context, name string) (ports.Operation, error)
}

// QueueStat holds per-queue timing the scheduler's queue listing does not carry.
type QueueStat struct {
	OldestQueuedAge time.Duration
	QueueTimeP95    time.Duration
}

// QueueStats is optional: per-queue timing from the controller's poll loop.
type QueueStats interface {
	QueueStats(ctx context.Context) (map[domain.QueueKey]QueueStat, error)
}

// InstanceShell runs a short, read-only diagnostic script on an EC2 worker through
// SSM Run Command (SendCommand with AWS-RunShellScript, or AWS-RunPowerShellScript
// when windows is true, then GetCommandInvocation until it finishes) and returns
// its standard output. SSM returns at most 24,000 characters of output; the scripts
// built by this package stay below that. Implementations must refuse instances
// that do not carry this cluster's tags.
type InstanceShell interface {
	RunScript(ctx context.Context, instanceID string, windows bool, script string) ([]byte, error)
}

// ---------------------------------------------------------------- Hosts

// HostView lists MacHost objects (spec and status) from the informer cache.
type HostView interface {
	Hosts(ctx context.Context) ([]v1alpha1.MacHost, error)
}

// DiagnosticsRequest selects what a host's diagnostics stream contains.
type DiagnosticsRequest struct {
	IncludeVMLogs bool
	// VM, Unit and TailLines narrow the stream to one VM's worker log (plain text).
	// Hosts that do not support narrowing return the full diagnostics.
	VM        string
	Unit      string // bb-worker | bb-runner | agent
	TailLines int
}

// HostAdmin sends maintenance commands to Mac hosts (R-MAC-6).
type HostAdmin interface {
	// SetCordon drains (true: VMs shut down when idle, no new VMs) or uncordons a host.
	SetCordon(ctx context.Context, serial string, cordoned bool) error
	// Reimage re-clones a VM ("" = every VM of the host) from its pool's golden image.
	Reimage(ctx context.Context, serial, vm string) error
	// Diagnostics collects host diagnostics. Hosts redact secrets (enrollment token,
	// keychain items) at the source; the stream is passed through unchanged.
	Diagnostics(ctx context.Context, serial string, req DiagnosticsRequest) (io.ReadCloser, error)
}

// EnrollTokenRequest asks for a new site enrollment token (R-SEC-3).
type EnrollTokenRequest struct {
	Site        string
	TTL         time.Duration
	MaxHosts    int
	Description string
	CreatedBy   string
}

// EnrollToken is a site enrollment token's metadata (never its secret).
type EnrollToken struct {
	ID          string
	Site        string
	Description string
	Created     time.Time
	ExpiresAt   time.Time
	MaxHosts    int
	UsedHosts   int
	Revoked     bool
}

// Enrollment manages site enrollment tokens and the serial-number allowlist
// (internal/enroll).
type Enrollment interface {
	// CreateEnrollToken returns the token metadata and its secret, which is shown
	// exactly once; only a hash is stored.
	CreateEnrollToken(ctx context.Context, req EnrollTokenRequest) (EnrollToken, string, error)
	ListEnrollTokens(ctx context.Context) ([]EnrollToken, error)
	RevokeEnrollToken(ctx context.Context, id string) error
	// RegisterSerials pre-registers (and approves) serial numbers.
	RegisterSerials(ctx context.Context, serials []string, site string, labels map[string]string) (registered, alreadyPresent []string, err error)
	ApproveHost(ctx context.Context, serial string) error
	RemoveHost(ctx context.Context, serial string) error
}

// ---------------------------------------------------------------- Identity

// ServiceKeyRequest asks for a new service-account key (R-AUTH-10).
type ServiceKeyRequest struct {
	Account     string
	Description string
	TTL         time.Duration // 0 = no expiry
	CreatedBy   string
}

// ServiceKey is a service-account key's metadata (never the key).
type ServiceKey struct {
	ID          string
	Account     string
	Description string
	Created     time.Time
	ExpiresAt   time.Time
	LastUsed    time.Time
	Revoked     bool
}

// KeyAdmin manages service-account keys (internal/keys).
type KeyAdmin interface {
	// CreateServiceKey returns the key metadata and the key, which is shown exactly
	// once; only a hash is stored.
	CreateServiceKey(ctx context.Context, req ServiceKeyRequest) (ServiceKey, string, error)
	ListServiceKeys(ctx context.Context, account string) ([]ServiceKey, error)
	RevokeServiceKey(ctx context.Context, id string) error
}

// Revocation is one deny-list entry (R-AUTH-9).
type Revocation struct {
	Subject   string
	SessionID string
	Reason    string
	Created   time.Time
	CreatedBy string
}

// RevocationAdmin manages the deny-list read by Buildbarn's authorizers and by the
// management API's own token verification (internal/keys).
type RevocationAdmin interface {
	// Revoke adds the entry and returns when every frontend enforces it (zero: the
	// server reports now + Options.RevocationPropagation).
	Revoke(ctx context.Context, r Revocation) (time.Time, error)
	ListRevocations(ctx context.Context) ([]Revocation, error)
}

// ---------------------------------------------------------------- Cost, images, status

// Micros is an amount in micro-dollars (USD).
type Micros int64

// CostQuery selects a cost report.
type CostQuery struct {
	Pool  string    // "" = every pool
	Since time.Time // zero = start of the current UTC day
}

// PoolCost is one pool's spend.
type PoolCost struct {
	Pool            string
	InstanceSeconds int64
	Compute         Micros
	EBS             Micros
	DataTransfer    Micros
	Standing        Micros
}

// CostLine is one itemised cost.
type CostLine struct {
	Pool     string
	Category string // compute | ebs | public-ipv4 | ami-storage | fast-launch | data-transfer
	Detail   string
	Quantity float64
	Unit     string
	Amount   Micros
}

// CostReport is the controller's cost model output (R-OBS-5, UC17).
type CostReport struct {
	Today            Micros
	MonthToDate      Micros
	StandingPerMonth Micros
	Pools            []PoolCost
	Lines            []CostLine
	Assumptions      string
}

// CostSource prices recorded usage (internal/cost).
type CostSource interface {
	Cost(ctx context.Context, q CostQuery) (CostReport, error)
}

// Image is one worker image version of a pool (R-OPS-2).
type Image struct {
	Pool       string
	Reference  string // AMI ID or Tart reference
	Version    string
	Generation string
	Current    bool
	Previous   bool // kept for rollback
	Created    time.Time
	FastLaunch string // enabled/disabled/n-a (+ snapshot count)
}

// ImageSource lists the current and previous images of every pool.
type ImageSource interface {
	Images(ctx context.Context) ([]Image, error)
}

// Component is the readiness of one control-plane component.
type Component struct {
	Name    string // frontend | storage | scheduler | controller | sts
	State   string // ready | degraded | down
	Message string
	Ready   int32
	Desired int32
}

// ComponentSource reports control-plane readiness (Deployments/StatefulSets).
type ComponentSource interface {
	Components(ctx context.Context) ([]Component, error)
}

// Alert is a firing (or recently fired) alert.
type Alert struct {
	Name     string
	Severity string // info | warning | critical
	Summary  string
	Since    time.Time
	Labels   map[string]string
}

// AlertSource reports alerts.
type AlertSource interface {
	Alerts(ctx context.Context) ([]Alert, error)
}

// ---------------------------------------------------------------- Support bundle

// SupportSource supplies support-bundle content the views above do not cover.
// Everything it returns is redacted before it enters the bundle.
type SupportSource interface {
	TrustPolicies(ctx context.Context) ([]v1alpha1.TrustPolicy, error)
	// Metrics returns the controller's Prometheus text exposition.
	Metrics(ctx context.Context) ([]byte, error)
}

// LogTail returns the most recent lines of the controller's own log (LogRing).
type LogTail interface {
	Tail(lines int) []byte
}
