package csi

import (
	"context"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

func newTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add clientgo scheme: %v", err)
	}
	if err := storagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add storage scheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&storagev1alpha1.ZfsDataset{}, &storagev1alpha1.ZfsShare{}, &storagev1alpha1.ZfsSnapshot{}).
		WithObjects(objs...).
		Build()
}

func mountCaps() []*csi.VolumeCapability {
	return []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
}

func blockCaps() []*csi.VolumeCapability {
	return []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
}

// markReadyAsync flips a ZfsDataset to Ready once it appears, simulating the agent.
func markReadyAsync(cl client.Client, name string) {
	go func() {
		for i := 0; i < 200; i++ {
			vol := &storagev1alpha1.ZfsDataset{}
			if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, vol); err == nil {
				vol.Status.Phase = storagev1alpha1.DatasetPhaseReady
				vol.Status.Path = "/mnt/tank/" + vol.Spec.Dataset
				_ = cl.Status().Update(context.Background(), vol)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func newController(cl client.Client) *ControllerServer {
	return &ControllerServer{
		Client:        cl,
		CreateTimeout: 2 * time.Second,
		PollInterval:  10 * time.Millisecond,
		Log:           logr.Discard(),
	}
}

func TestCreateVolume_NFSFilesystem(t *testing.T) {
	cl := newTestClient(t)
	cs := newController(cl)
	markReadyAsync(cl, "pvc-1")

	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: mountCaps(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         map[string]string{"poolGUID": "999", "protocol": "nfs", "datasetPrefix": "k8s"},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	if resp.GetVolume().GetVolumeId() != "pvc-1" {
		t.Errorf("volumeId = %q, want pvc-1", resp.GetVolume().GetVolumeId())
	}
	vctx := resp.GetVolume().GetVolumeContext()
	if vctx[CtxPoolGUID] != "999" || vctx[CtxDataset] != "k8s/pvc-1" || vctx[CtxProtocol] != "nfs" {
		t.Errorf("volume_context = %+v", vctx)
	}

	vol := &storagev1alpha1.ZfsDataset{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, vol); err != nil {
		t.Fatalf("get ZfsDataset: %v", err)
	}
	if vol.Spec.Type != storagev1alpha1.DatasetTypeFilesystem {
		t.Errorf("type = %q, want filesystem", vol.Spec.Type)
	}
	if vol.Spec.Filesystem == nil || vol.Spec.Filesystem.Quota == nil || vol.Spec.Filesystem.Quota.Value() != 1<<30 {
		t.Errorf("filesystem quota not set to 1Gi: %+v", vol.Spec.Filesystem)
	}

	share := &storagev1alpha1.ZfsShare{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-1"}, share); err != nil {
		t.Fatalf("get ZfsShare: %v", err)
	}
	if share.Spec.Protocol != storagev1alpha1.ProtocolNFS || share.Spec.Dataset != "k8s/pvc-1" {
		t.Errorf("share spec = %+v", share.Spec)
	}
	if share.Spec.NFS == nil || len(share.Spec.NFS.Clients) != 1 {
		t.Errorf("share NFS clients not defaulted: %+v", share.Spec.NFS)
	}
}

func TestCreateVolume_NVMeoFVolume(t *testing.T) {
	cl := newTestClient(t)
	cs := newController(cl)
	markReadyAsync(cl, "pvc-2")

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-2",
		VolumeCapabilities: blockCaps(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 10 << 30},
		Parameters:         map[string]string{"poolGUID": "999", "protocol": "nvmeof", "volblocksize": "16k"},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	vol := &storagev1alpha1.ZfsDataset{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-2"}, vol); err != nil {
		t.Fatalf("get ZfsDataset: %v", err)
	}
	if vol.Spec.Type != storagev1alpha1.DatasetTypeVolume {
		t.Errorf("type = %q, want volume", vol.Spec.Type)
	}
	if vol.Spec.Volume == nil || vol.Spec.Volume.Size.Value() != 10<<30 || vol.Spec.Volume.Volblocksize != "16k" {
		t.Errorf("volume config wrong: %+v", vol.Spec.Volume)
	}
}

func TestCreateVolume_BlockOnNFSRejected(t *testing.T) {
	cs := newController(newTestClient(t))
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-3",
		VolumeCapabilities: blockCaps(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         map[string]string{"poolGUID": "999", "protocol": "nfs"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

func TestCreateVolume_NVMeoFRequiresCapacity(t *testing.T) {
	cs := newController(newTestClient(t))
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-4",
		VolumeCapabilities: blockCaps(),
		Parameters:         map[string]string{"poolGUID": "999", "protocol": "nvmeof"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

func TestCreateVolume_IdempotentSameParams(t *testing.T) {
	cl := newTestClient(t)
	cs := newController(cl)
	markReadyAsync(cl, "pvc-5")

	req := &csi.CreateVolumeRequest{
		Name:               "pvc-5",
		VolumeCapabilities: mountCaps(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         map[string]string{"poolGUID": "999", "protocol": "nfs"},
	}
	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("second CreateVolume should be idempotent: %v", err)
	}
}

func TestCreateVolume_ConflictingParams(t *testing.T) {
	cl := newTestClient(t)
	cs := newController(cl)
	markReadyAsync(cl, "pvc-6")

	base := map[string]string{"poolGUID": "999", "protocol": "nfs"}
	if _, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-6", VolumeCapabilities: mountCaps(),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30}, Parameters: base,
	}); err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-6", VolumeCapabilities: mountCaps(),
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30}, Parameters: base,
	})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("err = %v, want AlreadyExists", err)
	}
}

func TestCreateVolume_PVCAnnotationsOverride(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claim-a",
			Namespace: "team-a",
			Annotations: map[string]string{
				// Non-restricted param: honoured.
				"param.simple-zfs-csi.io/nfsOptions": "rw no_root_squash",
				// StorageClass-only params: MUST be ignored.
				"param.simple-zfs-csi.io/poolGUID":      "annotated-pool",
				"param.simple-zfs-csi.io/datasetPrefix": "annotated-pfx",
			},
		},
	}
	cl := newTestClient(t, pvc)
	cs := newController(cl)
	cs.AnnotationPrefix = "param.simple-zfs-csi.io/"
	markReadyAsync(cl, "pvc-7")

	resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-7",
		VolumeCapabilities: mountCaps(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters: map[string]string{
			"poolGUID":                         "sc-pool",
			"datasetPrefix":                    "sc-pfx",
			"protocol":                         "nfs",
			"csi.storage.k8s.io/pvc/name":      "claim-a",
			"csi.storage.k8s.io/pvc/namespace": "team-a",
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vctx := resp.GetVolume().GetVolumeContext()
	// StorageClass-only params keep their StorageClass values.
	if vctx[CtxPoolGUID] != "sc-pool" {
		t.Errorf("poolGUID = %q, want sc-pool (PVC annotation must not override)", vctx[CtxPoolGUID])
	}
	if vctx[CtxDataset] != "sc-pfx/pvc-7" {
		t.Errorf("dataset = %q, want sc-pfx/pvc-7 (datasetPrefix is StorageClass-only)", vctx[CtxDataset])
	}
	// Non-restricted param from the annotation layer takes effect.
	share := &storagev1alpha1.ZfsShare{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-7"}, share); err != nil {
		t.Fatalf("get ZfsShare: %v", err)
	}
	if share.Spec.NFS == nil || len(share.Spec.NFS.Clients) != 1 ||
		len(share.Spec.NFS.Clients[0].Options) != 2 ||
		share.Spec.NFS.Clients[0].Options[0] != "rw" ||
		share.Spec.NFS.Clients[0].Options[1] != "no_root_squash" {
		t.Errorf("NFS options not overridden by annotation: %+v", share.Spec.NFS)
	}
}

func TestCreateVolume_TimeoutWhenNotReady(t *testing.T) {
	cl := newTestClient(t)
	cs := newController(cl)
	cs.CreateTimeout = 60 * time.Millisecond
	// no markReadyAsync: the volume never becomes Ready.

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-8",
		VolumeCapabilities: mountCaps(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         map[string]string{"poolGUID": "999", "protocol": "nfs"},
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestDeleteVolume_RemovesBoth(t *testing.T) {
	vol := &storagev1alpha1.ZfsDataset{ObjectMeta: metav1.ObjectMeta{Name: "pvc-9"}}
	share := &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: "pvc-9"}}
	cl := newTestClient(t, vol, share)
	cs := newController(cl)

	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "pvc-9"}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-9"}, &storagev1alpha1.ZfsDataset{}); !apierrors.IsNotFound(err) {
		t.Errorf("ZfsDataset still present: %v", err)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-9"}, &storagev1alpha1.ZfsShare{}); !apierrors.IsNotFound(err) {
		t.Errorf("ZfsShare still present: %v", err)
	}
}

func TestDeleteVolume_IdempotentWhenAbsent(t *testing.T) {
	cs := newController(newTestClient(t))
	if _, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "ghost"}); err != nil {
		t.Fatalf("DeleteVolume on absent volume should succeed: %v", err)
	}
}

