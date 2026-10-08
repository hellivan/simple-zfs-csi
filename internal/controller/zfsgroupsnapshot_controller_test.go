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

func groupFixture(t *testing.T, extra ...client.Object) (client.Client, *ZfsGroupSnapshotReconciler, *fakeZFS) {
	t.Helper()
	scheme := newTestScheme(t)
	mk := func(name, ds string) *storagev1alpha1.ZfsDataset {
		return &storagev1alpha1.ZfsDataset{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: storagev1alpha1.ZfsDatasetSpec{
				PoolGUID: "999", Dataset: ds, Type: storagev1alpha1.DatasetTypeFilesystem,
			},
		}
	}
	grp := &storagev1alpha1.ZfsGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "grp-1"},
		Spec: storagev1alpha1.ZfsGroupSnapshotSpec{
			PoolGUID: "999",
			Members: []storagev1alpha1.ZfsGroupSnapshotMember{
				{SourceVolume: "data", Dataset: "k8s/data", SnapshotName: "raw-data", ZfsSnapshotRef: "child-data", SourceType: storagev1alpha1.DatasetTypeFilesystem},
				{SourceVolume: "wal", Dataset: "k8s/wal", SnapshotName: "raw-wal", ZfsSnapshotRef: "child-wal", SourceType: storagev1alpha1.DatasetTypeFilesystem},
			},
		},
	}
	objs := append([]client.Object{onlinePool(), mk("data", "k8s/data"), mk("wal", "k8s/wal"), grp}, extra...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.ZfsGroupSnapshot{}, &storagev1alpha1.ZfsSnapshot{}).Build()
	z := newFakeZFS("tank/k8s/data", "tank/k8s/wal")
	return c, &ZfsGroupSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}, z
}

func reconcileGroup(t *testing.T, r *ZfsGroupSnapshotReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "grp-1"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getGroup(t *testing.T, c client.Client) *storagev1alpha1.ZfsGroupSnapshot {
	t.Helper()
	g := &storagev1alpha1.ZfsGroupSnapshot{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "grp-1"}, g); err != nil {
		t.Fatal(err)
	}
	return g
}

func markChildrenReady(t *testing.T, c client.Client) {
	t.Helper()
	for _, n := range []string{"child-data", "child-wal"} {
		s := &storagev1alpha1.ZfsSnapshot{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: n}, s); err != nil {
			t.Fatal(err)
		}
		s.Status.Phase = storagev1alpha1.SnapshotPhaseReady
		s.Status.ReadyToUse = true
		if err := c.Status().Update(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestZfsGroupSnapshot_OneAtomicExecThenChildren(t *testing.T) {
	c, r, z := groupFixture(t)
	reconcileGroup(t, r) // finalizer
	reconcileGroup(t, r)

	want := [][]string{{"tank/k8s/data@raw-data", "tank/k8s/wal@raw-wal"}}
	if !reflect.DeepEqual(z.snapshotCalls, want) {
		t.Fatalf("snapshot calls = %v, want one exec %v", z.snapshotCalls, want)
	}
	for _, n := range []string{"child-data", "child-wal"} {
		s := &storagev1alpha1.ZfsSnapshot{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: n}, s); err != nil {
			t.Fatalf("child %s: %v", n, err)
		}
		if s.Spec.GroupSnapshotID != "grp-1" {
			t.Errorf("child %s GroupSnapshotID = %q", n, s.Spec.GroupSnapshotID)
		}
	}
	g := getGroup(t, c)
	if g.Status.Phase != storagev1alpha1.GroupSnapshotPhasePending || g.Status.CreationTime == nil {
		t.Fatalf("status = %+v", g.Status)
	}
	if g.Status.ProvisionedAt != nil {
		t.Fatal("provisionedAt must wait for Ready")
	}

	markChildrenReady(t, c)
	reconcileGroup(t, r)
	g = getGroup(t, c)
	if g.Status.Phase != storagev1alpha1.GroupSnapshotPhaseReady || g.Status.ProvisionedAt == nil || !g.Status.ReadyToUse {
		t.Fatalf("status = %+v", g.Status)
	}
	if len(z.snapshotCalls) != 1 {
		t.Fatalf("extra snapshot exec: %v", z.snapshotCalls)
	}
}

func TestZfsGroupSnapshot_FrozenAfterProvisionedBecomesLost(t *testing.T) {
	c, r, z := groupFixture(t)
	reconcileGroup(t, r)
	reconcileGroup(t, r)
	markChildrenReady(t, c)
	reconcileGroup(t, r)

	s := &storagev1alpha1.ZfsSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "child-wal"}}
	if err := c.Delete(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcileGroup(t, r)

	if g := getGroup(t, c); g.Status.Phase != storagev1alpha1.GroupSnapshotPhaseLost {
		t.Fatalf("phase = %s", g.Status.Phase)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "child-wal"}, &storagev1alpha1.ZfsSnapshot{}); !apierrors.IsNotFound(err) {
		t.Fatalf("child was re-created: %v", err)
	}
	if len(z.snapshotCalls) != 1 {
		t.Fatalf("snapshot re-run: %v", z.snapshotCalls)
	}
}

