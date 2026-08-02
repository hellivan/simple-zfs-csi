package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// TestZfsSnapshotReconcile_StandaloneCreatesBackingCloneAndSelfSnapshot drives
// both ZfsSnapshotReconciler and ZfsDatasetReconciler (as they'd run
// concurrently in the real DaemonSet) through a full standalone-mode create
// (D15): raw snapshot -> owned backing-clone ZfsDataset -> that dataset
// becomes Ready -> "@restore-source" self-snapshot -> ZfsSnapshot Ready.
func TestZfsSnapshotReconcile_StandaloneCreatesBackingCloneAndSelfSnapshot(t *testing.T) {
	scheme := newTestScheme(t)
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-src",
			Type:     storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID:     "999",
			Dataset:      "k8s/pvc-src",
			SnapshotName: "csi-snap-abc",
			SourceVolume: "pvc-src",
			SourceType:   storagev1alpha1.DatasetTypeFilesystem,
			Mode:         storagev1alpha1.SnapshotModeStandalone,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), src, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	snapR := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	dsR := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	snapReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}
	backingReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "csi-snap-abc"}}

	if _, err := snapR.Reconcile(context.Background(), snapReq); err != nil { // installs finalizer
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := snapR.Reconcile(context.Background(), snapReq); err != nil { // raw snapshot + backing-clone create
		t.Fatalf("reconcile 2: %v", err)
	}

	wantRaw := "tank/k8s/pvc-src@csi-snap-abc"
	if len(z.createdDS) != 1 || z.createdDS[0] != wantRaw {
		t.Fatalf("expected raw snapshot %q, got %v", wantRaw, z.createdDS)
	}

	var backing storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "csi-snap-abc"}, &backing); err != nil {
		t.Fatalf("get backing clone: %v", err)
	}
	if backing.Spec.Dataset != "k8s/csi-snap-abc" {
		t.Errorf("backing clone dataset = %q, want k8s/csi-snap-abc", backing.Spec.Dataset)
	}
	if backing.Spec.Source == nil || backing.Spec.Source.Snapshot != "k8s/pvc-src@csi-snap-abc" {
		t.Errorf("backing clone source = %+v, want k8s/pvc-src@csi-snap-abc", backing.Spec.Source)
	}
	if backing.Spec.Properties["canmount"] != "off" {
		t.Errorf("backing clone properties = %+v, want canmount=off", backing.Spec.Properties)
	}
	if len(backing.OwnerReferences) != 1 || backing.OwnerReferences[0].Name != "snap-1" || backing.OwnerReferences[0].Kind != "ZfsSnapshot" {
		t.Errorf("backing clone ownerReferences = %+v", backing.OwnerReferences)
	}
	if backing.OwnerReferences[0].BlockOwnerDeletion == nil || !*backing.OwnerReferences[0].BlockOwnerDeletion {
		t.Errorf("backing clone ownerReference.BlockOwnerDeletion = %v, want true", backing.OwnerReferences[0].BlockOwnerDeletion)
	}

	// Drive the backing clone's own reconciler to Ready (finalizer, then clone).
	if _, err := dsR.Reconcile(context.Background(), backingReq); err != nil {
		t.Fatalf("backing reconcile 1: %v", err)
	}
	if _, err := dsR.Reconcile(context.Background(), backingReq); err != nil {
		t.Fatalf("backing reconcile 2: %v", err)
	}

	if _, err := snapR.Reconcile(context.Background(), snapReq); err != nil { // self-snapshot + Ready
		t.Fatalf("reconcile 3: %v", err)
	}

	wantSelfSnap := "tank/k8s/csi-snap-abc@restore-source"
	found := false
	for _, d := range z.createdDS {
		if d == wantSelfSnap {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected self-snapshot %q, got %v", wantSelfSnap, z.createdDS)
	}

	var got storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if got.Status.Phase != storagev1alpha1.SnapshotPhaseReady || !got.Status.ReadyToUse {
		t.Errorf("phase = %q readyToUse = %v, want Ready/true", got.Status.Phase, got.Status.ReadyToUse)
	}
}

// TestZfsSnapshotReconcile_StandaloneDeleteDelegatesToBackingClone verifies
// D15's delete path: deleting a standalone-mode ZfsSnapshot deletes its owned
// backing-clone ZfsDataset and waits (polling) for ZfsDatasetReconciler to
// fully remove it before doing the required raw-origin-snapshot cleanup and
// releasing its own finalizer.
func TestZfsSnapshotReconcile_StandaloneDeleteDelegatesToBackingClone(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "snap-1",
			Finalizers:        []string{zfsSnapshotFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID:     "999",
			Dataset:      "k8s/pvc-src",
			SnapshotName: "csi-snap-abc",
			SourceVolume: "pvc-src",
			Mode:         storagev1alpha1.SnapshotModeStandalone,
		},
	}
	backing := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-abc", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/csi-snap-abc",
			Type:     storagev1alpha1.DatasetTypeFilesystem,
			Source:   &storagev1alpha1.DatasetSource{Snapshot: "k8s/pvc-src@csi-snap-abc"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), snap, backing).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-src@csi-snap-abc", "tank/k8s/csi-snap-abc")
	snapR := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	dsR := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	snapReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}
	backingReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "csi-snap-abc"}}

	// 1st pass: issues client.Delete on the backing clone, then polls.
	if _, err := snapR.Reconcile(context.Background(), snapReq); err != nil {
		t.Fatalf("snapshot reconcile 1: %v", err)
	}
	var stillBacking storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "csi-snap-abc"}, &stillBacking); err != nil {
		t.Fatalf("get backing clone: %v", err)
	}
	if stillBacking.DeletionTimestamp.IsZero() {
		t.Fatalf("expected backing clone to have a deletionTimestamp after snapshot delete pass 1")
	}
	if len(z.destroyed) != 0 {
		t.Fatalf("nothing should be destroyed yet, got %v", z.destroyed)
	}

	// ZfsDatasetReconciler tears down the (now-terminating) backing clone.
	if _, err := dsR.Reconcile(context.Background(), backingReq); err != nil {
		t.Fatalf("backing clone reconcile: %v", err)
	}
	var gone storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "csi-snap-abc"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("expected backing clone to be gone, err = %v", err)
	}

	// 2nd pass: backing clone confirmed gone -> required raw-origin cleanup + finalizer release.
	if _, err := snapR.Reconcile(context.Background(), snapReq); err != nil {
		t.Fatalf("snapshot reconcile 2: %v", err)
	}
	wantRawDestroyed := "tank/k8s/pvc-src@csi-snap-abc"
	found := false
	for _, d := range z.destroyed {
		if d == wantRawDestroyed {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected raw origin snapshot %q destroyed, got %v", wantRawDestroyed, z.destroyed)
	}
	var goneSnap storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &goneSnap); !apierrors.IsNotFound(err) {
		t.Fatalf("expected ZfsSnapshot to be gone, err = %v", err)
	}
}
