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
	nfsMounts      map[string]string // target -> source (NFS network mounts)
	dirMounts      map[string]string // target -> source (local bind-mount of directories)
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
	// formattedFS simulates the device's actual on-disk fsType: preset it in a
	// test to simulate an already-formatted device (FormatAndMount then returns
	// this value regardless of what's requested, mirroring hostMounter's real
	// existing-fs behavior); left empty, FormatAndMount "formats" with whatever
	// fsType is requested and remembers it for subsequent calls.
	formattedFS string
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{
		mounted:     map[string]bool{},
		nfsMounts:   map[string]string{},
		dirMounts:   map[string]string{},
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
func (f *fakeMounter) FormatAndMount(device, target, fsType string, _ []string) (string, error) {
	f.fsMounts[target] = device
	f.mounted[target] = true
	if fsType == "" {
		fsType = "ext4"
	}
	if f.formattedFS == "" {
		f.formattedFS = fsType
	}
	return f.formattedFS, nil
}
func (f *fakeMounter) BindMountDevice(device, target string, _ bool) error {
	f.blockMounts[target] = device
	f.mounted[target] = true
	return nil
}
func (f *fakeMounter) BindMountDir(source, target string, _ bool) error {
	f.dirMounts[target] = source
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

// onlinePool's CurrentNode ("node-a") matches newNodeServer's NodeID, i.e. it
// models the pool being *local* to this node plugin (ADR-0031). Use
// remotePool for tests that must exercise the NVMe-oF/NFS network path
// regardless of locality.
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

// remotePool is identical to onlinePool but hosted on a different node than
// the plugin under test ("node-a"), forcing the network (NVMe-oF/NFS) path.
func remotePool(guid, ip, mountPath, poolName string) *storagev1alpha1.ZfsPool {
	pool := onlinePool(guid, ip, mountPath, poolName)
	pool.Status.CurrentNode = "node-b"
	return pool
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

// dataset returns the ZfsDataset CR for a volume id. NodePublishVolume resolves
// poolGUID, dataset path and protocol exclusively from this object (ADR-0022),
// so every publish test must seed one.
func dataset(name, path string, typ storagev1alpha1.DatasetType) *storagev1alpha1.ZfsDataset {
	return &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: path, Type: typ},
	}
}

// datasetWithPath is dataset plus a Status.Path, as a Ready zvol would carry
// once the agent has reconciled it — the local-passthrough path (ADR-0031)
// reads this field directly instead of connecting over NVMe-oF.
func datasetWithPath(name, path string, typ storagev1alpha1.DatasetType, statusPath string) *storagev1alpha1.ZfsDataset {
	ds := dataset(name, path, typ)
	ds.Status.Path = statusPath
	return ds
}

func TestNodePublish_NFS(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, remotePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		dataset("pvc-1", "k8s/pvc-1", storagev1alpha1.DatasetTypeFilesystem))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-1",
		TargetPath:       "/var/lib/kubelet/pods/x/vol",
		VolumeCapability: mountCap(),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if got := m.nfsMounts["/var/lib/kubelet/pods/x/vol"]; got != "10.0.0.5:/mnt/tank/k8s/pvc-1" {
		t.Errorf("nfs source = %q, want 10.0.0.5:/mnt/tank/k8s/pvc-1", got)
	}
}

// TestNodePublish_NFS_LiveResolvesRenamedDataset is the regression test for the
// production incident where a manually-renamed ZFS dataset (with
// ZfsDataset.Spec.Dataset updated to match) was not picked up on mount, because
// NodePublishVolume trusted the CSI volume_context's cached dataset path — which
// external-provisioner bakes into the immutable PV object once, at CreateVolume
// time, and never refreshes. The mount source must follow the CR's current
// Spec.Dataset, whatever the PV was created with.
func TestNodePublish_NFS_LiveResolvesRenamedDataset(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, remotePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		dataset("pvc-1", "k8s/renamed-pvc-1", storagev1alpha1.DatasetTypeFilesystem))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-1",
		TargetPath:       "/var/lib/kubelet/pods/x/vol",
		VolumeCapability: mountCap(),
		// A stale pre-rename path, as an immutable PV would still carry it. It must
		// be ignored entirely, not merely deprioritized.
		VolumeContext: map[string]string{"poolGUID": "999", "dataset": "k8s/pvc-1", "protocol": "nfs"},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	want := "10.0.0.5:/mnt/tank/k8s/renamed-pvc-1"
	if got := m.nfsMounts["/var/lib/kubelet/pods/x/vol"]; got != want {
		t.Errorf("nfs source = %q, want %q (live ZfsDataset.Spec.Dataset, not the PV's cached copy)", got, want)
	}
}

