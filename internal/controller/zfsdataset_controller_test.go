package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// fakeZFS is an in-memory zpool.ZFS used to assert the reconciler's create,
// destroy and resize behaviour without shelling out.
//
// It deliberately models the ZFS semantics the delete path depends on. A double
// that cannot fail the way ZFS fails hides exactly the bugs that matter here
// (known-pitfalls.md class 17), so it implements:
//
//   - snapshot ownership — which dataset each snapshot currently belongs to,
//     in creation order;
//   - `zfs destroy` refusing with "filesystem has children" when a dataset
//     still owns snapshots, and with "snapshot has dependent clones" when
//     something still clones a snapshot;
//   - `zfs promote`'s full rewrite, which touches four things at once rather
//     than simply clearing an origin (zfs-promote.8 and OpenZFS
//     dsl_dataset_promote_sync).
//
// TestFakeZFSPromote_MatchesLivePoolVerification pins the promote model against
// the real-pool run recorded in docs/promote-order-verification-2026-07-31.md,
// so this fidelity is itself tested rather than assumed.
type fakeZFS struct {
	existing       map[string]bool
	props          map[string]map[string]string
	createdDS      []string
	createdZvol    map[string]int64
	destroyed      []string
	lastDSProps    map[string]string
	lastZvProps    map[string]string
	cloned         []string // records "snapshot -> dest" for each Clone
	lastCloneProps map[string]string
	setProps       []string            // records "name property=value" for each SetProperty
	ownership      []string            // records "mountpoint uid:gid mode" for each ApplyOwnership
	origin         map[string]string   // dataset -> its current origin snapshot ("" / absent = none)
	snapshots      map[string][]string // dataset -> its own snapshot suffixes, oldest first
	promoted       []string            // records each dataset name passed to Promote
	promoteErr     map[string]error    // optional: force Promote to fail for a given dataset
}

func newFakeZFS(existing ...string) *fakeZFS {
	f := &fakeZFS{
		existing:    map[string]bool{},
		props:       map[string]map[string]string{},
		createdZvol: map[string]int64{},
		origin:      map[string]string{},
		snapshots:   map[string][]string{},
		promoteErr:  map[string]error{},
	}
	for _, e := range existing {
		f.existing[e] = true
	}
	return f
}

// splitSnapshotName splits "pool/ds@snap" into ("pool/ds", "snap"). The suffix
// is empty when name is not a snapshot.
func splitSnapshotName(name string) (dataset, suffix string) {
	if i := strings.Index(name, "@"); i >= 0 {
		return name[:i], name[i+1:]
	}
	return name, ""
}

// seedSnapshot registers a pre-existing snapshot on dataset. Call order defines
// creation order, which is what `zfs promote` uses to decide how far back to
// drag snapshots along.
func (f *fakeZFS) seedSnapshot(dataset, suffix string) {
	for _, s := range f.snapshots[dataset] {
		if s == suffix {
			return
		}
	}
	f.snapshots[dataset] = append(f.snapshots[dataset], suffix)
	f.existing[dataset+"@"+suffix] = true
}

// seedClone registers dest as a pre-existing clone of dataset@suffix, creating
// the snapshot on dataset if it isn't there yet. Tests should use this rather
// than assigning origin directly: a clone whose origin snapshot does not exist
// is not a state real ZFS can be in, and promote would rightly reject it.
func (f *fakeZFS) seedClone(dataset, suffix, dest string) {
	f.seedSnapshot(dataset, suffix)
	f.existing[dest] = true
	f.origin[dest] = dataset + "@" + suffix
}

func (f *fakeZFS) store(name string, props map[string]string) {
	m := map[string]string{}
	for k, v := range props {
		m[k] = v
	}
	f.props[name] = m
}

func (f *fakeZFS) CreateDataset(_ context.Context, name string, props map[string]string) error {
	f.createdDS = append(f.createdDS, name)
	f.lastDSProps = props
	f.existing[name] = true
	f.store(name, props)
	return nil
}

func (f *fakeZFS) CreateZvol(_ context.Context, name string, sizeBytes int64, props map[string]string) error {
	f.createdZvol[name] = sizeBytes
	f.lastZvProps = props
	f.existing[name] = true
	f.store(name, props)
	f.props[name]["volsize"] = strconv.FormatInt(sizeBytes, 10)
	return nil
}