// markReadyTrackingGen keeps a ZfsDataset Ready with ObservedGeneration synced to
// its spec generation, simulating the agent across expansion (which bumps the
// spec). Used by expansion tests where waitVolumeReady requires the generation.
func markReadyTrackingGen(cl client.Client, name string) {
	go func() {
		for i := 0; i < 400; i++ {
			vol := &storagev1alpha1.ZfsDataset{}
			if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, vol); err == nil {
				if vol.Status.Phase != storagev1alpha1.DatasetPhaseReady || vol.Status.ObservedGeneration != vol.Generation {
					vol.Status.Phase = storagev1alpha1.DatasetPhaseReady
					vol.Status.Path = "/mnt/tank/" + vol.Spec.Dataset
					vol.Status.ObservedGeneration = vol.Generation
					_ = cl.Status().Update(context.Background(), vol)
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func TestControllerExpandVolume_Filesystem(t *testing.T) {
	small := resource.MustParse("1Gi")
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-e1"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID:   "999",
			Dataset:    "k8s/pvc-e1",
			Type:       storagev1alpha1.DatasetTypeFilesystem,
			Filesystem: &storagev1alpha1.FilesystemConfig{Quota: &small},
		},
	}
	cl := newTestClient(t, vol)
	cs := newController(cl)
	markReadyTrackingGen(cl, "pvc-e1")

	resp, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "pvc-e1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 5 << 30},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if resp.GetCapacityBytes() != 5<<30 {
		t.Errorf("capacity = %d, want %d", resp.GetCapacityBytes(), int64(5<<30))
	}
	if resp.GetNodeExpansionRequired() {
		t.Errorf("filesystem (NFS) expansion should not require node expansion")
	}
	got := &storagev1alpha1.ZfsDataset{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-e1"}, got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if got.Spec.Filesystem.Quota.Value() != int64(5<<30) {
		t.Errorf("quota = %d, want %d", got.Spec.Filesystem.Quota.Value(), int64(5<<30))
	}
}

func TestControllerExpandVolume_Zvol(t *testing.T) {
	small := resource.MustParse("1Gi")
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-e2"},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: "999",
			Dataset:  "k8s/pvc-e2",
			Type:     storagev1alpha1.DatasetTypeVolume,
			Volume:   &storagev1alpha1.VolumeConfig{Size: small},
		},
	}
	cl := newTestClient(t, vol)
	cs := newController(cl)
	markReadyTrackingGen(cl, "pvc-e2")

	resp, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "pvc-e2",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if !resp.GetNodeExpansionRequired() {
		t.Errorf("zvol (NVMe-oF) expansion must require node expansion")
	}
	got := &storagev1alpha1.ZfsDataset{}
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "pvc-e2"}, got); err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if got.Spec.Volume.Size.Value() != int64(4<<30) {
		t.Errorf("size = %d, want %d", got.Spec.Volume.Size.Value(), int64(4<<30))
	}
}

func TestControllerExpandVolume_NotFound(t *testing.T) {
	cs := newController(newTestClient(t))
	_, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "ghost",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestControllerGetCapabilities_Expand(t *testing.T) {
	cs := newController(newTestClient(t))
	resp, err := cs.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities: %v", err)
	}
	found := false
	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_EXPAND_VOLUME {
			found = true
		}
	}
	if !found {
		t.Errorf("EXPAND_VOLUME capability not advertised")
	}
}