// TestNodePublish_NFS_Local covers ADR-0031 Phase 2: when this node IS the pool's
// own node, a dataset is bind-mounted directly from its host mountpoint instead
// of round-tripping through NFS-over-loopback.
func TestNodePublish_NFS_Local(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m,
		onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		datasetWithPath("pvc-local-nfs", "k8s/pvc-local-nfs", storagev1alpha1.DatasetTypeFilesystem, "/mnt/tank/k8s/pvc-local-nfs"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-local-nfs",
		TargetPath:       "/var/lib/kubelet/pods/x/local-vol",
		VolumeCapability: mountCap(),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if len(m.nfsMounts) != 0 {
		t.Errorf("expected no NFS mount for a local dataset, got %v", m.nfsMounts)
	}
	if got := m.dirMounts["/var/lib/kubelet/pods/x/local-vol"]; got != "/mnt/tank/k8s/pvc-local-nfs" {
		t.Errorf("dirMounts source = %q, want /mnt/tank/k8s/pvc-local-nfs", got)
	}
}

// TestNodePublish_NFS_LocalRequiresStatusPath covers the case where the pool is
// local but the agent has not yet populated ZfsDataset.Status.Path: the publish
// must fail loudly rather than silently mounting the wrong thing.
func TestNodePublish_NFS_LocalRequiresStatusPath(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m,
		onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		dataset("pvc-local-nfs-nopath", "k8s/pvc-local-nfs-nopath", storagev1alpha1.DatasetTypeFilesystem))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-local-nfs-nopath",
		TargetPath:       "/var/lib/kubelet/pods/x/vol",
		VolumeCapability: mountCap(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

// TestNodePublish_NFS_RWX_NeverLocal covers ADR-0031's safety restriction: a
// multi-node (RWX) NFS volume always uses the NFS path, even when this node is
// the pool's own node. Using a bind-mount alongside NFS mounts from other nodes
// mixes POSIX and lockd lock domains and can corrupt data.
func TestNodePublish_NFS_RWX_NeverLocal(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m,
		// Pool is local to this node (node-a) — but RWX means NFS is still used.
		onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		datasetWithPath("pvc-rwx-local", "k8s/pvc-rwx-local", storagev1alpha1.DatasetTypeFilesystem, "/mnt/tank/k8s/pvc-rwx-local"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:   "pvc-rwx-local",
		TargetPath: "/var/lib/kubelet/pods/x/rwx-vol",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if len(m.dirMounts) != 0 {
		t.Errorf("expected no bind-mount for RWX volume (even on local node), got %v", m.dirMounts)
	}
	if got := m.nfsMounts["/var/lib/kubelet/pods/x/rwx-vol"]; got != "10.0.0.5:/mnt/tank/k8s/pvc-rwx-local" {
		t.Errorf("nfs source = %q, want 10.0.0.5:/mnt/tank/k8s/pvc-rwx-local", got)
	}
}

func TestNodePublish_NVMeoF_Filesystem(t *testing.T) {
	m := newFakeMounter()
	export := &storagev1alpha1.NetworkExport{ObjectMeta: metav1.ObjectMeta{Name: "pvc-2"}}
	export.Status.NQN = "nqn.2025-01.io.simple-zfs-csi:pvc-2"
	ns := newNodeServer(t, m, remotePool("999", "10.0.0.5", "/mnt/tank", "tank"), export,
		dataset("pvc-2", "k8s/pvc-2", storagev1alpha1.DatasetTypeVolume))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-2",
		TargetPath:       "/target/fs",
		VolumeCapability: mountCap(),
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

// TestNodePublish_NVMeoF_RecordsFSTypeOnce verifies D10
// (docs/snapshot-lifecycle-redesign.md): the first NodePublishVolume that
// formats a zvol records the effective fsType on ZfsDataset.Status.FSType, and
// a later publish to a second target path (e.g. after a remount) leaves it
// unchanged — the on-disk type, once set, is immutable. (A real mismatched
// fsType is refused at the mount layer itself — see
// TestHostMounterFormatAndMount's "fails loudly" case in mount_test.go — so
// recordFSType is never even reached in that situation; fakeMounter doesn't
// model that failure since it's exercised directly against hostMounter.)
func TestNodePublish_NVMeoF_RecordsFSTypeOnce(t *testing.T) {
	m := newFakeMounter()
	export := &storagev1alpha1.NetworkExport{ObjectMeta: metav1.ObjectMeta{Name: "pvc-2"}}
	export.Status.NQN = "nqn.2025-01.io.simple-zfs-csi:pvc-2"
	vol := &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-2"},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "999", Dataset: "k8s/pvc-2", Type: storagev1alpha1.DatasetTypeVolume},
	}
	ns := newNodeServer(t, m, remotePool("999", "10.0.0.5", "/mnt/tank", "tank"), export, vol)

	if _, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-2",
		TargetPath:       "/target/fs",
		VolumeCapability: mountCap(), // requests ext4
	}); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	var got storagev1alpha1.ZfsDataset
	if err := ns.Client.Get(context.Background(), client.ObjectKey{Name: "pvc-2"}, &got); err != nil {
		t.Fatalf("get ZfsDataset: %v", err)
	}
	if got.Status.FSType != "ext4" {
		t.Fatalf("Status.FSType = %q, want ext4", got.Status.FSType)
	}

	// A second publish (matching fsType, as any real remount of the same
	// volume would) must not disturb the already-recorded value.
	m.mounted["/target/fs2"] = false
	if _, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-2",
		TargetPath:       "/target/fs2",
		VolumeCapability: mountCap(),
	}); err != nil {
		t.Fatalf("NodePublishVolume (2nd): %v", err)
	}
	if err := ns.Client.Get(context.Background(), client.ObjectKey{Name: "pvc-2"}, &got); err != nil {
		t.Fatalf("get ZfsDataset (2nd): %v", err)
	}
	if got.Status.FSType != "ext4" {
		t.Errorf("Status.FSType changed to %q, want it to stay ext4 (immutable once set)", got.Status.FSType)
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
	ns := newNodeServer(t, m, remotePool("999", "10.0.0.5", "/mnt/tank", "tank"), export, sec,
		dataset("pvc-9", "k8s/pvc-9", storagev1alpha1.DatasetTypeVolume))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-9",
		TargetPath:       "/target/fs",
		VolumeCapability: mountCap(),
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
	ns := newNodeServer(t, m, remotePool("999", "10.0.0.5", "/mnt/tank", "tank"), export,
		dataset("pvc-3", "k8s/pvc-3", storagev1alpha1.DatasetTypeVolume))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-3",
		TargetPath:       "/target/block",
		VolumeCapability: blockCap(),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if m.blockMounts["/target/block"] != "/dev/nvme1n1" {
		t.Errorf("block mount device = %q, want /dev/nvme1n1", m.blockMounts["/target/block"])
	}
}

