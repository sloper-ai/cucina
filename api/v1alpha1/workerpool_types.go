// SPDX-License-Identifier: FSL-1.1-ALv2

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Finalizer placed on WorkerPool and MacHost so that uninstall drains and
// terminates controller-created instances and stops host VMs (R-OPS-3).
const Finalizer = "cucina.sloper.ai/fleet"

// Condition types reported on WorkerPool.status.conditions.
const (
	ConditionReady         = "Ready"         // pool can serve work (queue declared, image resolved, capacity possible)
	ConditionQueueDeclared = "QueueDeclared" // the scheduler has predeclared queues for every runner (R-RE-2)
	ConditionImageResolved = "ImageResolved" // AMI / Tart image exists
	ConditionCapacity      = "CapacityAvailable"
	ConditionDegraded      = "Degraded" // start failures, ICE persisting beyond the window, orphans
)

// Reasons used with the conditions above.
const (
	ReasonQueueNotDeclared = "QueueNotDeclared" // add the platform to values.pools and `helm upgrade`
	ReasonImageMissing     = "ImageMissing"
	ReasonNoCapacity       = "NoCapacity"
	ReasonQuotaExceeded    = "QuotaExceeded"
	ReasonStartupFailures  = "StartupFailures"
	ReasonPaused           = "Paused"
)

// WorkerPool is a set of identical workers for one platform: OS + ISA + toolchain
// image, one size class, one provider. Pools can be created from Helm values
// (the chart renders WorkerPool objects) or directly; changes apply without
// restarting the control plane, except that a new platform/size-class queue needs
// a scheduler config update (`helm upgrade`), see ConditionQueueDeclared.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=wp,categories=cucina
// +kubebuilder:printcolumn:name="Platform",type=string,JSONPath=`.spec.platform`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.capacity.max`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desired`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.registered`
// +kubebuilder:printcolumn:name="Generation",type=string,JSONPath=`.status.imageGeneration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type WorkerPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkerPoolSpec   `json:"spec"`
	Status WorkerPoolStatus `json:"status,omitempty"`
}

// WorkerPoolList is a list of WorkerPool.
//
// +kubebuilder:object:root=true
type WorkerPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkerPool `json:"items"`
}

