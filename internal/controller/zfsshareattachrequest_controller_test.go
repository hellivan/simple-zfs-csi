package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/nvmeauth"
)

func newAttachScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	return scheme
}

func nodeWithIP(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
		},
	}
}

func reconcileAttach(t *testing.T, r *ZfsShareAttachRequestReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile %q: %v", name, err)
	}
}

func TestAttachRequest_AggregatesShareAndReportsReady(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-1", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	node := nodeWithIP("node-a", "10.0.0.5")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-1", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, node, ar).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme}

	// First reconcile installs the finalizer.
	reconcileAttach(t, r, "pvc-1-node-a")
	// Second reconcile aggregates the ZfsShare (not yet Bound -> request not ready).
	reconcileAttach(t, r, "pvc-1-node-a")

	share := &storagev1alpha1.ZfsShare{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, share); err != nil {
		t.Fatalf("expected aggregated ZfsShare: %v", err)
	}
	if share.Spec.Protocol != storagev1alpha1.ProtocolNFS {
		t.Errorf("protocol = %q, want nfs", share.Spec.Protocol)
	}
	if share.Spec.NFS == nil || len(share.Spec.NFS.Clients) != 1 || share.Spec.NFS.Clients[0].Client != "10.0.0.5" {
		t.Errorf("allow-list not the node IP: %+v", share.Spec.NFS)
	}

	got := &storagev1alpha1.ZfsShareAttachRequest{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1-node-a"}, got); err != nil {
		t.Fatalf("get attach request: %v", err)
	}
	if got.Status.Ready {
		t.Errorf("request should not be Ready before the share is Bound")
	}

	// Simulate the ZfsShare reconciler confirming the export live.
	share.Status.Phase = storagev1alpha1.SharePhaseBound
	share.Status.ObservedGeneration = share.Generation
	if err := c.Status().Update(context.Background(), share); err != nil {
		t.Fatalf("update share status: %v", err)
	}
	reconcileAttach(t, r, "pvc-1-node-a")

	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-1-node-a"}, got); err != nil {
		t.Fatalf("get attach request: %v", err)
	}
	if !got.Status.Ready {
		t.Errorf("request should be Ready once share is Bound at the current generation")
	}
}

func TestAttachRequest_LastDetachDeletesShare(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-2"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-2", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	node := nodeWithIP("node-a", "10.0.0.6")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-2-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-2", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, node, ar).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme}
	reconcileAttach(t, r, "pvc-2-node-a") // add finalizer
	reconcileAttach(t, r, "pvc-2-node-a") // aggregate share

	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-2"}, &storagev1alpha1.ZfsShare{}); err != nil {
		t.Fatalf("expected ZfsShare to exist: %v", err)
	}

	// Detach: the finalizer keeps the object until we recompute and GC the share.
	if err := c.Delete(context.Background(), ar); err != nil {
		t.Fatalf("delete attach request: %v", err)
	}
	reconcileAttach(t, r, "pvc-2-node-a")

	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-2"}, &storagev1alpha1.ZfsShare{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected ZfsShare deleted after last detach, got err=%v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-2-node-a"}, &storagev1alpha1.ZfsShareAttachRequest{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected attach request finalizer released, got err=%v", err)
	}
}

func TestAttachRequest_NVMeoFSingleNodeShare(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-3"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-3", Type: storagev1alpha1.DatasetTypeVolume},
	}
	node := nodeWithIP("node-a", "10.0.0.7")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-3-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-3", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, node, ar).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme}
	reconcileAttach(t, r, "pvc-3-node-a")
	reconcileAttach(t, r, "pvc-3-node-a")

	share := &storagev1alpha1.ZfsShare{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-3"}, share); err != nil {
		t.Fatalf("expected aggregated ZfsShare: %v", err)
	}
	if share.Spec.Protocol != storagev1alpha1.ProtocolNVMeoF {
		t.Errorf("protocol = %q, want nvmeof", share.Spec.Protocol)
	}
	if share.Spec.NVMeoF == nil {
		t.Errorf("nvmeof export spec must be set")
	}
	if share.Spec.NFS != nil {
		t.Errorf("nfs export spec must be nil for nvmeof")
	}
}

func TestAttachRequest_NVMeoFAuthProgramsSecretAndNQN(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-4"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-4", Type: storagev1alpha1.DatasetTypeVolume},
	}
	node := nodeWithIP("node-a", "10.0.0.8")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-4-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-4", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, node, ar).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme, Namespace: "sys", DHChapEnabled: true}
	reconcileAttach(t, r, "pvc-4-node-a") // finalizer
	reconcileAttach(t, r, "pvc-4-node-a") // aggregate

	share := &storagev1alpha1.ZfsShare{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-4"}, share); err != nil {
		t.Fatalf("get share: %v", err)
	}
	wantNQN, _ := nvmeauth.HostIdentity("node-a", "pvc-4")
	if share.Spec.NVMeoF == nil || len(share.Spec.NVMeoF.AllowedHosts) != 1 || share.Spec.NVMeoF.AllowedHosts[0] != wantNQN {
		t.Errorf("allowedHosts = %+v, want [%s]", share.Spec.NVMeoF, wantNQN)
	}
	if share.Spec.NVMeoF.DHChapSecretName != "dhchap-pvc-4" || share.Spec.NVMeoF.DHChapSecretNamespace != "sys" {
		t.Errorf("dhchap secret ref = %q/%q", share.Spec.NVMeoF.DHChapSecretNamespace, share.Spec.NVMeoF.DHChapSecretName)
	}
	if share.Spec.NVMeoF.DHChapSecretKey != nvmeauth.SecretKeyDHChap {
		t.Errorf("dhchap secret key = %q, want %q", share.Spec.NVMeoF.DHChapSecretKey, nvmeauth.SecretKeyDHChap)
	}

	sec := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "dhchap-pvc-4"}, sec); err != nil {
		t.Fatalf("expected DH-CHAP secret: %v", err)
	}
	if len(sec.Data[nvmeauth.SecretKeyDHChap]) == 0 {
		t.Errorf("secret missing key %q", nvmeauth.SecretKeyDHChap)
	}

	// Detach removes the share and the secret.
	if err := c.Delete(context.Background(), ar); err != nil {
		t.Fatalf("delete attach request: %v", err)
	}
	reconcileAttach(t, r, "pvc-4-node-a")
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "dhchap-pvc-4"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected DH-CHAP secret deleted after detach, got err=%v", err)
	}
}
