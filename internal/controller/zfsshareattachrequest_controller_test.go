package controller

import (
	"context"
	"testing"
	"time"

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
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
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

// remoteAttachPool is a ZfsPool hosted on "node-c" — none of this file's attach
// requests ever use that node name, so a share aggregated against it always
// takes the network-export path (ADR-0031's local-only branch never matches).
func remoteAttachPool(guid string) *storagev1alpha1.ZfsPool {
	return &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: zpool.ResourceName(guid)},
		Status:     storagev1alpha1.ZfsPoolStatus{GUID: guid, CurrentNode: "node-c"},
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
		WithObjects(ds, remoteAttachPool("999"), node, ar).
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
		WithObjects(ds, remoteAttachPool("999"), node, ar).
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

// TestAttachRequest_StaleCacheDoesNotTearDownLiveShare pins ADR-0023: "no attach
// requests left" is the one answer that destroys a live export, so it is
// confirmed against the API server rather than taken from the informer. Attach
// requests are authored by the CSI controller with an uncached client, so a
// cache that has not caught up can show an empty set while another node is
// attaching — modelled here by a cached client that never sees node-b's request.
func TestAttachRequest_StaleCacheDoesNotTearDownLiveShare(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-4"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-4", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	nodeA := nodeWithIP("node-a", "10.0.0.7")
	nodeB := nodeWithIP("node-b", "10.0.0.8")
	arA := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-4-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-4", NodeName: "node-a"},
	}
	arB := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-4-node-b"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-4", NodeName: "node-b"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, remoteAttachPool("999"), nodeA, nodeB, arA).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()
	// The API server already holds node-b's request; the informer behind `c` does not.
	api := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, remoteAttachPool("999"), nodeA, nodeB, arB).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme, APIReader: api}
	reconcileAttach(t, r, "pvc-4-node-a") // add finalizer
	reconcileAttach(t, r, "pvc-4-node-a") // aggregate the share for node-a

	// node-a detaches. Its own request is terminating and node-b's is invisible
	// to the cache, so a cached read alone would conclude "no consumers left".
	if err := c.Delete(context.Background(), arA); err != nil {
		t.Fatalf("delete attach request: %v", err)
	}
	reconcileAttach(t, r, "pvc-4-node-a")

	share := &storagev1alpha1.ZfsShare{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-4"}, share); err != nil {
		t.Fatalf("share must survive while node-b still holds an attach request: %v", err)
	}
	if share.Spec.NFS == nil || len(share.Spec.NFS.Clients) != 1 || share.Spec.NFS.Clients[0].Client != "10.0.0.8" {
		t.Errorf("allow-list should have been re-rendered for node-b, got %+v", share.Spec.NFS)
	}
}

func TestAttachRequest_NVMeoFSingleNodeShare(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-3"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-3", Type: storagev1alpha1.DatasetTypeVolume},
	}
	pool := remoteAttachPool("999")
	node := nodeWithIP("node-a", "10.0.0.7")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-3-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-3", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, pool, node, ar).
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

func TestAttachRequest_NVMeoFRaceExportsOldestNodeOnly(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-9"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-9", Type: storagev1alpha1.DatasetTypeVolume},
	}
	pool := remoteAttachPool("999")
	// node-a attached first (older); node-b is a racing newcomer.
	older := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-9-node-a", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-9", NodeName: "node-a"},
	}
	newer := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-9-node-b", CreationTimestamp: metav1.NewTime(time.Now())},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-9", NodeName: "node-b"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, pool, older, newer).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme}
	reconcileAttach(t, r, "pvc-9-node-b") // finalizer
	reconcileAttach(t, r, "pvc-9-node-b") // aggregate

	// The share must be exported only to the oldest node (node-a).
	share := &storagev1alpha1.ZfsShare{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-9"}, share); err != nil {
		t.Fatalf("get share: %v", err)
	}
	wantNQN, _ := nvmeauth.HostIdentity("node-a", "pvc-9")
	if share.Spec.NVMeoF == nil || len(share.Spec.NVMeoF.AllowedHosts) != 1 || share.Spec.NVMeoF.AllowedHosts[0] != wantNQN {
		t.Fatalf("allowedHosts = %+v, want [%s] (oldest node)", share.Spec.NVMeoF, wantNQN)
	}

	// The racing newcomer's request must NOT be marked ready.
	got := &storagev1alpha1.ZfsShareAttachRequest{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-9-node-b"}, got); err != nil {
		t.Fatalf("get newer request: %v", err)
	}
	if got.Status.Ready {
		t.Errorf("racing node-b request marked Ready; want not ready (volume exported to node-a)")
	}
}

