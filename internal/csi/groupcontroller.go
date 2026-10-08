package csi

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// GroupControllerServer implements the CSI GroupController service on top of
// ZfsGroupSnapshot (ADR-0039). The RPC contract (error codes, set comparison of
// snapshot_ids against Spec.Members) is enforced here, in the CSI layer, not in
// the CRs (ADR-0040).
type GroupControllerServer struct {
	csi.UnimplementedGroupControllerServer

	Client client.Client
	// CreateTimeout bounds how long CreateVolumeGroupSnapshot waits for the group
	// to become Ready before returning DeadlineExceeded (the sidecar retries).
	CreateTimeout time.Duration
	// PollInterval is how often the readiness wait re-reads the group.
	PollInterval time.Duration
	Log          logr.Logger
}

// GroupControllerServiceCapabilities advertises the group snapshot RPCs.
func GroupControllerServiceCapabilities() []*csi.GroupControllerServiceCapability {
	return []*csi.GroupControllerServiceCapability{{
		Type: &csi.GroupControllerServiceCapability_Rpc{
			Rpc: &csi.GroupControllerServiceCapability_RPC{
				Type: csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT,
			},
		},
	}}
}

// GroupServiceCapability returns the plugin capability advertising the
// GROUP_CONTROLLER_SERVICE.
func GroupServiceCapability() *csi.PluginCapability {
	return &csi.PluginCapability{
		Type: &csi.PluginCapability_Service_{
			Service: &csi.PluginCapability_Service{
				Type: csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE,
			},
		},
	}
}

// GroupControllerGetCapabilities reports the supported group RPCs.
func (g *GroupControllerServer) GroupControllerGetCapabilities(_ context.Context, _ *csi.GroupControllerGetCapabilitiesRequest) (*csi.GroupControllerGetCapabilitiesResponse, error) {
	return &csi.GroupControllerGetCapabilitiesResponse{Capabilities: GroupControllerServiceCapabilities()}, nil
}

// CreateVolumeGroupSnapshot records a ZfsGroupSnapshot for the source volumes
// and waits for the agent hosting their pool to take one atomic ZFS snapshot
// over all of them.
func (g *GroupControllerServer) CreateVolumeGroupSnapshot(ctx context.Context, req *csi.CreateVolumeGroupSnapshotRequest) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot name is required")
	}
	ids := req.GetSourceVolumeIds()
	if len(ids) == 0 {
		return nil, status.Error(codes.InvalidArgument, "source_volume_ids is required")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			return nil, status.Error(codes.InvalidArgument, "source_volume_ids must not contain an empty id")
		}
		if seen[id] {
			return nil, status.Errorf(codes.InvalidArgument, "source volume %q is listed more than once", id)
		}
		seen[id] = true
	}

	existing := &storagev1alpha1.ZfsGroupSnapshot{}
	err := g.Client.Get(ctx, client.ObjectKey{Name: name}, existing)
	switch {
	case err == nil:
		if !existing.DeletionTimestamp.IsZero() {
			return nil, status.Errorf(codes.Aborted, "group snapshot %q is being deleted", name)
		}
		if !sameSourceVolumes(existing, ids) {
			return nil, status.Errorf(codes.AlreadyExists, "group snapshot %q already exists for a different set of source volumes", name)
		}
	case apierrors.IsNotFound(err):
		spec, serr := g.buildSpec(ctx, ids)
		if serr != nil {
			return nil, serr
		}
		grp := &storagev1alpha1.ZfsGroupSnapshot{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
		if cerr := g.Client.Create(ctx, grp); cerr != nil {
			if !apierrors.IsAlreadyExists(cerr) {
				return nil, status.Errorf(codes.Internal, "create ZfsGroupSnapshot %q: %v", name, cerr)
			}
			return g.CreateVolumeGroupSnapshot(ctx, req)
		}
	default:
		return nil, status.Errorf(codes.Internal, "get ZfsGroupSnapshot %q: %v", name, err)
	}

	grp, err := g.waitGroupReady(ctx, name)
	if err != nil {
		return nil, err
	}
	msg, err := g.groupMessage(ctx, grp)
	if err != nil {
		return nil, err
	}
	g.Log.Info("created group snapshot", "name", name, "members", len(grp.Spec.Members), "pool", grp.Spec.PoolGUID)
	return &csi.CreateVolumeGroupSnapshotResponse{GroupSnapshot: msg}, nil
}

