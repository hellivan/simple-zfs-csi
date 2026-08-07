package csi

import (
	"context"
	"path"
	"strconv"
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
// self-snapshot every backing clone carries.
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

// checkSamePrefix implements D6 (docs/snapshot-lifecycle-redesign.md): a
// clone/restore whose origin lives under a different datasetPrefix than the new
// volume would leave that volume's clone-origin outside its own replicated
// subtree, silently breaking `zfs send -R <prefix>` backup replication (§2.5).
// Reject it outright rather than letting the footgun exist.
//
// D6 says this applies to direct PVC-to-PVC clones too — the clone's origin
// snapshot lives under the source's prefix there as well. sourceDataset is
// whichever dataset physically holds the origin: the snapshot's backing clone
// for a restore, the source volume itself for a volume clone.
func checkSamePrefix(rp *ResolvedParams, sourceDataset, kind, id string) error {
	srcPrefix := path.Dir(strings.Trim(sourceDataset, "/"))
	if want := normalizedDatasetPrefix(rp.DatasetPrefix); srcPrefix != want {
		return status.Errorf(codes.InvalidArgument,
			"cross-prefix %s unsupported: %q's data lives under prefix %q, target prefix is %q",
			kind, id, srcPrefix, want)
	}
	return nil
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
		if err := c.checkCloneCompatibility(ctx, rp, snap, req.GetVolumeCapabilities()); err != nil {
			return nil, err
		}

		// Restores always clone from the backing clone's own "@restore-source"
		// self-snapshot (D0/D15), never from the raw snapshot directly, so
		// restoring keeps working whether the source volume is still alive,
		// deleted-but-not-yet-promoted, or already promoted away — and, unlike a
		// reference to the raw snapshot, it survives a promote relocating that
		// snapshot onto another dataset (§11.1).
		//
		// The backing clone is not an object (ADR-0030), so its path is derived
		// the same way the agent derives it: a flat sibling of the source dataset
		// named after Spec.SnapshotName. The "is it usable?" precondition becomes
		// the ZfsSnapshot's own state, which is more direct than the old check on
		// the backing clone object's deletionTimestamp — a snapshot that is
		// terminating or not yet Ready has no usable @restore-source.
		if !snap.DeletionTimestamp.IsZero() {
			return nil, status.Errorf(codes.FailedPrecondition, "snapshot %q is being deleted; retry", id)
		}
		if snap.Status.Phase != storagev1alpha1.SnapshotPhaseReady {
			return nil, status.Errorf(codes.FailedPrecondition,
				"snapshot %q is in phase %q, not yet restorable; retry", id, snap.Status.Phase)
		}
		backingDataset := path.Join(path.Dir(strings.Trim(c.snapshotSourceDataset(ctx, snap), "/")), snap.Spec.SnapshotName)
		if err := checkSamePrefix(rp, backingDataset, "restore", id); err != nil {
			return nil, err
		}
		return &storagev1alpha1.DatasetSource{Snapshot: backingDataset + "@" + restoreSourceSnapshotName}, nil

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
		// D6 applies here too: ADR-0009's intermediate "@clone-<hash>" snapshot is
		// taken on the source dataset, so a cross-prefix clone has the identical
		// `zfs send -R` locality problem as a cross-prefix restore.
		if err := checkSamePrefix(rp, src.Spec.Dataset, "clone", id); err != nil {
			return nil, err
		}
		if err := checkCloneCompatibility(rp, src, requestedFsType(req.GetVolumeCapabilities())); err != nil {
			return nil, err
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

// snapshotSourceDataset resolves the pool-relative path of the dataset a
// snapshot was taken on: the source ZfsDataset's current Spec.Dataset while that
// object exists, otherwise the copy recorded on the snapshot at creation time.
// A source renamed after the snapshot was taken therefore still restores
// (ADR-0025) — the ZFS objects move with the dataset, but the recorded copy
// cannot follow.
//
// The backing clone's path is derived from this (ADR-0030), mirroring exactly
// what ZfsSnapshotReconciler does when it creates the clone.
func (c *ControllerServer) snapshotSourceDataset(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot) string {
	if snap.Spec.SourceVolume == "" {
		return snap.Spec.Dataset
	}
	ds := &storagev1alpha1.ZfsDataset{}
	if err := c.Client.Get(ctx, client.ObjectKey{Name: snap.Spec.SourceVolume}, ds); err != nil {
		return snap.Spec.Dataset
	}
	return ds.Spec.Dataset
}

// requestedFsType returns the fsType carried by the first Mount capability, or
// "" if none is present (e.g. a Block volumeMode request).
func requestedFsType(caps []*csi.VolumeCapability) string {
	for _, c := range caps {
		if m := c.GetMount(); m != nil && m.GetFsType() != "" {
			return m.GetFsType()
		}
	}
	return ""
}

// volblocksizeBytes parses a ZFS volblocksize string (e.g. "16k", "8192") into
// bytes, mirroring internal/controller/zfsdataset_controller.go's volblockBytes
// so two independently-formatted values (e.g. "16k" vs "16384") compare
// correctly. Defaults to the modern OpenZFS zvol default (16 KiB) when
// empty/unparseable, matching the same convention.
func volblocksizeBytes(s string) int64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 16384
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k':
		mult, s = 1024, s[:len(s)-1]
	case 'm':
		mult, s = 1024*1024, s[:len(s)-1]
	case 'g':
		mult, s = 1024*1024*1024, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 16384
	}
	return n * mult
}

// checkCloneCompatibility implements D10 (docs/snapshot-lifecycle-redesign.md
// §2.7): rejects a clone/restore whose resolved volblocksize, ZFS property
// overrides, or requested fsType would silently diverge from the source's
// actual structure. Kubernetes explicitly permits cross-StorageClass
// cloning/restore with no compatibility checking of its own, so this is
// entirely the driver's responsibility. src may be nil (source deleted): the
// checks are then skipped entirely (nothing to compare against, not treated as
// a mismatch) — the same "absence means unconstrained" convention as
// Status.FSType itself.
func checkCloneCompatibility(rp *ResolvedParams, src *storagev1alpha1.ZfsDataset, requestedFS string) error {
	if src == nil {
		return nil
	}
	if rp.DatasetType == storagev1alpha1.DatasetTypeVolume && src.Spec.Volume != nil {
		if want, have := volblocksizeBytes(rp.Volblocksize), volblocksizeBytes(src.Spec.Volume.Volblocksize); want != have {
			return status.Errorf(codes.InvalidArgument,
				"volblocksize mismatch: target resolves to %d bytes, source %q is %d bytes (a clone/restore cannot change volblocksize)",
				want, src.Name, have)
		}
		if requestedFS != "" && src.Status.FSType != "" && requestedFS != src.Status.FSType {
			return status.Errorf(codes.InvalidArgument,
				"fsType mismatch: requested %q, source %q was formatted as %q (a clone/restore cannot change the on-disk filesystem)",
				requestedFS, src.Name, src.Status.FSType)
		}
	}
	for k, v := range rp.Properties {
		if have, ok := src.Spec.Properties[k]; ok && have != v {
			return status.Errorf(codes.InvalidArgument,
				"property %q mismatch: target requests %q, source %q has %q (a clone/restore cannot change structural ZFS properties)",
				k, v, src.Name, have)
		}
	}
	return nil
}

// checkCloneCompatibility (snapshot-restore variant) prefers the live source
// volume and falls back to the structural properties captured on the
// ZfsSnapshot at creation time (D25) when it is gone.
//
// That fallback is the important half: for a snapshot the
// source outliving the snapshot is the exception, not the rule, so looking the
// source up live and returning "compatible" when absent left D10's checks dead
// exactly where they matter. A mismatched fsType then surfaced much later as a
// NodeStageVolume failure, and a mismatched volblocksize was ignored entirely
// and silently (clone() never passes it, so ZFS raises nothing either).
//
// Deliberately does not use the backing clone as the comparison
// source: a backing clone is never mounted (canmount=off/volmode=none), so its
// own Status.FSType would always be empty regardless of what the real source
// was formatted as.
func (c *ControllerServer) checkCloneCompatibility(ctx context.Context, rp *ResolvedParams, snap *storagev1alpha1.ZfsSnapshot, caps []*csi.VolumeCapability) error {
	if snap.Spec.SourceVolume != "" {
		src := &storagev1alpha1.ZfsDataset{}
		if err := c.Client.Get(ctx, client.ObjectKey{Name: snap.Spec.SourceVolume}, src); err == nil {
			return checkCloneCompatibility(rp, src, requestedFsType(caps))
		}
	}
	return checkCloneCompatibility(rp, capturedSourceDataset(snap), requestedFsType(caps))
}

// capturedSourceDataset rebuilds just enough of the (possibly long-gone) source
// ZfsDataset from what CreateSnapshot recorded on the ZfsSnapshot, so the same
// Spec-to-Spec comparison can run against it. Returns nil when the snapshot
// carries no captured structure at all (created before D25), which
// checkCloneCompatibility treats as "unconstrained" — the same convention as an
// empty Status.FSType.
func capturedSourceDataset(snap *storagev1alpha1.ZfsSnapshot) *storagev1alpha1.ZfsDataset {
	if snap.Spec.SourceFSType == "" && snap.Spec.SourceVolblocksize == "" && len(snap.Spec.SourceProperties) == 0 {
		return nil
	}
	name := snap.Spec.SourceVolume
	if name == "" {
		name = snap.Name
	}
	ds := &storagev1alpha1.ZfsDataset{}
	ds.Name = name
	ds.Spec.Type = snap.Spec.SourceType
	ds.Spec.Properties = snap.Spec.SourceProperties
	ds.Status.FSType = snap.Spec.SourceFSType
	if snap.Spec.SourceVolblocksize != "" {
		ds.Spec.Volume = &storagev1alpha1.VolumeConfig{Volblocksize: snap.Spec.SourceVolblocksize}
	}
	return ds
}
