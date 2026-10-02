// SPDX-License-Identifier: FSL-1.1-ALv2

package reconcile

import (
	"time"

	"github.com/sloper-ai/cucina/internal/domain"
	"github.com/sloper-ai/cucina/internal/pools"
	"github.com/sloper-ai/cucina/internal/ports"
	"github.com/sloper-ai/cucina/internal/scaling"
)

// PoolRuntime is everything one autoscaler loop needs. The WorkerPool
// reconciler rebuilds it on every reconcile and hands it to the Fleet, which
// swaps it in atomically between two decisions.
type PoolRuntime struct {
	Spec scaling.Spec
	// Resolved is nil for a pool that no longer resolves but must still be
	// retired (deleted while its platform is gone from the catalog).
	Resolved *pools.Resolved
	// Queues are the pool's size class queues (instance names x runners x size class).
	Queues []domain.QueueKey
	// ImageErr is the image resolution error (nil when resolved).
	ImageErr error
	// ImageRef is the AMI ID or Tart image reference; ImageVersion its version label.
	ImageRef     string
	ImageVersion string
	// ImageSizeGiB is the AMI's snapshot size (cost: AMI storage is standing cost).
	ImageSizeGiB int

	EC2  *EC2Launch
	Tart *TartLaunch

	// Ledger is the persisted launch ledger read from the WorkerPool; used only
	// when the loop is created (afterwards the loop owns the ledger).
	Ledger scaling.Ledger
	// InstanceSecondsToday restores daily accounting from the status after a restart.
	InstanceSecondsToday float64
}

// EC2Launch holds the launch parameters of an EC2 pool that are not decided by
// the planner (it picks token, generation, instance types and subnets).
type EC2Launch struct {
	ImageID           string
	CapacityType      ports.CapacityType
	FallbackOnDemand  bool
	SecurityGroupIDs  []string
	InstanceProfile   string
	AssociatePublicIP bool
	RootVolume        ports.VolumeSpec
	ExtraVolumes      []ports.VolumeSpec
	// Tags are the operator's extra tags (config.aws.extraTags + spec.ec2.extraTags);
	// the controller's own cucina:* tags always win.
	Tags    map[string]string
	Windows bool
}

// TartLaunch holds the parameters of a Tart pool's VMs.
type TartLaunch struct {
	Image        string
	CPU          int
	MemoryGiB    int
	DiskGiB      int
	HostSelector map[string]string
	VMsPerHost   int
	MaxAge       time.Duration
	// Hosts is the operator's intent per host serial from the MacHost objects
	// (approval, labels, cordon, slots). When non-nil it is authoritative: a
	// host without a MacHost is never used (R-SEC-3 admission).
	Hosts map[string]HostPolicy
}

// HostPolicy is the MacHost spec part that placement honours.
type HostPolicy struct {
	Approved bool
	Cordoned bool
	Labels   map[string]string
	Slots    int // 0 = the host's own
}

// Snapshot is the latest outcome of a pool's loop, read by the reconciler for
// the WorkerPool status and by the management API.
type Snapshot struct {
	At      time.Time
	Desired int
	Demand  scaling.Demand
	Hold    scaling.Reason
	// Counts are VMs by cucina_pool_vms state (launching, registered, busy, idle,
	// draining, stopped, failed); registered = busy + idle.
	Counts map[string]int
	VMs    []domain.VM
	Status scaling.Status
	// Headroom is the decision's thread headroom per runner platform key
	// (input of scaling.AttributeShared for shared queues).
	Headroom map[string]int
	// Deleting is true when the decision was computed for a retiring pool.
	Deleting bool
	// Observed is true once the loop has seen the provider at least once.
	Observed bool
	// QueuesKnown is false when the last ListPlatformQueues failed (scheduler
	// unreachable): the queue-related status is then unknown, not "declared".
	QueuesKnown bool
	// LastScale is when the loop last launched, terminated or stopped a VM.
	LastScale time.Time
	// LastError summarizes the last failed executor call ("" when healthy).
	LastError            string
	InstanceSecondsToday float64
	Generation           string
}