// buildSpec resolves every source volume and records the members. All volumes
// must live on one pool: one `zfs snapshot` call is atomic only within a pool.
func (g *GroupControllerServer) buildSpec(ctx context.Context, ids []string) (storagev1alpha1.ZfsGroupSnapshotSpec, error) {
	var spec storagev1alpha1.ZfsGroupSnapshotSpec
	pools := map[string]bool{}
	for _, id := range ids {
		src := &storagev1alpha1.ZfsDataset{}
		if err := g.Client.Get(ctx, client.ObjectKey{Name: id}, src); err != nil {
			if apierrors.IsNotFound(err) {
				return spec, status.Errorf(codes.NotFound, "source volume %q not found", id)
			}
			return spec, status.Errorf(codes.Internal, "get source ZfsDataset %q: %v", id, err)
		}
		pools[src.Spec.PoolGUID] = true
		m := storagev1alpha1.ZfsGroupSnapshotMember{
			SourceVolume:   id,
			Dataset:        src.Spec.Dataset,
			SnapshotName:   "csi-snap-" + uuid.New().String(),
			ZfsSnapshotRef: "csi-gsnap-" + uuid.New().String(),
			SourceType:     src.Spec.Type,
			SourceFSType:   src.Status.FSType,
		}
		if src.Spec.Volume != nil {
			m.SourceVolblocksize = src.Spec.Volume.Volblocksize
		}
		if len(src.Spec.Properties) > 0 {
			m.SourceProperties = make(map[string]string, len(src.Spec.Properties))
			for k, v := range src.Spec.Properties {
				m.SourceProperties[k] = v
			}
		}
		spec.PoolGUID = src.Spec.PoolGUID
		spec.Members = append(spec.Members, m)
	}
	if len(pools) > 1 {
		names := make([]string, 0, len(pools))
		for p := range pools {
			names = append(names, p)
		}
		sort.Strings(names)
		return spec, status.Errorf(codes.FailedPrecondition,
			"source volumes span several pools (%s); a group snapshot is atomic only within one pool", strings.Join(names, ", "))
	}
	return spec, nil
}

func sameSourceVolumes(grp *storagev1alpha1.ZfsGroupSnapshot, ids []string) bool {
	have := make([]string, 0, len(grp.Spec.Members))
	for _, m := range grp.Spec.Members {
		have = append(have, m.SourceVolume)
	}
	return sameSet(have, ids)
}

func memberSnapshotIDs(grp *storagev1alpha1.ZfsGroupSnapshot) []string {
	out := make([]string, 0, len(grp.Spec.Members))
	for _, m := range grp.Spec.Members {
		out = append(out, m.ZfsSnapshotRef)
	}
	return out
}

// sameSet compares two id lists as sets, ignoring order.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, x := range a {
		set[x]++
	}
	for _, x := range b {
		if set[x] == 0 {
			return false
		}
		set[x]--
	}
	return true
}

func (g *GroupControllerServer) waitGroupReady(ctx context.Context, name string) (*storagev1alpha1.ZfsGroupSnapshot, error) {
	interval := g.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	waitCtx := ctx
	if g.CreateTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, g.CreateTimeout)
		defer cancel()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		grp := &storagev1alpha1.ZfsGroupSnapshot{}
		if err := g.Client.Get(waitCtx, client.ObjectKey{Name: name}, grp); err != nil {
			return nil, status.Errorf(codes.Internal, "get ZfsGroupSnapshot %q: %v", name, err)
		}
		switch {
		case !grp.DeletionTimestamp.IsZero():
			return nil, status.Errorf(codes.Aborted, "group snapshot %q is being deleted", name)
		case grp.Status.Phase == storagev1alpha1.GroupSnapshotPhaseReady && grp.Status.ReadyToUse && grp.Status.CreationTime != nil:
			return grp, nil
		case grp.Status.Phase == storagev1alpha1.GroupSnapshotPhaseError, grp.Status.Phase == storagev1alpha1.GroupSnapshotPhaseLost:
			return nil, status.Errorf(codes.Internal, "group snapshot %q failed: %s", name, grp.Status.Message)
		}
		select {
		case <-waitCtx.Done():
			return nil, status.Errorf(codes.DeadlineExceeded, "timed out waiting for group snapshot %q to become ready", name)
		case <-ticker.C:
		}
	}
}

