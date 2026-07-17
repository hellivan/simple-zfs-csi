package csi

import (
	"context"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// markSnapshotReadyAsync flips a ZfsSnapshot to Ready once it appears,
// simulating the agent taking the ZFS snapshot.
func markSnapshotReadyAsync(cl client.Client, name string) {
	go func() {
		for i := 0; i < 200; i++ {
			snap := &storagev1alpha1.ZfsSnapshot{}
			if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, snap); err == nil {
				snap.Status.Phase = storagev1alpha1.SnapshotPhaseReady
				snap.Status.ReadyToUse = true
				ct := metav1.NewTime(time.Unix(1700000000, 0).UTC())
				snap.Status.CreationTime = &ct
				snap.Status.RestoreSize = resource.NewQuantity(1<<20, resource.BinarySI)
				_ = cl.Status().Update(context.Background(), snap)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func sourceDataset(name string) *storagev1alpha1.ZfsDataset {
	return &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/" + name,
			Type:     storagev1alpha1.DatasetTypeFilesystem,
		},
	}
}

func TestCreateSnapshot_CreatesAndReturnsReady(t *testing.T) {
	cl := newTestClient(t, sourceDataset("pvc-1"))
	cs := newController(cl)
	markSnapshotReadyAsync(cl, "snap-1")

	resp, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snap-1",
		SourceVolumeId: "pvc-1",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap := resp.GetSnapshot()
	if snap.GetSnapshotId() != "snap-1" || snap.GetSourceVolumeId() != "pvc-1" {
		t.Errorf("snapshot id/source = %q/%q", snap.GetSnapshotId(), snap.GetSourceVolumeId())
	}
	if !snap.GetReadyToUse() {
		t.Errorf("snapshot not ready to use")
	}
	if snap.GetSizeBytes() != 1<<20 {
		t.Errorf("size = %d, want %d", snap.GetSizeBytes(), 1<<20)
	}
	if snap.GetCreationTime() == nil {
		t.Errorf("creation time not set")
	}

	got := &storagev1alpha1.ZfsSnapshot{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, got); err != nil {
		t.Fatalf("get ZfsSnapshot: %v", err)
	}
	if got.Spec.PoolGUID != "999" || got.Spec.Dataset != "k8s/pvc-1" || got.Spec.SnapshotName != "snap-1" || got.Spec.SourceVolume != "pvc-1" {
		t.Errorf("snapshot spec = %+v", got.Spec)
	}
}

func TestCreateSnapshot_MissingSourceVolume(t *testing.T) {
	cl := newTestClient(t)
	cs := newController(cl)
	_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-x", SourceVolumeId: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestCreateSnapshot_Idempotent(t *testing.T) {
	cl := newTestClient(t, sourceDataset("pvc-1"))
	cs := newController(cl)
	markSnapshotReadyAsync(cl, "snap-1")
	if _, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: "pvc-1"}); err != nil {
		t.Fatalf("first CreateSnapshot: %v", err)
	}
	if _, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: "pvc-1"}); err != nil {
		t.Fatalf("second CreateSnapshot should be idempotent: %v", err)
	}
}

func TestCreateSnapshot_DifferentSourceConflicts(t *testing.T) {
	cl := newTestClient(t, sourceDataset("pvc-1"), sourceDataset("pvc-2"))
	cs := newController(cl)
	markSnapshotReadyAsync(cl, "snap-1")
	if _, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: "pvc-1"}); err != nil {
		t.Fatalf("first CreateSnapshot: %v", err)
	}
	_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1", SourceVolumeId: "pvc-2"})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists for reused name, got %v", err)
	}
}

func TestDeleteSnapshot_RemovesObject(t *testing.T) {
	existing := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec:       storagev1alpha1.ZfsSnapshotSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", SnapshotName: "snap-1", SourceVolume: "pvc-1"},
	}
	cl := newTestClient(t, existing)
	cs := newController(cl)
	if _, err := cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	got := &storagev1alpha1.ZfsSnapshot{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, got); err == nil {
		t.Fatalf("snapshot still present after delete")
	}
	// Deleting a missing snapshot is a no-op success.
	if _, err := cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "snap-1"}); err != nil {
		t.Fatalf("idempotent DeleteSnapshot: %v", err)
	}
}

func TestListSnapshots_FiltersAndReports(t *testing.T) {
	s1 := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec:       storagev1alpha1.ZfsSnapshotSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", SnapshotName: "snap-1", SourceVolume: "pvc-1"},
		Status:     storagev1alpha1.ZfsSnapshotStatus{ReadyToUse: true, RestoreSize: resource.NewQuantity(1<<20, resource.BinarySI)},
	}
	s2 := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-2"},
		Spec:       storagev1alpha1.ZfsSnapshotSpec{PoolGUID: "999", Dataset: "k8s/pvc-2", SnapshotName: "snap-2", SourceVolume: "pvc-2"},
	}
	cl := newTestClient(t, s1, s2)
	cs := newController(cl)

	all, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(all.GetEntries()) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all.GetEntries()))
	}

	bySource, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SourceVolumeId: "pvc-1"})
	if err != nil {
		t.Fatalf("ListSnapshots by source: %v", err)
	}
	if len(bySource.GetEntries()) != 1 || bySource.GetEntries()[0].GetSnapshot().GetSnapshotId() != "snap-1" {
		t.Fatalf("source filter failed: %+v", bySource.GetEntries())
	}

	byID, err := cs.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: "snap-2"})
	if err != nil {
		t.Fatalf("ListSnapshots by id: %v", err)
	}
	if len(byID.GetEntries()) != 1 || byID.GetEntries()[0].GetSnapshot().GetSnapshotId() != "snap-2" {
		t.Fatalf("id filter failed: %+v", byID.GetEntries())
	}
}
