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

// ZfsSnapshotMode selects the ZFS-level mechanism CreateSnapshot uses to take
// (and, later, tear down) a snapshot. See docs/snapshot-lifecycle-redesign.md,
// D8/D14.
// +kubebuilder:validation:Enum=standalone;integrated
type ZfsSnapshotMode string

const (
	// SnapshotModeStandalone takes a raw ZFS snapshot and immediately provisions
	// an owned backing-clone ZfsDataset from it (D15), so the snapshot is fully
	// independent of its source volume: DeleteVolume promotes it away (zfs
	// promote, D0) instead of blocking. This is the Ceph-style, new default.
	SnapshotModeStandalone ZfsSnapshotMode = "standalone"
	// SnapshotModeIntegrated takes a plain ZFS snapshot only — today's original,
	// pre-redesign behaviour. Cheaper (no extra clone dataset), but DeleteVolume
	// blocks (requeues) while any dependent snapshot in this mode still exists,
	// since there is no promote mechanism to fall back on.
	SnapshotModeIntegrated ZfsSnapshotMode = "integrated"
)

// ZfsSnapshotSpec is the desired point-in-time snapshot of a source dataset/zvol
// on the pool identified by PoolGUID. It is a separate lifecycle from ZfsDataset
// (derive-from-source, read-only, restore/clone) — see design-decisions ADR-0006.
// The agent on the node currently hosting the pool takes `<dataset>@<snapshotName>`.
type ZfsSnapshotSpec struct {
	// PoolGUID is the immutable ZFS pool GUID (the ZfsPool metadata.name without
	// the "zpool-" prefix) that hosts the source dataset. The agent derives the
	// concrete pool name from the matching ZfsPool.status.
	// +kubebuilder:validation:MinLength=1
	PoolGUID string `json:"poolGUID"`

	// Dataset is the source dataset's logical path relative to the pool root, e.g.
	// "k8s/pvc-123". Combined with the pool name and SnapshotName it yields the
	// full ZFS snapshot name "<poolName>/<dataset>@<snapshotName>".
	// +kubebuilder:validation:MinLength=1
	Dataset string `json:"dataset"`

	// SnapshotName is the ZFS snapshot short name (the part after "@"). It is
	// immutable for the lifetime of the snapshot.
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
	// PVC was removed but the snapshot was retained). Immutable once set.
	// +optional
	SourceType DatasetType `json:"sourceType,omitempty"`

	// Mode selects standalone (Ceph-style, via zfs promote; new default) or
	// integrated (today's original, plain-snapshot-only behaviour). Immutable
	// once set, resolved at CreateSnapshot time from the VolumeSnapshotClass
	// "mode" parameter with a chart-configured default (D8). Empty is treated as
	// Integrated for backward compatibility with snapshots created before this
	// field existed.
	// +optional
	Mode ZfsSnapshotMode `json:"mode,omitempty"`
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
