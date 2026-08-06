package csi

import (
	"context"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

func snapshotObj(name, pool, dataset, source string) *storagev1alpha1.ZfsSnapshot {
	return &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       storagev1alpha1.ZfsSnapshotSpec{PoolGUID: pool, Dataset: dataset, SnapshotName: name, SourceVolume: source},
	}
}

func snapshotSource(id string) *csi.VolumeContentSource {
	return &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: id}}}
}

func volumeSource(id string) *csi.VolumeContentSource {
	return &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: id}}}
}

func TestCreateVolume_RestoreFromSnapshot(t *testing.T) {
	// Every snapshot owns a backing clone, and restores clone that clone's
	// "@restore-source" self-snapshot rather than the raw snapshot (§11).
	backing := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/snap-1", Type: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	cl := newTestClient(t, sourceDataset("pvc-src"), snapshotObj("snap-1", "999", "k8s/pvc-src", "pvc-src"), backing)
	cs := newController(cl)
	markReadyAsync(cl, "pvc-restore")

	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-restore",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nfs", "datasetPrefix": "k8s"},
		VolumeContentSource: snapshotSource("snap-1"),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.GetVolume().GetContentSource().GetSnapshot().GetSnapshotId() != "snap-1" {
		t.Errorf("content source not echoed: %+v", resp.GetVolume().GetContentSource())
	}

	vol := &storagev1alpha1.ZfsDataset{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-restore"}, vol); err != nil {
		t.Fatalf("get ZfsDataset: %v", err)
	}
	if vol.Spec.Source == nil || vol.Spec.Source.Snapshot != "k8s/snap-1@restore-source" {
		t.Errorf("clone source = %+v, want snapshot k8s/snap-1@restore-source", vol.Spec.Source)
	}
}

// TestCreateVolume_RestorePropertyMismatchRejected verifies D10 on the
// restore-from-snapshot path: the source volume is looked up live via
// ZfsSnapshot.Spec.SourceVolume rather than through the backing clone, which is
// never mounted and so never has a useful Status.FSType of its own.
func TestCreateVolume_RestorePropertyMismatchRejected(t *testing.T) {
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", Type: storagev1alpha1.DatasetTypeFilesystem,
			Properties: map[string]string{"compression": "lz4"},
		},
	}
	cl := newTestClient(t, src, snapshotObj("snap-1", "999", "k8s/pvc-src", "pvc-src"))
	cs := newController(cl)
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-restore",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nfs", "property.compression": "gzip"},
		VolumeContentSource: snapshotSource("snap-1"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for property mismatch, got %v", err)
	}
}

func TestCreateVolume_CloneFromVolume(t *testing.T) {
	cl := newTestClient(t, sourceDataset("pvc-src"))
	cs := newController(cl)
	markReadyAsync(cl, "pvc-clone")

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-clone",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nfs", "datasetPrefix": "k8s"},
		VolumeContentSource: volumeSource("pvc-src"),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vol := &storagev1alpha1.ZfsDataset{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-clone"}, vol); err != nil {
		t.Fatalf("get ZfsDataset: %v", err)
	}
	if vol.Spec.Source == nil || vol.Spec.Source.Volume != "k8s/pvc-src" {
		t.Errorf("clone source = %+v, want volume k8s/pvc-src", vol.Spec.Source)
	}
}

// TestCreateVolume_CloneVolblocksizeMismatchRejected verifies D10
// (docs/snapshot-lifecycle-redesign.md §2.7): a clone target resolving to a
// different volblocksize than the source zvol actually has is rejected — ZFS
// itself would reject this at the `zfs clone` layer, but only with an opaque
// error; the driver should reject it earlier with a clear one.
func TestCreateVolume_CloneVolblocksizeMismatchRejected(t *testing.T) {
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", Type: storagev1alpha1.DatasetTypeVolume,
			Volume: &storagev1alpha1.VolumeConfig{Size: resource.MustParse("1Gi"), Volblocksize: "8k"},
		},
	}
	cl := newTestClient(t, src)
	cs := newController(cl)
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-clone",
		VolumeCapabilities:  blockCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nvmeof", "volblocksize": "16k"},
		VolumeContentSource: volumeSource("pvc-src"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for volblocksize mismatch, got %v", err)
	}
}

