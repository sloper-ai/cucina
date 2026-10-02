// SPDX-License-Identifier: FSL-1.1-ALv2

// Package ports declares the narrow, consumer-owned interfaces behind which
// every external dependency of Cucina sits (R-TEST-8a): Compute (EC2),
// VMRuntime (Tart), BuildQueue (Buildbarn BuildQueueState), IdentityProvider,
// SecretStore (Keychain), Exec, FS, Clock and Rand.
//
// Each port has exactly one maintained, stateful, fault-injectable fake in
// internal/fakes and one conformance suite in internal/ports/porttest that runs
// against the fake (integration tier) and against the real adapter (acceptance).
// When a real adapter fails the suite, fix the fake.
package ports

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
)

// ---------------------------------------------------------------- Clock, Rand

// Clock abstracts time so that reconcilers, state machines and the deterministic
// simulation never sleep (R-TEST-5.3). Production uses the system clock; tests use
// internal/fakes.Clock (manual advance) or testing/synctest.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once after d.
	After(d time.Duration) <-chan time.Time
	// Sleep blocks for d or until ctx is done.
	Sleep(ctx context.Context, d time.Duration) error
}

// Rand is a seeded source of randomness (jitter, ids). Simulation runs are
// reproducible from one seed.
type Rand interface {
	Float64() float64
	Int63n(n int64) int64
	// Token returns a random hex string of 2*n characters.
	Token(n int) string
}

// ------------------------------------------------------------------ Exec, FS

// Command describes a process to run.
type Command struct {
	Path string
	Args []string
	Env  []string // KEY=VALUE; nil = inherit minimal environment
	Dir  string
	// Stdin is optional input.
	Stdin []byte
	// RunAs, if non-nil, drops privileges to this user before exec (hostd).
	RunAs *RunAs
}

// RunAs identifies the user a command runs as (R-MAC-2 privilege drop).
type RunAs struct {
	User string
	UID  uint32
	GID  uint32
}

// ExecResult is the outcome of a finished command.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Process is a long-running child process.
type Process interface {
	PID() int
	// Wait blocks until the process exits.
	Wait(ctx context.Context) (ExecResult, error)
	// Signal sends a signal by name ("TERM", "INT", "KILL").
	Signal(sig string) error
}

// Exec runs external commands (tart, bb_storage, pmset, …).
type Exec interface {
	Run(ctx context.Context, c Command) (ExecResult, error)
	Start(ctx context.Context, c Command) (Process, error)
}

// FS is the minimal filesystem surface used by hostd and the worker agent.
type FS interface {
	ReadFile(path string) ([]byte, error)
	// WriteFileAtomic writes via temp file + rename with the given mode.
	WriteFileAtomic(path string, data []byte, mode uint32) error
	MkdirAll(path string, mode uint32) error
	Remove(path string) error
	RemoveAll(path string) error
	Rename(oldpath, newpath string) error
	Exists(path string) (bool, error)
	// DiskUsage returns used and free bytes of the volume holding path.
	DiskUsage(path string) (used, free uint64, err error)
	// ListDir returns entry names.
	ListDir(path string) ([]string, error)
}

// SecretStore is the macOS System keychain (or a file fallback) holding host
// identity keys. Values are opaque.
type SecretStore interface {
	Get(ctx context.Context, key string) ([]byte, error) // ErrNotFound if absent
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
}

// ---------------------------------------------------------------- Compute (EC2)

// Errors returned by Compute. Adapters wrap provider errors with these sentinels
// (errors.Is) so the controller's retry/backoff logic is provider-neutral.
var (
	ErrInsufficientCapacity = errors.New("insufficient capacity")                 // ICE / no capacity in this type+subnet
	ErrQuotaExceeded        = errors.New("quota exceeded")                        // vCPU/instance quota
	ErrThrottled            = errors.New("request throttled")                     // API rate limit; back off
	ErrImageNotFound        = errors.New("image not found")                       // AMI missing/unavailable
	ErrNotFound             = errors.New("not found")                             //
	ErrNotOwned             = errors.New("resource not owned by this controller") // tag check failed (never touch)
	ErrInvalid              = errors.New("invalid request")                       // permanent request error (not retryable)
)

// CapacityType selects on-demand or spot.
type CapacityType string

const (
	OnDemand CapacityType = "on-demand"
	Spot     CapacityType = "spot"
)