func TestAttachRequest_NVMeoFAuthProgramsSecretAndNQN(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-4"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-4", Type: storagev1alpha1.DatasetTypeVolume},
	}
	pool := remoteAttachPool("999")
	node := nodeWithIP("node-a", "10.0.0.8")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-4-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-4", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, pool, node, ar).
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

// TestAttachRequest_NVMeoFLocalSkipsShare covers ADR-0031: when the winning
// node is also the pool's own current node, no ZfsShare is created at all (a
// ZfsShare's existence should always mean "this is exported over the
// network" — never sometimes silently a no-op), no DH-CHAP secret is wasted,
// and the attach request is still Ready.
func TestAttachRequest_NVMeoFLocalSkipsShare(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-10"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-10", Type: storagev1alpha1.DatasetTypeVolume},
	}
	pool := &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: zpool.ResourceName("999")},
		Status:     storagev1alpha1.ZfsPoolStatus{GUID: "999", CurrentNode: "node-a"},
	}
	node := nodeWithIP("node-a", "10.0.0.9")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-10-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-10", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, pool, node, ar).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme, Namespace: "sys", DHChapEnabled: true}
	reconcileAttach(t, r, "pvc-10-node-a") // finalizer
	reconcileAttach(t, r, "pvc-10-node-a") // local: no share, ready immediately

	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-10"}, &storagev1alpha1.ZfsShare{}); err == nil {
		t.Fatalf("expected no ZfsShare for a local attach")
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "dhchap-pvc-10"}, &corev1.Secret{}); err == nil {
		t.Errorf("expected no DH-CHAP secret for a local attach")
	}

	got := &storagev1alpha1.ZfsShareAttachRequest{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-10-node-a"}, got); err != nil {
		t.Fatalf("get attach request: %v", err)
	}
	if !got.Status.Ready {
		t.Errorf("local attach should be Ready with no ZfsShare to wait on, got %+v", got.Status)
	}
}

// TestAttachRequest_NVMeoFPoolMovesAwayCreatesShare covers the transition: an
// attach that started local must fall back to a real ZfsShare once the pool
// moves off the attached node (the new ZfsPool watch, requestsForPool, is what
// re-drives this in a live cluster; here we call Reconcile again directly to
// simulate that watch firing).
func TestAttachRequest_NVMeoFPoolMovesAwayCreatesShare(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-11"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-11", Type: storagev1alpha1.DatasetTypeVolume},
	}
	pool := &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: zpool.ResourceName("999")},
		Status:     storagev1alpha1.ZfsPoolStatus{GUID: "999", CurrentNode: "node-a"},
	}
	node := nodeWithIP("node-a", "10.0.0.10")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-11-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-11", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, pool, node, ar).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}, &storagev1alpha1.ZfsPool{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme}
	reconcileAttach(t, r, "pvc-11-node-a") // finalizer
	reconcileAttach(t, r, "pvc-11-node-a") // local: no share

	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-11"}, &storagev1alpha1.ZfsShare{}); err == nil {
		t.Fatalf("expected no ZfsShare while local")
	}

	var moved storagev1alpha1.ZfsPool
	if err := c.Get(context.Background(), client.ObjectKey{Name: zpool.ResourceName("999")}, &moved); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	moved.Status.CurrentNode = "node-b"
	if err := c.Status().Update(context.Background(), &moved); err != nil {
		t.Fatalf("update pool: %v", err)
	}

	reconcileAttach(t, r, "pvc-11-node-a")

	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-11"}, &storagev1alpha1.ZfsShare{}); err != nil {
		t.Fatalf("expected a ZfsShare once the pool is no longer local: %v", err)
	}
}