// TestCreateVolume_ClonePropertyMismatchRejected verifies D10: an explicit
// property.* override that differs from what the source actually has is
// rejected rather than silently applied as a partial, confusing override.
func TestCreateVolume_ClonePropertyMismatchRejected(t *testing.T) {
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", Type: storagev1alpha1.DatasetTypeFilesystem,
			Properties: map[string]string{"compression": "lz4"},
		},
	}
	cl := newTestClient(t, src)
	cs := newController(cl)
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-clone",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nfs", "property.compression": "gzip"},
		VolumeContentSource: volumeSource("pvc-src"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for property mismatch, got %v", err)
	}
}

// TestCreateVolume_CloneFsTypeMismatchRejected verifies D10: a clone/restore
// requesting a different fsType than the source zvol was actually formatted
// with (ZfsDataset.Status.FSType, set once by the node plugin) is rejected —
// otherwise NodeStageVolume would fail later with an opaque bad-superblock
// mount error instead of a clear CreateVolume-time rejection.
func TestCreateVolume_CloneFsTypeMismatchRejected(t *testing.T) {
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", Type: storagev1alpha1.DatasetTypeVolume,
			Volume: &storagev1alpha1.VolumeConfig{Size: resource.MustParse("1Gi")},
		},
		Status: storagev1alpha1.ZfsDatasetStatus{FSType: "xfs"},
	}
	cl := newTestClient(t, src)
	cs := newController(cl)
	caps := []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-clone",
		VolumeCapabilities:  caps,
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nvmeof"},
		VolumeContentSource: volumeSource("pvc-src"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for fsType mismatch, got %v", err)
	}
}

func TestCreateVolume_RestoreMissingSnapshot(t *testing.T) {
	cl := newTestClient(t)
	cs := newController(cl)
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-restore",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nfs"},
		VolumeContentSource: snapshotSource("nope"),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestCreateVolume_RestoreCrossPoolRejected(t *testing.T) {
	cl := newTestClient(t, sourceDataset("pvc-src"), snapshotObj("snap-1", "111", "k8s/pvc-src", "pvc-src"))
	cs := newController(cl)
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-restore",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nfs"},
		VolumeContentSource: snapshotSource("snap-1"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for cross-pool restore, got %v", err)
	}
}

func TestCreateVolume_RestoreTypeMismatchRejected(t *testing.T) {
	// source is a filesystem; requesting nvmeof (zvol) must be rejected.
	cl := newTestClient(t, sourceDataset("pvc-src"), snapshotObj("snap-1", "999", "k8s/pvc-src", "pvc-src"))
	cs := newController(cl)
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-restore",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nvmeof"},
		VolumeContentSource: snapshotSource("snap-1"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for type mismatch, got %v", err)
	}
}

// TestCreateVolume_RestoreTypeMismatchRejectedAfterSourceDeleted verifies the
// type check still fires when the source ZfsDataset no longer exists (e.g. the
// original PVC was deleted but the snapshot was retained), relying on the
// SourceType recorded on the ZfsSnapshot at creation time rather than a live
// lookup of the (now-gone) source.
func TestCreateVolume_RestoreTypeMismatchRejectedAfterSourceDeleted(t *testing.T) {
	snap := snapshotObj("snap-1", "999", "k8s/pvc-src", "pvc-src")
	snap.Spec.SourceType = storagev1alpha1.DatasetTypeFilesystem
	// No sourceDataset("pvc-src") in the fake client: the source PVC is gone.
	cl := newTestClient(t, snap)
	cs := newController(cl)
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-restore",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nvmeof"},
		VolumeContentSource: snapshotSource("snap-1"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for type mismatch with deleted source, got %v", err)
	}
}

