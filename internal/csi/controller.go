package csi

import (
	"context"
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

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// Volume context keys returned to the node plugin. The controller never returns
// an absolute path; the node resolves routing from ZfsPool.status.
const (
	CtxPoolGUID = "poolGUID"
	CtxDataset  = "dataset"
	CtxProtocol = "protocol"
)

// ControllerServer implements the CSI Controller service by writing the ZFS
// CRDs. CreateVolume writes a ZfsDataset, waits for it to become Ready, writes a
// ZfsShare, and returns a routing-only volume_context. DeleteVolume deletes both
// CRDs; finalizers on the agent/operator drive the actual teardown.
type ControllerServer struct {
	csi.UnimplementedControllerServer

	Client        client.Client
	DefaultParams map[string]string
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

// CreateVolume provisions a ZfsDataset (+ ZfsShare) and returns the volume context.
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
	merged := ResolveParameters(c.DefaultParams, req.GetParameters(), pvcAnnotations, c.AnnotationPrefix)
	rp, err := ParseParams(merged)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if hasBlockCapability(caps) && rp.Protocol == storagev1alpha1.ProtocolNFS {
		return nil, status.Error(codes.InvalidArgument, "block volumeMode requires protocol nvmeof")
	}

	sizeBytes := capacityBytes(req.GetCapacityRange())
	if rp.DatasetType == storagev1alpha1.DatasetTypeVolume && sizeBytes <= 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity is required for nvmeof (zvol) volumes")
	}

	dataset := rp.Dataset(name)
	desiredVol := volumeSpec(rp, dataset, sizeBytes)

	if err := c.ensureVolume(ctx, name, desiredVol); err != nil {
		return nil, err
	}
	ready, err := c.waitVolumeReady(ctx, name, 0)
	if err != nil {
		return nil, err
	}
	if err := c.ensureShare(ctx, name, shareSpec(rp, dataset)); err != nil {
		return nil, err
	}

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
		},
	}, nil
}

// DeleteVolume removes the ZfsShare and ZfsDataset; finalizers drive teardown.
func (c *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
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

// ControllerGetCapabilities advertises CREATE_DELETE_VOLUME.
func (c *ControllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			rpcCapability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
			rpcCapability(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
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
func (c *ControllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetAccessMode() == nil {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "missing access mode"}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
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

// ensureShare creates the ZfsShare, or validates compatibility of an existing one.
func (c *ControllerServer) ensureShare(ctx context.Context, name string, desired storagev1alpha1.ZfsShareSpec) error {
	existing := &storagev1alpha1.ZfsShare{}
	err := c.Client.Get(ctx, client.ObjectKey{Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		share := &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: desired}
		if err := c.Client.Create(ctx, share); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return c.ensureShare(ctx, name, desired)
			}
			return status.Errorf(codes.Internal, "create ZfsShare %q: %v", name, err)
		}
		return nil
	case err != nil:
		return status.Errorf(codes.Internal, "get ZfsShare %q: %v", name, err)
	default:
		if existing.Spec.PoolGUID != desired.PoolGUID ||
			existing.Spec.Dataset != desired.Dataset ||
			existing.Spec.Protocol != desired.Protocol {
			return status.Errorf(codes.AlreadyExists, "share %q already exists with different parameters", name)
		}
		return nil
	}
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
		if sizeBytes > 0 {
			spec.Filesystem = &storagev1alpha1.FilesystemConfig{Quota: resource.NewQuantity(sizeBytes, resource.BinarySI)}
		}
	case storagev1alpha1.DatasetTypeVolume:
		spec.Volume = &storagev1alpha1.VolumeConfig{
			Size:         *resource.NewQuantity(sizeBytes, resource.BinarySI),
			Volblocksize: rp.Volblocksize,
		}
	}
	return spec
}

// shareSpec builds the desired ZfsShare spec from resolved parameters.
func shareSpec(rp *ResolvedParams, dataset string) storagev1alpha1.ZfsShareSpec {
	spec := storagev1alpha1.ZfsShareSpec{
		PoolGUID: rp.PoolGUID,
		Dataset:  dataset,
		Protocol: rp.Protocol,
	}
	switch rp.Protocol {
	case storagev1alpha1.ProtocolNFS:
		spec.NFS = &storagev1alpha1.NFSExportSpec{Clients: rp.NFSClients}
	case storagev1alpha1.ProtocolNVMeoF:
		spec.NVMeoF = &storagev1alpha1.NVMeoFExportSpec{AllowedHosts: rp.NVMeoFAllowedHosts}
	}
	return spec
}

// volumeSpecCompatible reports whether an existing ZfsDataset can satisfy a
// repeated CreateVolume for the same name (identity + size must match).
func volumeSpecCompatible(existing, desired storagev1alpha1.ZfsDatasetSpec) bool {
	if existing.PoolGUID != desired.PoolGUID || existing.Dataset != desired.Dataset || existing.Type != desired.Type {
		return false
	}
	return quantityEqual(volumeSize(existing), volumeSize(desired))
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

func rpcCapability(t csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
		},
	}
}
