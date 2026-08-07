package controller

import (
	"context"
	"reflect"
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

// TestZfsSnapshotReconcile_CreatesSnapshotAndSetsReady covers the create path's
// bookkeeping — finalizer, raw snapshot, and the reported creation time and
// restore size. The structural assertions about the backing clone itself live
// in TestZfsSnapshotReconcile_CreatesBackingCloneAndSelfSnapshot.
func TestZfsSnapshotReconcile_CreatesSnapshotAndSetsReady(t *testing.T) {
	scheme := newTestScheme(t)
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID:     "999",
			Dataset:      "k8s/pvc-1",
			SnapshotName: "csi-snap-1",
			SourceVolume: "pvc-1",
			SourceType:   storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), src, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	r := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	dsR := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}
	backingReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "csi-snap-1"}}

	// Finalizer, then raw snapshot + backing-clone create.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	// Drive the backing clone to Ready, then let the snapshot finish.
	for i := 0; i < 2; i++ {
		if _, err := dsR.Reconcile(context.Background(), backingReq); err != nil {
			t.Fatalf("backing reconcile %d: %v", i+1, err)
		}
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}

	if len(z.createdDS) == 0 || z.createdDS[0] != "tank/k8s/pvc-1@csi-snap-1" {
		t.Fatalf("expected raw snapshot tank/k8s/pvc-1@csi-snap-1 first, got %v", z.createdDS)
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
	// Both halves go in one pass now (ADR-0030): the backing clone is a plain
	// dataset owned by this reconciler, so there is no object handshake.
	if !reflect.DeepEqual(z.destroyed, []string{"tank/k8s/snap-3", "tank/k8s/pvc-3@snap-3"}) {
		t.Fatalf("expected backing clone then raw snapshot destroyed, got %v", z.destroyed)
	}
}

// TestZfsSnapshotReconcile_FollowsRenamedSourceDataset pins ADR-0025: the
// snapshot's Spec.Dataset is only the path recorded when it was taken. Once the
// source dataset is renamed (`zfs rename` plus a repointed
// ZfsDataset.Spec.Dataset — a supported recovery workflow, api-conventions §5),
// the ZFS snapshot has moved with its dataset, so the reconciler must follow
// the source object rather than the stale copy.
func TestZfsSnapshotReconcile_FollowsRenamedSourceDataset(t *testing.T) {
	scheme := newTestScheme(t)
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/renamed-pvc-1",
			Type:     storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID:     "999",
			Dataset:      "k8s/pvc-1", // pre-rename copy
			SnapshotName: "snap-1",
			SourceVolume: "pvc-1",
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), src, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}).
		Build()

	z := newFakeZFS()
	r := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	// The raw snapshot must land on the source's *current* path; the backing
	// clone's self-snapshot follows in the same pass.
	if len(z.createdDS) == 0 || z.createdDS[0] != "tank/k8s/renamed-pvc-1@snap-1" {
		t.Fatalf("expected snapshot on the source's current path tank/k8s/renamed-pvc-1@snap-1, got %v", z.createdDS)
	}
}