// Destroy models `zfs destroy`, including the two refusals the non-recursive
// delete path (D11) depends on. Destroying something that does not exist stays
// a success, mirroring CLI.Destroy's isNotExist tolerance. Every call is
// recorded, successful or not, so tests can assert the call sequence.
func (f *fakeZFS) Destroy(_ context.Context, name string, recursive bool) error {
	f.destroyed = append(f.destroyed, name)
	if !f.existing[name] {
		return nil
	}
	dataset, suffix := splitSnapshotName(name)
	if suffix != "" {
		for other, o := range f.origin {
			if o == name {
				return fmt.Errorf("cannot destroy %q: snapshot has dependent clones (%s)", name, other)
			}
		}
		remaining := f.snapshots[dataset][:0:0]
		for _, s := range f.snapshots[dataset] {
			if s != suffix {
				remaining = append(remaining, s)
			}
		}
		f.snapshots[dataset] = remaining
		delete(f.existing, name)
		delete(f.props, name)
		return nil
	}
	if snaps := f.snapshots[name]; len(snaps) > 0 {
		if !recursive {
			return fmt.Errorf("cannot destroy %q: filesystem has children\nuse '-r' to destroy the following datasets:\n%s@%s",
				name, name, strings.Join(snaps, "\n"+name+"@"))
		}
		for _, s := range append([]string(nil), snaps...) {
			if err := f.Destroy(context.Background(), name+"@"+s, false); err != nil {
				return err
			}
		}
	}
	delete(f.existing, name)
	delete(f.props, name)
	delete(f.origin, name)
	delete(f.snapshots, name)
	return nil
}

func (f *fakeZFS) Snapshot(_ context.Context, name string) error {
	dataset, suffix := splitSnapshotName(name)
	if suffix == "" {
		return fmt.Errorf("snapshot name %q must be of the form pool/dataset@snap", name)
	}
	f.createdDS = append(f.createdDS, name)
	f.seedSnapshot(dataset, suffix)
	f.props[name] = map[string]string{"creation": "1700000000", "referenced": "1048576"}
	return nil
}

func (f *fakeZFS) Clone(_ context.Context, snapshot, dest string, props map[string]string) error {
	f.cloned = append(f.cloned, snapshot+" -> "+dest)
	f.lastCloneProps = props
	f.existing[dest] = true
	f.store(dest, props)
	f.origin[dest] = snapshot
	return nil
}

