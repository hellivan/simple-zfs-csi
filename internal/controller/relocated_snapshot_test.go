package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

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
	z.seedSnapshot("tank/k8s/csi-snap-x", legacyRestoreSourceSnapshot)

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

// TestZfsDatasetReconcile_RecordsDeleteRefusalOnStatus is the other half of
// ADR-0029: a refused destroy must leave the reason on the object, not only in
// this agent's logs, or DeleteVolume has nothing to report to the CO.
//
// The trigger used here is the realistic one: a snapshot taken on a PVC's
// dataset by an external replication or auto-snapshot tool, which
// assertDriverSnapshot (D18) correctly refuses to destroy.
func TestZfsDatasetReconcile_RecordsDeleteRefusalOnStatus(t *testing.T) {
	scheme := newTestScheme(t)
	ctx := context.Background()
	now := metav1.Now()

	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).Build()

	z := newFakeZFS("tank/k8s/pvc-1")
	z.seedSnapshot("tank/k8s/pvc-1", "auto-2026-08-06-0300")

	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err == nil {
		t.Fatal("expected the destroy to be refused")
	}
	if len(z.destroyed) != 0 {
		t.Fatalf("nothing may be destroyed when the guard refuses, got %v", z.destroyed)
	}

	var got storagev1alpha1.ZfsDataset
	if err := c.Get(ctx, client.ObjectKey{Name: "pvc-1"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	var found *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Reason == storagev1alpha1.DeleteBlockedReason {
			found = &got.Status.Conditions[i]
		}
	}
	if found == nil {
		t.Fatalf("no %q condition recorded; conditions = %+v", storagev1alpha1.DeleteBlockedReason, got.Status.Conditions)
	}
	if !strings.Contains(found.Message, "auto-2026-08-06-0300") {
		t.Errorf("condition message does not name the offending snapshot: %q", found.Message)
	}
	if !strings.Contains(got.Status.Message, "auto-2026-08-06-0300") {
		t.Errorf("status.message does not name the offending snapshot: %q", got.Status.Message)
	}
}
