package csi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// Volume context keys returned to the node plugin. The controller never returns
// an absolute path; the node resolves routing from ZfsPool.status.
const (
	CtxPoolGUID = "poolGUID"
	CtxDataset  = "dataset"
	CtxProtocol = "protocol"
)

// defaultsConfigMapKey is the ConfigMap data key holding the provisioner default
// parameters (a YAML map of parameter name -> value).
const defaultsConfigMapKey = "parameters.yaml"

// ControllerServer implements the CSI Controller service by writing the ZFS
// CRDs. CreateVolume writes a ZfsDataset, waits for it to become Ready, writes a
// ZfsShare, and returns a routing-only volume_context. DeleteVolume deletes both
// CRDs; finalizers on the agent/operator drive the actual teardown.
type ControllerServer struct {
	csi.UnimplementedControllerServer

	Client client.Client
	// DefaultsConfigMap names a ConfigMap in DefaultsNamespace whose
	// "parameters.yaml" key holds the provisioner default parameters (the
	// lowest-precedence layer). It is read live from the API on every CreateVolume
	// (no file mount, edits take effect without a restart); empty disables the
	// defaults layer.
	DefaultsConfigMap string
	DefaultsNamespace string
	// AnnotationPrefix selects which PVC annotations override parameters, e.g.
	// "param.simple-zfs-csi.io/". Empty disables the PVC-annotation layer.
	AnnotationPrefix string
	// CreateTimeout bounds how long CreateVolume waits for the ZfsDataset to reach
	// Ready before returning DeadlineExceeded (external-provisioner retries).
	CreateTimeout time.Duration
	// PollInterval is how often the readiness wait re-reads the ZfsDataset.
	PollInterval time.Duration
	Log          logr.Logger
}

// CreateVolume provisions a ZfsDataset (the export stays lazy until attach) and
// returns the volume context.
func (c *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	caps := req.GetVolumeCapabilities()
	if len(caps) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}

	pvcAnnotations, err := c.pvcAnnotations(ctx, req.GetParameters())
	if err != nil {
		return nil, err
	}
	defaults, err := c.defaultParams(ctx)
	if err != nil {
		return nil, err
	}
	merged := ResolveParameters(defaults, req.GetParameters(), pvcAnnotations, c.AnnotationPrefix)
	rp, err := ParseParams(merged)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if hasBlockCapability(caps) && rp.Protocol == storagev1alpha1.ProtocolNFS {
		return nil, status.Error(codes.InvalidArgument, "block volumeMode requires protocol nvmeof")
	}
	if rp.Protocol == storagev1alpha1.ProtocolNVMeoF && hasMultiNodeAccessMode(caps) {
		return nil, status.Error(codes.InvalidArgument,
			"protocol nvmeof is single-node only; multi-node (RWX) access modes are not supported (use protocol nfs)")
	}

	sizeBytes := capacityBytes(req.GetCapacityRange())
	if rp.DatasetType == storagev1alpha1.DatasetTypeVolume && sizeBytes <= 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity is required for nvmeof (zvol) volumes")
	}

	source, err := c.resolveContentSource(ctx, req, rp)
	if err != nil {
		return nil, err
	}

	dataset := rp.Dataset(name)
	desiredVol := volumeSpec(rp, dataset, sizeBytes)
	desiredVol.Source = source

	if err := c.ensureVolume(ctx, name, desiredVol); err != nil {
		return nil, err
	}
	ready, err := c.waitVolumeReady(ctx, name, 0)
	if err != nil {
		return nil, err
	}

	// The share is lazy (ADR-0010): CreateVolume writes only the ZfsDataset. The
	// export is created on demand at ControllerPublishVolume (attach) and torn
	// down at the last detach, so nothing is exported until a node is authorized.
	c.Log.Info("provisioned volume", "name", name, "pool", rp.PoolGUID, "dataset", dataset, "protocol", rp.Protocol, "path", ready.Status.Path)
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      name,
			CapacityBytes: sizeBytes,
			VolumeContext: map[string]string{
				CtxPoolGUID: rp.PoolGUID,
				CtxDataset:  dataset,
				CtxProtocol: string(rp.Protocol),
			},
			ContentSource: req.GetVolumeContentSource(),
		},
	}, nil
}

