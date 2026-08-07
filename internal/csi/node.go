package csi

import (
	"context"
	"fmt"
	"path"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/nvmeauth"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// Default NVMe-oF transport parameters. The port mirrors the nvmeof controller's
// serviceId (4420); the transport is TCP.
const (
	defaultNVMeTransport = "tcp"
	defaultNVMePort      = "4420"
)

// NodeServer implements the CSI Node service. It runs as a privileged DaemonSet
// on every node and turns a CSI VolumeId into a real mount: NodePublishVolume
// resolves the volume's ZfsDataset and its ZfsPool's current node/IP/mount root
// live from the API on every call, refuses when the storage node is offline, and
// mounts NFS or connects+mounts NVMe-oF. It learns no absolute path from the
// controller (ADR-0022).
type NodeServer struct {
	csi.UnimplementedNodeServer

	Client  client.Client
	Mounter NodeMounter
	// NodeID is the Kubernetes node name this plugin runs on (from the downward
	// API); returned by NodeGetInfo.
	NodeID string
	// NVMeTransport and NVMePort target the storage node's NVMe-oF listener.
	NVMeTransport string
	NVMePort      string
	Log           logr.Logger
}

// NodeGetInfo returns the node identity. No topology is advertised.
func (n *NodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: n.NodeID}, nil
}

// NodeGetCapabilities advertises EXPAND_VOLUME: the plugin can finish an online
// zvol expansion by growing the on-device filesystem. It still publishes
// directly in NodePublishVolume without a separate stage step.
func (n *NodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			nodeRPCCapability(csi.NodeServiceCapability_RPC_EXPAND_VOLUME),
		},
	}, nil
}

// NodePublishVolume mounts the volume at the target path.
func (n *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	// The CSI VolumeId doubles as the ZfsDataset CR's name (guaranteed stable —
	// independent-resource-naming-redesign.md), so it is already a proper
	// reference to the authoritative object: poolGUID, dataset path and protocol
	// are resolved live from that CR on every publish. The request's
	// volume_context is deliberately not consulted at all (ADR-0022) —
	// external-provisioner bakes it into the PV's spec.csi.volumeAttributes once,
	// at CreateVolume time, and that field is immutable thereafter, so it can
	// only ever be a stale mirror of the CR (a dataset rename would be invisible
	// to every later mount). A failed lookup fails the publish rather than
	// falling back to a cached path nobody is reconciling any more.
	poolGUID, dataset, statusPath, protocol, err := n.resolveVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}

	pool, err := n.resolvePool(ctx, poolGUID)
	if err != nil {
		return nil, err
	}

	// Idempotency: an already-mounted target is a success.
	mounted, err := n.Mounter.IsMountPoint(targetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check mount %q: %v", targetPath, err)
	}
	if mounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	block := volCap.GetBlock() != nil
	readOnly := req.GetReadonly()
	mountFlags := volCap.GetMount().GetMountFlags()
	fsType := volCap.GetMount().GetFsType()

	switch protocol {
	case storagev1alpha1.ProtocolNFS:
		if block {
			return nil, status.Error(codes.InvalidArgument, "block volumeMode is not supported for nfs")
		}
		if err := n.publishNFS(pool, dataset, targetPath, readOnly, mountFlags); err != nil {
			return nil, err
		}
	case storagev1alpha1.ProtocolNVMeoF:
		if isLocalToPool(n.NodeID, pool) {
			if err := n.publishLocalZvol(ctx, volumeID, statusPath, targetPath, block, readOnly, fsType, mountFlags); err != nil {
				return nil, err
			}
		} else if err := n.publishNVMeoF(ctx, volumeID, pool, targetPath, block, readOnly, fsType, mountFlags); err != nil {
			return nil, err
		}
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown protocol %q", protocol)
	}

	n.Log.Info("published volume", "volume", volumeID, "protocol", protocol, "target", targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts the target path (and disconnects NVMe-oF).
func (n *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	if err := n.Mounter.Unmount(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount %q: %v", targetPath, err)
	}
	if err := n.Mounter.RemovePath(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "remove target %q: %v", targetPath, err)
	}

	// Best-effort NVMe-oF disconnect: look up the export's NQN if it still exists.
	if nqn := n.lookupExportNQN(ctx, volumeID); nqn != "" {
		if err := n.Mounter.NVMeDisconnect(ctx, nqn); err != nil {
			n.Log.Error(err, "nvme disconnect failed", "volume", volumeID, "nqn", nqn)
		}
	}

	n.Log.Info("unpublished volume", "volume", volumeID, "target", targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeExpandVolume finishes an online expansion on the node. For NVMe-oF zvols it
// grows the on-device filesystem: the remote case first rescans the namespace so
// the block device reflects the grown zvol, while a local volume's device
// (ZfsDataset.Status.Path) already reflects the new size with no rescan needed
// (ADR-0031 — no NVMe-oF hop is involved at all). NFS volumes and raw-block
// volumes need no node work.
func (n *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	volumePath := req.GetVolumePath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}

	poolGUID, _, statusPath, protocol, err := n.resolveVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	if protocol != storagev1alpha1.ProtocolNVMeoF {
		// NFS volumes have no node-local device to grow.
		return &csi.NodeExpandVolumeResponse{}, nil
	}
	pool, err := n.resolvePool(ctx, poolGUID)
	if err != nil {
		return nil, err
	}

	var device string
	if isLocalToPool(n.NodeID, pool) {
		if statusPath == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %q has no local device path yet", volumeID)
		}
		device = statusPath
	} else {
		nqn := n.lookupExportNQN(ctx, volumeID)
		if nqn == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "NVMe-oF export for volume %q not found", volumeID)
		}
		device, err = n.Mounter.NVMeDevice(ctx, nqn)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "locate nvme device for %q: %v", volumeID, err)
		}
		if device == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "nvme device for volume %q is not connected", volumeID)
		}
		if err := n.Mounter.RescanNVMe(ctx, nqn); err != nil {
			return nil, status.Errorf(codes.Internal, "rescan nvme device %q: %v", device, err)
		}
	}

	// Raw block volumes have no filesystem to grow; the rescan (if any) above is
	// sufficient.
	if req.GetVolumeCapability().GetBlock() != nil {
		return &csi.NodeExpandVolumeResponse{}, nil
	}
	if err := n.Mounter.ResizeFS(device, volumePath); err != nil {
		return nil, status.Errorf(codes.Internal, "resize filesystem on %q: %v", device, err)
	}

	n.Log.Info("expanded volume on node", "volume", volumeID, "device", device, "path", volumePath)
	return &csi.NodeExpandVolumeResponse{}, nil
}