// Promote models `zfs promote` faithfully. A single call rewrites four things,
// not one:
//
//  1. the origin snapshot *and every snapshot created before it* move from the
//     former parent onto dataset (zfs-promote.8: "The snapshot that was cloned,
//     and any snapshots previous to this snapshot, are now owned by the
//     promoted clone");
//  2. every other clone of a relocated snapshot is re-parented onto dataset
//     (dsl_dataset_promote_sync's "move any clone references");
//  3. dataset inherits the former parent's own previous origin — so a promoted
//     dataset does *not* necessarily end up independent, which the live-pool
//     run confirmed and which D13's verification check was corrected for;
//  4. the former parent becomes a clone of dataset at that snapshot.
//
// A no-op (recorded, but no state change) when dataset has no origin, mirroring
// the real CLI.Promote's early return.
func (f *fakeZFS) Promote(_ context.Context, dataset string) error {
	if err := f.promoteErr[dataset]; err != nil {
		f.promoted = append(f.promoted, dataset)
		return err
	}
	origin, ok := f.origin[dataset]
	if !ok || origin == "" {
		return nil // already independent; real CLI.Promote never shells out here either
	}
	f.promoted = append(f.promoted, dataset)

	parent, suffix := splitSnapshotName(origin)
	if suffix == "" {
		return fmt.Errorf("origin %q is not a snapshot", origin)
	}
	parentSnaps := f.snapshots[parent]
	idx := -1
	for i, s := range parentSnaps {
		if s == suffix {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("cannot promote %q: origin snapshot %q does not exist on %q", dataset, origin, parent)
	}
	parentOrigin := f.origin[parent]

	// (1) relocate the origin snapshot and everything older than it.
	moved := append([]string(nil), parentSnaps[:idx+1]...)
	f.snapshots[parent] = append([]string(nil), parentSnaps[idx+1:]...)
	f.snapshots[dataset] = append(moved, f.snapshots[dataset]...)

	for _, s := range moved {
		delete(f.existing, parent+"@"+s)
		f.existing[dataset+"@"+s] = true
		// (2) re-parent sibling clones of each relocated snapshot.
		for other, o := range f.origin {
			if other == dataset || other == parent {
				continue
			}
			if o == parent+"@"+s {
				f.origin[other] = dataset + "@" + s
			}
		}
	}

	// (3) dataset takes over the former parent's lineage, which may be empty.
	if parentOrigin == "" {
		delete(f.origin, dataset)
	} else {
		f.origin[dataset] = parentOrigin
	}
	// (4) the former parent is now a clone of the promoted dataset.
	f.origin[parent] = dataset + "@" + suffix
	return nil
}

// ListSnapshots returns dataset's own snapshots, oldest first, mirroring
// `zfs list -t snapshot -d 1 -s creation`.
func (f *fakeZFS) ListSnapshots(_ context.Context, dataset string) ([]string, error) {
	if !f.existing[dataset] {
		return nil, fmt.Errorf("%w: %s", zpool.ErrNotExist, dataset)
	}
	var out []string
	for _, s := range f.snapshots[dataset] {
		out = append(out, dataset+"@"+s)
	}
	return out, nil
}

// Clones returns the datasets that currently clone snapshot, mirroring the ZFS
// `clones` property. Sorted so the delete path's "promote the first one" choice
// is deterministic in tests.
func (f *fakeZFS) Clones(_ context.Context, snapshot string) ([]string, error) {
	if !f.existing[snapshot] {
		return nil, fmt.Errorf("%w: %s", zpool.ErrNotExist, snapshot)
	}
	var out []string
	for ds, o := range f.origin {
		if o == snapshot {
			out = append(out, ds)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeZFS) Get(_ context.Context, name, property string) (string, error) {
	if !f.existing[name] {
		return "", fmt.Errorf("%w: %s", zpool.ErrNotExist, name)
	}
	if property == "type" {
		return "filesystem", nil
	}
	if property == "origin" {
		if o := f.origin[name]; o != "" {
			return o, nil
		}
		return "-", nil
	}
	if m := f.props[name]; m != nil {
		return m[property], nil
	}
	return "", nil
}

func (f *fakeZFS) SetProperty(_ context.Context, name, property, value string) error {
	if f.props[name] == nil {
		f.props[name] = map[string]string{}
	}
	f.props[name][property] = value
	f.setProps = append(f.setProps, name+" "+property+"="+value)
	return nil
}

func (f *fakeZFS) List(context.Context, zpool.DatasetKind) ([]zpool.Dataset, error) {
	return nil, nil
}

func (f *fakeZFS) ApplyOwnership(_ context.Context, mountpoint string, uid, gid *int64, mode string) error {
	u, g := "", ""
	if uid != nil {
		u = strconv.FormatInt(*uid, 10)
	}
	if gid != nil {
		g = strconv.FormatInt(*gid, 10)
	}
	f.ownership = append(f.ownership, fmt.Sprintf("%s %s:%s %s", mountpoint, u, g, mode))
	return nil
}

// TestFakeZFSPromote_MatchesLivePoolVerification pins fakeZFS.Promote against
// the real-pool run recorded in docs/promote-order-verification-2026-07-31.md:
// six snapshots of one volume, each with its own backing clone, promoted in the
// same deliberately scrambled order (t3, t1, t6, t2, t4, t5). The model must
// reproduce that run's converged final state exactly — including the *non-empty*
// chained origins, which is precisely what the previous test double got wrong
// (it always cleared origin) and what D13's verification check had to be
// corrected for.
//
// This test exists so the double's fidelity is verified rather than assumed: if
// it drifts from real ZFS, every delete-path test built on it silently stops
// meaning anything (known-pitfalls.md class 17).
func TestFakeZFSPromote_MatchesLivePoolVerification(t *testing.T) {
	const vol = "tank/test/vol1"
	clone := func(n int) string { return fmt.Sprintf("tank/test/csi-snap-t%d", n) }
	snap := func(n int) string { return fmt.Sprintf("snap_t%d", n) }

	z := newFakeZFS(vol)
	for n := 1; n <= 6; n++ {
		z.seedClone(vol, snap(n), clone(n))
	}

	for _, n := range []int{3, 1, 6, 2, 4, 5} {
		if err := z.Promote(context.Background(), clone(n)); err != nil {
			t.Fatalf("promote csi-snap-t%d: %v", n, err)
		}
	}

	// Final state, verbatim from the live run's §3.6 listing.
	for ds, want := range map[string]string{
		clone(1): "",
		clone(2): clone(1) + "@" + snap(1),
		clone(3): clone(2) + "@" + snap(2),
		clone(4): clone(3) + "@" + snap(3),
		clone(5): clone(4) + "@" + snap(4),
		clone(6): clone(5) + "@" + snap(5),
		vol:      clone(6) + "@" + snap(6),
	} {
		if got := z.origin[ds]; got != want {
			t.Errorf("origin[%s] = %q, want %q", ds, got, want)
		}
	}

	// Every backing clone owns exactly and only its own snapshot...
	for n := 1; n <= 6; n++ {
		if got := z.snapshots[clone(n)]; !reflect.DeepEqual(got, []string{snap(n)}) {
			t.Errorf("snapshots[csi-snap-t%d] = %v, want [%s]", n, got, snap(n))
		}
	}
	// ...and vol1 has none left, which is the precondition that makes D11's
	// plain (non-recursive) destroy succeed.
	if got := z.snapshots[vol]; len(got) != 0 {
		t.Errorf("snapshots[vol1] = %v, want none", got)
	}
	if err := z.Destroy(context.Background(), vol, false); err != nil {
		t.Errorf("non-recursive destroy of vol1 should succeed: %v", err)
	}
}

func onlinePool() *storagev1alpha1.ZfsPool {
	return &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: "zpool-999"},
		Status: storagev1alpha1.ZfsPoolStatus{
			GUID:          "999",
			PoolName:      "tank",
			CurrentNode:   "node-a",
			BaseMountPath: "/mnt/tank",
			Health:        storagev1alpha1.PoolHealthOnline,
		},
	}
}

func TestDatasetName(t *testing.T) {
	tests := []struct {
		pool, dataset, want string
		wantErr             bool
	}{
		{"tank", "k8s/pvc-1", "tank/k8s/pvc-1", false},
		{"tank", "/media/movies/", "tank/media/movies", false},
		{"", "x", "", true},
		{"tank", "/", "", true},
	}
	for _, tt := range tests {
		got, err := datasetName(tt.pool, tt.dataset)
		if tt.wantErr {
			if err == nil {
				t.Errorf("datasetName(%q,%q) expected error", tt.pool, tt.dataset)
			}
			continue
		}
		if err != nil {
			t.Errorf("datasetName(%q,%q) unexpected error: %v", tt.pool, tt.dataset, err)
		}
		if got != tt.want {
			t.Errorf("datasetName(%q,%q) = %q, want %q", tt.pool, tt.dataset, got, tt.want)
		}
	}
}

func TestDeriveVolumePath(t *testing.T) {
	tests := []struct {
		name          string
		volType       storagev1alpha1.DatasetType
		baseMountPath string
		poolName      string
		dataset       string
		want          string
		wantErr       bool
	}{
		{name: "filesystem joins base mount path", volType: storagev1alpha1.DatasetTypeFilesystem, baseMountPath: "/mnt/tank", dataset: "k8s/pvc-1", want: "/mnt/tank/k8s/pvc-1"},
		{name: "volume device node", volType: storagev1alpha1.DatasetTypeVolume, poolName: "tank", dataset: "k8s/pvc-1", want: "/dev/zvol/tank/k8s/pvc-1"},
		{name: "filesystem without mount path errors", volType: storagev1alpha1.DatasetTypeFilesystem, dataset: "x", wantErr: true},
		{name: "volume without pool name errors", volType: storagev1alpha1.DatasetTypeVolume, dataset: "x", wantErr: true},
		{name: "empty dataset errors", volType: storagev1alpha1.DatasetTypeFilesystem, baseMountPath: "/tank", dataset: "/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deriveVolumePath(tt.volType, tt.baseMountPath, tt.poolName, tt.dataset)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("deriveVolumePath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestZfsDatasetReconcile_CreatesDatasetAndSetsReady(t *testing.T) {
	scheme := newTestScheme(t)
	quota := resource.MustParse("1Gi")
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID:   "999",
			Dataset:    "k8s/pvc-1",
			Type:       storagev1alpha1.DatasetTypeFilesystem,
			Properties: map[string]string{"compression": "lz4"},
			Filesystem: &storagev1alpha1.FilesystemConfig{Quota: &quota},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}

	// First pass installs the finalizer; second pass provisions and reports Ready.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	if len(z.createdDS) != 1 || z.createdDS[0] != "tank/k8s/pvc-1" {
		t.Fatalf("expected dataset tank/k8s/pvc-1 created, got %v", z.createdDS)
	}
	if z.lastDSProps["compression"] != "lz4" {
		t.Errorf("compression prop not passed: %v", z.lastDSProps)
	}
	if z.lastDSProps["refquota"] == "" {
		t.Errorf("refquota not derived from quota: %v", z.lastDSProps)
	}

	var got storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, zfsDatasetFinalizer) {
		t.Errorf("finalizer not set")
	}
	if got.Status.Phase != storagev1alpha1.DatasetPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.Path != "/mnt/tank/k8s/pvc-1" {
		t.Errorf("path = %q, want /mnt/tank/k8s/pvc-1", got.Status.Path)
	}
	// No uid/gid/mode requested -> ownership must be left untouched.
	if len(z.ownership) != 0 {
		t.Errorf("expected no ApplyOwnership calls, got %v", z.ownership)
	}
}

func TestZfsDatasetReconcile_AppliesRootOwnership(t *testing.T) {
	scheme := newTestScheme(t)
	uid := int64(1000)
	gid := int64(2000)
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-own"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-own",
			Type:     storagev1alpha1.DatasetTypeFilesystem,
			Filesystem: &storagev1alpha1.FilesystemConfig{
				UID:  &uid,
				GID:  &gid,
				Mode: "0770",
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-own"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	want := "/mnt/tank/k8s/pvc-own 1000:2000 0770"
	if len(z.ownership) != 1 || z.ownership[0] != want {
		t.Fatalf("ApplyOwnership = %v, want [%q]", z.ownership, want)
	}

	var got storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-own"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if got.Status.Phase != storagev1alpha1.DatasetPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
}

func TestZfsDatasetReconcile_ZvolUsesSize(t *testing.T) {
	scheme := newTestScheme(t)
	size := resource.MustParse("10Gi")
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-blk", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-blk",
			Type:     storagev1alpha1.DatasetTypeVolume,
			Volume:   &storagev1alpha1.VolumeConfig{Size: size, Volblocksize: "16k"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-blk"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := z.createdZvol["tank/k8s/pvc-blk"]; got != size.Value() {
		t.Fatalf("zvol size = %d, want %d", got, size.Value())
	}
	if z.lastZvProps["volblocksize"] != "16k" {
		t.Errorf("volblocksize not passed: %v", z.lastZvProps)
	}

	var got storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-blk"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if got.Status.Path != "/dev/zvol/tank/k8s/pvc-blk" {
		t.Errorf("path = %q, want /dev/zvol/tank/k8s/pvc-blk", got.Status.Path)
	}
}

func TestZfsDatasetReconcile_ClonesFromSnapshot(t *testing.T) {
	scheme := newTestScheme(t)
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-restore", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-restore",
			Type:     storagev1alpha1.DatasetTypeFilesystem,
			Source:   &storagev1alpha1.DatasetSource{Snapshot: "k8s/pvc-src@snap-1"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-restore"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(z.cloned) != 1 || z.cloned[0] != "tank/k8s/pvc-src@snap-1 -> tank/k8s/pvc-restore" {
		t.Fatalf("expected clone from snapshot, got %v", z.cloned)
	}
	if len(z.createdDS) != 0 {
		t.Errorf("clone should not create an empty dataset, got %v", z.createdDS)
	}
}

func TestZfsDatasetReconcile_ClonesFromVolume(t *testing.T) {
	scheme := newTestScheme(t)
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-clone", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-clone",
			Type:     storagev1alpha1.DatasetTypeFilesystem,
			Source:   &storagev1alpha1.DatasetSource{Volume: "k8s/pvc-src"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-clone"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The intermediate snapshot suffix is now a hash of the destination object
	// name, not the raw name itself (independent-resource-naming-redesign.md).
	wantSnap := "tank/k8s/pvc-src@clone-" + cloneSnapshotSuffix("pvc-clone")
	if len(z.createdDS) != 1 || z.createdDS[0] != wantSnap {
		t.Fatalf("expected intermediate snapshot %q, got %v", wantSnap, z.createdDS)
	}
	if len(z.cloned) != 1 || z.cloned[0] != wantSnap+" -> tank/k8s/pvc-clone" {
		t.Fatalf("expected clone from volume snapshot, got %v", z.cloned)
	}
}

func TestZfsDatasetReconcile_ExpandsFilesystemQuota(t *testing.T) {
	scheme := newTestScheme(t)
	small := resource.MustParse("1Gi")
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-fs", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID:   "999",
			Dataset:    "k8s/pvc-fs",
			Type:       storagev1alpha1.DatasetTypeFilesystem,
			Filesystem: &storagev1alpha1.FilesystemConfig{Quota: &small},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-fs"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}

	// Grow the quota and reconcile: the agent must apply refquota via SetProperty.
	var cur storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-fs"}, &cur); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	large := resource.MustParse("5Gi")
	cur.Spec.Filesystem.Quota = &large
	if err := c.Update(context.Background(), &cur); err != nil {
		t.Fatalf("update quota: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile expand: %v", err)
	}

	want := "tank/k8s/pvc-fs refquota=" + strconv.FormatInt(large.Value(), 10)
	if !containsString(z.setProps, want) {
		t.Fatalf("expected SetProperty %q, got %v", want, z.setProps)
	}
}

func TestZfsDatasetReconcile_GrowsZvolVolsize(t *testing.T) {
	scheme := newTestScheme(t)
	small := resource.MustParse("1Gi")
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-blk", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-blk",
			Type:     storagev1alpha1.DatasetTypeVolume,
			Volume:   &storagev1alpha1.VolumeConfig{Size: small, Volblocksize: "16k"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-blk"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}

	var cur storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-blk"}, &cur); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	large := resource.MustParse("4Gi")
	cur.Spec.Volume.Size = large
	if err := c.Update(context.Background(), &cur); err != nil {
		t.Fatalf("update size: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile expand: %v", err)
	}

	want := "tank/k8s/pvc-blk volsize=" + strconv.FormatInt(large.Value(), 10)
	if !containsString(z.setProps, want) {
		t.Fatalf("expected SetProperty %q, got %v", want, z.setProps)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestZfsDatasetReconcile_IdempotentWhenExists(t *testing.T) {
	scheme := newTestScheme(t)
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-1",
			Type:     storagev1alpha1.DatasetTypeFilesystem,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-1")
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(z.createdDS) != 0 {
		t.Fatalf("expected no create when dataset already exists, got %v", z.createdDS)
	}
	var got storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if got.Status.Phase != storagev1alpha1.DatasetPhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
}

func TestZfsDatasetReconcile_IgnoresVolumeOnOtherNode(t *testing.T) {
	scheme := newTestScheme(t)
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-1",
			Type:     storagev1alpha1.DatasetTypeFilesystem,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS()
	// This agent runs on node-b, but the pool is hosted on node-a.
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-b", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(z.createdDS) != 0 {
		t.Fatalf("expected no create on non-hosting node, got %v", z.createdDS)
	}
	var got storagev1alpha1.ZfsDataset
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if controllerutil.ContainsFinalizer(&got, zfsDatasetFinalizer) {
		t.Errorf("non-hosting node should not add finalizer")
	}
	if got.Status.Phase != "" {
		t.Errorf("non-hosting node should not set status, got %q", got.Status.Phase)
	}
}

func TestZfsDatasetReconcile_DeleteDestroysAndReleases(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "pvc-1",
			Finalizers:        []string{zfsDatasetFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-1",
			Type:     storagev1alpha1.DatasetTypeFilesystem,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-1")
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(z.destroyed) != 1 || z.destroyed[0] != "tank/k8s/pvc-1" {
		t.Fatalf("expected destroy of tank/k8s/pvc-1, got %v", z.destroyed)
	}

	var got storagev1alpha1.ZfsDataset
	err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got)
	if err == nil {
		t.Fatalf("expected volume to be removed after finalizer release")
	}
}

// TestZfsDatasetReconcile_DeleteBlocksOnPendingSnapshot verifies D3: a volume
// with an in-flight (not yet Ready) dependent snapshot must not be destroyed.
func TestZfsDatasetReconcile_DeleteBlocksOnPendingSnapshot(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-1", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-1",
		},
		Status: storagev1alpha1.ZfsSnapshotStatus{Phase: storagev1alpha1.SnapshotPhasePending},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}, &storagev1alpha1.ZfsSnapshot{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-1")
	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}})
	if err == nil {
		t.Fatal("expected reconcile to block/error while a dependent snapshot is still in-flight")
	}
	if len(z.destroyed) != 0 {
		t.Fatalf("volume should not be destroyed while a dependent snapshot is in-flight, got %v", z.destroyed)
	}
}

