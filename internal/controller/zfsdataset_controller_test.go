package controller

import (
	"context"
	"fmt"
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
	setProps       []string          // records "name property=value" for each SetProperty
	ownership      []string          // records "mountpoint uid:gid mode" for each ApplyOwnership
	origin         map[string]string // dataset -> its current origin snapshot ("" / absent = none)
	promoted       []string          // records each dataset name passed to Promote
	promoteErr     map[string]error  // optional: force Promote to fail for a given dataset
}

func newFakeZFS(existing ...string) *fakeZFS {
	f := &fakeZFS{
		existing:    map[string]bool{},
		props:       map[string]map[string]string{},
		createdZvol: map[string]int64{},
		origin:      map[string]string{},
		promoteErr:  map[string]error{},
	}
	for _, e := range existing {
		f.existing[e] = true
	}
	return f
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

func (f *fakeZFS) Destroy(_ context.Context, name string, _ bool) error {
	f.destroyed = append(f.destroyed, name)
	delete(f.existing, name)
	delete(f.props, name)
	return nil
}

func (f *fakeZFS) Snapshot(_ context.Context, name string) error {
	f.createdDS = append(f.createdDS, name)
	f.existing[name] = true
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

// Promote is a simplified model of `zfs promote` (real semantics: D0/D12/D13 in
// snapshot-lifecycle-redesign.md). It clears dest's origin and, mirroring real
// ZFS's "move any clone references" behaviour (D12), reparents any *other*
// tracked dataset whose origin was exactly the same snapshot onto the newly
// relocated one instead. A no-op (recorded, but no state change) if dataset
// has no origin, mirroring the real CLI.Promote's early-return.
func (f *fakeZFS) Promote(_ context.Context, dataset string) error {
	if err := f.promoteErr[dataset]; err != nil {
		f.promoted = append(f.promoted, dataset)
		return err
	}
	origin, ok := f.origin[dataset]
	if !ok || origin == "" {
		return nil // already independent; real CLI.Promote never calls `zfs promote` here either
	}
	f.promoted = append(f.promoted, dataset)
	at := strings.Index(origin, "@")
	if at < 0 {
		return fmt.Errorf("origin %q is not a snapshot", origin)
	}
	suffix := origin[at:]
	relocated := dataset + suffix
	delete(f.origin, dataset)
	for other, o := range f.origin {
		if o == origin {
			f.origin[other] = relocated
		}
	}
	return nil
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
			SourceVolume: "pvc-1", Mode: storagev1alpha1.SnapshotModeStandalone,
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

// TestZfsDatasetReconcile_DeleteBlocksOnIntegratedModeSnapshot verifies §3.2:
// an integrated-mode dependent has no promote mechanism, so DeleteVolume must
// keep blocking (requeuing) until it's gone, exactly like before this redesign.
func TestZfsDatasetReconcile_DeleteBlocksOnIntegratedModeSnapshot(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Finalizers: []string{zfsDatasetFinalizer}, DeletionTimestamp: &now},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	snap := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-1", SnapshotName: "snap-1",
			SourceVolume: "pvc-1", Mode: storagev1alpha1.SnapshotModeIntegrated,
		},
		Status: storagev1alpha1.ZfsSnapshotStatus{Phase: storagev1alpha1.SnapshotPhaseReady, ReadyToUse: true},
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
		t.Fatal("expected reconcile to block/error while a live integrated-mode snapshot exists")
	}
	if len(z.destroyed) != 0 {
		t.Fatalf("volume should not be destroyed while an integrated-mode dependent exists, got %v", z.destroyed)
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
			SourceVolume: "pvc-1", Mode: storagev1alpha1.SnapshotModeStandalone,
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

	z := newFakeZFS("tank/k8s/pvc-1", "tank/k8s/csi-snap-x")
	z.origin["tank/k8s/csi-snap-x"] = "tank/k8s/pvc-1@csi-snap-x"

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

	z := newFakeZFS("tank/k8s/pvc-src", "tank/k8s/pvc-clone")
	z.origin["tank/k8s/pvc-clone"] = "tank/k8s/pvc-src@clone-" + cloneSnapshotSuffix("pvc-clone")

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
// D12/D15's generalized dependent tracking: deleting a standalone-mode backing
// clone with two simultaneous restored-PVC dependents (each tracked via its
// own restored-by.* finalizer) promotes both away and still succeeds,
// regardless of real ZFS's sibling-clone-reparenting behaviour when one is
// promoted before the other (§2.9).
func TestZfsDatasetReconcile_DeletePromotesMultipleRestoredDependents(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	isController, blockOwnerDeletion := true, true
	backing := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{
			Name: "csi-snap-x",
			Finalizers: []string{
				zfsDatasetFinalizer,
				restoredByFinalizer("pvc-r1"),
				restoredByFinalizer("pvc-r2"),
			},
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
	snapOwner := &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: storagev1alpha1.ZfsSnapshotSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-src", SnapshotName: "csi-snap-x",
			SourceVolume: "pvc-src", Mode: storagev1alpha1.SnapshotModeStandalone,
		},
	}
	r1 := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-r1"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999", Dataset: "k8s/pvc-r1", Type: storagev1alpha1.DatasetTypeFilesystem,
			Source: &storagev1alpha1.DatasetSource{Snapshot: "k8s/csi-snap-x@restore-source"},
		},
	}
	r2 := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-r2"},
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

	z := newFakeZFS("tank/k8s/csi-snap-x", "tank/k8s/pvc-r1", "tank/k8s/pvc-r2")
	z.origin["tank/k8s/pvc-r1"] = "tank/k8s/csi-snap-x@restore-source"
	z.origin["tank/k8s/pvc-r2"] = "tank/k8s/csi-snap-x@restore-source"

	r := &ZfsDatasetReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "csi-snap-x"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(z.promoted) != 2 {
		t.Fatalf("expected both restored dependents promoted exactly once, got %v", z.promoted)
	}
	if o := z.origin["tank/k8s/pvc-r1"]; o != "" {
		t.Errorf("pvc-r1 origin = %q, want cleared (fully independent)", o)
	}
	if o := z.origin["tank/k8s/pvc-r2"]; o != "" {
		t.Errorf("pvc-r2 origin = %q, want cleared (fully independent)", o)
	}
	if len(z.destroyed) != 1 || z.destroyed[0] != "tank/k8s/csi-snap-x" {
		t.Fatalf("expected backing clone destroyed, got %v", z.destroyed)
	}

	// pvc-r1/pvc-r2 remain untouched, independent objects — only the backing
	// clone (and its own tracking finalizers) were affected.
	for _, name := range []string{"pvc-r1", "pvc-r2"} {
		var got storagev1alpha1.ZfsDataset
		if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &got); err != nil {
			t.Errorf("get %q: %v", name, err)
		}
	}
}

