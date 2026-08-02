package csi

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	cl := newTestClient(t, sourceDataset("pvc-src"), snapshotObj("snap-1", "999", "k8s/pvc-src", "pvc-src"))
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
	if vol.Spec.Source == nil || vol.Spec.Source.Snapshot != "k8s/pvc-src@snap-1" {
		t.Errorf("clone source = %+v, want snapshot k8s/pvc-src@snap-1", vol.Spec.Source)
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

// TestCreateVolume_RestoreFromStandaloneSnapshot verifies D0/D15: a
// standalone-mode snapshot's restore clones from its backing clone's
// "@restore-source" self-snapshot, never the raw origin snapshot, and
// registers a restored-by.<pvcName> finalizer (D4) on that backing clone.
func TestCreateVolume_RestoreFromStandaloneSnapshot(t *testing.T) {
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-src", SourceType: storagev1alpha1.DatasetTypeFilesystem,
			Mode: storagev1alpha1.SnapshotModeStandalone,
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

	var gotBacking storagev1alpha1.ZfsDataset
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "csi-snap-x"}, &gotBacking); err != nil {
		t.Fatalf("get backing clone: %v", err)
	}
	wantFinalizer := "storage.simple-zfs-csi.io/restored-by.pvc-restore"
	found := false
	for _, f := range gotBacking.Finalizers {
		if f == wantFinalizer {
			found = true
		}
	}
	if !found {
		t.Errorf("backing clone finalizers = %v, want %q present", gotBacking.Finalizers, wantFinalizer)
	}
}

// TestCreateVolume_RestoreCrossPrefixRejected verifies D6: restoring into a
// different datasetPrefix than the standalone-mode backing clone's own prefix
// is rejected outright (would break zfs send -R backup replication, §2.5).
func TestCreateVolume_RestoreCrossPrefixRejected(t *testing.T) {
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-src", SourceType: storagev1alpha1.DatasetTypeFilesystem,
			Mode: storagev1alpha1.SnapshotModeStandalone,
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