// groupMessage renders the group from its members, in Spec.Members order. A
// member whose ZfsSnapshot is gone fails the call (FAILED_PRECONDITION, naming
// it): there is no partial list and no placeholder (ADR-0039).
func (g *GroupControllerServer) groupMessage(ctx context.Context, grp *storagev1alpha1.ZfsGroupSnapshot) (*csi.VolumeGroupSnapshot, error) {
	if grp.Status.CreationTime == nil {
		return nil, status.Errorf(codes.Aborted, "group snapshot %q has not been taken yet", grp.Name)
	}
	ready := grp.Status.ReadyToUse
	snaps := make([]*csi.Snapshot, 0, len(grp.Spec.Members))
	for _, m := range grp.Spec.Members {
		child := &storagev1alpha1.ZfsSnapshot{}
		if err := g.Client.Get(ctx, client.ObjectKey{Name: m.ZfsSnapshotRef}, child); err != nil {
			if apierrors.IsNotFound(err) {
				// Before provisionedAt the agent is still creating the members one after
				// the other (creationTime is written first): the member is not lost, it
				// does not exist yet.
				if grp.Status.ProvisionedAt == nil {
					return nil, status.Errorf(codes.Aborted,
						"group snapshot %q is still being provisioned: member snapshot %q (source volume %q) has not been created yet", grp.Name, m.ZfsSnapshotRef, m.SourceVolume)
				}
				return nil, status.Errorf(codes.FailedPrecondition,
					"member snapshot %q (source volume %q) of group snapshot %q no longer exists", m.ZfsSnapshotRef, m.SourceVolume, grp.Name)
			}
			return nil, status.Errorf(codes.Internal, "get ZfsSnapshot %q: %v", m.ZfsSnapshotRef, err)
		}
		if !child.Status.ReadyToUse {
			ready = false
		}
		msg := snapshotMessage(child)
		msg.GroupSnapshotId = grp.Name
		snaps = append(snaps, msg)
	}
	return &csi.VolumeGroupSnapshot{
		GroupSnapshotId: grp.Name,
		Snapshots:       snaps,
		CreationTime:    timestamppb.New(grp.Status.CreationTime.Time),
		ReadyToUse:      ready,
	}, nil
}

// GetVolumeGroupSnapshot returns the group, comparing the caller's snapshot_ids
// with Spec.Members as sets (not with the live children: a deleted child must
// not make the group undeletable).
func (g *GroupControllerServer) GetVolumeGroupSnapshot(ctx context.Context, req *csi.GetVolumeGroupSnapshotRequest) (*csi.GetVolumeGroupSnapshotResponse, error) {
	id := req.GetGroupSnapshotId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	grp := &storagev1alpha1.ZfsGroupSnapshot{}
	if err := g.Client.Get(ctx, client.ObjectKey{Name: id}, grp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "group snapshot %q not found", id)
		}
		return nil, status.Errorf(codes.Internal, "get ZfsGroupSnapshot %q: %v", id, err)
	}
	if !sameSet(memberSnapshotIDs(grp), req.GetSnapshotIds()) {
		return nil, status.Errorf(codes.InvalidArgument, "snapshot_ids do not match the members of group snapshot %q", id)
	}
	msg, err := g.groupMessage(ctx, grp)
	if err != nil {
		return nil, err
	}
	return &csi.GetVolumeGroupSnapshotResponse{GroupSnapshot: msg}, nil
}

// DeleteVolumeGroupSnapshot removes the ZfsGroupSnapshot; its finalizer deletes
// the members and cleans up. Idempotent.
func (g *GroupControllerServer) DeleteVolumeGroupSnapshot(ctx context.Context, req *csi.DeleteVolumeGroupSnapshotRequest) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
	id := req.GetGroupSnapshotId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	grp := &storagev1alpha1.ZfsGroupSnapshot{}
	if err := g.Client.Get(ctx, client.ObjectKey{Name: id}, grp); err != nil {
		if apierrors.IsNotFound(err) {
			return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "get ZfsGroupSnapshot %q: %v", id, err)
	}
	if !sameSet(memberSnapshotIDs(grp), req.GetSnapshotIds()) {
		return nil, status.Errorf(codes.InvalidArgument, "snapshot_ids do not match the members of group snapshot %q", id)
	}
	if err := g.Client.Delete(ctx, grp); err != nil && !apierrors.IsNotFound(err) {
		return nil, status.Errorf(codes.Internal, "delete ZfsGroupSnapshot %q: %v", id, err)
	}
	g.Log.Info("deleted group snapshot", "name", id)
	return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
}
