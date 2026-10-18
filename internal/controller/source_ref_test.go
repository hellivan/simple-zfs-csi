package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
	"github.com/hellivan/simple-zfs-csi/internal/zpool/memzfs"
)

const rawName = "csi-snap-x"

// refWorld is a pool with a source volume "pvc-src", its ZfsSnapshot "snap-1"
// (raw snapshot plus backing clone) on the in-memory ZFS, and a reconciler that
// can provision restores and clones from them.
type refWorld struct {
	t   *testing.T
	z   *memzfs.Mem
	c   client.Client
	r   *ZfsDatasetReconciler
	ctx context.Context
}

func newRefWorld(t *testing.T, snap *storagev1alpha1.ZfsSnapshot) *refWorld {
	t.Helper()
	z := memzfs.New("tank")
	ctx := context.Background()
	for _, d := range []string{"tank/k8s", "tank/k8s/pvc-src"} {
		if err := z.CreateDataset(ctx, d, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Snapshot(ctx, "tank/k8s/pvc-src@"+rawName); err != nil {
		t.Fatal(err)
	}
	if err := z.Clone(ctx, "tank/k8s/pvc-src@"+rawName, "tank/k8s/"+rawName, nil); err != nil {
		t.Fatal(err)
	}
	scheme := newTestScheme(t)
	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-src", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(onlinePool(), src).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}, &storagev1alpha1.ZfsSnapshot{})
	if snap != nil {
		b = b.WithObjects(snap)
	}
	c := b.Build()
	return &refWorld{t: t, z: z, c: c, ctx: ctx,
		r: &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}}
}

func readySnapshot() *storagev1alpha1.ZfsSnapshot {
	return &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", SnapshotName: rawName, SourceVolume: "pvc-src",
			SourceType: storagev1alpha1.DatasetTypeFilesystem,
		},
		Status: storagev1alpha1.ZfsSnapshotStatus{Phase: storagev1alpha1.SnapshotPhaseReady, ReadyToUse: true},
	}
}

// provision creates a ZfsDataset with the given source and reconciles it once.
func (w *refWorld) provision(name string, src *storagev1alpha1.DatasetSource) (ctrl.Result, *storagev1alpha1.ZfsDataset) {
	w.t.Helper()
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/" + name, Type: storagev1alpha1.DatasetTypeFilesystem, Source: src,
		},
	}
	if err := w.c.Create(w.ctx, vol); err != nil {
		w.t.Fatal(err)
	}
	res, err := w.r.Reconcile(w.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		w.t.Fatalf("reconcile: %v", err)
	}
	var got storagev1alpha1.ZfsDataset
	if err := w.c.Get(w.ctx, client.ObjectKey{Name: name}, &got); err != nil {
		w.t.Fatal(err)
	}
	return res, &got
}

func (w *refWorld) origin(ds string) string {
	w.t.Helper()
	v, err := w.z.Get(w.ctx, ds, "origin")
	if err != nil {
		w.t.Fatalf("origin of %s: %v", ds, err)
	}
	if v == "-" {
		return ""
	}
	return v
}

func (w *refWorld) exists(name string) bool {
	_, err := w.z.Get(w.ctx, name, "type")
	return err == nil
}

func snapRef(name string) *storagev1alpha1.DatasetSource {
	return &storagev1alpha1.DatasetSource{ZfsSnapshotRef: &storagev1alpha1.ObjectRef{Name: name}}
}

func datasetRef(name string) *storagev1alpha1.DatasetSource {
	return &storagev1alpha1.DatasetSource{ZfsDatasetRef: &storagev1alpha1.ObjectRef{Name: name}}
}

