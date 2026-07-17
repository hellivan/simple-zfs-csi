package csi

import (
	"context"
	"path"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/zfs-shares/api/v1alpha1"
	"github.com/hellivan/zfs-shares/internal/zpool"
)

// Default NVMe-oF transport parameters. The port mirrors the nvmeof controller's
// serviceId (4420); the transport is TCP.
const (
	defaultNVMeTransport = "tcp"
	defaultNVMePort      = "4420"
)

// NodeServer implements the CSI Node service. It runs as a privileged DaemonSet
// on every node and turns a routing-only volume_context into a real mount:
// NodePublishVolume resolves the ZfsPool's current node/IP/mount root, refuses
// when the storage node is offline, and mounts NFS or connects+mounts NVMe-oF.
// It writes no CRDs and learns no absolute path from the controller.
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

// NodeGetCapabilities advertises no optional capabilities: the plugin publishes
// directly in NodePublishVolume without a separate stage step.
func (n *NodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
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

	vctx := req.GetVolumeContext()
	poolGUID := vctx[CtxPoolGUID]
	dataset := vctx[CtxDataset]
	protocol := storagev1alpha1.Protocol(vctx[CtxProtocol])
	if poolGUID == "" || dataset == "" || protocol == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"volume_context must carry %s, %s and %s", CtxPoolGUID, CtxDataset, CtxProtocol)
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
		if err := n.publishNVMeoF(ctx, volumeID, pool, targetPath, block, readOnly, fsType, mountFlags); err != nil {
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
	if nqn := n.exportNQN(ctx, volumeID); nqn != "" {
		if err := n.Mounter.NVMeDisconnect(ctx, nqn); err != nil {
			n.Log.Error(err, "nvme disconnect failed", "volume", volumeID, "nqn", nqn)
		}
	}

	n.Log.Info("unpublished volume", "volume", volumeID, "target", targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
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
	nqn := n.exportNQN(ctx, volumeID)
	if nqn == "" {
		return status.Errorf(codes.FailedPrecondition, "NVMe-oF export for volume %q is not ready (no NQN)", volumeID)
	}

	device, err := n.Mounter.NVMeConnect(ctx, n.transport(), pool.Status.CurrentIP, n.port(), nqn)
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
	if err := n.Mounter.FormatAndMount(device, targetPath, fsType, mountOptions(flags, readOnly)); err != nil {
		return status.Errorf(codes.Internal, "format and mount %q: %v", device, err)
	}
	return nil
}

// exportNQN returns the effective subsystem NQN for a volume by reading its
// child NetworkExport status, or "" when absent/not-yet-rendered.
func (n *NodeServer) exportNQN(ctx context.Context, volumeID string) string {
	export := &storagev1alpha1.NetworkExport{}
	if err := n.Client.Get(ctx, client.ObjectKey{Name: volumeID}, export); err != nil {
		return ""
	}
	if export.Status.NQN != "" {
		return export.Status.NQN
	}
	if export.Spec.NVMeoF != nil {
		return export.Spec.NVMeoF.NQN
	}
	return ""
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