// WorkerPoolSpec declares one pool.
//
// +kubebuilder:validation:XValidation:rule="self.provider != 'ec2' || has(self.ec2)",message="spec.ec2 is required for provider ec2"
// +kubebuilder:validation:XValidation:rule="self.provider != 'tart' || has(self.tart)",message="spec.tart is required for provider tart"
// +kubebuilder:validation:XValidation:rule="self.provider != 'ec2' || !has(self.tart)",message="spec.tart is only valid for provider tart"
// +kubebuilder:validation:XValidation:rule="self.provider != 'tart' || !has(self.ec2)",message="spec.ec2 is only valid for provider ec2"
// +kubebuilder:validation:XValidation:rule="self.capacity.minRunning <= self.capacity.max",message="capacity.minRunning must not exceed capacity.max"
// +kubebuilder:validation:XValidation:rule="self.platform == oldSelf.platform && self.provider == oldSelf.provider && self.sizeClass == oldSelf.sizeClass",message="platform, provider and sizeClass are immutable",optionalOldSelf=true
type WorkerPoolSpec struct {
	// Platform names an entry of platforms/pools.json (shipped in the controller and the
	// chart), e.g. "linux-x86-64", "linux-aarch64", "windows-x86-64", "macos-arm64-xcode27.0".
	// The entry supplies the runners and their exact REAPI platform properties.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Platform string `json:"platform"`

	// SizeClass names a size class of the platform (pools differing only in machine
	// size are size classes of one platform). Each platform in pools.json declares
	// its size classes; "default" always exists.
	// +kubebuilder:default=default
	SizeClass string `json:"sizeClass,omitempty"`

	// Provider is "ec2" or "tart" and must match the platform's provider.
	// +kubebuilder:validation:Enum=ec2;tart
	Provider string `json:"provider"`

	// InstanceNames lists the Buildbarn instance names (tenants) this pool serves.
	// Empty means every configured instance name.
	// +optional
	InstanceNames []string `json:"instanceNames,omitempty"`

	// Capacity bounds the pool. minRunning > 0 means standing cost and is opt-in.
	Capacity CapacitySpec `json:"capacity"`

	// Timers default per platform: idle 5 m (EC2 Linux) / 10 m (EC2 Windows, macOS);
	// startup 5 m (Linux, macOS) / 15 m (Windows); drain 30 m.
	// +optional
	Timers TimersSpec `json:"timers,omitempty"`

	// Worker tunes the bb_worker/bb_runner configuration.
	// +optional
	Worker WorkerSpec `json:"worker,omitempty"`

	// Image selects the AMI or Tart image. Changing it starts a new pool generation (R-OPS-2).
	Image ImageSpec `json:"image"`

	// EC2 settings (provider ec2 only).
	// +optional
	EC2 *EC2Spec `json:"ec2,omitempty"`

	// Tart settings (provider tart only).
	// +optional
	Tart *TartSpec `json:"tart,omitempty"`

	// Rollout controls replacement of old-generation workers: "lazy" (default) replaces
	// them when idle, "eager" drains and replaces them immediately.
	// +kubebuilder:validation:Enum=lazy;eager
	// +kubebuilder:default=lazy
	Rollout string `json:"rollout,omitempty"`

	// Paused cordons the pool: no new launches; running workers finish and idle out.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// FloorSchedule raises minRunning during windows (opt-in standing cost, R-SCALE-7).
	// +optional
	FloorSchedule []FloorWindow `json:"floorSchedule,omitempty"`

	// DailyInstanceHourCap stops launching new instances once the pool consumed this
	// many instance-hours since 00:00 UTC (R-SCALE-7). Unset = no cap.
	// +optional
	DailyInstanceHourCap *int32 `json:"dailyInstanceHourCap,omitempty"`
}

// CapacitySpec bounds a pool.
type CapacitySpec struct {
	// MinRunning workers kept alive even when idle. Default 0 (zero idle cost).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MinRunning int32 `json:"minRunning"`
	// Max is the maximum number of VMs. 0 disables the pool: queued work is failed fast (R-RE-2).
	// +kubebuilder:validation:Minimum=0
	Max int32 `json:"max"`
}