// TestZfsSnapshotReconcile_CreatesBackingCloneAndSelfSnapshot drives a full
// create: raw snapshot -> backing ZFS clone -> "@restore-source" self-snapshot
// -> ZfsSnapshot Ready.
func TestZfsSnapshotReconcile_CreatesBackingCloneAndSelfSnapshot(t *testing.T) {
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
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), src, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	snapR := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	snapReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}

	if _, err := snapR.Reconcile(context.Background(), snapReq); err != nil { // installs finalizer
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := snapR.Reconcile(context.Background(), snapReq); err != nil { // raw snapshot + backing-clone create
		t.Fatalf("reconcile 2: %v", err)
	}

	wantRaw := "tank/k8s/pvc-src@csi-snap-abc"
	if len(z.createdDS) == 0 || z.createdDS[0] != wantRaw {
		t.Fatalf("expected raw snapshot %q first, got %v", wantRaw, z.createdDS)
	}

	// The backing clone is a plain ZFS dataset, not an object (ADR-0030): assert
	// it on the pool, and assert that nothing was authored in Kubernetes.
	if !z.existing["tank/k8s/csi-snap-abc"] {
		t.Fatalf("backing clone dataset not created; existing = %v", z.existing)
	}
	if origin := z.origin["tank/k8s/csi-snap-abc"]; origin != wantRaw {
		t.Errorf("backing clone origin = %q, want %q", origin, wantRaw)
	}
	if props := z.lastCloneProps; props["canmount"] != "off" {
		t.Errorf("backing clone properties = %+v, want canmount=off", props)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "csi-snap-abc"}, &storagev1alpha1.ZfsDataset{}); !apierrors.IsNotFound(err) {
		t.Errorf("no ZfsDataset object may be authored for a backing clone, got %v", err)
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

// TestZfsSnapshotReconcile_DeleteTearsDownBothHalves verifies
// D15's delete path: deleting a ZfsSnapshot deletes its owned
// backing-clone ZfsDataset and waits (polling) for ZfsDatasetReconciler to
// fully remove it before doing the required raw-origin-snapshot cleanup and
// releasing its own finalizer.
func TestZfsSnapshotReconcile_DeleteTearsDownBothHalves(t *testing.T) {
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
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-src@csi-snap-abc", "tank/k8s/csi-snap-abc")
	z.origin["tank/k8s/csi-snap-abc"] = "tank/k8s/pvc-src@csi-snap-abc"
	z.seedSnapshot("tank/k8s/csi-snap-abc", restoreSourceSnapshotName)
	snapR := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	snapReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}

	// One pass tears down both halves: no object to delete, nothing to poll for.
	if _, err := snapR.Reconcile(context.Background(), snapReq); err != nil {
		t.Fatalf("snapshot reconcile: %v", err)
	}

	// The backing clone's own @restore-source goes first (it blocks the
	// non-recursive destroy), then the clone, then the raw origin snapshot.
	want := []string{
		"tank/k8s/csi-snap-abc@" + restoreSourceSnapshotName,
		"tank/k8s/csi-snap-abc",
		"tank/k8s/pvc-src@csi-snap-abc",
	}
	if !reflect.DeepEqual(z.destroyed, want) {
		t.Fatalf("destroyed = %v, want %v", z.destroyed, want)
	}

	var goneSnap storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &goneSnap); !apierrors.IsNotFound(err) {
		t.Fatalf("expected ZfsSnapshot to be gone, err = %v", err)
	}
}

// TestZfsSnapshotReconcile_DeleteWithLiveRestore is the F2b
// regression. Deleting a snapshot that a live restored PVC was created from
// promotes that PVC while the backing clone is torn down, which leaves it a
// clone of the *raw* origin snapshot on the still-live source volume. ZFS then
// refuses to destroy that raw snapshot ("snapshot has dependent clones"), and
// because the zfssnapshot finalizer is gated on that destroy succeeding, the
// ZfsSnapshot used to stay Terminating forever — violating the design's own
// "DeleteSnapshot always succeeds, never blocks" guarantee.
//
// D19 promotes the raw snapshot's remaining clones away first, which relocates
// it onto the dependent and turns the destroy into a NotExist no-op.
func TestZfsSnapshotReconcile_DeleteWithLiveRestore(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	ctx := context.Background()

	source := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-a", Finalizers: []string{zfsDatasetFinalizer}},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-a", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1", Finalizers: []string{zfsSnapshotFinalizer}, DeletionTimestamp: &now},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-a", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-a",
		},
	}
	restored := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-r", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-r", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/csi-snap-x@" + restoreSourceSnapshotName},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), source, snap, restored).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-a")
	z.seedClone("tank/k8s/pvc-a", "csi-snap-x", "tank/k8s/csi-snap-x")
	z.seedClone("tank/k8s/csi-snap-x", restoreSourceSnapshotName, "tank/k8s/pvc-r")

	snapR := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	dsR := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	snapReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}

	// One pass now: promote the restore off the backing clone, destroy the clone,
	// then deal with the raw origin snapshot the restore has inherited.
	if _, err := snapR.Reconcile(ctx, snapReq); err != nil {
		t.Fatalf("snapshot reconcile: %v", err)
	}
	var gone storagev1alpha1.ZfsSnapshot
	if err := c.Get(ctx, client.ObjectKey{Name: "snap-1"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("ZfsSnapshot should be fully deleted, err = %v", err)
	}
	// The raw snapshot was relocated onto pvc-r rather than destroyed, and the
	// source volume is left owning no snapshots at all. pvc-r ends up holding
	// both relocated artifacts, oldest first: @csi-snap-x came off pvc-a, and
	// @restore-source came off the backing clone when it was torn down.
	if got, want := z.snapshots["tank/k8s/pvc-r"], []string{"csi-snap-x", restoreSourceSnapshotName}; !reflect.DeepEqual(got, want) {
		t.Errorf("snapshots[pvc-r] = %v, want %v", got, want)
	}
	if got := z.snapshots["tank/k8s/pvc-a"]; len(got) != 0 {
		t.Errorf("snapshots[pvc-a] = %v, want none", got)
	}
	// pvc-r must still be independently deletable afterwards.
	var dep storagev1alpha1.ZfsDataset
	if err := c.Get(ctx, client.ObjectKey{Name: "pvc-r"}, &dep); err != nil {
		t.Fatalf("get pvc-r: %v", err)
	}
	if err := c.Delete(ctx, &dep); err != nil {
		t.Fatalf("delete pvc-r: %v", err)
	}
	if _, err := dsR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-r"}}); err != nil {
		t.Fatalf("reconcile delete pvc-r: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "pvc-r"}, &dep); err == nil {
		t.Error("pvc-r still present after delete")
	}
}