// TestNodePublish_NVMeoF_LocalFilesystem covers ADR-0031: when this node is the
// pool's own node, the zvol is mounted straight from its local device path
// (ZfsDataset.Status.Path) — no `nvme connect`, no NetworkExport dependency at
// all (none is even seeded here).
func TestNodePublish_NVMeoF_LocalFilesystem(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		datasetWithPath("pvc-10", "k8s/pvc-10", storagev1alpha1.DatasetTypeVolume, "/dev/zvol/tank/k8s/pvc-10"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-10",
		TargetPath:       "/target/local-fs",
		VolumeCapability: mountCap(),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if m.connectedNQN != "" {
		t.Errorf("expected no nvme connect for a local volume, got NQN %q", m.connectedNQN)
	}
	if m.fsMounts["/target/local-fs"] != "/dev/zvol/tank/k8s/pvc-10" {
		t.Errorf("fs mount device = %q, want /dev/zvol/tank/k8s/pvc-10", m.fsMounts["/target/local-fs"])
	}
}

// TestNodePublish_NVMeoF_LocalBlock is the raw-block-mode counterpart of
// TestNodePublish_NVMeoF_LocalFilesystem.
func TestNodePublish_NVMeoF_LocalBlock(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		datasetWithPath("pvc-11", "k8s/pvc-11", storagev1alpha1.DatasetTypeVolume, "/dev/zvol/tank/k8s/pvc-11"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-11",
		TargetPath:       "/target/local-block",
		VolumeCapability: blockCap(),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if m.connectedNQN != "" {
		t.Errorf("expected no nvme connect for a local volume, got NQN %q", m.connectedNQN)
	}
	if m.blockMounts["/target/local-block"] != "/dev/zvol/tank/k8s/pvc-11" {
		t.Errorf("block mount device = %q, want /dev/zvol/tank/k8s/pvc-11", m.blockMounts["/target/local-block"])
	}
}

// TestNodePublish_NVMeoF_LocalRequiresStatusPath ensures a not-yet-Ready local
// zvol (no Status.Path yet) fails loudly instead of mounting a made-up path.
func TestNodePublish_NVMeoF_LocalRequiresStatusPath(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		dataset("pvc-12", "k8s/pvc-12", storagev1alpha1.DatasetTypeVolume))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-12",
		TargetPath:       "/target/local-pending",
		VolumeCapability: mountCap(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestNodePublish_RefusesNodeOffline(t *testing.T) {
	m := newFakeMounter()
	pool := onlinePool("999", "10.0.0.5", "/mnt/tank", "tank")
	pool.Status.Health = storagev1alpha1.PoolHealthNodeOffline
	ns := newNodeServer(t, m, pool, dataset("pvc-4", "k8s/pvc-4", storagev1alpha1.DatasetTypeFilesystem))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-4",
		TargetPath:       "/target",
		VolumeCapability: mountCap(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestNodePublish_BlockOnNFSRejected(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		dataset("pvc-5", "k8s/pvc-5", storagev1alpha1.DatasetTypeFilesystem))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-5",
		TargetPath:       "/target",
		VolumeCapability: blockCap(),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
}

func TestNodePublish_NVMeoFRequiresNQN(t *testing.T) {
	m := newFakeMounter()
	// No NetworkExport object -> no NQN available.
	ns := newNodeServer(t, m, remotePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		dataset("pvc-6", "k8s/pvc-6", storagev1alpha1.DatasetTypeVolume))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-6",
		TargetPath:       "/target",
		VolumeCapability: mountCap(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestNodePublish_IdempotentWhenMounted(t *testing.T) {
	m := newFakeMounter()
	m.mounted["/target"] = true
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		dataset("pvc-7", "k8s/pvc-7", storagev1alpha1.DatasetTypeFilesystem))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-7",
		TargetPath:       "/target",
		VolumeCapability: mountCap(),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if len(m.nfsMounts) != 0 {
		t.Errorf("expected no new mount when already mounted, got %v", m.nfsMounts)
	}
}

// TestNodePublish_UnknownVolumeRejected pins ADR-0022: with no ZfsDataset to
// resolve, the publish fails instead of falling back to whatever the PV's
// immutable volume_context happens to still say.
func TestNodePublish_UnknownVolumeRejected(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"))

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "pvc-8",
		TargetPath:       "/target",
		VolumeCapability: mountCap(),
		VolumeContext:    map[string]string{"poolGUID": "999", "dataset": "k8s/pvc-8", "protocol": "nfs"},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
	if len(m.nfsMounts) != 0 {
		t.Errorf("expected no mount for an unknown volume, got %v", m.nfsMounts)
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
	ns := newNodeServer(t, m, remotePool("999", "10.0.0.5", "/mnt/tank", "tank"), export,
		dataset("pvc-e", "k8s/pvc-e", storagev1alpha1.DatasetTypeVolume))

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
	ns := newNodeServer(t, m, remotePool("999", "10.0.0.5", "/mnt/tank", "tank"), export,
		dataset("pvc-e", "k8s/pvc-e", storagev1alpha1.DatasetTypeVolume))

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
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		dataset("pvc-nfs", "k8s/pvc-nfs", storagev1alpha1.DatasetTypeFilesystem))

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

// TestNodeExpand_NVMeoFLocal covers ADR-0031: a local zvol's device already
// reflects the grown size (ZfsDataset.Status.Path), so no NetworkExport lookup
// or nvme rescan is needed at all.
func TestNodeExpand_NVMeoFLocal(t *testing.T) {
	m := newFakeMounter()
	ns := newNodeServer(t, m, onlinePool("999", "10.0.0.5", "/mnt/tank", "tank"),
		datasetWithPath("pvc-13", "k8s/pvc-13", storagev1alpha1.DatasetTypeVolume, "/dev/zvol/tank/k8s/pvc-13"))

	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:         "pvc-13",
		VolumePath:       "/target/local-fs",
		VolumeCapability: mountCap(),
	})
	if err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if len(m.rescanned) != 0 {
		t.Errorf("expected no rescan for a local volume, got %v", m.rescanned)
	}
	if m.resized["/dev/zvol/tank/k8s/pvc-13"] != "/target/local-fs" {
		t.Errorf("resized = %v, want /dev/zvol/tank/k8s/pvc-13 -> /target/local-fs", m.resized)
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