// DeleteVolume removes the ZfsShare and ZfsDataset; finalizers drive teardown.
func (c *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}

	// Best-effort: remove any lingering attach requests so the operator tears the
	// share down instead of trying to re-aggregate against a deleted dataset.
	if err := c.deleteAttachRequests(ctx, id); err != nil {
		return nil, err
	}
	share := &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: id}}
	if err := c.Client.Delete(ctx, share); err != nil && !apierrors.IsNotFound(err) {
		return nil, status.Errorf(codes.Internal, "delete ZfsShare %q: %v", id, err)
	}
	vol := &storagev1alpha1.ZfsDataset{ObjectMeta: metav1.ObjectMeta{Name: id}}
	if err := c.Client.Delete(ctx, vol); err != nil && !apierrors.IsNotFound(err) {
		return nil, status.Errorf(codes.Internal, "delete ZfsDataset %q: %v", id, err)
	}

	c.Log.Info("deprovisioned volume", "name", id)
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume authorizes a node to access a volume by creating a
// ZfsShareAttachRequest{volume, node} and waiting for the operator to export the
// aggregated share for the node. It is the zero-trust attach hook (ADR-0010):
// the export exists only while at least one node holds an attach request.
func (c *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	nodeID := req.GetNodeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "node id is required")
	}

	vol := &storagev1alpha1.ZfsDataset{}
	if err := c.Client.Get(ctx, client.ObjectKey{Name: volumeID}, vol); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "volume %q not found", volumeID)
		}
		return nil, status.Errorf(codes.Internal, "get ZfsDataset %q: %v", volumeID, err)
	}
	// Defense in depth: a zvol (NVMe-oF) is single-node only.
	if vol.Spec.Type == storagev1alpha1.DatasetTypeVolume &&
		hasMultiNodeAccessMode([]*csi.VolumeCapability{req.GetVolumeCapability()}) {
		return nil, status.Error(codes.InvalidArgument,
			"protocol nvmeof is single-node only; multi-node (RWX) access modes are not supported")
	}

	// Enforce single-node publication for single-node (RWO) volumes: never export
	// the same volume to two nodes at once. For a zvol that would attach the block
	// device read-write on both nodes and corrupt it. If the volume is already
	// published elsewhere (e.g. a forced pod move that attaches to the new node
	// before the old node detaches), return FailedPrecondition so
	// external-attacher retries once the prior attachment is released.
	if !hasMultiNodeAccessMode([]*csi.VolumeCapability{req.GetVolumeCapability()}) {
		other, err := c.attachedNode(ctx, volumeID, nodeID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list attach requests for %q: %v", volumeID, err)
		}
		if other != "" {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %q is already published to node %q; single-node (RWO) volumes cannot be attached to %q simultaneously",
				volumeID, other, nodeID)
		}
	}

	name := attachRequestName(volumeID, nodeID)
	if err := c.ensureAttachRequest(ctx, name, volumeID, nodeID); err != nil {
		return nil, err
	}
	if err := c.waitAttachReady(ctx, name); err != nil {
		return nil, err
	}

	c.Log.Info("published volume", "name", volumeID, "node", nodeID, "attachRequest", name)
	return &csi.ControllerPublishVolumeResponse{}, nil
}

// ControllerUnpublishVolume revokes a node's access by deleting its attach
// request; the operator tears down the aggregated share once the last request is
// gone.
func (c *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	nodeID := req.GetNodeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "node id is required")
	}

	name := attachRequestName(volumeID, nodeID)
	ar := &storagev1alpha1.ZfsShareAttachRequest{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Client.Delete(ctx, ar); err != nil && !apierrors.IsNotFound(err) {
		return nil, status.Errorf(codes.Internal, "delete ZfsShareAttachRequest %q: %v", name, err)
	}

	c.Log.Info("unpublished volume", "name", volumeID, "node", nodeID, "attachRequest", name)
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ControllerGetCapabilities advertises CREATE_DELETE_VOLUME.
func (c *ControllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			rpcCapability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
			rpcCapability(csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME),
			rpcCapability(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
			rpcCapability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT),
			rpcCapability(csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS),
			rpcCapability(csi.ControllerServiceCapability_RPC_CLONE_VOLUME),
		},
	}, nil
}