func (w *refWorld) mustZ(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func TestSourceRef_RestoreFindsRawOnTheSource(t *testing.T) {
	w := newRefWorld(t, readySnapshot())
	_, got := w.provision("pvc-r1", snapRef("snap-1"))
	if want := "tank/k8s/pvc-src@" + rawName; w.origin("tank/k8s/pvc-r1") != want {
		t.Errorf("origin = %q, want %q", w.origin("tank/k8s/pvc-r1"), want)
	}
	if got.Status.Phase != storagev1alpha1.DatasetPhaseReady {
		t.Errorf("phase = %q (%s)", got.Status.Phase, got.Status.Message)
	}
	if w.z.Scans() != 0 {
		t.Errorf("the backing clone's origin must be enough, but the pool was scanned %d times", w.z.Scans())
	}
	if w.z.Check() != nil {
		t.Fatal(w.z.Check())
	}
}

func TestSourceRef_RestoreFollowsRawAfterAnotherRestoreWasPromoted(t *testing.T) {
	w := newRefWorld(t, readySnapshot())
	w.provision("pvc-r1", snapRef("snap-1"))
	w.mustZ(w.z.Promote(w.ctx, "tank/k8s/pvc-r1")) // raw now lives on pvc-r1
	if !w.exists("tank/k8s/pvc-r1@" + rawName) {
		t.Fatal("setup: raw should be on pvc-r1")
	}
	w.provision("pvc-r2", snapRef("snap-1"))
	if want := "tank/k8s/pvc-r1@" + rawName; w.origin("tank/k8s/pvc-r2") != want {
		t.Errorf("origin = %q, want %q", w.origin("tank/k8s/pvc-r2"), want)
	}
	if w.z.Scans() != 0 {
		t.Errorf("the backing clone's origin must follow the promoted raw snapshot, scans = %d", w.z.Scans())
	}
}

func TestSourceRef_RestoreFindsRawOnThePromotedBackingClone(t *testing.T) {
	w := newRefWorld(t, readySnapshot())
	w.mustZ(w.z.Promote(w.ctx, "tank/k8s/"+rawName)) // origin of the backing clone is now empty
	w.provision("pvc-r1", snapRef("snap-1"))
	if w.z.Scans() != 0 {
		t.Errorf("a promoted backing clone holds the raw snapshot itself, no scan needed, got %d", w.z.Scans())
	}
	if want := "tank/k8s/" + rawName + "@" + rawName; w.origin("tank/k8s/pvc-r1") != want {
		t.Errorf("origin = %q, want %q", w.origin("tank/k8s/pvc-r1"), want)
	}
}

func TestSourceRef_RestoreFindsRawOnTheSourceWhenTheBackingCloneIsGone(t *testing.T) {
	w := newRefWorld(t, readySnapshot())
	w.mustZ(w.z.Destroy(w.ctx, "tank/k8s/"+rawName, false))
	w.provision("pvc-r1", snapRef("snap-1"))
	if want := "tank/k8s/pvc-src@" + rawName; w.origin("tank/k8s/pvc-r1") != want {
		t.Errorf("origin = %q, want %q", w.origin("tank/k8s/pvc-r1"), want)
	}
}

func TestSourceRef_RestoreWaitsWhenNothingCanBeCloned(t *testing.T) {
	terminating := readySnapshot()
	now := metav1.NewTime(time.Now())
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"x"}
	notReadySnap := readySnapshot()
	notReadySnap.Status.Phase = storagev1alpha1.SnapshotPhasePending

	for name, snap := range map[string]*storagev1alpha1.ZfsSnapshot{
		"missing": nil, "terminating": terminating, "not-ready": notReadySnap,
	} {
		t.Run(name, func(t *testing.T) {
			w := newRefWorld(t, snap)
			res, got := w.provision("pvc-r1", snapRef("snap-1"))
			if w.exists("tank/k8s/pvc-r1") {
				t.Fatal("a dataset was created without a usable source")
			}
			if res.RequeueAfter <= 0 {
				t.Errorf("no retry scheduled: %+v", res)
			}
			if got.Status.Phase != storagev1alpha1.DatasetPhaseError || !hasReason(got, "SourceNotReady") {
				t.Errorf("status = %q %+v", got.Status.Phase, got.Status.Conditions)
			}
		})
	}
}

