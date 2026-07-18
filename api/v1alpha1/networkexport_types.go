package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Protocol selects the network sharing backend for a NetworkExport.
// +kubebuilder:validation:Enum=nfs;nvmeof
type Protocol string

const (
	// ProtocolNFS exports a filesystem mountpoint over NFS.
	ProtocolNFS Protocol = "nfs"
	// ProtocolNVMeoF exports a block device over NVMe-oF (TCP).
	ProtocolNVMeoF Protocol = "nvmeof"
)

// NetworkExportPhase is a high-level summary of the export state.
type NetworkExportPhase string

const (
	// PhasePending means the export has not been configured on the node yet.
	PhasePending NetworkExportPhase = "Pending"
	// PhaseExported means the export is actively served on the node.
	PhaseExported NetworkExportPhase = "Exported"
	// PhaseError means the last reconcile attempt failed.
	PhaseError NetworkExportPhase = "Error"
)

// NetworkExportSpec describes the desired network export of an already-existing
// node-local path: a filesystem mountpoint (NFS) or a block device (NVMe-oF).
// It is storage-agnostic — it carries no ZFS or sizing parameters. Higher-level
// controllers (e.g. the ZfsShare reconciler) compile their intent down into a
// NetworkExport, which is the only contract the node-local aggregators execute.
// +kubebuilder:validation:XValidation:rule="self.protocol != 'nfs' || has(self.nfs)",message="spec.nfs is required when protocol is nfs"
// +kubebuilder:validation:XValidation:rule="self.protocol != 'nvmeof' || has(self.nvmeof)",message="spec.nvmeof is required when protocol is nvmeof"
type NetworkExportSpec struct {
	// NodeName pins the export to the physical storage node that holds the
	// underlying path. Only the controller running on this node acts on it.
	// +kubebuilder:validation:MinLength=1
	NodeName string `json:"nodeName"`

	// Protocol selects the sharing backend.
	Protocol Protocol `json:"protocol"`

	// Path is the local, node-side source of the export.
	// For nfs: the mountpoint to export, e.g. "/tank/k8s/pvc-123".
	// For nvmeof: the block device, e.g. "/dev/zvol/tank/pvc-123".
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// NFS holds NFS-specific export configuration. Required when protocol is nfs.
	// +optional
	NFS *NFSExportSpec `json:"nfs,omitempty"`

	// NVMeoF holds NVMe-oF specific configuration. Required when protocol is nvmeof.
	// +optional
	NVMeoF *NVMeoFExportSpec `json:"nvmeof,omitempty"`
}

// NFSExportSpec configures how a mountpoint is exported via NFS.
type NFSExportSpec struct {
	// Clients is the set of client match specs allowed to mount the export.
	// Each entry maps to one line fragment in /etc/exports.
	// +kubebuilder:validation:MinItems=1
	Clients []NFSClient `json:"clients"`
}

// NFSClient is a single NFS export client rule.
type NFSClient struct {
	// Client is the host match: "*", a CIDR (10.0.0.0/24), an IP, or a hostname.
	// +kubebuilder:validation:MinLength=1
	Client string `json:"client"`

	// Options are raw NFS export options for this client, e.g.
	// ["rw", "no_root_squash", "no_subtree_check", "sync"].
	// When empty a safe default set is applied by the controller.
	// +optional
	Options []string `json:"options,omitempty"`
}

// NVMeoFExportSpec configures how a block device is exported via NVMe-oF (TCP).
type NVMeoFExportSpec struct {
	// NQN is the subsystem NQN clients use to connect. When empty the controller
	// derives a deterministic NQN from the resource metadata.
	// +optional
	NQN string `json:"nqn,omitempty"`

	// AllowedHosts is the list of host NQNs permitted to connect. When empty the
	// subsystem allows any host (allow_any_host=1).
	// +optional
	AllowedHosts []string `json:"allowedHosts,omitempty"`

	// DHChapSecretName references a Secret holding the per-attach DH-CHAP key
	// (data key "dhchap-key", DHHC-1 format) that the target programs onto the
	// allowed host and the node passes to `nvme connect --dhchap-secret`. Empty
	// means no in-band authentication. See design-decisions ADR-0011.
	// +optional
	DHChapSecretName string `json:"dhchapSecretName,omitempty"`

	// DHChapSecretNamespace is the namespace of DHChapSecretName (the driver's
	// release namespace). Required when DHChapSecretName is set.
	// +optional
	DHChapSecretNamespace string `json:"dhchapSecretNamespace,omitempty"`
}

// NetworkExportStatus reports the observed export state on the node.
type NetworkExportStatus struct {
	// Phase is a coarse summary of the current state.
	// +optional
	Phase NetworkExportPhase `json:"phase,omitempty"`

	// ObservedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// NQN is the effective subsystem NQN for nvmeof exports.
	// +optional
	NQN string `json:"nqn,omitempty"`

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
// +kubebuilder:resource:scope=Cluster,shortName=nexport
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocol`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.path`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NetworkExport is the Schema for the networkexports API. It is a generic,
// storage-agnostic "intent to export" a node-local path over the network from a
// specific node. It is the only resource the node-local NFS / NVMe-oF
// aggregators execute; ZFS-centric intent (ZfsShare) compiles down into it.
type NetworkExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetworkExportSpec   `json:"spec,omitempty"`
	Status NetworkExportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NetworkExportList contains a list of NetworkExport.
type NetworkExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetworkExport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NetworkExport{}, &NetworkExportList{})
}