// resolveVolume loads the current poolGUID, dataset path, node-local status
// path and protocol live from the ZfsDataset CR (ObjectMeta.Name == volumeID,
// immutable — independent-resource-naming-redesign.md). protocol is derived
// from Spec.Type (filesystem<->nfs, volume<->nvmeof, the same 1:1 mapping
// ParseParams enforces at CreateVolume time), so there is nothing
// protocol-specific left for volume_context to carry that isn't already on the
// CR. statusPath is the agent-reported Status.Path (a dataset's mountpoint, or
// a zvol's /dev/zvol device node) — only meaningful when this node is the
// pool's own node (ADR-0031's local-passthrough check).
func (n *NodeServer) resolveVolume(ctx context.Context, volumeID string) (poolGUID, dataset, statusPath string, protocol storagev1alpha1.Protocol, err error) {
	ds := &storagev1alpha1.ZfsDataset{}
	if err := n.Client.Get(ctx, client.ObjectKey{Name: volumeID}, ds); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", "", "", status.Errorf(codes.NotFound, "ZfsDataset %q not found", volumeID)
		}
		return "", "", "", "", status.Errorf(codes.Internal, "get ZfsDataset %q: %v", volumeID, err)
	}
	if ds.Spec.PoolGUID == "" || ds.Spec.Dataset == "" {
		return "", "", "", "", status.Errorf(codes.Internal, "ZfsDataset %q has no poolGUID/dataset", volumeID)
	}
	switch ds.Spec.Type {
	case storagev1alpha1.DatasetTypeFilesystem:
		protocol = storagev1alpha1.ProtocolNFS
	case storagev1alpha1.DatasetTypeVolume:
		protocol = storagev1alpha1.ProtocolNVMeoF
	default:
		return "", "", "", "", status.Errorf(codes.Internal, "ZfsDataset %q has unknown type %q", volumeID, ds.Spec.Type)
	}
	return ds.Spec.PoolGUID, ds.Spec.Dataset, ds.Status.Path, protocol, nil
}

// isLocalToPool reports whether nodeID is the node currently hosting pool. If
// so, the node plugin can read/write the volume's local path directly instead
// of looping back through NVMe-oF/NFS (ADR-0031) — those protocols exist to
// reach a pool hosted elsewhere, and add nothing but overhead when the pool is
// right here.
func isLocalToPool(nodeID string, pool *storagev1alpha1.ZfsPool) bool {
	return nodeID != "" && pool.Status.CurrentNode == nodeID
}

// resolvePool loads the ZfsPool for a GUID and validates it is reachable.
func (n *NodeServer) resolvePool(ctx context.Context, poolGUID string) (*storagev1alpha1.ZfsPool, error) {
	pool := &storagev1alpha1.ZfsPool{}
	if err := n.Client.Get(ctx, client.ObjectKey{Name: zpool.ResourceName(poolGUID)}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "ZfsPool for GUID %q not found", poolGUID)
		}
		return nil, status.Errorf(codes.Internal, "get ZfsPool %q: %v", poolGUID, err)
	}
	if pool.Status.Health == storagev1alpha1.PoolHealthNodeOffline {
		return nil, status.Errorf(codes.FailedPrecondition,
			"storage node %q for pool %q is offline", pool.Status.CurrentNode, poolGUID)
	}
	if pool.Status.CurrentIP == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "pool %q has no current IP", poolGUID)
	}
	return pool, nil
}