// TestZfsDatasetReconcile_DeleteGateReadsThroughAPIReader pins ADR-0023: the
// delete-path gates must never be satisfied by a lagging informer. The
// ZfsSnapshot that blocks this destroy is authored by the CSI controller (a
// different process, using an uncached client) and is a different kind than the
// ZfsDataset that triggered the reconcile, so no watch ordering relates the two
// — modelled here by a cached client that cannot see the snapshot and an
// APIReader that can. Reading the gate from the cache would fail *open* and
// destroy the source of an in-flight snapshot.
func TestZfsDatasetReconcile_DeleteGateReadsThroughAPIReader(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-1", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-1",
		},
		Status: storagev1alpha1.ZfsSnapshotStatus{Phase: storagev1alpha1.SnapshotPhasePending},
	}

	// The informer has not delivered the snapshot yet; the API server has it.
	cached := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()
	api := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol, snap).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}, &storagev1alpha1.ZfsSnapshot{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-1")
	r := &ZfsDatasetReconciler{Client: cached, Scheme: scheme, NodeName: "node-a", ZFS: z, APIReader: api}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}})
	if err == nil {
		t.Fatal("expected the destroy to block on the snapshot the API server can see")
	}
	if len(z.destroyed) != 0 {
		t.Fatalf("volume must not be destroyed on the strength of a stale cache, got %v", z.destroyed)
	}
}

