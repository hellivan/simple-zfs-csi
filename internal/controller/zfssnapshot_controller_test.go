package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

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
// in TestZfsSnapshotReconcile_CreatesRawSnapshotAndBackingClone.
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

	z := newFakeZFS()
	z.seedSnapshot("tank/k8s/pvc-3", "snap-3")
	r := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-3"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// One pass (ADR-0030): the raw snapshot goes first, with `destroy -d`, then
	// the backing clone.
	if !reflect.DeepEqual(z.destroyed, []string{"tank/k8s/pvc-3@snap-3", "tank/k8s/snap-3"}) {
		t.Fatalf("expected raw snapshot then backing clone destroyed, got %v", z.destroyed)
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

// TestZfsSnapshotReconcile_CreatesRawSnapshotAndBackingClone drives a full
// create: raw snapshot -> backing ZFS clone
// -> ZfsSnapshot Ready.
func TestZfsSnapshotReconcile_CreatesRawSnapshotAndBackingClone(t *testing.T) {
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

	// Nothing else is created: the backing clone takes no snapshot of its own.
	if len(z.createdDS) != 1 {
		t.Fatalf("only the raw snapshot may be created, got %v", z.createdDS)
	}

	var got storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if got.Status.Phase != storagev1alpha1.SnapshotPhaseReady || !got.Status.ReadyToUse {
		t.Errorf("phase = %q readyToUse = %v, want Ready/true", got.Status.Phase, got.Status.ReadyToUse)
	}
}

func settledSnapshotFixture(t *testing.T, phase storagev1alpha1.ZfsSnapshotPhase) (client.Client, *ZfsSnapshotReconciler, *fakeZFS) {
	t.Helper()
	scheme := newTestScheme(t)
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1", Finalizers: []string{zfsSnapshotFinalizer}},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-1", SnapshotName: "csi-snap-1",
			SourceVolume: "pvc-1", SourceType: storagev1alpha1.DatasetTypeFilesystem,
		},
		Status: storagev1alpha1.ZfsSnapshotStatus{Phase: phase, ReadyToUse: phase == storagev1alpha1.SnapshotPhaseReady},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), src, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).
		Build()
	z := newFakeZFS()
	return c, &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}, z
}

// A Ready snapshot whose primitives vanished must become Lost, and nothing may
// be re-created (ADR-0034).
func TestZfsSnapshotReconcile_ReadyWithVanishedPrimitivesBecomesLost(t *testing.T) {
	c, r, z := settledSnapshotFixture(t, storagev1alpha1.SnapshotPhaseReady)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}
	if len(z.createdDS) != 0 || len(z.cloned) != 0 {
		t.Fatalf("nothing may be re-created, got snapshots %v clones %v", z.createdDS, z.cloned)
	}
	var got storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != storagev1alpha1.SnapshotPhaseLost || got.Status.ReadyToUse {
		t.Errorf("phase = %q readyToUse = %v, want Lost/false", got.Status.Phase, got.Status.ReadyToUse)
	}
}

// A healthy Ready snapshot stays Ready and untouched.
func TestZfsSnapshotReconcile_ReadyWithIntactPrimitivesIsLeftAlone(t *testing.T) {
	c, r, z := settledSnapshotFixture(t, storagev1alpha1.SnapshotPhaseReady)
	z.seedSnapshot("tank/k8s/pvc-1", "csi-snap-1")
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var got storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != storagev1alpha1.SnapshotPhaseReady || len(z.createdDS) != 0 {
		t.Errorf("phase = %q, created = %v; want Ready and nothing created", got.Status.Phase, z.createdDS)
	}
}

// Lost recovers when the original restore source reappears (dataset moved away
// and back), and is otherwise never healed by re-creation.
func TestZfsSnapshotReconcile_LostRecoversWhenPrimitiveReturns(t *testing.T) {
	c, r, z := settledSnapshotFixture(t, storagev1alpha1.SnapshotPhaseLost)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}
	phase := func() storagev1alpha1.ZfsSnapshotPhase {
		var got storagev1alpha1.ZfsSnapshot
		if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
			t.Fatal(err)
		}
		return got.Status.Phase
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if phase() != storagev1alpha1.SnapshotPhaseLost || len(z.createdDS) != 0 {
		t.Fatalf("still absent: phase = %q created = %v, want Lost and nothing created", phase(), z.createdDS)
	}

	z.seedSnapshot("tank/k8s/pvc-1", "csi-snap-1")
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if phase() != storagev1alpha1.SnapshotPhaseReady || len(z.createdDS) != 0 {
		t.Errorf("phase = %q created = %v, want Ready and nothing created", phase(), z.createdDS)
	}
}

