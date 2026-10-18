package controller

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
	"github.com/hellivan/simple-zfs-csi/internal/zpool/memzfs"
)

// lifePool is the ZFS the lifecycle harness runs on: the in-memory model, or a
// real pool (real_lifecycle_test.go, build tag zfsreal).
type lifePool interface {
	zpool.ZFS
	Datasets() []string
	Snapshots() []string
	Deferred(snapshot string) bool
	// Scans is how many pool-wide snapshot searches the driver ran.
	Scans() int
	// Check validates the model's own invariants; a real pool has none.
	Check() error
}

// The reconcilers log through controller-runtime, which prints a stack trace
// when no logger is set. The harness has no use for the logs.
func init() { ctrl.SetLogger(logr.Discard()) }

type injector interface {
	Inject(op, name string, err error, times int)
}

// realPool is set by the real-pool test file: the sequence and walk counts
// shrink because every step runs real zfs commands.
var realPool bool

// worldFactory builds the world a test runs in; the real-pool test file swaps it.
var worldFactory = newMemWorld

// lifeWorld drives the real ZfsDataset and ZfsSnapshot reconcilers against the
// in-memory ZFS. It is the scenario harness for strategy D
// (docs/snapshot-delete-redesign-plan.md): every operation is what the CSI
// controller does (create or delete an object), and after every step the
// invariants below must hold.
type lifeWorld struct {
	t        *testing.T
	ctx      context.Context
	z        lifePool
	pool     string   // ZFS pool name
	base     string   // full name of the dataset all volumes live under
	prefix   string   // base relative to the pool, as written in spec.dataset
	baseline []string // datasets and snapshots present before the test started
	c        client.Client
	snapR    *ZfsSnapshotReconciler
	dsR      *ZfsDatasetReconciler
	n        int
	vols     map[string]string // volume -> "", "snapshot:<name>" or "volume:<name>"
	snaps    map[string]string // ZfsSnapshot name -> source volume
	hist     []string
	flaky    bool // a reconcile error during delete is a retry, not a failure
}

func newLifeWorld(t *testing.T) *lifeWorld { return worldFactory(t) }

func newMemWorld(t *testing.T) *lifeWorld {
	t.Helper()
	z := memzfs.New("tank")
	if err := z.CreateDataset(context.Background(), "tank/k8s", nil); err != nil {
		t.Fatal(err)
	}
	return buildWorld(t, z, "tank", "k8s")
}

// buildWorld wires the reconcilers to z. prefix is the dataset (relative to the
// pool) that already exists and holds every volume.
func buildWorld(t *testing.T, z lifePool, pool, prefix string) *lifeWorld {
	t.Helper()
	scheme := newTestScheme(t)
	zp := onlinePool()
	zp.Status.PoolName = pool
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zp).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}, &storagev1alpha1.ZfsSnapshot{}).Build()
	w := &lifeWorld{
		t: t, ctx: context.Background(), z: z, c: c, pool: pool, base: pool + "/" + prefix, prefix: prefix,
		snapR: &ZfsSnapshotReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z},
		dsR:   &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z},
		vols:  map[string]string{}, snaps: map[string]string{},
	}
	w.baseline = append(z.Datasets(), z.Snapshots()...)
	return w
}

func (w *lifeWorld) fail(format string, a ...any) {
	w.t.Helper()
	w.t.Fatalf("%s\nsequence: %s", fmt.Sprintf(format, a...), strings.Join(w.hist, " ; "))
}

