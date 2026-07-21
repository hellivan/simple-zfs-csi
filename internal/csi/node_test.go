package csi

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/nvmeauth"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// fakeMounter records operations and lets tests script mount state.
type fakeMounter struct {
	mounted        map[string]bool
	nfsMounts      map[string]string // target -> source
	fsMounts       map[string]string // target -> device
	blockMounts    map[string]string // target -> device
	connectedNQN   string
	connectHostNQN string
	connectHostID  string
	connectDHChap  string
	connectDev     string
	disconnected   []string
	removed        []string
	rescanned      []string
	resized        map[string]string // device -> volumePath
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{
		mounted:     map[string]bool{},
		nfsMounts:   map[string]string{},
		fsMounts:    map[string]string{},
		blockMounts: map[string]string{},
		resized:     map[string]string{},
		connectDev:  "/dev/nvme1n1",
	}
}

func (f *fakeMounter) IsMountPoint(path string) (bool, error) { return f.mounted[path], nil }
func (f *fakeMounter) MakeDir(string) error                   { return nil }
func (f *fakeMounter) MakeFile(string) error                  { return nil }
func (f *fakeMounter) RemovePath(path string) error {
	f.removed = append(f.removed, path)
	return nil
}
func (f *fakeMounter) MountNFS(source, target string, _ []string) error {
	f.nfsMounts[target] = source
	f.mounted[target] = true
	return nil
}
func (f *fakeMounter) FormatAndMount(device, target, _ string, _ []string) error {
	f.fsMounts[target] = device
	f.mounted[target] = true
	return nil
}
func (f *fakeMounter) BindMountDevice(device, target string, _ bool) error {
	f.blockMounts[target] = device
	f.mounted[target] = true
	return nil
}
func (f *fakeMounter) Unmount(target string) error {
	f.mounted[target] = false
	return nil
}
func (f *fakeMounter) NVMeConnect(_ context.Context, o NVMeConnectOptions) (string, error) {
	f.connectedNQN = o.NQN
	f.connectHostNQN = o.HostNQN
	f.connectHostID = o.HostID
	f.connectDHChap = o.DHChapKey
	return f.connectDev, nil
}
func (f *fakeMounter) NVMeDisconnect(_ context.Context, nqn string) error {
	f.disconnected = append(f.disconnected, nqn)
	return nil
}
func (f *fakeMounter) NVMeDevice(_ context.Context, nqn string) (string, error) {
	if f.connectedNQN == nqn {
		return f.connectDev, nil
	}
	return "", nil
}
func (f *fakeMounter) RescanNVMe(_ context.Context, nqn string) error {
	f.rescanned = append(f.rescanned, nqn)
	return nil
}
func (f *fakeMounter) ResizeFS(device, volumePath string) error {
	f.resized[device] = volumePath
	return nil
}

func onlinePool(guid, ip, mountPath, poolName string) *storagev1alpha1.ZfsPool {
	return &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: zpool.ResourceName(guid)},
		Status: storagev1alpha1.ZfsPoolStatus{
			GUID:          guid,
			PoolName:      poolName,
			CurrentNode:   "node-a",
			CurrentIP:     ip,
			BaseMountPath: mountPath,
			Health:        storagev1alpha1.PoolHealthOnline,
		},
	}
}

func newNodeServer(t *testing.T, m NodeMounter, objs ...client.Object) *NodeServer {
	return &NodeServer{
		Client:  newTestClient(t, objs...),
		Mounter: m,
		NodeID:  "node-a",
		Log:     logr.Discard(),
	}
}
func mountCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func blockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func TestNodePublish_NFS(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-1",
		TargetPath:       "/var/lib/kubelet/pods/x/vol",
		VolumeCapability: mountCap(),
		VolumeContext:    map[string]string{CtxPoolGUID: "999", CtxDataset: "k8s/pvc-1", CtxProtocol: "nfs"},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if got := m.nfsMounts["/var/lib/kubelet/pods/x/vol"]; got != "10.0.0.5:/mnt/tank/k8s/pvc-1" {
		t.Errorf("nfs source = %q, want 10.0.0.5:/mnt/tank/k8s/pvc-1", got)
	}
}

