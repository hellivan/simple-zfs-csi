package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZfsShareAttachRequestSpec is a single node's request to attach (gain network
// access to) a shared volume. The CSI controller creates exactly one per
// (volume, node) at ControllerPublishVolume and deletes it at
// ControllerUnpublishVolume. The operator aggregates all requests for a volume
// into a lazily-managed ZfsShare (see design-decisions ADR-0010): as long as at
// least one request exists the share is exported to the requesting nodes; when
// the last request is removed the share (and its child NetworkExport) is torn
// down. This makes the export zero-trust — it exists only while a node is
// authorized, and disappears when the last consumer detaches.
type ZfsShareAttachRequestSpec struct {
	// VolumeName is the CSI volume id (the source ZfsDataset metadata.name) the
	// requesting node wants to attach.
	// +kubebuilder:validation:MinLength=1
	VolumeName string `json:"volumeName"`

	// NodeName is the Kubernetes node requesting access. The operator resolves it
	// to a network client identity (for NFS: the node's internal IP) that is
	// added to the aggregated share's allow-list.
	// +kubebuilder:validation:MinLength=1
	NodeName string `json:"nodeName"`
}

// ZfsShareAttachRequestStatus reports whether the export is live for this node.
type ZfsShareAttachRequestStatus struct {
	// Ready is true once the aggregated ZfsShare is exported for the current
	// generation, so a subsequent NodePublish will find a live export. The CSI
	// controller's ControllerPublishVolume waits on this before returning.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// ShareName is the name of the aggregated ZfsShare serving this request.
	// +optional
	ShareName string `json:"shareName,omitempty"`

	// ObservedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Message carries human-readable detail about the current state.
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
// +kubebuilder:resource:scope=Cluster,shortName=zsar
// +kubebuilder:printcolumn:name="Volume",type=string,JSONPath=`.spec.volumeName`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZfsShareAttachRequest is the Schema for the zfsshareattachrequests API. It is a
// cluster-scoped, per-(volume, node) attach ticket created by the CSI controller
// at ControllerPublishVolume. The operator ref-counts these into a lazily-managed
// ZfsShare so a volume is exported only to nodes that currently hold an attach
// request (see design-decisions ADR-0010).
type ZfsShareAttachRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZfsShareAttachRequestSpec   `json:"spec,omitempty"`
	Status ZfsShareAttachRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZfsShareAttachRequestList contains a list of ZfsShareAttachRequest.
type ZfsShareAttachRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZfsShareAttachRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZfsShareAttachRequest{}, &ZfsShareAttachRequestList{})
}