func (w *lifeWorld) reconcileDS(name string) error {
	_, err := w.dsR.Reconcile(w.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	return err
}

func (w *lifeWorld) reconcileSnap(name string) error {
	_, err := w.snapR.Reconcile(w.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	return err
}

func (w *lifeWorld) gone(obj client.Object, name string) bool {
	err := w.c.Get(w.ctx, client.ObjectKey{Name: name}, obj)
	return apierrors.IsNotFound(err)
}

func (w *lifeWorld) mkVol(source *storagev1alpha1.DatasetSource, kind string) string {
	w.n++
	name := fmt.Sprintf("pvc-%d", w.n)
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: w.prefix + "/" + name, Type: storagev1alpha1.DatasetTypeFilesystem, Source: source,
		},
	}
	if err := w.c.Create(w.ctx, vol); err != nil {
		w.fail("create %s: %v", name, err)
	}
	for i := 0; i < 4; i++ {
		if err := w.reconcileDS(name); err != nil {
			w.fail("reconcile %s: %v", name, err)
		}
	}
	var got storagev1alpha1.ZfsDataset
	if err := w.c.Get(w.ctx, client.ObjectKey{Name: name}, &got); err != nil || got.Status.Phase != storagev1alpha1.DatasetPhaseReady {
		w.fail("%s not Ready: phase=%q msg=%q err=%v", name, got.Status.Phase, got.Status.Message, err)
	}
	w.vols[name] = kind
	return name
}

func (w *lifeWorld) newVol() string {
	w.hist = append(w.hist, "vol")
	return w.mkVol(nil, "")
}

func (w *lifeWorld) clone(src string) string {
	w.hist = append(w.hist, "clone("+src+")")
	return w.mkVol(datasetRef(src), "volume:"+src)
}

func (w *lifeWorld) restore(snap string) string {
	w.hist = append(w.hist, "restore("+snap+")")
	return w.mkVol(snapRef(snap), "snapshot:"+snap)
}

func (w *lifeWorld) snapshot(src string) string {
	w.hist = append(w.hist, "snap("+src+")")
	w.n++
	name := fmt.Sprintf("snap-%d", w.n)
	s := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: w.prefix + "/" + src, SnapshotName: "csi-snap-" + name, SourceVolume: src,
			SourceType: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
	if err := w.c.Create(w.ctx, s); err != nil {
		w.fail("create %s: %v", name, err)
	}
	for i := 0; i < 4; i++ {
		if err := w.reconcileSnap(name); err != nil {
			w.fail("reconcile %s: %v", name, err)
		}
	}
	var got storagev1alpha1.ZfsSnapshot
	if err := w.c.Get(w.ctx, client.ObjectKey{Name: name}, &got); err != nil || got.Status.Phase != storagev1alpha1.SnapshotPhaseReady {
		w.fail("%s not Ready: phase=%q msg=%q err=%v", name, got.Status.Phase, got.Status.Message, err)
	}
	w.snaps[name] = src
	return name
}

func (w *lifeWorld) delVol(name string) {
	w.hist = append(w.hist, "delvol("+name+")")
	var vol storagev1alpha1.ZfsDataset
	if err := w.c.Get(w.ctx, client.ObjectKey{Name: name}, &vol); err != nil {
		w.fail("get %s: %v", name, err)
	}
	if err := w.c.Delete(w.ctx, &vol); err != nil {
		w.fail("delete %s: %v", name, err)
	}
	for i := 0; i < 20 && !w.gone(&storagev1alpha1.ZfsDataset{}, name); i++ {
		if err := w.reconcileDS(name); err != nil && !w.flaky {
			w.fail("deleting %s is stuck: %v", name, err)
		}
	}
	if !w.gone(&storagev1alpha1.ZfsDataset{}, name) {
		w.fail("%s was not released", name)
	}
	delete(w.vols, name)
}

func (w *lifeWorld) delSnap(name string) {
	w.hist = append(w.hist, "delsnap("+name+")")
	var s storagev1alpha1.ZfsSnapshot
	if err := w.c.Get(w.ctx, client.ObjectKey{Name: name}, &s); err != nil {
		w.fail("get %s: %v", name, err)
	}
	if err := w.c.Delete(w.ctx, &s); err != nil {
		w.fail("delete %s: %v", name, err)
	}
	for i := 0; i < 20 && !w.gone(&storagev1alpha1.ZfsSnapshot{}, name); i++ {
		if err := w.reconcileSnap(name); err != nil && !w.flaky {
			w.fail("deleting %s is stuck: %v", name, err)
		}
	}
	if !w.gone(&storagev1alpha1.ZfsSnapshot{}, name) {
		w.fail("%s was not released", name)
	}
	delete(w.snaps, name)
}