// publishNFS mounts the pool's dataset export over NFS at targetPath.
func (n *NodeServer) publishNFS(pool *storagev1alpha1.ZfsPool, dataset, targetPath string, readOnly bool, flags []string) error {
	if pool.Status.BaseMountPath == "" {
		return status.Errorf(codes.FailedPrecondition, "pool %q has no base mount path", pool.Name)
	}
	exportPath := path.Join(pool.Status.BaseMountPath, dataset)
	source := pool.Status.CurrentIP + ":" + exportPath

	if err := n.Mounter.MakeDir(targetPath); err != nil {
		return status.Errorf(codes.Internal, "create target %q: %v", targetPath, err)
	}
	opts := mountOptions(flags, readOnly)
	if err := n.Mounter.MountNFS(source, targetPath, opts); err != nil {
		return status.Errorf(codes.Internal, "mount nfs %q: %v", source, err)
	}
	return nil
}

// publishNVMeoF connects the zvol over NVMe-oF and publishes it as a raw block
// device (block mode) or a formatted, mounted filesystem (filesystem mode).
func (n *NodeServer) publishNVMeoF(ctx context.Context, volumeID string, pool *storagev1alpha1.ZfsPool, targetPath string, block, readOnly bool, fsType string, flags []string) error {
	export := &storagev1alpha1.NetworkExport{}
	if err := n.Client.Get(ctx, client.ObjectKey{Name: volumeID}, export); err != nil {
		return status.Errorf(codes.FailedPrecondition, "NVMe-oF export for volume %q is not ready: %v", volumeID, err)
	}
	nqn := exportNQN(export)
	if nqn == "" {
		return status.Errorf(codes.FailedPrecondition, "NVMe-oF export for volume %q is not ready (no NQN)", volumeID)
	}

	// Per-attach initiator identity, derived identically to the operator's
	// target allow-list (ADR-0011); the DH-CHAP secret (when set) is read from
	// the Secret the export references.
	hostNQN, hostID := nvmeauth.HostIdentity(n.NodeID, volumeID)
	dhchapKey, err := n.dhchapKey(ctx, export)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "resolve DH-CHAP secret for volume %q: %v", volumeID, err)
	}

	device, err := n.Mounter.NVMeConnect(ctx, NVMeConnectOptions{
		Transport: n.transport(),
		Addr:      pool.Status.CurrentIP,
		Port:      n.port(),
		NQN:       nqn,
		HostNQN:   hostNQN,
		HostID:    hostID,
		DHChapKey: dhchapKey,
	})
	if err != nil {
		return status.Errorf(codes.Internal, "nvme connect: %v", err)
	}

	if block {
		if err := n.Mounter.MakeFile(targetPath); err != nil {
			return status.Errorf(codes.Internal, "create block target %q: %v", targetPath, err)
		}
		if err := n.Mounter.BindMountDevice(device, targetPath, readOnly); err != nil {
			return status.Errorf(codes.Internal, "bind-mount device %q: %v", device, err)
		}
		return nil
	}

	if err := n.Mounter.MakeDir(targetPath); err != nil {
		return status.Errorf(codes.Internal, "create target %q: %v", targetPath, err)
	}
	effectiveFS, err := n.Mounter.FormatAndMount(device, targetPath, fsType, mountOptions(flags, readOnly))
	if err != nil {
		return status.Errorf(codes.Internal, "format and mount %q: %v", device, err)
	}
	// Best-effort (D10): record the on-disk fsType once, so a later
	// clone/restore into a different fsType can be rejected instead of
	// silently producing a bad-superblock mount failure. Never fails the
	// publish itself — the mount already succeeded.
	if err := n.recordFSType(ctx, volumeID, effectiveFS); err != nil {
		n.Log.Error(err, "failed to record fsType on ZfsDataset status", "volume", volumeID, "fsType", effectiveFS)
	}
	return nil
}

