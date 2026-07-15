package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZfsPoolHealth is the observed availability of a ZFS pool. Values ONLINE,
// DEGRADED, FAULTED and SUSPENDED are reported by the per-node discovery
// DaemonSet from local `zpool` state. NODE_OFFLINE is set exclusively by the
// central watcher when the storage node itself goes away and can no longer
// self-report, preventing stale ONLINE claims at a dead IP.
// +kubebuilder:validation:Enum=ONLINE;DEGRADED;FAULTED;SUSPENDED;NODE_OFFLINE;UNKNOWN
type ZfsPoolHealth string

const (
	// PoolHealthOnline means the pool is healthy and serving I/O.
	PoolHealthOnline ZfsPoolHealth = "ONLINE"
	// PoolHealthDegraded means a device failed but the pool still serves I/O.
	PoolHealthDegraded ZfsPoolHealth = "DEGRADED"
	// PoolHealthFaulted means too many devices failed and the pool is locked.
	PoolHealthFaulted ZfsPoolHealth = "FAULTED"
	// PoolHealthSuspended means I/O is paused (HBA crash or detached cabling).
	PoolHealthSuspended ZfsPoolHealth = "SUSPENDED"
	// PoolHealthNodeOffline means the storage node is unreachable; set by the
	// central watcher, never by the (dead) node itself.
	PoolHealthNodeOffline ZfsPoolHealth = "NODE_OFFLINE"
	// PoolHealthUnknown means the pool state could not be determined.
	PoolHealthUnknown ZfsPoolHealth = "UNKNOWN"
)

// ZfsPoolSpec declares the stable, human-facing identity of a ZFS pool. The
// object's metadata.name is the immutable ZFS pool GUID (e.g.
// "zpool-12140134988506841113") so the same physical pool maps to one CRD no
// matter which node currently imports it or how it is renamed.
type ZfsPoolSpec struct {
	// PoolName is the human-readable ZFS pool name (e.g. "tank"). It may be
	// renamed on the host without breaking routing, since consumers key off the
	// immutable GUID (metadata.name) and status.baseMountPath instead.
	// +kubebuilder:validation:MinLength=1
	PoolName string `json:"poolName"`
}

// ZfsPoolStatus holds the dynamic routing and health data. It is written by two
// independent components: the per-node discovery DaemonSet (Tier 1) publishes
// live pool state, and the central watcher (Tier 2) overrides health with
// NODE_OFFLINE when the owning node dies.
type ZfsPoolStatus struct {
	// GUID is the immutable ZFS pool GUID (without the "zpool-" name prefix).
	// +optional
	GUID string `json:"guid,omitempty"`

	// CurrentNode is the node that most recently reported importing this pool.
	// It is kept for historical reference even after the node goes offline.
	// +optional
	CurrentNode string `json:"currentNode,omitempty"`

	// CurrentIP is the routable address of CurrentNode used by CSI clients to
	// reach the network share. Kept for reference after a node goes offline.
	// +optional
	CurrentIP string `json:"currentIP,omitempty"`

	// BaseMountPath is the pool's current ZFS mountpoint on the host, e.g.
	// "/mnt/watertank". CSI node plugins join this with the logical dataset name
	// to build the real export path, so pool renames and alternate import roots
	// never break PersistentVolumes.
	// +optional
	BaseMountPath string `json:"baseMountPath,omitempty"`

	// Health is the current pool availability.
	// +optional
	Health ZfsPoolHealth `json:"health,omitempty"`

	// LastUpdated is when the status was last written. Consumers can use it as a
	// freshness signal for stale-reporter detection.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Message carries human-readable detail about the current health.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=zpool
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolName`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.currentNode`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.currentIP`
// +kubebuilder:printcolumn:name="MountPath",type=string,JSONPath=`.status.baseMountPath`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZfsPool is the Schema for the zfspools API. It is a globally-unique, node-
// agnostic handle to a physical ZFS pool: the name is the immutable pool GUID,
// while the status tracks which node currently serves it, at which IP and mount
// path, and how healthy it is.
type ZfsPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZfsPoolSpec   `json:"spec,omitempty"`
	Status ZfsPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZfsPoolList contains a list of ZfsPool.
type ZfsPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZfsPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZfsPool{}, &ZfsPoolList{})
}