func sorted(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// verify checks the invariants that must hold after every step: the model's
// own, every live volume still has its dataset, every live snapshot is still
// restorable (its raw snapshot exists and is not marked for destruction), and
// nothing on the pool is unowned: a snapshot is the raw snapshot of a live
// ZfsSnapshot, the "@clone-" snapshot of a live volume, or marked for deferred
// destruction (ZFS removes those itself); a dataset is a live volume or the
// backing clone of a live ZfsSnapshot.
func (w *lifeWorld) verify() {
	w.t.Helper()
	if err := w.z.Check(); err != nil {
		w.fail("invariant: %v", err)
	}
	if n := w.z.Scans(); n != 0 {
		w.fail("the driver scanned the pool %d times; every snapshot is found through its backing clone or its dataset", n)
	}
	datasets := map[string]bool{}
	for _, d := range w.z.Datasets() {
		datasets[d] = true
	}
	owned := map[string]bool{}
	for _, n := range w.baseline {
		owned[n] = true
	}
	for v := range w.vols {
		if !datasets[w.base+"/"+v] {
			w.fail("live volume %s lost its dataset", v)
		}
		owned[w.base+"/"+v] = true
	}
	rawOwned := map[string]bool{}
	for s := range w.snaps {
		raw := "csi-snap-" + s
		rawOwned[raw] = true
		owned[w.base+"/"+raw] = true
		at, err := locateRawSnapshot(w.ctx, w.z, w.base+"/"+raw, w.base+"/"+w.snaps[s]+"@"+raw, raw)
		if err != nil || at == "" {
			w.fail("live snapshot %s lost its raw snapshot (err=%v)", s, err)
		}
		if w.z.Deferred(at) {
			w.fail("live snapshot %s has a raw snapshot marked for destruction", s)
		}
	}
	for d := range datasets {
		if !owned[d] {
			w.fail("unowned dataset %s", d)
		}
	}
	for _, sn := range w.z.Snapshots() {
		short := sn[strings.Index(sn, "@")+1:]
		switch {
		case rawOwned[short], w.z.Deferred(sn):
		case strings.HasPrefix(short, "clone-") && w.vols[strings.TrimPrefix(short, "clone-")] != "":
		default:
			w.fail("ghost snapshot %s", sn)
		}
	}
}

// drain deletes everything that is left, snapshots and volumes in the given
// order, and requires an empty pool afterwards.
func (w *lifeWorld) drain(snapsFirst bool) {
	w.t.Helper()
	delSnaps := func() {
		for _, s := range sorted(w.snaps) {
			w.delSnap(s)
			w.verify()
		}
	}
	delVols := func() {
		for _, v := range sorted(w.vols) {
			w.delVol(v)
			w.verify()
		}
	}
	if snapsFirst {
		delSnaps()
		delVols()
	} else {
		delVols()
		delSnaps()
	}
	w.requireEmpty()
}

func (w *lifeWorld) requireEmpty() {
	w.t.Helper()
	got := append(w.z.Datasets(), w.z.Snapshots()...)
	sort.Strings(got)
	want := append([]string{}, w.baseline...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		w.fail("left behind: %v, want only %v", got, want)
	}
}

// Named scenarios from the redesign plan.

func TestLifecycle_RestoreThenDeleteSnapshotPinsNothing(t *testing.T) {
	w := newLifeWorld(t)
	v := w.newVol()
	s := w.snapshot(v)
	r := w.restore(s)
	w.delSnap(s)
	w.verify()
	// Only the restore still depends on the data; nothing is pinned on the source.
	for _, sn := range w.z.Snapshots() {
		if strings.HasPrefix(sn, w.base+"/"+v+"@") && !w.z.Deferred(sn) {
			t.Errorf("source %s still owns a live snapshot %s", v, sn)
		}
	}
	// Deleting the restore must release everything: no old data is kept.
	w.delVol(r)
	w.verify()
	w.delVol(v)
	w.requireEmpty()
}

func TestLifecycle_DeleteSourceKeepsSnapshotAndRestoreWorking(t *testing.T) {
	w := newLifeWorld(t)
	v := w.newVol()
	s := w.snapshot(v)
	w.delVol(v)
	w.verify()
	// Deleting the source promoted the backing clone: it now holds the raw
	// snapshot and has no origin, so the raw snapshot is found by the pool scan.
	bk := w.base + "/csi-snap-" + s
	if o, _ := w.z.Get(w.ctx, bk, "origin"); o != "-" {
		t.Fatalf("precondition: backing clone origin = %q, want -", o)
	}
	if got := w.z.Snapshots(); len(got) != 1 || got[0] != bk+"@csi-snap-"+s {
		t.Fatalf("precondition: raw snapshot should sit on the backing clone, got %v", got)
	}
	w.restore(s)
	w.verify()
	w.drain(true)
}

func TestLifecycle_FanOutRestoresOfOneSnapshot(t *testing.T) {
	for _, snapsFirst := range []bool{true, false} {
		w := newLifeWorld(t)
		v := w.newVol()
		s := w.snapshot(v)
		for i := 0; i < 3; i++ {
			w.restore(s)
			w.verify()
		}
		w.drain(snapsFirst)
	}
}

func TestLifecycle_ChainedRestoresAndSnapshots(t *testing.T) {
	w := newLifeWorld(t)
	v := w.newVol()
	s1 := w.snapshot(v)
	r1 := w.restore(s1)
	s2 := w.snapshot(r1)
	r2 := w.restore(s2)
	w.verify()
	w.delVol(v)
	w.verify()
	w.delSnap(s1)
	w.verify()
	w.delVol(r1)
	w.verify()
	w.delSnap(s2)
	w.verify()
	w.delVol(r2)
	w.requireEmpty()
}

func TestLifecycle_DirectCloneOriginIsReleased(t *testing.T) {
	w := newLifeWorld(t)
	v := w.newVol()
	c := w.clone(v)
	w.verify()
	w.delVol(v) // the clone is promoted off the source
	w.verify()
	w.delVol(c)
	w.requireEmpty()
}

// A single failing ZFS call anywhere in a delete must only delay it (the
// reconcile returns the error and is retried), never wedge it, leak something
// or damage a live object. P8: "dataset is busy" is one of these.
func TestLifecycle_SingleFailureDuringDeleteIsRetried(t *testing.T) {
	build := func() *lifeWorld {
		w := newLifeWorld(t)
		v := w.newVol()
		s := w.snapshot(v)
		r := w.restore(s)
		w.snapshot(r)
		w.clone(v)
		w.verify()
		return w
	}
	probe := build()
	if _, ok := probe.z.(injector); !ok {
		t.Skip("this pool cannot inject failures")
	}
	targets := append(probe.z.Datasets(), probe.z.Snapshots()...)
	for _, op := range []string{"destroy", "destroy-d", "promote"} {
		for _, target := range targets {
			for _, snapsFirst := range []bool{true, false} {
				w := build()
				w.flaky = true
				w.z.(injector).Inject(op, target, errors.New("cannot destroy: dataset is busy"), 1)
				w.hist = append(w.hist, "inject("+op+" "+target+")")
				w.drain(snapsFirst)
			}
		}
	}
}

// The same operations in every order, up to a bounded number of creations.
// Every sequence must keep the invariants after every step and end empty.
func TestLifecycle_AllSequences(t *testing.T) {
	// 3 creations is 131 sequences (about 2 s); 4 is 4705 (about 80 s), 5 more
	// still. LIFECYCLE_MAX_CREATES raises the bound for a deeper manual run.
	maxCreates := 3
	if testing.Short() || realPool {
		maxCreates = 2
	}
	if v, err := strconv.Atoi(os.Getenv("LIFECYCLE_MAX_CREATES")); err == nil && v > 0 {
		maxCreates = v
	}
	type op struct {
		kind string
		arg  string
	}
	var total int
	var walk func(seq []op, vols, snaps []string, creates int)
	run := func(seq []op) {
		total++
		w := newLifeWorld(t)
		names := map[string]string{} // abstract name -> real name
		real := func(a string) string { return names[a] }
		vn, sn := 0, 0
		for _, o := range seq {
			switch o.kind {
			case "vol":
				vn++
				names[fmt.Sprintf("v%d", vn)] = w.newVol()
			case "clone":
				vn++
				names[fmt.Sprintf("v%d", vn)] = w.clone(real(o.arg))
			case "restore":
				vn++
				names[fmt.Sprintf("v%d", vn)] = w.restore(real(o.arg))
			case "snap":
				sn++
				names[fmt.Sprintf("s%d", sn)] = w.snapshot(real(o.arg))
			case "delvol":
				w.delVol(real(o.arg))
			case "delsnap":
				w.delSnap(real(o.arg))
			}
			w.verify()
		}
		w.requireEmpty()
	}
	remove := func(l []string, x string) []string {
		var out []string
		for _, y := range l {
			if y != x {
				out = append(out, y)
			}
		}
		return out
	}
	walk = func(seq []op, vols, snaps []string, creates int) {
		if creates == 0 && len(vols) == 0 && len(snaps) == 0 {
			run(seq)
			return
		}
		next := func(o op, v, s []string, c int) {
			walk(append(append([]op{}, seq...), o), v, s, c)
		}
		countV, countS := 0, 0
		for _, o := range seq {
			switch o.kind {
			case "vol", "clone", "restore":
				countV++
			case "snap":
				countS++
			}
		}
		if creates > 0 {
			newV := fmt.Sprintf("v%d", countV+1)
			newS := fmt.Sprintf("s%d", countS+1)
			next(op{"vol", ""}, append(append([]string{}, vols...), newV), snaps, creates-1)
			for _, v := range vols {
				next(op{"clone", v}, append(append([]string{}, vols...), newV), snaps, creates-1)
				next(op{"snap", v}, vols, append(append([]string{}, snaps...), newS), creates-1)
			}
			for _, s := range snaps {
				next(op{"restore", s}, append(append([]string{}, vols...), newV), snaps, creates-1)
			}
		}
		for _, v := range vols {
			next(op{"delvol", v}, remove(vols, v), snaps, creates)
		}
		for _, s := range snaps {
			next(op{"delsnap", s}, vols, remove(snaps, s), creates)
		}
	}
	walk(nil, nil, nil, maxCreates)
	t.Logf("%d sequences", total)
}

func TestLifecycle_RandomWalks(t *testing.T) {
	walks := 150
	if testing.Short() {
		walks = 20
	}
	if realPool {
		walks = 6
		if v, err := strconv.Atoi(os.Getenv("LIFECYCLE_WALKS")); err == nil && v > 0 {
			walks = v
		}
	}
	for seed := int64(1); seed <= int64(walks); seed++ {
		rng := rand.New(rand.NewSource(seed))
		w := newLifeWorld(t)
		creates := 7
		for step := 0; step < 40 && (creates > 0 || len(w.vols)+len(w.snaps) > 0); step++ {
			vols, snaps := sorted(w.vols), sorted(w.snaps)
			var acts []func()
			if creates > 0 {
				acts = append(acts, func() { w.newVol() })
				for _, v := range vols {
					v := v
					acts = append(acts, func() { w.snapshot(v) }, func() { w.clone(v) })
				}
				for _, s := range snaps {
					s := s
					acts = append(acts, func() { w.restore(s) })
				}
			}
			for _, v := range vols {
				v := v
				acts = append(acts, func() { w.delVol(v) })
			}
			for _, s := range snaps {
				s := s
				acts = append(acts, func() { w.delSnap(s) })
			}
			// Prefer progress towards an empty pool once the budget is spent.
			if creates == 0 {
				acts = acts[len(acts)-(len(vols)+len(snaps)):]
			}
			before := len(w.vols) + len(w.snaps)
			acts[rng.Intn(len(acts))]()
			if len(w.vols)+len(w.snaps) > before {
				creates--
			}
			w.verify()
		}
		w.drain(rng.Intn(2) == 0)
	}
}