// TimersSpec holds lifecycle timers.
type TimersSpec struct {
	// IdleTimeout: a VM idle this long (with an empty queue) is drained and stopped (R-SCALE-3).
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`
	// DrainTimeout: after this long a draining VM is stopped even if actions still run.
	// +optional
	DrainTimeout *metav1.Duration `json:"drainTimeout,omitempty"`
	// StartupTimeout: a VM that has not registered by then is failed and replaced.
	// +optional
	StartupTimeout *metav1.Duration `json:"startupTimeout,omitempty"`
}

// WorkerSpec tunes the worker.
type WorkerSpec struct {
	// Concurrency is the number of execution slots of the pool's primary runner; default = vCPUs.
	// +optional
	Concurrency *int32 `json:"concurrency,omitempty"`
	// RunnerConcurrency overrides slots per runner name (e.g. "qemu-s390x": 2). Emulated
	// runners default to a low concurrency documented in docs/sizing.md.
	// +optional
	RunnerConcurrency map[string]int32 `json:"runnerConcurrency,omitempty"`
	// L1 places the worker-local cache (R-CACHE-2).
	// +optional
	L1 L1Spec `json:"l1,omitempty"`
	// BuildDirectory selects the build directory mode (R-CACHE-3); auto picks FUSE (Linux),
	// WinFSP (Windows) and the measured choice on macOS.
	// +kubebuilder:validation:Enum=auto;fuse;winfsp;nfsv4;native
	// +kubebuilder:default=auto
	BuildDirectory string `json:"buildDirectory,omitempty"`
}

// L1Spec places the worker-local cache.
type L1Spec struct {
	// Placement: auto prefers instance-store NVMe, then an ephemeral gp3 volume, then memory
	// (EC2); the VM disk on macOS. "vm-disk" is Tart only.
	// +kubebuilder:validation:Enum=auto;instance-store;ebs;memory;vm-disk
	// +kubebuilder:default=auto
	Placement string `json:"placement,omitempty"`
	// SizeGiB of the L1 CAS; default 40 on macOS, derived from the instance elsewhere.
	// +optional
	SizeGiB *int32 `json:"sizeGiB,omitempty"`
}

// ImageSpec selects the image a pool launches.
type ImageSpec struct {
	// AMI is an explicit AMI ID (EC2).
	// +optional
	AMI string `json:"ami,omitempty"`
	// AMISelector picks the newest AMI owned by the account whose tags match (EC2).
	// +optional
	AMISelector map[string]string `json:"amiSelector,omitempty"`
	// Reference is the OCI image reference of the Tart image (Tart), e.g.
	// ghcr.io/sloper-ai/cucina-worker-macos:27.0-0.1.0.
	// +optional
	Reference string `json:"reference,omitempty"`
	// Version is the image version label; a change starts a new generation (R-POOL-8).
	// Defaults to the resolved image's cucina:image-version tag.
	// +optional
	Version string `json:"version,omitempty"`
}

// EC2Spec holds EC2 provider settings.
type EC2Spec struct {
	// InstanceTypes is the ordered preference list, e.g. [c8i.8xlarge, c7i.8xlarge, c7a.8xlarge].
	// +kubebuilder:validation:MinItems=1
	InstanceTypes []string `json:"instanceTypes"`
	// CapacityType: on-demand (default) or spot (SHOULD; falls back to on-demand when spotFallback).
	// +kubebuilder:validation:Enum=on-demand;spot
	// +kubebuilder:default=on-demand
	CapacityType string `json:"capacityType,omitempty"`
	// SpotFallback allows on-demand launches when spot capacity is unavailable.
	// +optional
	SpotFallback bool `json:"spotFallback,omitempty"`
	// SubnetIDs in preference order; a single AZ by default (R-DATA-4).
	// +kubebuilder:validation:MinItems=1
	SubnetIDs []string `json:"subnetIDs"`
	// SecurityGroupIDs for the instances.
	// +kubebuilder:validation:MinItems=1
	SecurityGroupIDs []string `json:"securityGroupIDs"`
	// InstanceProfile (name) with SSM core and own-pool parameter read, no EC2 permissions.
	InstanceProfile string `json:"instanceProfile"`
	// AssociatePublicIP gives workers a public IPv4 — only when no other egress path exists.
	// +optional
	AssociatePublicIP bool `json:"associatePublicIP,omitempty"`
	// RootVolume: smallest that fits; gp3 baseline.
	// +optional
	RootVolume VolumeSpec `json:"rootVolume,omitempty"`
	// DataVolume is an extra ephemeral gp3 volume for L1/filePool when the type has no
	// instance store (DeleteOnTermination always true).
	// +optional
	DataVolume *VolumeSpec `json:"dataVolume,omitempty"`
	// FastLaunch applies to Windows pools: pre-provisioned snapshots (user-approved standing cost).
	// +optional
	FastLaunch *FastLaunchSpec `json:"fastLaunch,omitempty"`
	// ExtraTags are added to every resource (e.g. the campaign tags).
	// +optional
	ExtraTags map[string]string `json:"extraTags,omitempty"`
}

// VolumeSpec describes an EBS volume. DeleteOnTermination is always true.
type VolumeSpec struct {
	// +kubebuilder:validation:Minimum=1
	SizeGiB int32 `json:"sizeGiB,omitempty"`
	// +kubebuilder:default=gp3
	Type string `json:"type,omitempty"`
	// +optional
	IOPS int32 `json:"iops,omitempty"`
	// +optional
	ThroughputMiBps int32 `json:"throughputMiBps,omitempty"`
	// InitializationRateMiBps is the per-launch volume initialization rate (0 = off, R-POOL-2 SHOULD).
	// +optional
	InitializationRateMiBps int32 `json:"initializationRateMiBps,omitempty"`
}

// FastLaunchSpec configures EC2 Fast Launch for a Windows pool.
type FastLaunchSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`
	// TargetCount of pre-provisioned snapshots; default = capacity.max.
	// +optional
	TargetCount *int32 `json:"targetCount,omitempty"`
	// MaxParallelLaunches during snapshot preparation (within AWS limits).
	// +optional
	MaxParallelLaunches *int32 `json:"maxParallelLaunches,omitempty"`
	// LaunchTemplateID of the small, cheap prep instance type; created by the controller when empty.
	// +optional
	LaunchTemplateID string `json:"launchTemplateID,omitempty"`
}