// VolumeSpec is an additional or root EBS volume. Every volume is created with
// DeleteOnTermination=true (R-POOL-1); there are no per-worker persistent volumes.
type VolumeSpec struct {
	DeviceName string
	SizeGiB    int
	Type       string // "gp3"
	IOPS       int
	Throughput int // MiB/s
	// InitializationRate is the per-launch EBS volume initialization rate in MiB/s (0 = off).
	InitializationRate int
}

// LaunchRequest asks for exactly one instance.
type LaunchRequest struct {
	Pool       domain.PoolName
	Generation string
	// Token is the idempotency token: a second Launch with the same Token returns the
	// instance created by the first (never a duplicate, R-SCALE-5).
	Token         string
	ImageID       string
	InstanceTypes []string // ordered preference
	SubnetIDs     []string // ordered preference (single AZ by default)
	CapacityType  CapacityType
	// FallbackOnDemand lets a spot request fall back to on-demand.
	FallbackOnDemand  bool
	SecurityGroupIDs  []string
	InstanceProfile   string
	AssociatePublicIP bool // only when no other egress path exists
	RootVolume        VolumeSpec
	ExtraVolumes      []VolumeSpec
	UserData          []byte
	// Tags are applied to the instance, its volumes and its ENIs (TagSpecifications).
	// They also surface to the instance through IMDS instance-metadata tags (R-POOL-3).
	Tags map[string]string
}

// InstanceState mirrors the EC2 instance state names we care about.
type InstanceState string

const (
	InstancePending      InstanceState = "pending"
	InstanceRunning      InstanceState = "running"
	InstanceShuttingDown InstanceState = "shutting-down"
	InstanceTerminated   InstanceState = "terminated"
	InstanceStopping     InstanceState = "stopping" // must never occur (R-POOL-2); reported as an invariant violation
	InstanceStopped      InstanceState = "stopped"  // must never occur
)

// Instance is a worker instance as seen by the cloud API.
type Instance struct {
	ID           string
	Pool         domain.PoolName
	Generation   string
	State        InstanceState
	Type         string
	SubnetID     string
	AZ           string
	PrivateIP    netip.Addr
	LaunchTime   time.Time
	CapacityType CapacityType
	ImageID      string
	Tags         map[string]string
	VolumeIDs    []string
	ENIIDs       []string
}

// InstanceFilter always carries the cluster tag; an empty filter is invalid
// (R-SCALE-5/6: Describe calls are always tag-filtered).
type InstanceFilter struct {
	Cluster string
	Pool    domain.PoolName // optional
	States  []InstanceState // optional; default pending,running,shutting-down
	IDs     []string        // optional; batched by the adapter
}

// OrphanKind distinguishes orphaned resource types.
type OrphanKind string

const (
	OrphanVolume OrphanKind = "volume"
	OrphanENI    OrphanKind = "eni"
)

// Orphan is a pool-tagged volume or ENI not attached to a live instance.
type Orphan struct {
	Kind OrphanKind
	ID   string
	Pool domain.PoolName
	Age  time.Duration
}

// ImageSelector resolves an AMI by ID or by tag set (newest matching, owned by the account).
type ImageSelector struct {
	ID   string
	Tags map[string]string
}

// Image is a resolved AMI.
type Image struct {
	ID          string
	Name        string
	Arch        string // "x86_64" | "arm64"
	CreatedAt   time.Time
	Version     string // cucina:image-version tag
	Generation  string // cucina:generation tag
	SnapshotIDs []string
	SizeGiB     int
	Platform    string // "windows" | "linux"
}

// FastLaunchOp is an EC2 Fast Launch operation (Windows pools, R-POOL-2).
type FastLaunchOp struct {
	Action           string // "enable" | "disable" | "describe"
	ImageID          string
	TargetCount      int // snapshots to keep pre-provisioned (= pool max)
	MaxParallel      int
	LaunchTemplateID string
}

// FastLaunchStatus is the Fast Launch state of an AMI.
type FastLaunchStatus struct {
	ImageID   string
	State     string // "enabling" | "enabled" | "disabling" | "disabled" | "failed"
	Snapshots int    // pre-provisioned snapshots currently available
}

// InstancePrice is the on-demand hourly price of an instance type in the region.
type InstancePrice struct {
	Type       string
	USDPerHour float64
	VCPU       int
	MemoryGiB  float64
	NVMeGiB    int
	Windows    bool // price includes the Windows license
}

