package controller

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	storagev1alpha1 "github.com/hellivan/zfs-shares/api/v1alpha1"
	"github.com/hellivan/zfs-shares/internal/zpool"
)

// fakeZFS is an in-memory zpool.ZFS used to assert the reconciler's create and
// destroy behaviour without shelling out.
type fakeZFS struct {
	existing    map[string]bool
	createdDS   []string
	createdZvol map[string]int64
	destroyed   []string
	lastDSProps map[string]string
	lastZvProps map[string]string
}

func newFakeZFS(existing ...string) *fakeZFS {
	f := &fakeZFS{existing: map[string]bool{}, createdZvol: map[string]int64{}}
	for _, e := range existing {
		f.existing[e] = true
	}
	return f
}

func (f *fakeZFS) CreateDataset(_ context.Context, name string, props map[string]string) error {
	f.createdDS = append(f.createdDS, name)
	f.lastDSProps = props
	f.existing[name] = true
	return nil
}

func (f *fakeZFS) CreateZvol(_ context.Context, name string, sizeBytes int64, props map[string]string) error {
	f.createdZvol[name] = sizeBytes
	f.lastZvProps = props
	f.existing[name] = true
	return nil
}

func (f *fakeZFS) Destroy(_ context.Context, name string, _ bool) error {
	f.destroyed = append(f.destroyed, name)
	delete(f.existing, name)
	return nil
}

func (f *fakeZFS) Get(_ context.Context, name, _ string) (string, error) {
	if f.existing[name] {
		return "filesystem", nil
	}
	return "", fmt.Errorf("%w: %s", zpool.ErrNotExist, name)
}

func (f *fakeZFS) List(context.Context, zpool.DatasetKind) ([]zpool.Dataset, error) {
	return nil, nil
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
		volType       storagev1alpha1.VolumeType
		baseMountPath string
		poolName      string
		dataset       string
		want          string
		wantErr       bool
	}{
		{name: "filesystem joins base mount path", volType: storagev1alpha1.VolumeTypeFilesystem, baseMountPath: "/mnt/tank", dataset: "k8s/pvc-1", want: "/mnt/tank/k8s/pvc-1"},
		{name: "volume device node", volType: storagev1alpha1.VolumeTypeVolume, poolName: "tank", dataset: "k8s/pvc-1", want: "/dev/zvol/tank/k8s/pvc-1"},
		{name: "filesystem without mount path errors", volType: storagev1alpha1.VolumeTypeFilesystem, dataset: "x", wantErr: true},
		{name: "volume without pool name errors", volType: storagev1alpha1.VolumeTypeVolume, dataset: "x", wantErr: true},
		{name: "empty dataset errors", volType: storagev1alpha1.VolumeTypeFilesystem, baseMountPath: "/tank", dataset: "/", wantErr: true},
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

func TestZfsVolumeReconcile_CreatesDatasetAndSetsReady(t *testing.T) {
	scheme := newTestScheme(t)
	quota := resource.MustParse("1Gi")
	vol := &storagev1alpha1.ZfsVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec: storagev1alpha1.ZfsVolumeSpec{
			PoolGUID:   "999",
			Dataset:    "k8s/pvc-1",
			Type:       storagev1alpha1.VolumeTypeFilesystem,
			Properties: map[string]string{"compression": "lz4"},
			Filesystem: &storagev1alpha1.FilesystemConfig{Quota: &quota},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsVolume{}).
		Build()

	z := newFakeZFS()
	r := &ZfsVolumeReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
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

	var got storagev1alpha1.ZfsVolume
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, zfsVolumeFinalizer) {
		t.Errorf("finalizer not set")
	}
	if got.Status.Phase != storagev1alpha1.VolumePhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.Path != "/mnt/tank/k8s/pvc-1" {
		t.Errorf("path = %q, want /mnt/tank/k8s/pvc-1", got.Status.Path)
	}
}

