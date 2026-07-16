package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeType selects which kind of ZFS dataset a ZfsVolume provisions: a POSIX
// filesystem (shared over NFS) or a raw block volume/zvol (shared over NVMe-oF).
// Both are "datasets" in ZFS terms; this is the dataset's type.
// +kubebuilder:validation:Enum=filesystem;volume
type VolumeType string

const (
	// VolumeTypeFilesystem provisions a ZFS filesystem dataset, sized by quota.
	VolumeTypeFilesystem VolumeType = "filesystem"
	// VolumeTypeVolume provisions a ZFS volume (zvol / block device), sized by size.
	VolumeTypeVolume VolumeType = "volume"
)

// ZfsVolumePhase is a high-level summary of the allocation state.
type ZfsVolumePhase string

const (
	// VolumePhasePending means the dataset/zvol has not been created yet.
	VolumePhasePending ZfsVolumePhase = "Pending"
	// VolumePhaseReady means the dataset/zvol exists and is usable.
	VolumePhaseReady ZfsVolumePhase = "Ready"
	// VolumePhaseError means the last reconcile attempt failed.
	VolumePhaseError ZfsVolumePhase = "Error"
)

// FilesystemConfig holds the options that apply only to a filesystem dataset
// (type=filesystem). It is a discriminated-union arm of ZfsVolumeSpec: it must
// be set when (and only when) type is filesystem.
type FilesystemConfig struct {
	// Quota caps the dataset's referenced space (ZFS "refquota"). When omitted or
	// zero the dataset is unlimited.
	// +optional
	Quota *resource.Quantity `json:"quota,omitempty"`
}

// VolumeConfig holds the options that apply only to a volume/zvol (type=volume).
// It is a discriminated-union arm of ZfsVolumeSpec: it must be set when (and
// only when) type is volume.
type VolumeConfig struct {
	// Size is the logical volume size of the zvol. It is required.
	Size resource.Quantity `json:"size"`

	// Volblocksize sets the zvol's block size (ZFS "volblocksize"), e.g. "16k". It
	// is fixed at creation; empty means the ZFS default.
	// +optional
	Volblocksize string `json:"volblocksize,omitempty"`
}

// ZfsVolumeSpec is the desired allocation of a ZFS dataset on the pool identified
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
type ZfsVolumeSpec struct {
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
	Type VolumeType `json:"type"`

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
}

// ZfsVolumeStatus reports the observed allocation state on the node.
type ZfsVolumeStatus struct {
	// Phase is a coarse summary of the current state.
	// +optional
	Phase ZfsVolumePhase `json:"phase,omitempty"`

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
// +kubebuilder:resource:scope=Cluster,shortName=zvol
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolGUID`
// +kubebuilder:printcolumn:name="Dataset",type=string,JSONPath=`.spec.dataset`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.path`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZfsVolume is the Schema for the zfsvolumes API. It is a cluster-scoped
// "intent to allocate" a ZFS dataset or zvol on a specific pool (by GUID). The
// CSI controller creates it; the node agent currently hosting the pool creates
// the underlying ZFS object and reports the result in the status.
type ZfsVolume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZfsVolumeSpec   `json:"spec,omitempty"`
	Status ZfsVolumeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZfsVolumeList contains a list of ZfsVolume.
type ZfsVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZfsVolume `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZfsVolume{}, &ZfsVolumeList{})
}
