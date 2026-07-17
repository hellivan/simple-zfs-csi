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
