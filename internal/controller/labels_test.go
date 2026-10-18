package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// wantLabels fails unless name carries the role and owner labels, read the way
// the delete path will read them.
func wantLabels(t *testing.T, z *fakeZFS, name, role, owner string) {
	t.Helper()
	ctx := context.Background()
	gotRole, err := z.Get(ctx, name, zpool.PropRole)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	gotOwner, _ := z.Get(ctx, name, zpool.PropOwner)
	if gotRole != role || gotOwner != owner {
		t.Errorf("%s labels = role %q owner %q, want role %q owner %q", name, gotRole, gotOwner, role, owner)
	}
}

func reconcileTwiceOrMore(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, name string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}
}

func labelDataset(name string, src *storagev1alpha1.DatasetSource) *storagev1alpha1.ZfsDataset {
	return &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/" + name, Type: storagev1alpha1.DatasetTypeFilesystem, Source: src,
		},
	}
}

func datasetReconciler(t *testing.T, z *fakeZFS, objs ...*storagev1alpha1.ZfsDataset) *ZfsDatasetReconciler {
	t.Helper()
	scheme := newTestScheme(t)
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(onlinePool()).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{})
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return &ZfsDatasetReconciler{Client: b.Build(), Scheme: scheme, NodeName: "node-a", ZFS: z}
}

func TestLabels_NewVolume(t *testing.T) {
	z := newFakeZFS()
	r := datasetReconciler(t, z, labelDataset("pvc-new", nil))
	reconcileTwiceOrMore(t, r, "pvc-new", 1)
	wantLabels(t, z, "tank/k8s/pvc-new", zpool.RoleVolume, "uid-pvc-new")
	if z.lastDSProps[zpool.PropRole] != zpool.RoleVolume {
		t.Errorf("labels must be passed at create time, got %v", z.lastDSProps)
	}
}

func TestLabels_RestoredAndClonedVolumesAreVolumes(t *testing.T) {
	z := newFakeZFS()
	r := datasetReconciler(t, z,
		labelDataset("pvc-restore", &storagev1alpha1.DatasetSource{Snapshot: "k8s/csi-snap-a@csi-snap-a"}),
		labelDataset("pvc-clone", &storagev1alpha1.DatasetSource{Volume: "k8s/pvc-src"}))
	reconcileTwiceOrMore(t, r, "pvc-restore", 1)
	reconcileTwiceOrMore(t, r, "pvc-clone", 1)
	wantLabels(t, z, "tank/k8s/pvc-restore", zpool.RoleVolume, "uid-pvc-restore")
	wantLabels(t, z, "tank/k8s/pvc-clone", zpool.RoleVolume, "uid-pvc-clone")
	if z.lastCloneProps[zpool.PropOwner] != "uid-pvc-clone" {
		t.Errorf("labels must be passed to zfs clone, got %v", z.lastCloneProps)
	}
}

func TestLabels_DirectCloneOriginSnapshot(t *testing.T) {
	z := newFakeZFS()
	r := datasetReconciler(t, z, labelDataset("pvc-clone", &storagev1alpha1.DatasetSource{Volume: "k8s/pvc-src"}))
	reconcileTwiceOrMore(t, r, "pvc-clone", 1)
	origin := "tank/k8s/pvc-src@clone-" + cloneSnapshotSuffix("pvc-clone")
	wantLabels(t, z, origin, zpool.RoleCloneOrigin, "uid-pvc-clone")
}

func TestLabels_ProvisionedVolumeIsNotTouched(t *testing.T) {
	z := newFakeZFS("tank/k8s/pvc-old")
	vol := labelDataset("pvc-old", nil)
	vol.Status.Phase = storagev1alpha1.DatasetPhaseReady
	vol.Status.ProvisionedAt = provisionedAtFor(true)
	r := datasetReconciler(t, z, vol)
	reconcileTwiceOrMore(t, r, "pvc-old", 2)
	for _, s := range z.setProps {
		if strings.Contains(s, zpool.PropRole) || strings.Contains(s, zpool.PropOwner) {
			t.Errorf("provisioned volume was labelled by the driver: %s", s)
		}
	}
}

func TestLabels_UnprovisionedExistingDatasetIsLabelled(t *testing.T) {
	z := newFakeZFS("tank/k8s/pvc-new")
	r := datasetReconciler(t, z, labelDataset("pvc-new", nil))
	reconcileTwiceOrMore(t, r, "pvc-new", 1)
	wantLabels(t, z, "tank/k8s/pvc-new", zpool.RoleVolume, "uid-pvc-new")
}

func snapshotFixtureWithUID(t *testing.T, group string) (*ZfsSnapshotReconciler, *fakeZFS) {
	t.Helper()
	scheme := newTestScheme(t)
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-src", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1", UID: "uid-snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", SnapshotName: "csi-snap-abc", SourceVolume: "pvc-src",
			SourceType: storagev1alpha1.DatasetTypeFilesystem, GroupSnapshotID: group,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(onlinePool(), src, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsSnapshot{}, &storagev1alpha1.ZfsDataset{}).Build()
	z := newFakeZFS("tank/k8s/pvc-src")
	if group != "" {
		z.seedSnapshot("tank/k8s/pvc-src", "csi-snap-abc") // taken by the group's atomic exec
	}
	return &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}, z
}

func TestLabels_RawSnapshotAndBackingClone(t *testing.T) {
	r, z := snapshotFixtureWithUID(t, "")
	reconcileTwiceOrMore(t, r, "snap-1", 3)
	wantLabels(t, z, "tank/k8s/pvc-src@csi-snap-abc", zpool.RoleRaw, "uid-snap-1")
	wantLabels(t, z, "tank/k8s/csi-snap-abc", zpool.RoleBacking, "uid-snap-1")
	if z.lastCloneProps[zpool.PropRole] != zpool.RoleBacking || z.lastCloneProps["canmount"] != "off" {
		t.Errorf("backing clone props = %v, want labels next to canmount=off", z.lastCloneProps)
	}
}

// A group member's raw snapshot is taken by the group reconciler in one atomic
// call, which knows no owner UID; the member's ZfsSnapshot labels it.
func TestLabels_GroupMemberRawSnapshotIsLabelledByItsSnapshot(t *testing.T) {
	r, z := snapshotFixtureWithUID(t, "grp-1")
	reconcileTwiceOrMore(t, r, "snap-1", 3)
	wantLabels(t, z, "tank/k8s/pvc-src@csi-snap-abc", zpool.RoleRaw, "uid-snap-1")
	wantLabels(t, z, "tank/k8s/csi-snap-abc", zpool.RoleBacking, "uid-snap-1")
}
