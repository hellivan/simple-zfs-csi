package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

func TestSnapshotFullName(t *testing.T) {
	tests := []struct {
		pool, dataset, snap, want string
		wantErr                   bool
	}{
		{"tank", "k8s/pvc-1", "snap-1", "tank/k8s/pvc-1@snap-1", false},
		{"tank", "/media/movies/", "s", "tank/media/movies@s", false},
		{"", "x", "s", "", true},
		{"tank", "/", "s", "", true},
		{"tank", "x", "", "", true},
	}
	for _, tt := range tests {
		got, err := snapshotFullName(tt.pool, tt.dataset, tt.snap)
		if tt.wantErr {
			if err == nil {
				t.Errorf("snapshotFullName(%q,%q,%q) expected error", tt.pool, tt.dataset, tt.snap)
			}
			continue
		}
		if err != nil {
			t.Errorf("snapshotFullName(%q,%q,%q) unexpected error: %v", tt.pool, tt.dataset, tt.snap, err)
		}
		if got != tt.want {
			t.Errorf("snapshotFullName(%q,%q,%q) = %q, want %q", tt.pool, tt.dataset, tt.snap, got, tt.want)
		}
	}
}

func TestZfsSnapshotReconcile_CreatesSnapshotAndSetsReady(t *testing.T) {
	scheme := newTestScheme(t)
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID:     "999",
			Dataset:      "k8s/pvc-1",
			SnapshotName: "snap-1",
			SourceVolume: "pvc-1",
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}).
		Build()

	z := newFakeZFS()
	r := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}

	// First pass installs the finalizer; second pass snapshots and reports Ready.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	if len(z.createdDS) != 1 || z.createdDS[0] != "tank/k8s/pvc-1@snap-1" {
		t.Fatalf("expected snapshot tank/k8s/pvc-1@snap-1, got %v", z.createdDS)
	}

	var got storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, zfsSnapshotFinalizer) {
		t.Errorf("finalizer not set")
	}
	if got.Status.Phase != storagev1alpha1.SnapshotPhaseReady || !got.Status.ReadyToUse {
		t.Errorf("phase = %q readyToUse = %v, want Ready/true", got.Status.Phase, got.Status.ReadyToUse)
	}
	if got.Status.CreationTime == nil {
		t.Errorf("creation time not set")
	}
	if got.Status.RestoreSize == nil || got.Status.RestoreSize.Value() != 1048576 {
		t.Errorf("restore size = %v, want 1048576", got.Status.RestoreSize)
	}
}

func TestZfsSnapshotReconcile_IgnoresPoolOnOtherNode(t *testing.T) {
	scheme := newTestScheme(t)
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-2"},
		Spec:       storagev1alpha1.ZfsSnapshotSpec{PoolGUID: "999", Dataset: "k8s/pvc-2", SnapshotName: "snap-2"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}).
		Build()

	z := newFakeZFS()
	r := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-b", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-2"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(z.createdDS) != 0 {
		t.Fatalf("node-b should not snapshot a pool hosted on node-a, got %v", z.createdDS)
	}
}

func TestZfsSnapshotReconcile_DestroysOnDeletion(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "snap-3",
			Finalizers:        []string{zfsSnapshotFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: storagev1alpha1.ZfsSnapshotSpec{PoolGUID: "999", Dataset: "k8s/pvc-3", SnapshotName: "snap-3"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-3@snap-3")
	r := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-3"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(z.destroyed) != 1 || z.destroyed[0] != "tank/k8s/pvc-3@snap-3" {
		t.Fatalf("expected snapshot destroyed, got %v", z.destroyed)
	}
}