func TestNodePublish_NVMeoF_Filesystem(t *testing.T) {
	m := newFakeMounter()
	export := &storagev1alpha1.NetworkExport{ObjectMeta: metav1.ObjectMeta{Name: "pvc-2"}}
	export.Status.NQN = "nqn.2025-01.io.simple-zfs-csi:pvc-2"
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"), export)

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-2",
		TargetPath:       "/target/fs",
		VolumeCapability: mountCap(),
		VolumeContext:    map[string]string{CtxPoolGUID: "999", CtxDataset: "k8s/pvc-2", CtxProtocol: "nvmeof"},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if m.connectedNQN != "nqn.2025-01.io.simple-zfs-csi:pvc-2" {
		t.Errorf("connected NQN = %q", m.connectedNQN)
	}
	wantHostNQN, wantHostID := nvmeauth.HostIdentity("node-a", "pvc-2")
	if m.connectHostNQN != wantHostNQN || m.connectHostID != wantHostID {
		t.Errorf("host identity = %q/%q, want %q/%q", m.connectHostNQN, m.connectHostID, wantHostNQN, wantHostID)
	}
	if m.connectDHChap != "" {
		t.Errorf("expected no DH-CHAP key without a secret ref, got %q", m.connectDHChap)
	}
	if m.fsMounts["/target/fs"] != "/dev/nvme1n1" {
		t.Errorf("fs mount device = %q, want /dev/nvme1n1", m.fsMounts["/target/fs"])
	}
}

func TestNodePublish_NVMeoF_DHChap(t *testing.T) {
	m := newFakeMounter()
	export := &storagev1alpha1.NetworkExport{ObjectMeta: metav1.ObjectMeta{Name: "pvc-9"}}
	export.Status.NQN = "nqn.2025-01.io.simple-zfs-csi:pvc-9"
	export.Spec.NVMeoF = &storagev1alpha1.NVMeoFExportSpec{
		DHChapSecretName:      "dhchap-pvc-9",
		DHChapSecretNamespace: "sys",
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dhchap-pvc-9", Namespace: "sys"},
		Data:       map[string][]byte{nvmeauth.SecretKeyDHChap: []byte("DHHC-1:00:Zm9v:")},
	}
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"), export, sec)

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-9",
		TargetPath:       "/target/fs",
		VolumeCapability: mountCap(),
		VolumeContext:    map[string]string{CtxPoolGUID: "999", CtxDataset: "k8s/pvc-9", CtxProtocol: "nvmeof"},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if m.connectDHChap != "DHHC-1:00:Zm9v:" {
		t.Errorf("DH-CHAP key = %q, want the referenced secret value", m.connectDHChap)
	}
}

func TestNodePublish_NVMeoF_Block(t *testing.T) {
	m := newFakeMounter()
	export := &storagev1alpha1.NetworkExport{ObjectMeta: metav1.ObjectMeta{Name: "pvc-3"}}
	export.Status.NQN = "nqn.block"
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"), export)

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-3",
		TargetPath:       "/target/block",
		VolumeCapability: blockCap(),
		VolumeContext:    map[string]string{CtxPoolGUID: "999", CtxDataset: "k8s/pvc-3", CtxProtocol: "nvmeof"},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if m.blockMounts["/target/block"] != "/dev/nvme1n1" {
		t.Errorf("block mount device = %q, want /dev/nvme1n1", m.blockMounts["/target/block"])
	}
}

