package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DatasetType selects which kind of ZFS dataset a ZfsDataset provisions: a POSIX
// filesystem (shared over NFS) or a raw block volume/zvol (shared over NVMe-oF).
// Both are "datasets" in ZFS terms; this is the dataset's type.
// +kubebuilder:validation:Enum=filesystem;volume
type DatasetType string

const (
	// DatasetTypeFilesystem provisions a ZFS filesystem dataset, sized by quota.
	DatasetTypeFilesystem DatasetType = "filesystem"
	// DatasetTypeVolume provisions a ZFS volume (zvol / block device), sized by size.
	DatasetTypeVolume DatasetType = "volume"
)

// ZfsDatasetPhase is a high-level summary of the allocation state.
type ZfsDatasetPhase string

const (
	// DatasetPhasePending means the dataset/zvol has not been created yet.
	DatasetPhasePending ZfsDatasetPhase = "Pending"
	// DatasetPhaseReady means the dataset/zvol exists and is usable.
	DatasetPhaseReady ZfsDatasetPhase = "Ready"
	// DatasetPhaseError means the last reconcile attempt failed.
	DatasetPhaseError ZfsDatasetPhase = "Error"
)

// FilesystemConfig holds the options that apply only to a filesystem dataset
// (type=filesystem). It is a discriminated-union arm of ZfsDatasetSpec: it must
// be set when (and only when) type is filesystem.
type FilesystemConfig struct {
	// Quota caps the dataset's referenced space (ZFS "refquota"). When omitted or
	// zero the dataset is unlimited.
	// +optional
	Quota *resource.Quantity `json:"quota,omitempty"`

	// UID sets the numeric owner of the dataset's root directory, applied once
	// with `chown` right after creation. Nil leaves the ZFS default (root, uid 0).
	// It is a provision-time convenience for NFS shares, whose ownership is set
	// server-side (kubelet's fsGroup does not apply to RWX volumes); the
	// application remains free to change ownership of files it later creates.
	// +optional
	UID *int64 `json:"uid,omitempty"`

	// GID sets the numeric group of the dataset's root directory, applied once
	// with `chown` right after creation. Nil leaves the ZFS default (root, gid 0).
	// +optional
	GID *int64 `json:"gid,omitempty"`

	// Mode sets the permission bits of the dataset's root directory as an octal
	// string (e.g. "0770"), applied once with `chmod` right after creation. Empty
	// leaves the ZFS default (0755).
	// +optional
	Mode string `json:"mode,omitempty"`
}

// VolumeConfig holds the options that apply only to a volume/zvol (type=volume).
// It is a discriminated-union arm of ZfsDatasetSpec: it must be set when (and
// only when) type is volume.
type VolumeConfig struct {
	// Size is the logical volume size of the zvol. It is required.
	Size resource.Quantity `json:"size"`

	// Volblocksize sets the zvol's block size (ZFS "volblocksize"), e.g. "16k". It
	// is fixed at creation; empty means the ZFS default.
	// +optional
	Volblocksize string `json:"volblocksize,omitempty"`
}

// DatasetSource references a ZFS source to clone the new dataset from instead of
// creating it empty (CSI VolumeContentSource). Clones are same-pool by ZFS
// constraint, so the source lives on the same pool (PoolGUID) as the dataset.
// Exactly one of Snapshot or Volume is set.
type DatasetSource struct {
	// Snapshot is the source snapshot's logical reference "<dataset>@<snapshot>"
	// relative to the pool root, when restoring from a snapshot. The agent runs
	// `zfs clone <pool>/<Snapshot> <pool>/<dataset>`.
	// +optional
	Snapshot string `json:"snapshot,omitempty"`

	// Volume is the source dataset's logical path relative to the pool root, when
	// cloning from another volume. The agent takes an intermediate snapshot of it
	// first, then clones that.
	// +optional
	Volume string `json:"volume,omitempty"`
}