// ControllerExpandVolume grows a volume by bumping the backing ZfsDataset's size
// (filesystem refquota or zvol volsize) and waiting for the agent to apply it.
// For zvol (NVMe-oF) volumes the node must still grow the on-device filesystem,
// so NodeExpansionRequired is set; NFS filesystem quotas take effect immediately.
func (c *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	required := capacityBytes(req.GetCapacityRange())
	if required <= 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity range is required")
	}

	vol := &storagev1alpha1.ZfsDataset{}
	if err := c.Client.Get(ctx, client.ObjectKey{Name: id}, vol); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "volume %q not found", id)
		}
		return nil, status.Errorf(codes.Internal, "get ZfsDataset %q: %v", id, err)
	}

	var nodeExpansionRequired bool
	switch vol.Spec.Type {
	case storagev1alpha1.DatasetTypeFilesystem:
		// NFS quota grows live; no node-side work.
	case storagev1alpha1.DatasetTypeVolume:
		if vol.Spec.Volume == nil {
			return nil, status.Errorf(codes.Internal, "volume %q has no size", id)
		}
		nodeExpansionRequired = true
	default:
		return nil, status.Errorf(codes.Internal, "volume %q has unknown type %q", id, vol.Spec.Type)
	}

	// Bump the backing size, retrying on conflict with the agent's status writes.
	var targetGen int64
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.ZfsDataset{}
		if err := c.Client.Get(ctx, client.ObjectKey{Name: id}, cur); err != nil {
			return err
		}
		changed := false
		switch cur.Spec.Type {
		case storagev1alpha1.DatasetTypeFilesystem:
			if cur.Spec.Filesystem == nil {
				cur.Spec.Filesystem = &storagev1alpha1.FilesystemConfig{}
			}
			if q := cur.Spec.Filesystem.Quota; q == nil || q.Value() < required {
				cur.Spec.Filesystem.Quota = resource.NewQuantity(required, resource.BinarySI)
				changed = true
			}
		case storagev1alpha1.DatasetTypeVolume:
			if cur.Spec.Volume != nil && cur.Spec.Volume.Size.Value() < required {
				cur.Spec.Volume.Size = *resource.NewQuantity(required, resource.BinarySI)
				changed = true
			}
		}
		if changed {
			if err := c.Client.Update(ctx, cur); err != nil {
				return err
			}
		}
		targetGen = cur.Generation
		return nil
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "update ZfsDataset %q: %v", id, err)
	}

	if _, err := c.waitVolumeReady(ctx, id, targetGen); err != nil {
		return nil, err
	}

	c.Log.Info("expanded volume", "name", id, "capacity", required, "nodeExpansionRequired", nodeExpansionRequired)
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         required,
		NodeExpansionRequired: nodeExpansionRequired,
	}, nil
}

// ValidateVolumeCapabilities confirms the requested capabilities are supported.
func (c *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	caps := req.GetVolumeCapabilities()
	if len(caps) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for _, cap := range caps {
		if cap.GetAccessMode() == nil {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "missing access mode"}, nil
		}
	}

	// NVMe-oF (zvol) is single-node only: a multi-node access mode on a zvol +
	// filesystem corrupts data (ext4/xfs are not cluster filesystems).
	vol := &storagev1alpha1.ZfsDataset{}
	if err := c.Client.Get(ctx, client.ObjectKey{Name: req.GetVolumeId()}, vol); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "volume %q not found", req.GetVolumeId())
		}
		return nil, status.Errorf(codes.Internal, "get ZfsDataset %q: %v", req.GetVolumeId(), err)
	}
	if vol.Spec.Type == storagev1alpha1.DatasetTypeVolume && hasMultiNodeAccessMode(caps) {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: "protocol nvmeof is single-node only; multi-node (RWX) access modes are not supported",
		}, nil
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: caps,
		},
	}, nil
}