// TestZfsDatasetReconcile_DeletePromotesStandaloneBackingCloneAndSucceeds
// verifies D0/D3/D11: a Ready standalone-mode dependent's backing clone is
// promoted away (never blocked on), after which the source volume's own
// (non-recursive) destroy succeeds.
func TestZfsDatasetReconcile_DeletePromotesStandaloneBackingCloneAndSucceeds(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-1", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-1",
		},
		Status: storagev1alpha1.ZfsSnapshotStatus{Phase: storagev1alpha1.SnapshotPhaseReady, ReadyToUse: true},
	}
	backing := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-x"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/csi-snap-x", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/pvc-1@csi-snap-x"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol, snap, backing).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}, &storagev1alpha1.ZfsSnapshot{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-1")
	z.seedClone("tank/k8s/pvc-1", "csi-snap-x", "tank/k8s/csi-snap-x")

	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(z.promoted) != 1 || z.promoted[0] != "tank/k8s/csi-snap-x" {
		t.Fatalf("expected backing clone promoted, got %v", z.promoted)
	}
	if len(z.destroyed) != 1 || z.destroyed[0] != "tank/k8s/pvc-1" {
		t.Fatalf("expected source volume destroyed, got %v", z.destroyed)
	}
}

// TestZfsDatasetReconcile_DeletePromotesDirectCloneDependents verifies D7/D9:
// a direct PVC-to-PVC clone (ADR-0009, no VolumeSnapshot involved) is always
// promoted away unconditionally before the source is destroyed.
func TestZfsDatasetReconcile_DeletePromotesDirectCloneDependents(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-src", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	clone := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-clone"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-clone", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Volume: "k8s/pvc-src"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol, clone).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-src")
	z.seedClone("tank/k8s/pvc-src", "clone-"+cloneSnapshotSuffix("pvc-clone"), "tank/k8s/pvc-clone")

	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-src"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(z.promoted) != 1 || z.promoted[0] != "tank/k8s/pvc-clone" {
		t.Fatalf("expected direct clone promoted, got %v", z.promoted)
	}
	if len(z.destroyed) != 1 || z.destroyed[0] != "tank/k8s/pvc-src" {
		t.Fatalf("expected source destroyed, got %v", z.destroyed)
	}
}

