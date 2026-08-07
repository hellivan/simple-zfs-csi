package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

func TestDerivePath(t *testing.T) {
	tests := []struct {
		name          string
		protocol      storagev1alpha1.Protocol
		baseMountPath string
		poolName      string
		dataset       string
		want          string
		wantErr       bool
	}{
		{
			name:          "nfs joins base mount path",
			protocol:      storagev1alpha1.ProtocolNFS,
			baseMountPath: "/mnt/tank",
			dataset:       "k8s/pvc-123",
			want:          "/mnt/tank/k8s/pvc-123",
		},
		{
			name:          "nfs trims leading and trailing slashes on dataset",
			protocol:      storagev1alpha1.ProtocolNFS,
			baseMountPath: "/tank",
			dataset:       "/media/movies/",
			want:          "/tank/media/movies",
		},
		{
			name:     "nvmeof uses zvol device path",
			protocol: storagev1alpha1.ProtocolNVMeoF,
			poolName: "tank",
			dataset:  "k8s/pvc-123",
			want:     "/dev/zvol/tank/k8s/pvc-123",
		},
		{
			name:          "nfs without base mount path errors",
			protocol:      storagev1alpha1.ProtocolNFS,
			baseMountPath: "",
			dataset:       "x",
			wantErr:       true,
		},
		{
			name:     "nvmeof without pool name errors",
			protocol: storagev1alpha1.ProtocolNVMeoF,
			poolName: "",
			dataset:  "x",
			wantErr:  true,
		},
		{
			name:          "empty dataset errors",
			protocol:      storagev1alpha1.ProtocolNFS,
			baseMountPath: "/tank",
			dataset:       "/",
			wantErr:       true,
		},
		{
			name:     "unknown protocol errors",
			protocol: storagev1alpha1.Protocol("smb"),
			dataset:  "x",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := derivePath(tt.protocol, tt.baseMountPath, tt.poolName, tt.dataset)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got path %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("derivePath = %q, want %q", got, tt.want)
			}
		})
	}
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return scheme
}

func TestZfsShareReconcile_RendersNetworkExport(t *testing.T) {
	scheme := newTestScheme(t)

	pool := &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: "zpool-999"},
		Status: storagev1alpha1.ZfsPoolStatus{
			GUID:          "999",
			PoolName:      "tank",
			CurrentNode:   "node-a",
			BaseMountPath: "/mnt/tank",
			Health:        storagev1alpha1.PoolHealthOnline,
		},
	}
	share := &storagev1alpha1.ZfsShare{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec: storagev1alpha1.ZfsShareSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-1",
			Protocol: storagev1alpha1.ProtocolNFS,
			NFS: &storagev1alpha1.NFSExportSpec{
				Clients: []storagev1alpha1.NFSClient{{Client: "10.0.0.0/24", Options: []string{"rw"}}},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pool, share).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.NetworkExport{}).
		Build()

	r := &ZfsShareReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var export storagev1alpha1.NetworkExport
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &export); err != nil {
		t.Fatalf("expected NetworkExport to be created: %v", err)
	}
	if export.Spec.NodeName != "node-a" {
		t.Errorf("NodeName = %q, want node-a", export.Spec.NodeName)
	}
	if export.Spec.Protocol != storagev1alpha1.ProtocolNFS {
		t.Errorf("Protocol = %q, want nfs", export.Spec.Protocol)
	}
	if export.Spec.Path != "/mnt/tank/k8s/pvc-1" {
		t.Errorf("Path = %q, want /mnt/tank/k8s/pvc-1", export.Spec.Path)
	}
	if export.Spec.NFS == nil || len(export.Spec.NFS.Clients) != 1 || export.Spec.NFS.Clients[0].Client != "10.0.0.0/24" {
		t.Errorf("NFS clients not copied: %+v", export.Spec.NFS)
	}
	if len(export.OwnerReferences) != 1 || export.OwnerReferences[0].Name != "pvc-1" || export.OwnerReferences[0].Kind != "ZfsShare" {
		t.Errorf("owner reference not set to ZfsShare: %+v", export.OwnerReferences)
	}

	// Before the node-local aggregator confirms the export, the share is only
	// Exporting (not yet Bound) so consumers do not mount prematurely.
	var exporting storagev1alpha1.ZfsShare
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &exporting); err != nil {
		t.Fatalf("get share: %v", err)
	}
	if exporting.Status.Phase != storagev1alpha1.SharePhaseExporting {
		t.Errorf("phase = %q, want Exporting before export confirmed", exporting.Status.Phase)
	}

	// Simulate the aggregator confirming the export live for its generation.
	export.Status.Phase = storagev1alpha1.PhaseExported
	export.Status.ObservedGeneration = export.Generation
	if err := c.Status().Update(context.Background(), &export); err != nil {
		t.Fatalf("update export status: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile (after export ready): %v", err)
	}

	var got storagev1alpha1.ZfsShare
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got); err != nil {
		t.Fatalf("get share: %v", err)
	}
	if got.Status.Phase != storagev1alpha1.SharePhaseBound {
		t.Errorf("phase = %q, want Bound", got.Status.Phase)
	}
	if got.Status.NodeName != "node-a" || got.Status.Path != "/mnt/tank/k8s/pvc-1" {
		t.Errorf("status routing = node %q path %q", got.Status.NodeName, got.Status.Path)
	}
}

func TestZfsShareReconcile_PendingWhenPoolMissing(t *testing.T) {
	scheme := newTestScheme(t)

	share := &storagev1alpha1.ZfsShare{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec: storagev1alpha1.ZfsShareSpec{
			PoolGUID: "does-not-exist",
			Dataset:  "k8s/pvc-1",
			Protocol: storagev1alpha1.ProtocolNFS,
			NFS:      &storagev1alpha1.NFSExportSpec{Clients: []storagev1alpha1.NFSClient{{Client: "*"}}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(share).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}).
		Build()

	r := &ZfsShareReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pvc-1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var export storagev1alpha1.NetworkExport
	err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &export)
	if err == nil {
		t.Fatalf("expected no NetworkExport when pool is missing")
	}

	var got storagev1alpha1.ZfsShare
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, &got); err != nil {
		t.Fatalf("get share: %v", err)
	}
	if got.Status.Phase != storagev1alpha1.SharePhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
}