// ensureVolume creates the ZfsDataset, or validates that an existing one with the
// same name is compatible (CSI idempotency).
func (c *ControllerServer) ensureVolume(ctx context.Context, name string, desired storagev1alpha1.ZfsDatasetSpec) error {
	existing := &storagev1alpha1.ZfsDataset{}
	err := c.Client.Get(ctx, client.ObjectKey{Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		vol := &storagev1alpha1.ZfsDataset{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: desired}
		if err := c.Client.Create(ctx, vol); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return c.ensureVolume(ctx, name, desired)
			}
			return status.Errorf(codes.Internal, "create ZfsDataset %q: %v", name, err)
		}
		return nil
	case err != nil:
		return status.Errorf(codes.Internal, "get ZfsDataset %q: %v", name, err)
	default:
		if !volumeSpecCompatible(existing.Spec, desired) {
			return status.Errorf(codes.AlreadyExists, "volume %q already exists with different parameters", name)
		}
		return nil
	}
}

// ensureAttachRequest creates the ZfsShareAttachRequest for a (volume, node)
// pair, or validates that an existing one matches (idempotent publish).
func (c *ControllerServer) ensureAttachRequest(ctx context.Context, name, volume, node string) error {
	existing := &storagev1alpha1.ZfsShareAttachRequest{}
	err := c.Client.Get(ctx, client.ObjectKey{Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		ar := &storagev1alpha1.ZfsShareAttachRequest{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: volume, NodeName: node},
		}
		if err := c.Client.Create(ctx, ar); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return c.ensureAttachRequest(ctx, name, volume, node)
			}
			return status.Errorf(codes.Internal, "create ZfsShareAttachRequest %q: %v", name, err)
		}
		return nil
	case err != nil:
		return status.Errorf(codes.Internal, "get ZfsShareAttachRequest %q: %v", name, err)
	default:
		if existing.Spec.VolumeName != volume || existing.Spec.NodeName != node {
			return status.Errorf(codes.AlreadyExists, "attach request %q already exists for a different (volume, node)", name)
		}
		return nil
	}
}

// waitAttachReady polls the ZfsShareAttachRequest until the operator reports the
// aggregated share exported for the current generation, or the deadline elapses.
func (c *ControllerServer) waitAttachReady(ctx context.Context, name string) error {
	interval := c.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	waitCtx := ctx
	if c.CreateTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, c.CreateTimeout)
		defer cancel()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		ar := &storagev1alpha1.ZfsShareAttachRequest{}
		if err := c.Client.Get(waitCtx, client.ObjectKey{Name: name}, ar); err != nil {
			return status.Errorf(codes.Internal, "get ZfsShareAttachRequest %q: %v", name, err)
		}
		if ar.Status.Ready && ar.Status.ObservedGeneration >= ar.Generation {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return status.Errorf(codes.DeadlineExceeded, "timed out waiting for volume attach %q to become ready", name)
		case <-ticker.C:
		}
	}
}

// attachedNode returns the name of a node other than `exclude` that currently
// holds an attach request for `volume`, or "" if none. Terminating requests are
// counted on purpose: the aggregated share still lists that node until its
// finalizer clears, so publishing elsewhere before then could momentarily
// dual-export the volume.
func (c *ControllerServer) attachedNode(ctx context.Context, volume, exclude string) (string, error) {
	var list storagev1alpha1.ZfsShareAttachRequestList
	if err := c.Client.List(ctx, &list); err != nil {
		return "", err
	}
	for i := range list.Items {
		ar := &list.Items[i]
		if ar.Spec.VolumeName == volume && ar.Spec.NodeName != exclude {
			return ar.Spec.NodeName, nil
		}
	}
	return "", nil
}