func TestSourceRef_RestoreWaitsWhenTheRawSnapshotIsNowhere(t *testing.T) {
	w := newRefWorld(t, readySnapshot())
	w.mustZ(w.z.Destroy(w.ctx, "tank/k8s/"+rawName, false))
	w.mustZ(w.z.Destroy(w.ctx, "tank/k8s/pvc-src@"+rawName, false))
	res, _ := w.provision("pvc-r1", snapRef("snap-1"))
	if w.exists("tank/k8s/pvc-r1") || res.RequeueAfter <= 0 {
		t.Fatalf("must wait: exists=%v res=%+v", w.exists("tank/k8s/pvc-r1"), res)
	}
}

func TestSourceRef_RestoreFromOtherPoolFails(t *testing.T) {
	snap := readySnapshot()
	snap.Spec.PoolGUID = "other"
	w := newRefWorld(t, snap)
	res, got := w.provision("pvc-r1", snapRef("snap-1"))
	if w.exists("tank/k8s/pvc-r1") || res.RequeueAfter != 0 || !hasReason(got, "CreateFailed") {
		t.Errorf("cross-pool restore must fail hard: res=%+v status=%+v", res, got.Status.Conditions)
	}
}

func TestSourceRef_CloneFromDatasetReference(t *testing.T) {
	w := newRefWorld(t, nil)
	_, got := w.provision("pvc-c1", datasetRef("pvc-src"))
	snap := "tank/k8s/pvc-src@clone-pvc-c1"
	if w.origin("tank/k8s/pvc-c1") != snap {
		t.Errorf("origin = %q, want %q", w.origin("tank/k8s/pvc-c1"), snap)
	}
	if v, _ := w.z.Get(w.ctx, snap, zpool.PropRole); v != zpool.RoleCloneOrigin {
		t.Errorf("clone origin label = %q", v)
	}
	if got.Status.Phase != storagev1alpha1.DatasetPhaseReady {
		t.Errorf("phase = %q", got.Status.Phase)
	}
}

func TestSourceRef_CloneFromMissingOrDeletingDatasetWaits(t *testing.T) {
	w := newRefWorld(t, nil)
	res, got := w.provision("pvc-c1", datasetRef("nope"))
	if w.exists("tank/k8s/pvc-c1") || res.RequeueAfter <= 0 || !hasReason(got, "SourceNotReady") {
		t.Errorf("missing source: res=%+v status=%+v", res, got.Status.Conditions)
	}
	var src storagev1alpha1.ZfsDataset
	w.mustZ(w.c.Get(w.ctx, client.ObjectKey{Name: "pvc-src"}, &src))
	src.Finalizers = []string{"x"}
	w.mustZ(w.c.Update(w.ctx, &src))
	w.mustZ(w.c.Delete(w.ctx, &src))
	res, _ = w.provision("pvc-c2", datasetRef("pvc-src"))
	if w.exists("tank/k8s/pvc-c2") || res.RequeueAfter <= 0 {
		t.Errorf("deleting source must not be cloned: res=%+v", res)
	}
}

func TestSourceRef_PathSourcesStillWork(t *testing.T) {
	w := newRefWorld(t, nil)
	w.provision("pvc-p1", &storagev1alpha1.DatasetSource{Snapshot: "k8s/pvc-src@" + rawName})
	if want := "tank/k8s/pvc-src@" + rawName; w.origin("tank/k8s/pvc-p1") != want {
		t.Errorf("snapshot path: origin %q", w.origin("tank/k8s/pvc-p1"))
	}
	w.provision("pvc-p2", &storagev1alpha1.DatasetSource{Volume: "k8s/pvc-src"})
	if want := "tank/k8s/pvc-src@clone-pvc-p2"; w.origin("tank/k8s/pvc-p2") != want {
		t.Errorf("volume path: origin %q", w.origin("tank/k8s/pvc-p2"))
	}
}

func hasReason(v *storagev1alpha1.ZfsDataset, reason string) bool {
	for _, c := range v.Status.Conditions {
		if c.Reason == reason {
			return true
		}
	}
	return false
}