// TestZfsDatasetReconcile_DeletePromotesMultipleRestoredDependents verifies
// D17 for the §2.9 case: deleting a standalone-mode backing clone with two
// simultaneous restored-PVC dependents succeeds. Promoting *one* of them
// detaches the snapshot from both, because ZFS re-parents the sibling onto the
// promoted clone in the same operation — so a single promote is enough, and
// nothing needs to have been tracked in advance.
func TestZfsDatasetReconcile_DeletePromotesMultipleRestoredDependents(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	isController, blockOwnerDeletion := true, true
	backing := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "csi-snap-x",
			Finalizers:        []string{zfsDatasetFinalizer},
			DeletionTimestamp: &now,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         storagev1alpha1.GroupVersion.String(),
				Kind:               "ZfsSnapshot",
				Name:               "snap-1",
				Controller:         &isController,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/csi-snap-x", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/pvc-src@csi-snap-x"},
		},
	}
	// The owning snapshot must itself be terminating, or F7's guard refuses.
	snapOwner := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "snap-1",
			Finalizers:        []string{zfsSnapshotFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-src",
		},
	}
	r1 := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-r1", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-r1", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/csi-snap-x@restore-source"},
		},
	}
	r2 := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-r2", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-r2", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/csi-snap-x@restore-source"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), backing, snapOwner, r1, r2).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/csi-snap-x")
	// The backing clone's own state after its source volume was deleted earlier:
	// the raw origin snapshot relocated onto it (older), then its own
	// @restore-source (newer), with both restores cloning the latter.
	z.seedSnapshot("tank/k8s/csi-snap-x", "csi-snap-x")
	z.seedClone("tank/k8s/csi-snap-x", restoreSourceSnapshotName, "tank/k8s/pvc-r1")
	z.seedClone("tank/k8s/csi-snap-x", restoreSourceSnapshotName, "tank/k8s/pvc-r2")

	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "csi-snap-x"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// One promote detaches the snapshot from both dependents: ZFS re-parents
	// pvc-r2 onto pvc-r1 as part of the same operation (§2.9).
	if !reflect.DeepEqual(z.promoted, []string{"tank/k8s/pvc-r1"}) {
		t.Fatalf("promoted = %v, want exactly [tank/k8s/pvc-r1]", z.promoted)
	}
	if o, want := z.origin["tank/k8s/pvc-r2"], "tank/k8s/pvc-r1@"+restoreSourceSnapshotName; o != want {
		t.Errorf("pvc-r2 origin = %q, want %q (sibling re-parented onto pvc-r1)", o, want)
	}
	// Both of the backing clone's snapshots relocated onto pvc-r1, so it owns no
	// snapshots by the time destroy runs and no artifact cleanup is needed.
	if !reflect.DeepEqual(z.destroyed, []string{"tank/k8s/csi-snap-x"}) {
		t.Fatalf("destroyed = %v, want exactly [tank/k8s/csi-snap-x]", z.destroyed)
	}

	// Both restored PVCs survive and stay deletable in either order — that is
	// the property F2c was breaking. Delete pvc-r1 (which now owns the shared
	// history) first, then pvc-r2.
	for _, name := range []string{"pvc-r1", "pvc-r2"} {
		var dep storagev1alpha1.ZfsDataset
		if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &dep); err != nil {
			t.Fatalf("get %q: %v", name, err)
		}
		if err := c.Delete(context.Background(), &dep); err != nil {
			t.Fatalf("delete %q: %v", name, err)
		}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
			t.Fatalf("reconcile delete %q: %v", name, err)
		}
		if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &dep); err == nil {
			t.Errorf("%q still present after delete", name)
		}
	}
}