// TestZfsSnapshotReconcile_ChainedBackingClonesRemainDeletable is the F2d
// regression, the case this project's own live-pool run documented without
// noticing. After DeleteVolume on a source with several snapshots,
// the backing clones end up chained to one another — each owning a real
// snapshot that the next one clones — and none of it was ever tracked. Deleting
// the first snapshot then failed on "snapshot has dependent clones" forever.
func TestZfsSnapshotReconcile_ChainedBackingClonesRemainDeletable(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	ctx := context.Background()

	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "vol1", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/vol1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	objs := []client.Object{onlinePool(), vol}
	snaps := map[string]*storagev1alpha1.ZfsSnapshot{}
	for _, n := range []string{"1", "2"} {
		s := &storagev1alpha1.ZfsSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap-" + n, Finalizers: []string{zfsSnapshotFinalizer}},
			Spec: storagev1alpha1.ZfsSnapshotSpec{
				PoolGUID: "999", Dataset: "k8s/vol1", SnapshotName: "csi-snap-" + n,
				SourceVolume: "vol1",
			},
			Status: storagev1alpha1.ZfsSnapshotStatus{Phase: storagev1alpha1.SnapshotPhaseReady, ReadyToUse: true},
		}
		bc := &storagev1alpha1.ZfsDataset{
			ObjectMeta: metav1.ObjectMeta{
				Name: "csi-snap-" + n, Finalizers: []string{zfsDatasetFinalizer},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(s, storagev1alpha1.GroupVersion.WithKind("ZfsSnapshot")),
				},
			},
			Spec: storagev1alpha1.ZfsDatasetSpec{
				PoolGUID: "999", Dataset: "k8s/csi-snap-" + n, Type: storagev1alpha1.DatasetTypeFilesystem,
				Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/vol1@csi-snap-" + n},
			},
		}
		snaps[n] = s
		objs = append(objs, s, bc)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/vol1")
	for _, n := range []string{"1", "2"} {
		z.seedClone("tank/k8s/vol1", "csi-snap-"+n, "tank/k8s/csi-snap-"+n)
		z.seedSnapshot("tank/k8s/csi-snap-"+n, restoreSourceSnapshotName)
	}

	snapR := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	dsR := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}

	// DeleteVolume promotes both backing clones away and destroys the source.
	if _, err := dsR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "vol1"}}); err != nil {
		t.Fatalf("reconcile delete vol1: %v", err)
	}
	// This is the chained state the live-pool run recorded: csi-snap-2 is now a
	// clone of a snapshot owned by csi-snap-1.
	if o, want := z.origin["tank/k8s/csi-snap-2"], "tank/k8s/csi-snap-1@csi-snap-1"; o != want {
		t.Fatalf("csi-snap-2 origin = %q, want %q", o, want)
	}

	// Deleting the *first* snapshot must still work: its backing clone owns the
	// snapshot the second one depends on.
	if err := c.Delete(ctx, snaps["1"]); err != nil {
		t.Fatalf("delete snap-1: %v", err)
	}
	snap1Req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}
	if _, err := snapR.Reconcile(ctx, snap1Req); err != nil {
		t.Fatalf("snapshot reconcile 1: %v", err)
	}
	if _, err := dsR.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "csi-snap-1"}}); err != nil {
		t.Fatalf("backing clone reconcile: %v", err)
	}
	if _, err := snapR.Reconcile(ctx, snap1Req); err != nil {
		t.Fatalf("snapshot reconcile 2: %v", err)
	}

	var gone storagev1alpha1.ZfsSnapshot
	if err := c.Get(ctx, client.ObjectKey{Name: "snap-1"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("snap-1 should be fully deleted, err = %v", err)
	}
	// The surviving snapshot is untouched and still restorable.
	var survivor storagev1alpha1.ZfsDataset
	if err := c.Get(ctx, client.ObjectKey{Name: "csi-snap-2"}, &survivor); err != nil {
		t.Fatalf("csi-snap-2 should survive: %v", err)
	}
	if !z.existing["tank/k8s/csi-snap-2@"+restoreSourceSnapshotName] {
		t.Error("csi-snap-2 lost its @restore-source snapshot")
	}
}
