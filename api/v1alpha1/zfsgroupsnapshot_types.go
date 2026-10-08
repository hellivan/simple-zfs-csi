package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZfsGroupSnapshotPhase is a high-level summary of the group snapshot state. It
// is derived from the member ZfsSnapshots, never from ZFS (ADR-0039).
type ZfsGroupSnapshotPhase string

const (
	// GroupSnapshotPhasePending means the group is not complete yet.
	GroupSnapshotPhasePending ZfsGroupSnapshotPhase = "Pending"
	// GroupSnapshotPhaseReady means every member snapshot is Ready.
	GroupSnapshotPhaseReady ZfsGroupSnapshotPhase = "Ready"
	// GroupSnapshotPhaseError means provisioning failed in a way that needs a human.
	GroupSnapshotPhaseError ZfsGroupSnapshotPhase = "Error"
	// GroupSnapshotPhaseLost means the group was Ready once but a member
	// ZfsSnapshot is gone or no longer Ready. Informational only: nothing is
	// ever re-created (ADR-0038).
	GroupSnapshotPhaseLost ZfsGroupSnapshotPhase = "Lost"
)

// ZfsGroupSnapshotMember is one source volume of a group, recorded before
// anything is touched (ADR-0039). It carries what the standalone ZfsSnapshot
// would record, plus the two names this member will use.
type ZfsGroupSnapshotMember struct {
	// SourceVolume is the source ZfsDataset's metadata.name (the CSI volume id).
	// +kubebuilder:validation:MinLength=1
	SourceVolume string `json:"sourceVolume"`

	// Dataset is the source dataset's logical path relative to the pool root.
	// +kubebuilder:validation:MinLength=1
	Dataset string `json:"dataset"`

	// SnapshotName is this member's raw ZFS snapshot short name
	// ("csi-snap-<uuid>"), unique per member.
	// +kubebuilder:validation:MinLength=1
	SnapshotName string `json:"snapshotName"`

	// ChildSnapshotName is the metadata.name of the member's ZfsSnapshot, which
	// is also its CSI snapshot_id.
	// +kubebuilder:validation:MinLength=1
	ChildSnapshotName string `json:"childSnapshotName"`

	// SourceType, SourceFSType, SourceVolblocksize and SourceProperties are copied
	// to the child ZfsSnapshot; see the fields of the same name there.
	// +optional
	SourceType DatasetType `json:"sourceType,omitempty"`
	// +optional
	SourceFSType string `json:"sourceFSType,omitempty"`
	// +optional
	SourceVolblocksize string `json:"sourceVolblocksize,omitempty"`
	// +optional
	SourceProperties map[string]string `json:"sourceProperties,omitempty"`
}

// ZfsGroupSnapshotSpec is the set of volumes to snapshot at one instant. All
// members live on one pool: a single `zfs snapshot` call commits in one
// transaction group, which is what makes the instant shared. Members and the
// pool are fixed at creation.
type ZfsGroupSnapshotSpec struct {
	// PoolGUID is the ZFS pool GUID hosting every member.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="poolGUID is immutable"
	PoolGUID string `json:"poolGUID"`

	// Members is the authoritative member list.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="members are immutable"
	Members []ZfsGroupSnapshotMember `json:"members"`
}

// ZfsGroupSnapshotStatus reports the observed group state.
type ZfsGroupSnapshotStatus struct {
	// ProvisionedAt is when all member snapshots were first Ready at the same
	// time. Written once; from then on the group creates nothing (ADR-0038).
	// +optional
	ProvisionedAt *metav1.Time `json:"provisionedAt,omitempty"`

	// Phase is derived from the member ZfsSnapshots.
	// +optional
	Phase ZfsGroupSnapshotPhase `json:"phase,omitempty"`

	// ReadyToUse is true while every member is Ready. Maps to CSI
	// VolumeGroupSnapshot.ready_to_use.
	// +optional
	ReadyToUse bool `json:"readyToUse,omitempty"`

	// CreationTime is the ZFS creation time of one raw snapshot, recorded once
	// after the atomic exec. Maps to CSI VolumeGroupSnapshot.creation_time.
	// +optional
	CreationTime *metav1.Time `json:"creationTime,omitempty"`

	// ObservedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Message carries human-readable detail about the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions represents the latest available observations.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=zgsnap
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolGUID`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.readyToUse`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZfsGroupSnapshot takes one atomic ZFS snapshot over several volumes of one
// pool and owns the member ZfsSnapshots created from it (ADR-0039). The CSI
// controller creates it; the node agent hosting the pool does the work.
type ZfsGroupSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZfsGroupSnapshotSpec   `json:"spec,omitempty"`
	Status ZfsGroupSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZfsGroupSnapshotList contains a list of ZfsGroupSnapshot.
type ZfsGroupSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZfsGroupSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZfsGroupSnapshot{}, &ZfsGroupSnapshotList{})
}