// TestZfsDatasetReconcile_DirectCloneRemainsDeletableAfterSourceDeleted is the
// F2a regression. Deleting a direct PVC-to-PVC clone's source promotes the
// clone, which relocates ADR-0009's intermediate "@clone-<hash>" snapshot onto
// it. Before D17/D18 nothing ever cleaned that relocated snapshot up, so the
// clone's own non-recursive destroy failed with "filesystem has children" on
// every retry and its ZfsDataset stayed Terminating forever — the PV never
// released and the space never reclaimed.
func TestZfsDatasetReconcile_DirectCloneRemainsDeletableAfterSourceDeleted(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	cloneSnap := "clone-" + cloneSnapshotSuffix("pvc-clone")

	src := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-src", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-src", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	clone := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-clone", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-clone", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Volume: "k8s/pvc-src"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), src, clone).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-src")
	z.seedClone("tank/k8s/pvc-src", cloneSnap, "tank/k8s/pvc-clone")

	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-src"}}); err != nil {
		t.Fatalf("reconcile delete source: %v", err)
	}
	// The intermediate snapshot is now owned by the clone, exactly as real ZFS
	// leaves it — this is the state the old code could never recover from.
	if got := z.snapshots["tank/k8s/pvc-clone"]; !reflect.DeepEqual(got, []string{cloneSnap}) {
		t.Fatalf("snapshots[pvc-clone] = %v, want [%s]", got, cloneSnap)
	}

	var dep storagev1alpha1.ZfsDataset
	if err := c.Get(ctx, client.ObjectKey{Name: "pvc-clone"}, &dep); err != nil {
		t.Fatalf("get pvc-clone: %v", err)
	}
	if err := c.Delete(ctx, &dep); err != nil {
		t.Fatalf("delete pvc-clone: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-clone"}}); err != nil {
		t.Fatalf("reconcile delete clone: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "pvc-clone"}, &dep); err == nil {
		t.Fatal("pvc-clone still present: its non-recursive destroy did not succeed")
	}

	// The relocated artifact was destroyed explicitly, before the dataset, and
	// never via `zfs destroy -r`.
	wantOrder := []string{"tank/k8s/pvc-src", "tank/k8s/pvc-clone@" + cloneSnap, "tank/k8s/pvc-clone"}
	if !reflect.DeepEqual(z.destroyed, wantOrder) {
		t.Fatalf("destroyed = %v, want %v", z.destroyed, wantOrder)
	}
}