// Compute abstracts EC2 for pools (R-POOL-2). The real adapter is
// internal/providers/ec2; the fake is internal/fakes.Compute.
//
// Contract highlights (verified by porttest.Compute):
//   - Launch is idempotent per Token and tags instance, volumes and ENIs.
//   - Launch walks InstanceTypes x SubnetIDs in order and returns the first success; if
//     every combination fails it returns ErrInsufficientCapacity (or ErrQuotaExceeded).
//   - Terminate refuses (ErrNotOwned) any instance lacking the controller's tags.
//   - Instances never enter stopped/stopping through this port.
type Compute interface {
	Launch(ctx context.Context, req LaunchRequest) (Instance, error)
	Describe(ctx context.Context, f InstanceFilter) ([]Instance, error)
	// Terminate terminates the instances; the map holds per-ID errors for failures.
	Terminate(ctx context.Context, cluster string, ids []string) (map[string]error, error)
	ListOrphans(ctx context.Context, cluster string) ([]Orphan, error)
	DeleteOrphans(ctx context.Context, cluster string, orphans []Orphan) (map[string]error, error)
	ResolveImage(ctx context.Context, sel ImageSelector) (Image, error)
	FastLaunch(ctx context.Context, op FastLaunchOp) (FastLaunchStatus, error)
	InstancePrices(ctx context.Context, types []string, windows bool) (map[string]InstancePrice, error)
}

// -------------------------------------------------------------- VMRuntime (Tart)

// VMState is the state of a Tart VM.
type TartState string

const (
	TartRunning   TartState = "running"
	TartStopped   TartState = "stopped"
	TartSuspended TartState = "suspended"
)

// TartVM is a VM known to Tart on a host.
type TartVM struct {
	Name            string
	Image           string // OCI reference it was cloned from
	State           TartState
	DiskGiB         int
	SizeOnDiskBytes uint64
	CPU             int
	MemoryMiB       int
}

// RunOptions configures `tart run` (R-MAC-3).
type RunOptions struct {
	NoGraphics   bool
	RootDiskOpts string // "caching=cached,sync=none"
	Suspendable  bool
	CPU          int
	MemoryMiB    int
	DiskGiB      int // grow on clone
}

// ImageInfo describes a locally cached OCI image.
type ImageInfo struct {
	Reference string
	SizeBytes uint64
	Pulled    time.Time
}

// RegistryCreds are short-lived credentials passed via TART_REGISTRY_* env vars (R-MAC-5).
type RegistryCreds struct {
	Host     string
	Username string
	Password string
}

// Errors returned by VMRuntime.
var (
	ErrVMNotFound = errors.New("vm not found")
	ErrVMLimit    = errors.New("at most 2 macOS VMs may run per host") // Apple licence limit (R-MAC-3)
	ErrVMExists   = errors.New("vm already exists")
	ErrDiskFull   = errors.New("disk full")
	ErrGuestAgent = errors.New("guest agent unavailable")
)

// VMRuntime abstracts Tart (hostd). The real adapter shells out to `tart`; the
// fake simulates clone/run/stop latency, the 2-VM cap, disk full and crashes.
type VMRuntime interface {
	List(ctx context.Context) ([]TartVM, error)
	Clone(ctx context.Context, image, name string, diskGiB int) error
	// Run starts the VM (non-blocking); ErrVMLimit if two VMs already run.
	Run(ctx context.Context, name string, opts RunOptions) error
	// Stop performs a graceful shutdown (tart stop --timeout) keeping the disk.
	Stop(ctx context.Context, name string, timeout time.Duration) error
	Delete(ctx context.Context, name string) error
	// IP waits up to wait for the VM's address.
	IP(ctx context.Context, name string, wait time.Duration) (netip.Addr, error)
	// GuestExec runs a command in the guest through the Tart Guest Agent (tart exec).
	GuestExec(ctx context.Context, name string, c Command) (ExecResult, error)
	Pull(ctx context.Context, image string, creds *RegistryCreds) error
	Images(ctx context.Context) ([]ImageInfo, error)
	Prune(ctx context.Context, spaceBudgetBytes uint64) error
}

// ----------------------------------------------------- BuildQueue (BuildQueueState)

// WorkerID is a Buildbarn worker ID: the labels from the worker configuration
// (pool, node, …) plus the automatic "thread" label.
type WorkerID map[string]string

// Worker is one runner thread as listed by the scheduler.
type Worker struct {
	ID        WorkerID
	Queue     domain.QueueKey
	Drained   bool
	Executing bool
	Operation string // operation name when executing
	Timeout   time.Time
}