// deleteAttachRequests best-effort removes every attach request for a volume,
// used on DeleteVolume so no orphan keeps a share aggregated.
func (c *ControllerServer) deleteAttachRequests(ctx context.Context, volume string) error {
	var list storagev1alpha1.ZfsShareAttachRequestList
	if err := c.Client.List(ctx, &list); err != nil {
		return status.Errorf(codes.Internal, "list attach requests for %q: %v", volume, err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.VolumeName != volume {
			continue
		}
		if err := c.Client.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return status.Errorf(codes.Internal, "delete attach request %q: %v", list.Items[i].Name, err)
		}
	}
	return nil
}

// attachRequestName derives a stable, DNS-safe object name for a (volume, node)
// attach request. The node is hashed to bound the length and avoid invalid
// characters while keeping the volume id readable.
func attachRequestName(volume, node string) string {
	sum := sha256.Sum256([]byte(node))
	return fmt.Sprintf("%s-%s", volume, hex.EncodeToString(sum[:])[:12])
}

// waitVolumeReady polls the ZfsDataset until it is Ready, fails, or the deadline
// elapses. A timeout returns DeadlineExceeded so external-provisioner retries.
// minGeneration requires the agent to have observed at least that spec
// generation (used after an expansion bumps the spec); 0 accepts any Ready.
func (c *ControllerServer) waitVolumeReady(ctx context.Context, name string, minGeneration int64) (*storagev1alpha1.ZfsDataset, error) {
	interval := c.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	waitCtx := ctx
	if c.CreateTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, c.CreateTimeout)
		defer cancel()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		vol := &storagev1alpha1.ZfsDataset{}
		if err := c.Client.Get(waitCtx, client.ObjectKey{Name: name}, vol); err != nil {
			return nil, status.Errorf(codes.Internal, "get ZfsDataset %q: %v", name, err)
		}
		observed := vol.Status.ObservedGeneration >= minGeneration
		switch {
		case observed && vol.Status.Phase == storagev1alpha1.DatasetPhaseReady:
			return vol, nil
		case observed && vol.Status.Phase == storagev1alpha1.DatasetPhaseError:
			return nil, status.Errorf(codes.Internal, "volume %q provisioning failed: %s", name, vol.Status.Message)
		}
		select {
		case <-waitCtx.Done():
			return nil, status.Errorf(codes.DeadlineExceeded, "timed out waiting for volume %q to become ready", name)
		case <-ticker.C:
		}
	}
}

// defaultParams reads the provisioner default parameters live from the defaults
// ConfigMap on every CreateVolume. It returns nil (no defaults layer) when the
// ConfigMap is unconfigured, absent, or empty, so a missing ConfigMap is not an
// error.
func (c *ControllerServer) defaultParams(ctx context.Context) (map[string]string, error) {
	if c.DefaultsConfigMap == "" {
		return nil, nil
	}
	var cm corev1.ConfigMap
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.DefaultsNamespace, Name: c.DefaultsConfigMap}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, status.Errorf(codes.Internal, "get default-parameters ConfigMap %s/%s: %v", c.DefaultsNamespace, c.DefaultsConfigMap, err)
	}
	raw := cm.Data[defaultsConfigMapKey]
	if raw == "" {
		return nil, nil
	}
	out := map[string]string{}
	if err := yaml.Unmarshal([]byte(raw), &out); err != nil {
		return nil, status.Errorf(codes.Internal, "parse default parameters from %s/%s[%s]: %v", c.DefaultsNamespace, c.DefaultsConfigMap, defaultsConfigMapKey, err)
	}
	return out, nil
}

// pvcAnnotations fetches the source PVC (identified by the reserved metadata
// parameters that external-provisioner injects) and returns its annotations. It
// returns nil without error when the metadata or the annotation layer is absent.
func (c *ControllerServer) pvcAnnotations(ctx context.Context, params map[string]string) (map[string]string, error) {
	if c.AnnotationPrefix == "" {
		return nil, nil
	}
	pvcName := params[ReservedPVCName]
	pvcNamespace := params[ReservedPVCNamespace]
	if pvcName == "" || pvcNamespace == "" {
		return nil, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: pvcNamespace, Name: pvcName}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, status.Errorf(codes.Internal, "get PVC %s/%s: %v", pvcNamespace, pvcName, err)
	}
	return pvc.Annotations, nil
}