// publishLocalZvol publishes a zvol directly from its local device path
// (ADR-0031) when this node hosts the pool itself: no `nvme connect`, no
// nvmet/NetworkExport dependency, no loopback network hop at all. devicePath
// is the agent-reported ZfsDataset.Status.Path (populated once the dataset is
// Ready), the same "/dev/zvol/<pool>/<dataset>" node NVMe-oF would otherwise
// export.
func (n *NodeServer) publishLocalZvol(ctx context.Context, volumeID, devicePath, targetPath string, block, readOnly bool, fsType string, flags []string) error {
	if devicePath == "" {
		return status.Errorf(codes.FailedPrecondition, "volume %q has no local device path yet", volumeID)
	}

	if block {
		if err := n.Mounter.MakeFile(targetPath); err != nil {
			return status.Errorf(codes.Internal, "create block target %q: %v", targetPath, err)
		}
		if err := n.Mounter.BindMountDevice(devicePath, targetPath, readOnly); err != nil {
			return status.Errorf(codes.Internal, "bind-mount device %q: %v", devicePath, err)
		}
		return nil
	}

	if err := n.Mounter.MakeDir(targetPath); err != nil {
		return status.Errorf(codes.Internal, "create target %q: %v", targetPath, err)
	}
	effectiveFS, err := n.Mounter.FormatAndMount(devicePath, targetPath, fsType, mountOptions(flags, readOnly))
	if err != nil {
		return status.Errorf(codes.Internal, "format and mount %q: %v", devicePath, err)
	}
	// Best-effort (D10), same as publishNVMeoF above.
	if err := n.recordFSType(ctx, volumeID, effectiveFS); err != nil {
		n.Log.Error(err, "failed to record fsType on ZfsDataset status", "volume", volumeID, "fsType", effectiveFS)
	}
	return nil
}

// recordFSType sets ZfsDataset.Status.FSType once, the first time a volume-type
// dataset is formatted/mounted (D10, docs/snapshot-lifecycle-redesign.md). A
// no-op once already set (immutable) or if fsType is empty.
func (n *NodeServer) recordFSType(ctx context.Context, volumeID, fsType string) error {
	if fsType == "" {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vol := &storagev1alpha1.ZfsDataset{}
		if err := n.Client.Get(ctx, client.ObjectKey{Name: volumeID}, vol); err != nil {
			return client.IgnoreNotFound(err)
		}
		if vol.Status.FSType != "" {
			return nil
		}
		patched := vol.DeepCopy()
		patched.Status.FSType = fsType
		return n.Client.Status().Patch(ctx, patched, client.MergeFrom(vol))
	})
}

// exportNQN returns the effective subsystem NQN for an nvmeof export, preferring
// the aggregator-reported status over the spec.
func exportNQN(export *storagev1alpha1.NetworkExport) string {
	if export.Status.NQN != "" {
		return export.Status.NQN
	}
	if export.Spec.NVMeoF != nil {
		return export.Spec.NVMeoF.NQN
	}
	return ""
}

// lookupExportNQN fetches a volume's child NetworkExport and returns its
// effective NQN, or "" when absent/not-yet-rendered.
func (n *NodeServer) lookupExportNQN(ctx context.Context, volumeID string) string {
	export := &storagev1alpha1.NetworkExport{}
	if err := n.Client.Get(ctx, client.ObjectKey{Name: volumeID}, export); err != nil {
		return ""
	}
	return exportNQN(export)
}

// dhchapKey reads the DH-CHAP secret referenced by an nvmeof export, or "" when
// in-band authentication is not configured.
func (n *NodeServer) dhchapKey(ctx context.Context, export *storagev1alpha1.NetworkExport) (string, error) {
	if export.Spec.NVMeoF == nil || export.Spec.NVMeoF.DHChapSecretName == "" {
		return "", nil
	}
	ns := export.Spec.NVMeoF.DHChapSecretNamespace
	name := export.Spec.NVMeoF.DHChapSecretName
	if ns == "" {
		return "", fmt.Errorf("dhchap secret %q has no namespace", name)
	}
	sec := &corev1.Secret{}
	if err := n.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sec); err != nil {
		return "", fmt.Errorf("get dhchap secret %s/%s: %w", ns, name, err)
	}
	dataKey := nvmeauth.ResolveSecretKey(export.Spec.NVMeoF.DHChapSecretKey)
	key := sec.Data[dataKey]
	if len(key) == 0 {
		return "", fmt.Errorf("dhchap secret %s/%s missing data key %q", ns, name, dataKey)
	}
	return string(key), nil
}

func (n *NodeServer) transport() string {
	if n.NVMeTransport != "" {
		return n.NVMeTransport
	}
	return defaultNVMeTransport
}

func (n *NodeServer) port() string {
	if n.NVMePort != "" {
		return n.NVMePort
	}
	return defaultNVMePort
}

// mountOptions appends "ro" to the requested mount flags when readOnly is set.
func mountOptions(flags []string, readOnly bool) []string {
	opts := append([]string{}, flags...)
	if readOnly {
		opts = append(opts, "ro")
	}
	return opts
}

// nodeRPCCapability wraps a node service RPC type as a NodeServiceCapability.
func nodeRPCCapability(t csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
	return &csi.NodeServiceCapability{
		Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{Type: t},
		},
	}
}