// ZfsDatasetSpec is the desired allocation of a ZFS dataset on the pool identified
// by PoolGUID. It expresses storage intent only (create/size); the network
// export of the resulting path is a separate concern (ZfsShare -> NetworkExport).
// The agent on the node currently hosting the pool reconciles it.
//
// Type-specific options live in a nested discriminated union: exactly the arm
// matching Type is honoured. Use Filesystem for type=filesystem and Volume for
// type=volume; the other arm must be absent.
// +kubebuilder:validation:XValidation:rule="self.type != 'volume' || has(self.volume)",message="spec.volume is required when type is volume"
// +kubebuilder:validation:XValidation:rule="self.type != 'volume' || !has(self.filesystem)",message="spec.filesystem is only valid when type is filesystem"
// +kubebuilder:validation:XValidation:rule="self.type != 'filesystem' || !has(self.volume)",message="spec.volume is only valid when type is volume"
type ZfsDatasetSpec struct {
	// PoolGUID is the immutable ZFS pool GUID (the ZfsPool metadata.name, without
	// the "zpool-" prefix) that this volume is allocated on. The agent derives the
	// concrete pool name and mount root from the matching ZfsPool.status.
	// +kubebuilder:validation:MinLength=1
	PoolGUID string `json:"poolGUID"`

	// Dataset is the logical dataset path relative to the pool root, e.g.
	// "k8s/pvc-123". It names the ZFS object (filesystem or volume) and is
	// immutable for the lifetime of the volume.
	// +kubebuilder:validation:MinLength=1
	Dataset string `json:"dataset"`

	// Type selects a filesystem or a volume/zvol and determines which of the
	// Filesystem/Volume option arms is honoured.
	Type DatasetType `json:"type"`

	// Properties are extra ZFS properties applied verbatim at creation, e.g.
	// {"compression": "lz4", "recordsize": "1M"}. Keys are ZFS property names.
	// They apply to both filesystem and volume datasets.
	// +optional
	Properties map[string]string `json:"properties,omitempty"`

	// Filesystem holds filesystem-only options. Set it when (and only when)
	// type=filesystem.
	// +optional
	Filesystem *FilesystemConfig `json:"filesystem,omitempty"`

	// Volume holds volume/zvol-only options. Set it when (and only when)
	// type=volume.
	// +optional
	Volume *VolumeConfig `json:"volume,omitempty"`

	// Source, when set, clones the dataset from an existing snapshot or volume
	// instead of creating it empty (CSI VolumeContentSource). The source must be
	// on the same pool (PoolGUID) and of the same Type.
	// +optional
	Source *DatasetSource `json:"source,omitempty"`
}

// ZfsDatasetStatus reports the observed allocation state on the node.
type ZfsDatasetStatus struct {
	// Phase is a coarse summary of the current state.
	// +optional
	Phase ZfsDatasetPhase `json:"phase,omitempty"`

	// Path is the node-local path of the created volume once Ready: the dataset
	// mountpoint (type=dataset) or the zvol device node (type=zvol). Consumers
	// should still resolve routing via ZfsPool.status rather than pinning this.
	// +optional
	Path string `json:"path,omitempty"`

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
// +kubebuilder:resource:scope=Cluster,shortName=zds
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolGUID`
// +kubebuilder:printcolumn:name="Dataset",type=string,JSONPath=`.spec.dataset`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.path`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZfsDataset is the Schema for the zfsdatasets API. It is a cluster-scoped
// "intent to allocate" a ZFS dataset or zvol on a specific pool (by GUID). The
// CSI controller creates it; the node agent currently hosting the pool creates
// the underlying ZFS object and reports the result in the status.
type ZfsDataset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZfsDatasetSpec   `json:"spec,omitempty"`
	Status ZfsDatasetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZfsDatasetList contains a list of ZfsDataset.
type ZfsDatasetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZfsDataset `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZfsDataset{}, &ZfsDatasetList{})
}