// volumeSpec builds the desired ZfsDataset spec from resolved parameters.
func volumeSpec(rp *ResolvedParams, dataset string, sizeBytes int64) storagev1alpha1.ZfsDatasetSpec {
	spec := storagev1alpha1.ZfsDatasetSpec{
		PoolGUID:   rp.PoolGUID,
		Dataset:    dataset,
		Type:       rp.DatasetType,
		Properties: rp.Properties,
	}
	switch rp.DatasetType {
	case storagev1alpha1.DatasetTypeFilesystem:
		fs := &storagev1alpha1.FilesystemConfig{
			UID:  rp.UID,
			GID:  rp.GID,
			Mode: rp.Mode,
		}
		if sizeBytes > 0 {
			fs.Quota = resource.NewQuantity(sizeBytes, resource.BinarySI)
		}
		// Only attach the arm when it carries intent, so an unbounded fs with no
		// ownership stays nil (matching prior behaviour and volumeSpecCompatible).
		if fs.Quota != nil || fs.UID != nil || fs.GID != nil || fs.Mode != "" {
			spec.Filesystem = fs
		}
	case storagev1alpha1.DatasetTypeVolume:
		spec.Volume = &storagev1alpha1.VolumeConfig{
			Size:         *resource.NewQuantity(sizeBytes, resource.BinarySI),
			Volblocksize: rp.Volblocksize,
		}
	}
	return spec
}

// volumeSpecCompatible reports whether an existing ZfsDataset can satisfy a
// repeated CreateVolume for the same name (identity + size must match).
func volumeSpecCompatible(existing, desired storagev1alpha1.ZfsDatasetSpec) bool {
	if existing.PoolGUID != desired.PoolGUID || existing.Dataset != desired.Dataset || existing.Type != desired.Type {
		return false
	}
	if !datasetSourceEqual(existing.Source, desired.Source) {
		return false
	}
	return quantityEqual(volumeSize(existing), volumeSize(desired))
}

// datasetSourceEqual compares two optional clone sources for equality.
func datasetSourceEqual(a, b *storagev1alpha1.DatasetSource) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Snapshot == b.Snapshot && a.Volume == b.Volume
}

// volumeSize returns the sizing quantity for a spec (zvol size or fs quota), or
// nil when unbounded.
func volumeSize(spec storagev1alpha1.ZfsDatasetSpec) *resource.Quantity {
	switch spec.Type {
	case storagev1alpha1.DatasetTypeVolume:
		if spec.Volume != nil {
			return &spec.Volume.Size
		}
	case storagev1alpha1.DatasetTypeFilesystem:
		if spec.Filesystem != nil {
			return spec.Filesystem.Quota
		}
	}
	return nil
}

func quantityEqual(a, b *resource.Quantity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(*b) == 0
}

// capacityBytes picks the requested size from a CSI capacity range.
func capacityBytes(cr *csi.CapacityRange) int64 {
	if cr == nil {
		return 0
	}
	if cr.GetRequiredBytes() > 0 {
		return cr.GetRequiredBytes()
	}
	return cr.GetLimitBytes()
}

// hasBlockCapability reports whether any requested capability asks for raw block
// access (volumeMode: Block).
func hasBlockCapability(caps []*csi.VolumeCapability) bool {
	for _, cap := range caps {
		if cap.GetBlock() != nil {
			return true
		}
	}
	return false
}

// hasMultiNodeAccessMode reports whether any requested capability asks for a
// multi-node (RWX-style) access mode. NVMe-oF exports a zvol to a single node
// only; sharing it across nodes with a non-cluster filesystem corrupts data.
func hasMultiNodeAccessMode(caps []*csi.VolumeCapability) bool {
	for _, cap := range caps {
		switch cap.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			return true
		}
	}
	return false
}

func rpcCapability(t csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
		},
	}
}