// Drain is a drain pattern registered on a size class queue.
type Drain struct {
	Queue   domain.QueueKey
	Pattern WorkerID
	Created time.Time
}

// Operation is a (queued/executing/completed) operation.
type Operation struct {
	Name         string
	Queue        domain.QueueKey
	ActionDigest string // "hash-size"
	Stage        string // queued | executing | completed
	QueuedAt     time.Time
	TargetID     string
	InvocationID string
	Priority     int32
	Worker       WorkerID // when executing
}

// OperationFilter selects operations to list.
type OperationFilter struct {
	Queue      *domain.QueueKey
	Stage      string // optional
	PageSize   int
	StartAfter string
}

// KillFilter selects operations to kill.
type KillFilter struct {
	OperationName string
	// QueueWithoutWorkers kills every queued operation of a queue that has no workers
	// (R-RE-2: fail fast when a pool can't obtain capacity).
	QueueWithoutWorkers *domain.QueueKey
}

// ErrQueueUnknown is returned when a queue is not declared in the scheduler.
var ErrQueueUnknown = errors.New("queue not known to the scheduler")

// BuildQueue abstracts the scheduler's BuildQueueState gRPC API. It is the
// autoscaler's signal (polled every 1–2 s, R-SCALE-1) and the drain/kill control.
type BuildQueue interface {
	ListPlatformQueues(ctx context.Context) ([]domain.QueueObservation, error)
	ListWorkers(ctx context.Context, queue domain.QueueKey) ([]Worker, error)
	AddDrain(ctx context.Context, queue domain.QueueKey, pattern WorkerID) error
	RemoveDrain(ctx context.Context, queue domain.QueueKey, pattern WorkerID) error
	ListDrains(ctx context.Context, queue domain.QueueKey) ([]Drain, error)
	KillOperations(ctx context.Context, f KillFilter, code int32, message string) error
	ListOperations(ctx context.Context, f OperationFilter) ([]Operation, error)
	GetOperation(ctx context.Context, name string) (Operation, error)
}

// ----------------------------------------------------------- IdentityProvider

// Claims is the verified claim set of an external token (a map of JSON values).
type Claims map[string]any

// IdentityProvider verifies external OIDC/JWT tokens for the STS (R-AUTH-2).
// The real adapter wraps go-oidc (discovery + JWKS caching); fakes sign tokens
// for arbitrary issuers/claims (Google- and GitHub-shaped).
type IdentityProvider interface {
	// Verify checks signature, exact iss, aud and exp of rawToken for the issuer configuration
	// and returns the claims. Any failure is an error (fail closed).
	Verify(ctx context.Context, issuerURL string, audiences []string, rawToken string) (Claims, error)
}

// ------------------------------------------------------------ HostFleet (macOS)

// HostState is the controller-side view of one Mac host (from hostd's Hello/Heartbeat).
type HostState struct {
	Serial       string
	Name         string
	Site         string
	Labels       map[string]string
	Online       bool
	Approved     bool
	Cordoned     bool
	Slots        int // VMs per host (1–2)
	RunningVMs   int
	VMs          []domain.VM // Tart VMs on the host (State maps the hostd state)
	Images       []string    // cached Tart images
	LastSeen     time.Time
	AgentVersion string
}

// StartVMRequest asks a host to start (cloning if needed) one VM for a pool.
type StartVMRequest struct {
	Pool       domain.PoolName
	Generation string
	Image      string
	VMName     string
	CPU        int
	MemoryGiB  int
	DiskGiB    int
	MaxAge     time.Duration
}

// HostFleet is the controller's port to the Mac hosts connected through
// HostService.Connect (R-MAC-6). The real implementation lives in
// internal/hostlink (served over the hostd stream); the fake in internal/fakes
// simulates hosts, the 2-VM cap, offline hosts and command latency.
type HostFleet interface {
	// Hosts returns every known host (online or not).
	Hosts(ctx context.Context) ([]HostState, error)
	StartVM(ctx context.Context, hostSerial string, req StartVMRequest) error
	// StopVM performs `tart stop --timeout`, keeping the disk and therefore the L1 cache.
	StopVM(ctx context.Context, hostSerial, vmName string, timeout time.Duration, reason string) error
	DeleteVM(ctx context.Context, hostSerial, vmName string) error
	ReimageVM(ctx context.Context, hostSerial, vmName, image string) error
	PullImage(ctx context.Context, hostSerial, image string) error
	SetCordon(ctx context.Context, hostSerial string, cordoned bool) error
}