// TestZfsDatasetReconcile_DeleteBlocksOnUnprovisionedDependent verifies D21: a
// restore whose ZfsDataset object exists but whose `zfs clone` has not run yet
// is invisible in the ZFS clone graph, so the delete path must block on spec
// rather than destroy the source out from under it.
func TestZfsDatasetReconcile_DeleteBlocksOnUnprovisionedDependent(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	backing := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-x", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/csi-snap-x", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	pending := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-restore", Finalizers: []string{zfsDatasetFinalizer}},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-restore", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/csi-snap-x@" + restoreSourceSnapshotName},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), backing, pending).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	// Only the backing clone exists on disk; pvc-restore has not been cloned yet.
	z := newFakeZFS("tank/k8s/csi-snap-x")
	z.seedSnapshot("tank/k8s/csi-snap-x", restoreSourceSnapshotName)

	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "csi-snap-x"}})
	if err == nil {
		t.Fatal("expected the delete to block while a declared dependent is not provisioned yet")
	}
	if len(z.destroyed) != 0 {
		t.Fatalf("nothing should have been destroyed, got %v", z.destroyed)
	}
}

// TestZfsDatasetReconcile_DeleteRefusesForeignSnapshot verifies D18's
// fail-loud allow-list: a snapshot the driver did not create blocks the destroy
// with a clear error instead of being silently removed or triggering a
// fallback to `zfs destroy -r`.
func TestZfsDatasetReconcile_DeleteRefusesForeignSnapshot(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-1")
	z.seedSnapshot("tank/k8s/pvc-1", "sanoid_daily_2026-08-03")

	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}})
	if err == nil {
		t.Fatal("expected the delete to refuse a snapshot the driver did not create")
	}
	if !strings.Contains(err.Error(), "not created by this driver") {
		t.Errorf("error = %v, want it to name the foreign snapshot as the reason", err)
	}
	if len(z.destroyed) != 0 {
		t.Fatalf("nothing should have been destroyed, got %v", z.destroyed)
	}
}