func TestNodePublish_RefusesNodeOffline(t *testing.T) {
	m := newFakeMounter()
	pool := onlinePool("999", "10.0.0.5", "/mnt/tank", "tank")
	pool.Status.Health = storagev1alpha1.PoolHealthNodeOffline
	ns := newNodeServer(t, m, pool)

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-4",
		TargetPath:       "/target",
		VolumeCapability: mountCap(),
		VolumeContext:    map[string]string{CtxPoolGUID: "999", CtxDataset: "k8s/pvc-4", CtxProtocol: "nfs"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestNodePublish_BlockOnNFSRejected(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-5",
		TargetPath:       "/target",
		VolumeCapability: blockCap(),
		VolumeContext:    map[string]string{CtxPoolGUID: "999", CtxDataset: "k8s/pvc-5", CtxProtocol: "nfs"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

func TestNodePublish_NVMeoFRequiresNQN(t *testing.T) {
	m := newFakeMounter()
	// No NetworkExport object -> no NQN available.
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-6",
		TargetPath:       "/target",
		VolumeCapability: mountCap(),
		VolumeContext:    map[string]string{CtxPoolGUID: "999", CtxDataset: "k8s/pvc-6", CtxProtocol: "nvmeof"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestNodePublish_IdempotentWhenMounted(t *testing.T) {
	m := newFakeMounter()
	m.mounted["/target"] = true
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-7",
		TargetPath:       "/target",
		VolumeCapability: mountCap(),
		VolumeContext:    map[string]string{CtxPoolGUID: "999", CtxDataset: "k8s/pvc-7", CtxProtocol: "nfs"},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if len(m.nfsMounts) != 0 {
		t.Errorf("expected no new mount when already mounted, got %v", m.nfsMounts)
	}
}

func TestNodePublish_MissingContext(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-8",
		TargetPath:       "/target",
		VolumeCapability: mountCap(),
		VolumeContext:    map[string]string{CtxPoolGUID: "999"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

func TestNodeUnpublish_UnmountsAndDisconnects(t *testing.T) {
	m := newFakeMounter()
	m.mounted["/target"] = true
	export := &storagev1alpha1.NetworkExport{ObjectMeta: metav1.ObjectMeta{Name: "pvc-9"}}
	export.Status.NQN = "nqn.9"
	ns := newNodeServer(t, m, export)

	_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pvc-9",
		TargetPath: "/target",
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}
	if m.mounted["/target"] {
		t.Errorf("target still mounted after unpublish")
	}
	if len(m.disconnected) != 1 || m.disconnected[0] != "nqn.9" {
		t.Errorf("disconnected = %v, want [nqn.9]", m.disconnected)
	}
}

func TestNodeGetInfo(t *testing.T) {
	ns := newNodeServer(t, newFakeMounter())
	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if resp.GetNodeId() != "node-a" {
		t.Errorf("nodeId = %q, want node-a", resp.GetNodeId())
	}
}

func TestNodeExpand_NVMeoFFilesystem(t *testing.T) {
	m := newFakeMounter()
	m.connectedNQN = "nqn.exp" // already connected from an earlier publish
	export := &storagev1alpha1.NetworkExport{ObjectMeta: metav1.ObjectMeta{Name: "pvc-e"}}
	export.Status.NQN = "nqn.exp"
	ns := newNodeServer(t, m, export)

	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:         "pvc-e",
		VolumePath:       "/target/fs",
		VolumeCapability: mountCap(),
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if len(m.rescanned) != 1 || m.rescanned[0] != "nqn.exp" {
		t.Errorf("rescanned = %v, want [nqn.exp]", m.rescanned)
	}
	if m.resized["/dev/nvme1n1"] != "/target/fs" {
		t.Errorf("resized = %v, want /dev/nvme1n1 -> /target/fs", m.resized)
	}
}

func TestNodeExpand_NVMeoFBlockSkipsResize(t *testing.T) {
	m := newFakeMounter()
	m.connectedNQN = "nqn.exp"
	export := &storagev1alpha1.NetworkExport{ObjectMeta: metav1.ObjectMeta{Name: "pvc-e"}}
	export.Status.NQN = "nqn.exp"
	ns := newNodeServer(t, m, export)

	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:         "pvc-e",
		VolumePath:       "/target/block",
		VolumeCapability: blockCap(),
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if len(m.rescanned) != 1 {
		t.Errorf("expected one rescan, got %v", m.rescanned)
	}
	if len(m.resized) != 0 {
		t.Errorf("block volume should not be resized, got %v", m.resized)
	}
}

func TestNodeExpand_NFSNoop(t *testing.T) {
	m := newFakeMounter()
	// No NetworkExport -> NFS volume; nothing to grow on the node.
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"))

	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   "pvc-nfs",
		VolumePath: "/target/fs",
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if len(m.rescanned) != 0 || len(m.resized) != 0 {
		t.Errorf("nfs expand should be a no-op, rescanned=%v resized=%v", m.rescanned, m.resized)
	}
}

func TestNodeGetCapabilities_Expand(t *testing.T) {
	ns := newNodeServer(t, newFakeMounter())
	resp, err := ns.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities: %v", err)
	}
	found := false
	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.NodeServiceCapability_RPC_EXPAND_VOLUME {
			found = true
		}
	}
	if !found {
		t.Errorf("EXPAND_VOLUME capability not advertised")
	}
}
