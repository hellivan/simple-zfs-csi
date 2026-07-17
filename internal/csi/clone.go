package csi

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// resolveContentSource turns a CSI VolumeContentSource (snapshot or volume) into
// a ZfsDataset clone source. Clones are same-pool and same-type by ZFS
// constraint, so it validates that the source lives on the target pool and
// matches the protocol-derived dataset type. It returns nil when there is no
// content source (an empty create).
func (c *ControllerServer) resolveContentSource(ctx context.Context, req *csi.CreateVolumeRequest, rp *ResolvedParams) (*storagev1alpha1.DatasetSource, error) {
	cs := req.GetVolumeContentSource()
	if cs == nil {
		return nil, nil
	}

	switch cs.GetType().(type) {
	case *csi.VolumeContentSource_Snapshot:
		id := cs.GetSnapshot().GetSnapshotId()
		if id == "" {
			return nil, status.Error(codes.InvalidArgument, "snapshot content source has no snapshot id")
		}
		snap := &storagev1alpha1.ZfsSnapshot{}
		if err := c.Client.Get(ctx, client.ObjectKey{Name: id}, snap); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, status.Errorf(codes.NotFound, "source snapshot %q not found", id)
			}
			return nil, status.Errorf(codes.Internal, "get source snapshot %q: %v", id, err)
		}
		if snap.Spec.PoolGUID != rp.PoolGUID {
			return nil, status.Errorf(codes.InvalidArgument, "cross-pool restore unsupported: snapshot %q is on pool %s, target pool is %s", id, snap.Spec.PoolGUID, rp.PoolGUID)
		}
		if srcType := c.sourceDatasetType(ctx, snap.Spec.SourceVolume); srcType != "" && srcType != rp.DatasetType {
			return nil, status.Errorf(codes.InvalidArgument, "cannot restore a %s snapshot into a %s (protocol %s) volume", srcType, rp.DatasetType, rp.Protocol)
		}
		return &storagev1alpha1.DatasetSource{Snapshot: snap.Spec.Dataset + "@" + snap.Spec.SnapshotName}, nil

	case *csi.VolumeContentSource_Volume:
		id := cs.GetVolume().GetVolumeId()
		if id == "" {
			return nil, status.Error(codes.InvalidArgument, "volume content source has no volume id")
		}
		src := &storagev1alpha1.ZfsDataset{}
		if err := c.Client.Get(ctx, client.ObjectKey{Name: id}, src); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, status.Errorf(codes.NotFound, "source volume %q not found", id)
			}
			return nil, status.Errorf(codes.Internal, "get source volume %q: %v", id, err)
		}
		if src.Spec.PoolGUID != rp.PoolGUID {
			return nil, status.Errorf(codes.InvalidArgument, "cross-pool clone unsupported: volume %q is on pool %s, target pool is %s", id, src.Spec.PoolGUID, rp.PoolGUID)
		}
		if src.Spec.Type != rp.DatasetType {
			return nil, status.Errorf(codes.InvalidArgument, "cannot clone a %s volume into a %s (protocol %s) volume", src.Spec.Type, rp.DatasetType, rp.Protocol)
		}
		return &storagev1alpha1.DatasetSource{Volume: src.Spec.Dataset}, nil

	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported volume content source")
	}
}

// sourceDatasetType looks up the ZFS type of a source ZfsDataset by name,
// returning "" when it cannot be determined (e.g. the source was deleted).
func (c *ControllerServer) sourceDatasetType(ctx context.Context, name string) storagev1alpha1.DatasetType {
	if name == "" {
		return ""
	}
	ds := &storagev1alpha1.ZfsDataset{}
	if err := c.Client.Get(ctx, client.ObjectKey{Name: name}, ds); err != nil {
		return ""
	}
	return ds.Spec.Type
}