// TartSpec holds Tart provider settings.
type TartSpec struct {
	// CPU and memory override the host-derived defaults ((cores-2)/slots, (RAM-8GB)/slots).
	// +optional
	CPU *int32 `json:"cpu,omitempty"`
	// +optional
	MemoryGiB *int32 `json:"memoryGiB,omitempty"`
	// +optional
	DiskGiB *int32 `json:"diskGiB,omitempty"`
	// HostSelector matches MacHost labels.
	// +optional
	HostSelector map[string]string `json:"hostSelector,omitempty"`
	// VMsPerHost is 1 or 2 (Apple licence limit).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2
	// +kubebuilder:default=2
	VMsPerHost int32 `json:"vmsPerHost,omitempty"`
	// MaxAge: VMs older than this are re-cloned from the golden image (default 168h).
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
	// Suspendable evaluates `tart suspend` as the "off" state (R-MAC-3 SHOULD).
	// +optional
	Suspendable bool `json:"suspendable,omitempty"`
}

// FloorWindow raises the pool floor during a recurring window (UTC cron-like).
type FloorWindow struct {
	// Name of the window (for status/audit).
	Name string `json:"name"`
	// Days are weekday names ("Mon"…"Sun").
	Days []string `json:"days"`
	// Start and End are "HH:MM" in UTC.
	Start string `json:"start"`
	End   string `json:"end"`
	// MinRunning during the window.
	// +kubebuilder:validation:Minimum=1
	MinRunning int32 `json:"minRunning"`
}

// WorkerPoolStatus is the observed state; written only by the controller.
type WorkerPoolStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Desired VMs from the last autoscaler decision.
	Desired int32 `json:"desired"`
	// Launching VMs (booting, not yet registered).
	Launching int32 `json:"launching"`
	// Registered VMs (at least one runner thread connected).
	Registered int32 `json:"registered"`
	// Busy VMs (running at least one operation) and Idle VMs.
	Busy int32 `json:"busy"`
	Idle int32 `json:"idle"`
	// Draining VMs.
	Draining int32 `json:"draining"`
	// Stopped VMs (Tart only; disk kept).
	Stopped int32 `json:"stopped,omitempty"`

	// ImageGeneration is the active generation label and ResolvedImage the AMI ID / Tart reference.
	ImageGeneration string `json:"imageGeneration,omitempty"`
	ResolvedImage   string `json:"resolvedImage,omitempty"`
	// OldGenerationVMs are VMs still running a previous generation.
	OldGenerationVMs int32 `json:"oldGenerationVMs,omitempty"`

	// QueueDeclared reports that the scheduler knows the pool's queues.
	QueueDeclared bool `json:"queueDeclared,omitempty"`

	// LastScaleTime is when the controller last launched or stopped a VM.
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`
	// LastCapacityFailure records the last ICE/quota/missing-image error and when it started.
	LastCapacityFailure *CapacityFailure `json:"lastCapacityFailure,omitempty"`

	// InstanceSecondsToday and EstimatedCostTodayUSD feed `cucinactl cost` (R-OBS-5).
	InstanceSecondsToday  int64  `json:"instanceSecondsToday,omitempty"`
	EstimatedCostTodayUSD string `json:"estimatedCostTodayUSD,omitempty"`

	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CapacityFailure describes persistent capacity trouble.
type CapacityFailure struct {
	Reason  string      `json:"reason"`
	Message string      `json:"message,omitempty"`
	Since   metav1.Time `json:"since"`
}