// TestCreateVolume_RestoreClonesFromBackingClone verifies D0/D15: a
// snapshot's restore clones from its backing clone's
// "@restore-source" self-snapshot, never the raw origin snapshot, and records
// no dependency bookkeeping on that backing clone (D17).
func TestCreateVolume_RestoreClonesFromBackingClone(t *testing.T) {
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-src", SourceType: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	backing := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-x"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/csi-snap-x", Type: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	cl := newTestClient(t, sourceDataset("pvc-src"), snap, backing)
	cs := newController(cl)
	markReadyAsync(cl, "pvc-restore")

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-restore",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nfs", "datasetPrefix": "k8s"},
		VolumeContentSource: snapshotSource("snap-1"),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	vol := &storagev1alpha1.ZfsDataset{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-restore"}, vol); err != nil {
		t.Fatalf("get ZfsDataset: %v", err)
	}
	if vol.Spec.Source == nil || vol.Spec.Source.Snapshot != "k8s/csi-snap-x@restore-source" {
		t.Errorf("clone source = %+v, want snapshot k8s/csi-snap-x@restore-source", vol.Spec.Source)
	}

	// D17 removed the restored-by.* tracking finalizer entirely: the agent's
	// delete path discovers dependents from the live ZFS clone graph, so a
	// restore records nothing on the backing clone beyond the ZfsDataset it
	// creates. All that remains here is the precondition check.
	var gotBacking storagev1alpha1.ZfsDataset
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "csi-snap-x"}, &gotBacking); err != nil {
		t.Fatalf("get backing clone: %v", err)
	}
	for _, f := range gotBacking.Finalizers {
		if strings.HasPrefix(f, "storage.simple-zfs-csi.io/restored-by.") ||
			strings.HasPrefix(f, "storage.simple-zfs-csi.io/promoted-onto.") {
			t.Errorf("backing clone still carries dependency-tracking finalizer %q; D17 removed them", f)
		}
	}
}

// TestCreateVolume_RestoreCrossPrefixRejected verifies D6: restoring into a
// different datasetPrefix than the backing clone's own prefix
// is rejected outright (would break zfs send -R backup replication, §2.5).
func TestCreateVolume_RestoreCrossPrefixRejected(t *testing.T) {
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-src", SourceType: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	backing := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-x"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/csi-snap-x", Type: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	cl := newTestClient(t, snap, backing)
	cs := newController(cl)

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-restore",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nfs", "datasetPrefix": "other-prefix"},
		VolumeContentSource: snapshotSource("snap-1"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for cross-prefix restore, got %v", err)
	}
}

// TestCreateVolume_CloneCrossPrefixRejected verifies F5 for the direct
// PVC-to-PVC clone path, which had no cross-prefix check at all even though
// ADR-0009's intermediate "@clone-<hash>" snapshot is taken on the source and
// therefore lives under the source's prefix.
func TestCreateVolume_CloneCrossPrefixRejected(t *testing.T) {
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", Type: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	cl := newTestClient(t, src)
	cs := newController(cl)

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-clone",
		VolumeCapabilities:  mountCaps(),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nfs", "datasetPrefix": "other-prefix"},
		VolumeContentSource: volumeSource("pvc-src"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for cross-prefix clone, got %v", err)
	}
}

// TestCreateVolume_RestoreChecksCapturedSourceFSType verifies D25/F12: with the
// source volume deleted — the headline scenario for a snapshot — the
// D10 fsType check must still fire, using what CreateSnapshot captured on the
// ZfsSnapshot rather than a live lookup that returns nothing.
func TestCreateVolume_RestoreChecksCapturedSourceFSType(t *testing.T) {
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", SnapshotName: "csi-snap-x",
			// SourceVolume names a ZfsDataset that no longer exists.
			SourceVolume: "pvc-src", SourceType: storagev1alpha1.DatasetTypeVolume,
			SourceFSType:       "ext4",
			SourceVolblocksize: "16k",
		},
	}
	backing := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-x"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/csi-snap-x", Type: storagev1alpha1.DatasetTypeVolume,
		},
	}
	cl := newTestClient(t, snap, backing)
	cs := newController(cl)

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                "pvc-restore",
		VolumeCapabilities:  mountCapsWithFsType("xfs"),
		CapacityRange:       &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:          map[string]string{"poolGUID": "999", "protocol": "nvmeof", "datasetPrefix": "k8s"},
		VolumeContentSource: snapshotSource("snap-1"),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for fsType mismatch against captured source state, got %v", err)
	}
	if !strings.Contains(err.Error(), "fsType mismatch") {
		t.Errorf("error = %v, want an fsType mismatch", err)
	}
}