func TestZfsGroupSnapshot_PartialRawSnapshotsIsError(t *testing.T) {
	c, r, z := groupFixture(t)
	z.seedSnapshot("tank/k8s/wal", "raw-wal")
	z.existing["tank/k8s/wal@raw-wal"] = true
	reconcileGroup(t, r)
	reconcileGroup(t, r)

	g := getGroup(t, c)
	if g.Status.Phase != storagev1alpha1.GroupSnapshotPhaseError {
		t.Fatalf("phase = %s", g.Status.Phase)
	}
	if len(z.snapshotCalls) != 0 {
		t.Fatalf("gap was filled: %v", z.snapshotCalls)
	}
}

func TestZfsGroupSnapshot_MissingSourceStaysPending(t *testing.T) {
	c, r, z := groupFixture(t)
	delete(z.existing, "tank/k8s/wal")
	reconcileGroup(t, r)
	res := reconcileGroup(t, r)

	if g := getGroup(t, c); g.Status.Phase != storagev1alpha1.GroupSnapshotPhasePending {
		t.Fatalf("phase = %s", g.Status.Phase)
	}
	if res.RequeueAfter == 0 || len(z.snapshotCalls) != 0 {
		t.Fatalf("res=%+v calls=%v", res, z.snapshotCalls)
	}
}

func TestZfsGroupSnapshot_IgnoredOnOtherNode(t *testing.T) {
	c, r, z := groupFixture(t)
	r.NodeName = "node-b"
	reconcileGroup(t, r)
	reconcileGroup(t, r)
	if len(z.snapshotCalls) != 0 || controllerutil.ContainsFinalizer(getGroup(t, c), zfsGroupSnapshotFinalizer) {
		t.Fatal("foreign node acted")
	}
}

func TestZfsGroupSnapshot_DeleteBeforeProvisionedDestroysOrphans(t *testing.T) {
	c, r, z := groupFixture(t)
	reconcileGroup(t, r) // finalizer
	z.seedSnapshot("tank/k8s/data", "raw-data")
	z.existing["tank/k8s/data@raw-data"] = true
	if err := c.Delete(context.Background(), getGroup(t, c)); err != nil {
		t.Fatal(err)
	}
	reconcileGroup(t, r)

	if z.existing["tank/k8s/data@raw-data"] {
		t.Fatal("orphan raw snapshot not destroyed")
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "grp-1"}, &storagev1alpha1.ZfsGroupSnapshot{}); !apierrors.IsNotFound(err) {
		t.Fatalf("group not released: %v", err)
	}
	if len(z.snapshotCalls) != 0 {
		t.Fatal("terminating group created snapshots")
	}
}

