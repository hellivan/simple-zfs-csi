package controller

import (
	"context"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// TestZfsSnapshotReconcile_DestroysRelocatedRawSnapshot is the §12/ADR-0028
// regression.
//
// vol1 carries csi-snap-a (older) and csi-snap-b, each with its own backing
// clone, and a PVC was restored from snap-b. Deleting snap-b promotes the
// restored PVC, which relocates *both* raw snapshots onto it — including
// csi-snap-a, which belongs to a snapshot nobody touched.
//
// Before ADR-0028, deleting snap-a then computed its recorded address
// (tank/k8s/vol1@csi-snap-a), found nothing there, and released the finalizer
// anyway because Destroy is NotExist-tolerant — leaving the real snapshot
// orphaned on the restored PVC with nothing referencing it. The delete path now
// asks ZFS where the snapshot actually is, so it is destroyed wherever it moved.
func TestZfsSnapshotReconcile_DestroysRelocatedRawSnapshot(t *testing.T) {
	scheme := newTestScheme(t)
	ctx := context.Background()

	mkSnap := func(name, suffix string) *storagev1alpha1.ZfsSnapshot {
		return &storagev1alpha1.ZfsSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{zfsSnapshotFinalizer}},
			Spec: storagev1alpha1.ZfsSnapshotSpec{
				PoolGUID: "999", Dataset: "k8s/vol1", SnapshotName: suffix, SourceVolume: "vol1",
			},
			Status: storagev1alpha1.ZfsSnapshotStatus{Phase: storagev1alpha1.SnapshotPhaseReady, ReadyToUse: true},
		}
	}
	snapA, snapB := mkSnap("snap-a", "csi-snap-a"), mkSnap("snap-b", "csi-snap-b")
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "vol1", Finalizers: []string{zfsDatasetFinalizer}},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/vol1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	objs := []client.Object{onlinePool(), vol, snapA, snapB}

	z := newFakeZFS("tank/k8s/vol1")
	for _, n := range []string{"a", "b"} {
		owner := snapA
		if n == "b" {
			owner = snapB
		}
		objs = append(objs, &storagev1alpha1.ZfsDataset{
			ObjectMeta: metav1.ObjectMeta{
				Name: "csi-snap-" + n, Finalizers: []string{zfsDatasetFinalizer},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(owner, storagev1alpha1.GroupVersion.WithKind("ZfsSnapshot")),
				},
			},
			Spec: storagev1alpha1.ZfsDatasetSpec{
				PoolGUID: "999", Dataset: "k8s/csi-snap-" + n, Type: storagev1alpha1.DatasetTypeFilesystem,
				Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/vol1@csi-snap-" + n},
			},
		})
		z.seedClone("tank/k8s/vol1", "csi-snap-"+n, "tank/k8s/csi-snap-"+n)
		z.seedSnapshot("tank/k8s/csi-snap-"+n, restoreSourceSnapshotName)
	}
	z.seedClone("tank/k8s/csi-snap-b", restoreSourceSnapshotName, "tank/k8s/pvc-restored")
	objs = append(objs, &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-restored", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-restored", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/csi-snap-b@" + restoreSourceSnapshotName},
		},
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).Build()
	snapR := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	dsR := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}

	drive := func(t *testing.T, snapName string, dsNames ...string) {
		t.Helper()
		for i := 0; i < 12; i++ {
			if _, err := snapR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: snapName}}); err != nil {
				t.Fatalf("reconcile %s: %v", snapName, err)
			}
			for _, d := range dsNames {
				if _, err := dsR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: d}}); err != nil {
					t.Fatalf("reconcile %s: %v", d, err)
				}
			}
		}
	}
	locate := func(suffix string) []string {
		var out []string
		for name, ok := range z.existing {
			if ok && strings.HasSuffix(name, "@"+suffix) {
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return out
	}

	// Deleting snap-b promotes the restored PVC, which drags the untouched
	// csi-snap-a off vol1 along with it.
	if err := c.Delete(ctx, snapB); err != nil {
		t.Fatalf("delete snap-b: %v", err)
	}
	drive(t, "snap-b", "csi-snap-b", "pvc-restored")

	relocated := locate("csi-snap-a")
	if len(relocated) != 1 || relocated[0] == "tank/k8s/vol1@csi-snap-a" {
		t.Fatalf("precondition: expected csi-snap-a to have been relocated off vol1, got %v", relocated)
	}
	t.Logf("csi-snap-a was relocated to %v while its ZfsSnapshot still records k8s/vol1", relocated)

	// Deleting snap-a must now destroy the snapshot where it actually is.
	if err := c.Delete(ctx, snapA); err != nil {
		t.Fatalf("delete snap-a: %v", err)
	}
	drive(t, "snap-a", "csi-snap-a", "pvc-restored")

	var gone storagev1alpha1.ZfsSnapshot
	if err := c.Get(ctx, client.ObjectKey{Name: "snap-a"}, &gone); err == nil {
		t.Fatalf("ZfsSnapshot snap-a still present, finalizer not released")
	}
	if left := locate("csi-snap-a"); len(left) != 0 {
		t.Errorf("raw snapshot orphaned after its ZfsSnapshot was deleted: %v", left)
	}
}

// TestFakeZFSFindSnapshot_MatchesAfterAtOnly guards the one way a pool-wide
// search by short name could go wrong: a backing clone is itself named
// csi-snap-<uuid>, so its own "@restore-source" snapshot renders as
// "…/csi-snap-<uuid>@restore-source". Matching must key on the part after "@",
// never on the dataset component, or deleting a snapshot would find its own
// backing clone's self-snapshot instead.
func TestFakeZFSFindSnapshot_MatchesAfterAtOnly(t *testing.T) {
	ctx := context.Background()
	z := newFakeZFS("tank/k8s/vol1")
	z.seedClone("tank/k8s/vol1", "csi-snap-x", "tank/k8s/csi-snap-x")
	z.seedSnapshot("tank/k8s/csi-snap-x", restoreSourceSnapshotName)

	got, err := z.FindSnapshot(ctx, "tank", "csi-snap-x")
	if err != nil {
		t.Fatalf("FindSnapshot: %v", err)
	}
	if got != "tank/k8s/vol1@csi-snap-x" {
		t.Errorf("FindSnapshot = %q, want the raw snapshot tank/k8s/vol1@csi-snap-x", got)
	}

	got, err = z.FindSnapshot(ctx, "tank", "restore-source")
	if err != nil {
		t.Fatalf("FindSnapshot: %v", err)
	}
	if got != "tank/k8s/csi-snap-x@restore-source" {
		t.Errorf("FindSnapshot = %q, want tank/k8s/csi-snap-x@restore-source", got)
	}

	if got, err = z.FindSnapshot(ctx, "tank", "csi-snap-absent"); err != nil || got != "" {
		t.Errorf("FindSnapshot(absent) = %q, %v; want \"\", nil", got, err)
	}
}
