package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZfsSnapshotPhase is a high-level summary of the snapshot state.
type ZfsSnapshotPhase string

const (
	// SnapshotPhasePending means the ZFS snapshot has not been taken yet.
	SnapshotPhasePending ZfsSnapshotPhase = "Pending"
	// SnapshotPhaseReady means the snapshot exists and can be restored/cloned.
	SnapshotPhaseReady ZfsSnapshotPhase = "Ready"
	// SnapshotPhaseError means the last reconcile attempt failed.
	SnapshotPhaseError ZfsSnapshotPhase = "Error"
)

// ZfsSnapshotSpec is the desired point-in-time snapshot of a source dataset/zvol
// on the pool identified by PoolGUID. It is a separate lifecycle from ZfsDataset
// (derive-from-source, read-only, restore/clone) — see design-decisions ADR-0006.
// The agent on the node currently hosting the pool takes `<dataset>@<snapshotName>`.
//
// Every snapshot is taken the same way (ADR-0026): a raw ZFS snapshot plus an
// owned backing-clone ZfsDataset, which makes the snapshot independent of its
// source volume. The former `mode` field, which could select a plain-snapshot
// variant instead, was removed — see snapshot-lifecycle-redesign.md §11.
type ZfsSnapshotSpec struct {
	// PoolGUID is the immutable ZFS pool GUID (the ZfsPool metadata.name without
	// the "zpool-" prefix) that hosts the source dataset. The agent derives the
	// concrete pool name from the matching ZfsPool.status.
	// +kubebuilder:validation:MinLength=1
	PoolGUID string `json:"poolGUID"`

	// Dataset is the source dataset's logical path relative to the pool root, e.g.
	// "k8s/pvc-123". Combined with the pool name and SnapshotName it yields the
	// full ZFS snapshot name "<poolName>/<dataset>@<snapshotName>".
	//
	// It records where the source was when the snapshot was taken. While
	// SourceVolume still resolves, that ZfsDataset's current Spec.Dataset is
	// authoritative instead, so a source renamed afterwards stays usable; this
	// field is the fallback for a snapshot that has outlived its source
	// (ADR-0025).
	// +kubebuilder:validation:MinLength=1
	Dataset string `json:"dataset"`

	// SnapshotName is the ZFS snapshot short name (the part after "@").
	//
	// Nothing in the driver rewrites it after creation, but like the other
	// location fields it is deliberately left mutable so it can be repointed by
	// hand during a recovery. Contrast SourceType below, which records what the
	// source *was* and is CEL-immutable. See api-conventions.md §5.
	// +kubebuilder:validation:MinLength=1
	SnapshotName string `json:"snapshotName"`

	// SourceVolume is the CSI source volume id (the source ZfsDataset's
	// metadata.name). It is carried for back-reference and CSI ListSnapshots
	// reporting; the agent does not need it to take the snapshot.
	// +optional
	SourceVolume string `json:"sourceVolume,omitempty"`

	// SourceType is the source ZfsDataset's type (filesystem or volume) at
	// snapshot creation time. The CSI controller captures it from the source
	// so that a later restore can still reject a filesystem/zvol type mismatch
	// even if the source ZfsDataset has since been deleted (e.g. the original
	// PVC was removed but the snapshot was retained).
	//
	// Immutable once set (D24): it is a record of what the source *was*, not a
	// pointer, so changing it could only ever make a restore-compatibility
	// check lie.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sourceType is immutable"
	SourceType DatasetType `json:"sourceType,omitempty"`

	// SourceFSType is the filesystem the source volume was formatted with at
	// snapshot creation time, copied from its Status.FSType. Like SourceType it
	// exists so a restore can still be checked for compatibility (D10/D25) once
	// the source ZfsDataset is gone — which, since a snapshot outlives its
	// source volume by design, is the headline scenario rather than an edge
	// case. Empty means the source had never been formatted (or predates this
	// field), which imposes no constraint.
	// +optional
	SourceFSType string `json:"sourceFSType,omitempty"`

	// SourceVolblocksize is the source zvol's volblocksize at snapshot creation
	// time. Captured for the same reason as SourceFSType (D25): a clone cannot
	// change volblocksize, and without this a restore into a StorageClass with a
	// different value passed unnoticed and was then silently ignored by ZFS.
	// Empty for filesystem sources.
	// +optional
	SourceVolblocksize string `json:"sourceVolblocksize,omitempty"`

	// SourceProperties are the structural ZFS property overrides recorded on the
	// source ZfsDataset at snapshot creation time (D25), compared against the
	// target's resolved property.* overrides on restore when the source itself
	// is no longer available.
	// +optional
	SourceProperties map[string]string `json:"sourceProperties,omitempty"`
}

// ZfsSnapshotStatus reports the observed snapshot state on the node.
type ZfsSnapshotStatus struct {
	// Phase is a coarse summary of the current state.
	// +optional
	Phase ZfsSnapshotPhase `json:"phase,omitempty"`

	// ReadyToUse is true once the snapshot exists and can be restored/cloned. It
	// maps directly to the CSI Snapshot.ready_to_use field.
	// +optional
	ReadyToUse bool `json:"readyToUse,omitempty"`

	// CreationTime is the snapshot's ZFS creation time (from the `creation`
	// property). It maps to the CSI Snapshot.creation_time field.
	// +optional
	CreationTime *metav1.Time `json:"creationTime,omitempty"`

	// RestoreSize is the referenced logical size of the snapshot in bytes — the
	// minimum volume size needed to restore it. It maps to CSI Snapshot.size_bytes.
	// +optional
	RestoreSize *resource.Quantity `json:"restoreSize,omitempty"`

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
// +kubebuilder:resource:scope=Cluster,shortName=zsnap
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolGUID`
// +kubebuilder:printcolumn:name="Dataset",type=string,JSONPath=`.spec.dataset`
// +kubebuilder:printcolumn:name="Snapshot",type=string,JSONPath=`.spec.snapshotName`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.readyToUse`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZfsSnapshot is the Schema for the zfssnapshots API. It is a cluster-scoped
// "intent to snapshot" a ZFS dataset or zvol on a specific pool (by GUID). The
// CSI controller creates it; the node agent currently hosting the pool takes the
// ZFS snapshot and reports readiness, creation time and restore size.
type ZfsSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZfsSnapshotSpec   `json:"spec,omitempty"`
	Status ZfsSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZfsSnapshotList contains a list of ZfsSnapshot.
type ZfsSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZfsSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZfsSnapshot{}, &ZfsSnapshotList{})
}
