package csi

import (
	"context"
	"path"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// restoreSourceSnapshotName mirrors internal/controller's constant of the same
// name (D5, snapshot-lifecycle-redesign.md): the fixed, CSI-invisible
// self-snapshot every standalone-mode backing-clone ZfsDataset carries.
// Restores always clone from this, never from the raw origin snapshot.
const restoreSourceSnapshotName = "restore-source"

// normalizedDatasetPrefix normalises a StorageClass datasetPrefix for
// comparison against path.Dir() of a full dataset path: path.Dir() of a
// single-segment (no-prefix) path returns ".", so an empty prefix must
// normalise to the same value for the D6 cross-prefix check to compare
// correctly.
func normalizedDatasetPrefix(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "."
	}
	return prefix
}

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
		if srcType := c.snapshotSourceType(ctx, snap); srcType != "" && srcType != rp.DatasetType {
			return nil, status.Errorf(codes.InvalidArgument, "cannot restore a %s snapshot into a %s (protocol %s) volume", srcType, rp.DatasetType, rp.Protocol)
		}

		if effectiveMode(snap.Spec) != storagev1alpha1.SnapshotModeStandalone {
			// integrated mode: unchanged, clone directly from the raw snapshot.
			return &storagev1alpha1.DatasetSource{Snapshot: snap.Spec.Dataset + "@" + snap.Spec.SnapshotName}, nil
		}

		// standalone mode (D0/D15): restores always clone from the backing
		// clone's own "@restore-source" self-snapshot, never from the raw
		// snapshot directly, so restoring keeps working whether the source
		// volume is still alive, deleted-but-not-yet-promoted, or promoted away.
		backing := &storagev1alpha1.ZfsDataset{}
		if err := c.Client.Get(ctx, client.ObjectKey{Name: snap.Spec.SnapshotName}, backing); err != nil {
			return nil, status.Errorf(codes.Internal, "get backing clone %q for snapshot %q: %v", snap.Spec.SnapshotName, id, err)
		}
		// D6: restoring into a different datasetPrefix than the source would
		// leave the new volume's clone-origin outside its own replicated subtree
		// (zfs send -R backup compatibility, §2.5) — reject outright rather than
		// let that footgun exist silently.
		if path.Dir(strings.Trim(backing.Spec.Dataset, "/")) != normalizedDatasetPrefix(rp.DatasetPrefix) {
			return nil, status.Errorf(codes.InvalidArgument,
				"cross-prefix restore unsupported: snapshot %q's data lives under prefix %q, target prefix is %q",
				id, path.Dir(strings.Trim(backing.Spec.Dataset, "/")), normalizedDatasetPrefix(rp.DatasetPrefix))
		}
		if err := c.addRestoredByFinalizer(ctx, backing.Name, req.GetName()); err != nil {
			return nil, err
		}
		return &storagev1alpha1.DatasetSource{Snapshot: backing.Spec.Dataset + "@" + restoreSourceSnapshotName}, nil

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

// snapshotSourceType returns the ZFS dataset type (filesystem or volume) of a
// snapshot's source, preferring the type recorded on the ZfsSnapshot itself
// (captured at creation time, so it survives the source being deleted later,
// e.g. the original PVC was removed but the snapshot retained). Falls back to
// a live lookup of the source ZfsDataset for snapshots created before
// SourceType existed. Returns "" when neither is available.
func (c *ControllerServer) snapshotSourceType(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot) storagev1alpha1.DatasetType {
	if snap.Spec.SourceType != "" {
		return snap.Spec.SourceType
	}
	return c.sourceDatasetType(ctx, snap.Spec.SourceVolume)
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
