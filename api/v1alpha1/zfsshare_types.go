package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZfsSharePhase is a high-level summary of the share state.
type ZfsSharePhase string

const (
	// SharePhasePending means the target pool is not yet resolvable (unknown
	// GUID, no current node, or the node is offline); no export is rendered.
	SharePhasePending ZfsSharePhase = "Pending"
	// SharePhaseExporting means the child NetworkExport has been rendered but the
	// node-local aggregator has not yet confirmed it live for the current
	// generation; consumers must not mount yet.
	SharePhaseExporting ZfsSharePhase = "Exporting"
	// SharePhaseBound means the share resolved to a node, a child NetworkExport
	// has been rendered, and the aggregator confirmed it exported for the
	// current generation.
	SharePhaseBound ZfsSharePhase = "Bound"
	// SharePhaseError means the last reconcile attempt failed.
	SharePhaseError ZfsSharePhase = "Error"
)

// ZfsShareSpec is the ZFS-centric intent to share an already-provisioned dataset
// or zvol over the network. It is keyed on the immutable pool GUID plus the
// logical dataset path, so it survives pool takeover: the operator resolves the
// GUID to the pool's current node and mount root, derives the node-local path,
// and compiles this down into a node-pinned NetworkExport. Consumers never
// author NetworkExports directly for ZFS-backed volumes.
// +kubebuilder:validation:XValidation:rule="self.protocol != 'nfs' || has(self.nfs)",message="spec.nfs is required when protocol is nfs"
// +kubebuilder:validation:XValidation:rule="self.protocol != 'nvmeof' || has(self.nvmeof)",message="spec.nvmeof is required when protocol is nvmeof"
type ZfsShareSpec struct {
	// PoolGUID is the immutable ZFS pool GUID (the ZfsPool metadata.name without
	// the "zpool-" prefix) that hosts the dataset. The operator resolves it to
	// the pool's current node, name and mount root via ZfsPool.status.
	// +kubebuilder:validation:MinLength=1
	PoolGUID string `json:"poolGUID"`

	// Dataset is the logical dataset path relative to the pool root, e.g.
	// "k8s/pvc-123". Combined with the resolved pool it yields the export path:
	// NFS uses <baseMountPath>/<dataset>; NVMe-oF uses /dev/zvol/<poolName>/<dataset>.
	// +kubebuilder:validation:MinLength=1
	Dataset string `json:"dataset"`

	// Protocol selects the sharing backend.
	Protocol Protocol `json:"protocol"`

	// NFS holds NFS-specific export configuration. Required when protocol is nfs.
	// +optional
	NFS *NFSExportSpec `json:"nfs,omitempty"`

	// NVMeoF holds NVMe-oF specific configuration. Required when protocol is nvmeof.
	// +optional
	NVMeoF *NVMeoFExportSpec `json:"nvmeof,omitempty"`
}

// ZfsShareStatus reports the resolution result and the rendered child export.
type ZfsShareStatus struct {
	// Phase is a coarse summary of the current state.
	// +optional
	Phase ZfsSharePhase `json:"phase,omitempty"`

	// NodeName is the storage node the share currently resolves to (the pool's
	// current host). It changes on pool takeover.
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// Path is the node-local export path derived from the resolved pool.
	// +optional
	Path string `json:"path,omitempty"`

	// NetworkExportName is the name of the child NetworkExport this share owns.
	// +optional
	NetworkExportName string `json:"networkExportName,omitempty"`

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
// +kubebuilder:resource:scope=Cluster,shortName=zshare
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolGUID`
// +kubebuilder:printcolumn:name="Dataset",type=string,JSONPath=`.spec.dataset`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocol`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZfsShare is the Schema for the zfsshares API. It is a ZFS-centric, pool-GUID-
// keyed "intent to share" that the operator compiles down into a node-pinned
// NetworkExport, re-targeting it automatically when the pool moves nodes.
type ZfsShare struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZfsShareSpec   `json:"spec,omitempty"`
	Status ZfsShareStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZfsShareList contains a list of ZfsShare.
type ZfsShareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZfsShare `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZfsShare{}, &ZfsShareList{})
}