func TestZfsGroupSnapshot_DeleteWaitsForChildren(t *testing.T) {
	c, r, _ := groupFixture(t)
	reconcileGroup(t, r)
	reconcileGroup(t, r)
	markChildrenReady(t, c)
	reconcileGroup(t, r)

	// A finalizer keeps the child around so the group must wait.
	for _, n := range []string{"child-data", "child-wal"} {
		s := &storagev1alpha1.ZfsSnapshot{}
		_ = c.Get(context.Background(), client.ObjectKey{Name: n}, s)
		controllerutil.AddFinalizer(s, "test/hold")
		if err := c.Update(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Delete(context.Background(), getGroup(t, c)); err != nil {
		t.Fatal(err)
	}
	if res := reconcileGroup(t, r); res.RequeueAfter == 0 {
		t.Fatal("expected requeue while children exist")
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "grp-1"}, &storagev1alpha1.ZfsGroupSnapshot{}); err != nil {
		t.Fatalf("group released too early: %v", err)
	}
}

func TestZfsSnapshotReconcile_GroupMemberWithoutRawSnapshotFailsLoud(t *testing.T) {
	scheme := newTestScheme(t)
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "child"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-1", SnapshotName: "raw", SourceVolume: "pvc-1",
			SourceType: storagev1alpha1.DatasetTypeFilesystem, GroupSnapshotID: "grp-1",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(onlinePool(), src, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}).Build()
	z := newFakeZFS("tank/k8s/pvc-1")
	r := &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "child"}}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	got := &storagev1alpha1.ZfsSnapshot{}
	_ = c.Get(context.Background(), client.ObjectKey{Name: "child"}, got)
	if got.Status.Phase != storagev1alpha1.SnapshotPhaseError {
		t.Fatalf("phase = %s", got.Status.Phase)
	}
	if len(z.snapshotCalls) != 0 {
		t.Fatalf("member took its own snapshot: %v", z.snapshotCalls)
	}
}

func TestZfsGroupSnapshot_StaleCacheCannotRestartFrozenGroup(t *testing.T) {
	c, r, z := groupFixture(t)
	reconcileGroup(t, r) // finalizer

	// The direct reader already sees provisionedAt; the cached client does not.
	g := getGroup(t, c)
	now := metav1.Now()
	g.Status.ProvisionedAt = &now
	apiOnly := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(g).Build()
	r.APIReader = apiOnly

	res := reconcileGroup(t, r)
	if len(z.snapshotCalls) != 0 || res.RequeueAfter == 0 {
		t.Fatalf("stale cache re-provisioned: calls=%v res=%+v", z.snapshotCalls, res)
	}
	for _, n := range []string{"child-data", "child-wal"} {
		if err := c.Get(context.Background(), client.ObjectKey{Name: n}, &storagev1alpha1.ZfsSnapshot{}); !apierrors.IsNotFound(err) {
			t.Fatalf("child %s created from stale view", n)
		}
	}
}

func TestZfsGroupSnapshot_CrashAfterExecAdoptsRawSnapshots(t *testing.T) {
	c, r, z := groupFixture(t)
	for _, n := range []string{"tank/k8s/data@raw-data", "tank/k8s/wal@raw-wal"} {
		z.existing[n] = true
		d, s := splitSnapshotName(n)
		z.seedSnapshot(d, s)
	}
	reconcileGroup(t, r) // finalizer
	reconcileGroup(t, r)

	if len(z.snapshotCalls) != 0 {
		t.Fatalf("re-ran the atomic exec: %v", z.snapshotCalls)
	}
	if g := getGroup(t, c); g.Status.CreationTime == nil {
		t.Fatal("creationTime not recorded from the existing raw snapshot")
	}
	for _, n := range []string{"child-data", "child-wal"} {
		if err := c.Get(context.Background(), client.ObjectKey{Name: n}, &storagev1alpha1.ZfsSnapshot{}); err != nil {
			t.Fatalf("child %s: %v", n, err)
		}
	}
}