func TestZfsVolumeReconcile_ZvolUsesSize(t *testing.T) {
	scheme := newTestScheme(t)
	size := resource.MustParse("10Gi")
	vol := &storagev1alpha1.ZfsVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-blk", Finalizers: []string{zfsVolumeFinalizer}},
		Spec: storagev1alpha1.ZfsVolumeSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-blk",
			Type:     storagev1alpha1.VolumeTypeVolume,
			Volume:   &storagev1alpha1.VolumeConfig{Size: size, Volblocksize: "16k"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsVolume{}).
		Build()

	z := newFakeZFS()
	r := &ZfsVolumeReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-blk"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := z.createdZvol["tank/k8s/pvc-blk"]; got != size.Value() {
		t.Fatalf("zvol size = %d, want %d", got, size.Value())
	}
	if z.lastZvProps["volblocksize"] != "16k" {
		t.Errorf("volblocksize not passed: %v", z.lastZvProps)
	}

	var got storagev1alpha1.ZfsVolume
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-blk"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if got.Status.Path != "/dev/zvol/tank/k8s/pvc-blk" {
		t.Errorf("path = %q, want /dev/zvol/tank/k8s/pvc-blk", got.Status.Path)
	}
}

func TestZfsVolumeReconcile_IdempotentWhenExists(t *testing.T) {
	scheme := newTestScheme(t)
	vol := &storagev1alpha1.ZfsVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1", Finalizers: []string{zfsVolumeFinalizer}},
		Spec: storagev1alpha1.ZfsVolumeSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-1",
			Type:     storagev1alpha1.VolumeTypeFilesystem,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsVolume{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-1")
	r := &ZfsVolumeReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(z.createdDS) != 0 {
		t.Fatalf("expected no create when dataset already exists, got %v", z.createdDS)
	}
	var got storagev1alpha1.ZfsVolume
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if got.Status.Phase != storagev1alpha1.VolumePhaseReady {
		t.Errorf("phase = %q, want Ready", got.Status.Phase)
	}
}

func TestZfsVolumeReconcile_IgnoresVolumeOnOtherNode(t *testing.T) {
	scheme := newTestScheme(t)
	vol := &storagev1alpha1.ZfsVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec: storagev1alpha1.ZfsVolumeSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-1",
			Type:     storagev1alpha1.VolumeTypeFilesystem,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsVolume{}).
		Build()

	z := newFakeZFS()
	// This agent runs on node-b, but the pool is hosted on node-a.
	r := &ZfsVolumeReconciler{Client: c, Scheme: scheme, NodeName: "node-b", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(z.createdDS) != 0 {
		t.Fatalf("expected no create on non-hosting node, got %v", z.createdDS)
	}
	var got storagev1alpha1.ZfsVolume
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if controllerutil.ContainsFinalizer(&got, zfsVolumeFinalizer) {
		t.Errorf("non-hosting node should not add finalizer")
	}
	if got.Status.Phase != "" {
		t.Errorf("non-hosting node should not set status, got %q", got.Status.Phase)
	}
}

func TestZfsVolumeReconcile_DeleteDestroysAndReleases(t *testing.T) {
	scheme := newTestScheme(t)
	now := metav1.Now()
	vol := &storagev1alpha1.ZfsVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "pvc-1",
			Finalizers:        []string{zfsVolumeFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: storagev1alpha1.ZfsVolumeSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-1",
			Type:     storagev1alpha1.VolumeTypeFilesystem,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(onlinePool(), vol).
		WithStatusSubresource(&storagev1alpha1.ZfsVolume{}).
		Build()

	z := newFakeZFS("tank/k8s/pvc-1")
	r := &ZfsVolumeReconciler{Client: c, Scheme: scheme, NodeName: "node-a", ZFS: z}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(z.destroyed) != 1 || z.destroyed[0] != "tank/k8s/pvc-1" {
		t.Fatalf("expected destroy of tank/k8s/pvc-1, got %v", z.destroyed)
	}

	var got storagev1alpha1.ZfsVolume
	err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got)
	if err == nil {
		t.Fatalf("expected volume to be removed after finalizer release")
	}
}
