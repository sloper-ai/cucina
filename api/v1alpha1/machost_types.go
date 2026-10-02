// SPDX-License-Identifier: FSL-1.1-ALv2

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MacHost phases (status.phase).
const (
	MacHostPending  = "Pending"  // enrolled-but-not-approved, or pre-registered and not yet connected
	MacHostOnline   = "Online"   // hostd connected, schedulable
	MacHostOffline  = "Offline"  // heartbeat lost: unavailable, in-flight actions retried elsewhere
	MacHostDraining = "Draining" // cordoned, VMs shutting down
	MacHostCordoned = "Cordoned" // drained and idle (maintenance)
	MacHostDenied   = "Denied"   // enrollment refused/revoked
)

// MacHost is one always-on Apple-silicon Mac mini running cucina-hostd. Objects
// are created by `cucinactl hosts register <serial>…` (pre-registration),
// by `cucinactl hosts approve <serial>` (approval of a pending enrollment), or by
// the controller when an unknown serial enrolls with a valid site token (the new
// object stays Pending until approved, R-SEC-3).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=mh,categories=cucina
// +kubebuilder:printcolumn:name="Serial",type=string,JSONPath=`.spec.serial`
// +kubebuilder:printcolumn:name="Site",type=string,JSONPath=`.spec.site`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="VMs",type=integer,JSONPath=`.status.runningVMs`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.status.facts.agentVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MacHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MacHostSpec   `json:"spec"`
	Status MacHostStatus `json:"status,omitempty"`
}

// MacHostList is a list of MacHost.
//
// +kubebuilder:object:root=true
type MacHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MacHost `json:"items"`
}

// MacHostSpec is the operator-declared intent for a host.
//
// +kubebuilder:validation:XValidation:rule="self.serial == oldSelf.serial",message="serial is immutable"
type MacHostSpec struct {
	// Serial is the hardware serial number (the admission key, R-SEC-3).
	// +kubebuilder:validation:MinLength=6
	Serial string `json:"serial"`
	// Site groups hosts that share a LAN / site tier / image mirror.
	// +optional
	Site string `json:"site,omitempty"`
	// Labels select hosts for pools (TartSpec.HostSelector).
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// Approved admits the host to enrollment (set by `hosts register` / `hosts approve`).
	Approved bool `json:"approved,omitempty"`
	// Cordoned drains the host: VMs shut down, no new VMs (R-MAC-6).
	Cordoned bool `json:"cordoned,omitempty"`
	// Slots overrides the VM slots per host (1–2).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2
	// +optional
	Slots *int32 `json:"slots,omitempty"`
	// DesiredImages are Tart image references the host should hold (pre-pull, R-MAC-5).
	// +optional
	DesiredImages []string `json:"desiredImages,omitempty"`
}

// HostFacts are reported by hostd at every connect (R-MAC-5).
type HostFacts struct {
	Model        string `json:"model,omitempty"`
	Chip         string `json:"chip,omitempty"`
	Cores        int32  `json:"cores,omitempty"`
	MemoryGiB    int32  `json:"memoryGiB,omitempty"`
	DiskFreeGiB  int32  `json:"diskFreeGiB,omitempty"`
	MacOSVersion string `json:"macOSVersion,omitempty"`
	TartVersion  string `json:"tartVersion,omitempty"`
	AgentVersion string `json:"agentVersion,omitempty"`
	// ProtocolVersion of the hostd↔controller protocol (handshake, R-TEST-7).
	ProtocolVersion int32  `json:"protocolVersion,omitempty"`
	FileVault       string `json:"fileVault,omitempty"` // "off" expected
}

// HostVMStatus is one VM on a host.
type HostVMStatus struct {
	Name       string `json:"name"`
	Pool       string `json:"pool,omitempty"`
	State      string `json:"state"` // running | stopped | starting | stopping | failed
	Image      string `json:"image,omitempty"`
	Generation string `json:"generation,omitempty"`
	Registered bool   `json:"registered,omitempty"` // worker connected to the scheduler
}

// HostCacheStatus is the L2 cache's effectiveness (R-CACHE-4, R-DATA-7).
type HostCacheStatus struct {
	HitRatio         string `json:"hitRatio,omitempty"`
	WANBytesReceived int64  `json:"wanBytesReceived,omitempty"`
	WANBytesSent     int64  `json:"wanBytesSent,omitempty"`
	SizeGiB          int32  `json:"sizeGiB,omitempty"`
}

// MacHostStatus is the observed state; written only by the controller.
type MacHostStatus struct {
	Phase         string          `json:"phase,omitempty"`
	LastHeartbeat *metav1.Time    `json:"lastHeartbeat,omitempty"`
	Facts         HostFacts       `json:"facts,omitempty"`
	RunningVMs    int32           `json:"runningVMs"`
	VMs           []HostVMStatus  `json:"vms,omitempty"`
	Images        []string        `json:"images,omitempty"`
	L2            HostCacheStatus `json:"l2,omitempty"`
	// CertificateExpiry is when the host's mTLS certificate expires (renewed before expiry).
	CertificateExpiry *metav1.Time `json:"certificateExpiry,omitempty"`

	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