// TestAttachRequest_NFSLocalSkipsShare covers ADR-0031 Phase 2: when all
// requesting nodes are the pool's own current node, no ZfsShare is created (a
// ZfsShare's existence means "exported over the network") and the attach request
// is immediately Ready.
func TestAttachRequest_NFSLocalSkipsShare(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-20"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-20", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	pool := &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: zpool.ResourceName("999")},
		Status:     storagev1alpha1.ZfsPoolStatus{GUID: "999", CurrentNode: "node-a"},
	}
	node := nodeWithIP("node-a", "10.0.0.20")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-20-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-20", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, pool, node, ar).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme}
	reconcileAttach(t, r, "pvc-20-node-a") // finalizer
	reconcileAttach(t, r, "pvc-20-node-a") // local: no share, ready immediately

	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-20"}, &storagev1alpha1.ZfsShare{}); err == nil {
		t.Fatalf("expected no ZfsShare for a local NFS attach")
	}

	got := &storagev1alpha1.ZfsShareAttachRequest{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-20-node-a"}, got); err != nil {
		t.Fatalf("get attach request: %v", err)
	}
	if !got.Status.Ready {
		t.Errorf("local NFS attach should be Ready with no ZfsShare to wait on, got %+v", got.Status)
	}
}

// TestAttachRequest_NFSMixedNodesCreatesShare covers the case where some nodes
// are local and some are remote: a ZfsShare must still be created so the remote
// nodes can reach the volume via NFS.
func TestAttachRequest_NFSMixedNodesCreatesShare(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-21"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-21", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	// Pool is on node-a; node-b is remote.
	pool := &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: zpool.ResourceName("999")},
		Status:     storagev1alpha1.ZfsPoolStatus{GUID: "999", CurrentNode: "node-a"},
	}
	nodeA := nodeWithIP("node-a", "10.0.0.21")
	nodeB := nodeWithIP("node-b", "10.0.0.22")
	arA := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-21-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-21", NodeName: "node-a"},
	}
	arB := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-21-node-b"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-21", NodeName: "node-b"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, pool, nodeA, nodeB, arA, arB).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme}
	reconcileAttach(t, r, "pvc-21-node-a") // finalizer
	reconcileAttach(t, r, "pvc-21-node-a") // mixed: share must be created

	share := &storagev1alpha1.ZfsShare{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-21"}, share); err != nil {
		t.Fatalf("expected ZfsShare when any requesting node is remote: %v", err)
	}
	if share.Spec.NFS == nil {
		t.Fatalf("expected NFS spec on share")
	}
	// Both nodes must be in the allow-list.
	if len(share.Spec.NFS.Clients) != 2 {
		t.Errorf("expected 2 NFS clients, got %d: %+v", len(share.Spec.NFS.Clients), share.Spec.NFS.Clients)
	}
}

// TestAttachRequest_NFSPoolMovesAwayCreatesShare covers the NFS pool-migration
// transition: an NFS attach that was local must fall back to a ZfsShare once
// the pool moves off the node.
func TestAttachRequest_NFSPoolMovesAwayCreatesShare(t *testing.T) {
	scheme := newAttachScheme(t)

	ds := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-22"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-22", Type: storagev1alpha1.DatasetTypeFilesystem},
	}
	pool := &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: zpool.ResourceName("999")},
		Status:     storagev1alpha1.ZfsPoolStatus{GUID: "999", CurrentNode: "node-a"},
	}
	node := nodeWithIP("node-a", "10.0.0.23")
	ar := &storagev1alpha1.ZfsShareAttachRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-22-node-a"},
		Spec:       storagev1alpha1.ZfsShareAttachRequestSpec{VolumeName: "pvc-22", NodeName: "node-a"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds, pool, node, ar).
		WithStatusSubresource(&storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsShareAttachRequest{}, &storagev1alpha1.ZfsPool{}).
		Build()

	r := &ZfsShareAttachRequestReconciler{Client: c, Scheme: scheme}
	reconcileAttach(t, r, "pvc-22-node-a") // finalizer
	reconcileAttach(t, r, "pvc-22-node-a") // local: no share

	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-22"}, &storagev1alpha1.ZfsShare{}); err == nil {
		t.Fatalf("expected no ZfsShare while pool is local")
	}

	var moved storagev1alpha1.ZfsPool
	if err := c.Get(context.Background(), client.ObjectKey{Name: zpool.ResourceName("999")}, &moved); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	moved.Status.CurrentNode = "node-b"
	if err := c.Status().Update(context.Background(), &moved); err != nil {
		t.Fatalf("update pool: %v", err)
	}

	reconcileAttach(t, r, "pvc-22-node-a")

	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-22"}, &storagev1alpha1.ZfsShare{}); err != nil {
		t.Fatalf("expected ZfsShare once the NFS pool is no longer local: %v", err)
	}
}