// The reported creation time comes from the raw snapshot, not the
// backing clone (ADR-0036).
func TestZfsSnapshotReconcile_CreationTimeComesFromRawSnapshot(t *testing.T) {
	scheme := newTestScheme(t)
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-1", SnapshotName: "csi-snap-1",
			SourceVolume: "pvc-1", SourceType: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(onlinePool(), src, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).Build()
	z := newFakeZFS()
	r := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}

	// Pre-existing raw snapshot with a distinctive time; the fake stamps every
	// snapshot it creates itself with 1700000000.
	z.seedSnapshot("tank/k8s/pvc-1", "csi-snap-1")
	z.props["tank/k8s/pvc-1@csi-snap-1"] = map[string]string{"creation": "1600000000", "referenced": "1048576"}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	var got storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.CreationTime == nil || got.Status.CreationTime.Unix() != 1600000000 {
		t.Errorf("creation time = %v, want unix 1600000000 from the raw snapshot", got.Status.CreationTime)
	}
}

// A legacy Ready snapshot (no ProvisionedAt) is backfilled; CreationTime is
// read from the raw snapshot.
func TestZfsSnapshotReconcile_LegacyReadyIsBackfilled(t *testing.T) {
	c, r, z := settledSnapshotFixture(t, storagev1alpha1.SnapshotPhaseReady)
	z.seedSnapshot("tank/k8s/pvc-1", "csi-snap-1")
	z.props["tank/k8s/pvc-1@csi-snap-1"] = map[string]string{"creation": "1600000000"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var got storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ProvisionedAt == nil {
		t.Errorf("provisionedAt not backfilled")
	}
	if got.Status.CreationTime == nil || got.Status.CreationTime.Unix() != 1600000000 {
		t.Errorf("creationTime = %v, want 1600000000 from the raw snapshot", got.Status.CreationTime)
	}
	if got.Status.Phase != storagev1alpha1.SnapshotPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
}

// Promoting the backing clone moves the raw snapshot onto it: the snapshot must
// stay Ready, not become Lost (ADR-0038), and is found by the pool scan.
func TestZfsSnapshotReconcile_RelocatedRawSnapshotIsNotLost(t *testing.T) {
	c, r, z := settledSnapshotFixture(t, storagev1alpha1.SnapshotPhaseReady)
	z.existing["tank/k8s/csi-snap-1"] = true
	z.existing["tank/k8s/csi-snap-1@csi-snap-1"] = true
	z.seedSnapshot("tank/k8s/csi-snap-1", "csi-snap-1")
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}); err != nil {
		t.Fatal(err)
	}
	var got storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != storagev1alpha1.SnapshotPhaseReady {
		t.Errorf("phase = %q, want Ready (raw snapshot relocated, not lost)", got.Status.Phase)
	}
}

// A Lost snapshot that returns to Ready keeps its recorded creationTime.
func TestZfsSnapshotReconcile_RecoveryKeepsCreationTime(t *testing.T) {
	c, r, z := settledSnapshotFixture(t, storagev1alpha1.SnapshotPhaseLost)
	var cur storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &cur); err != nil {
		t.Fatal(err)
	}
	first := metav1.NewTime(time.Unix(1500000000, 0).UTC())
	cur.Status.CreationTime = &first
	if err := c.Status().Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	z.seedSnapshot("tank/k8s/pvc-1", "csi-snap-1")
	z.props["tank/k8s/pvc-1@csi-snap-1"] = map[string]string{"creation": "1600000000"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "snap-1"}}); err != nil {
		t.Fatal(err)
	}
	var got storagev1alpha1.ZfsSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "snap-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != storagev1alpha1.SnapshotPhaseReady || got.Status.CreationTime.Unix() != 1500000000 {
		t.Errorf("phase = %q creationTime = %v, want Ready and unchanged 1500000000", got.Status.Phase, got.Status.CreationTime)
	}
}
