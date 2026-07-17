package csi

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// CreateSnapshot records a ZfsSnapshot for the source volume and waits for the
// agent hosting its pool to take the ZFS snapshot. The CSI snapshot id is the
// ZfsSnapshot object name (== the requested snapshot name), which is also used as
// the ZFS snapshot short name.
func (c *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot name is required")
	}
	sourceID := req.GetSourceVolumeId()
	if sourceID == "" {
		return nil, status.Error(codes.InvalidArgument, "source volume id is required")
	}

	src := &storagev1alpha1.ZfsDataset{}
	if err := c.Client.Get(ctx, client.ObjectKey{Name: sourceID}, src); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "source volume %q not found", sourceID)
		}
		return nil, status.Errorf(codes.Internal, "get source ZfsDataset %q: %v", sourceID, err)
	}

	desired := storagev1alpha1.ZfsSnapshotSpec{
		PoolGUID:     src.Spec.PoolGUID,
		Dataset:      src.Spec.Dataset,
		SnapshotName: name,
		SourceVolume: sourceID,
	}
	if err := c.ensureSnapshot(ctx, name, desired); err != nil {
		return nil, err
	}
	snap, err := c.waitSnapshotReady(ctx, name)
	if err != nil {
		return nil, err
	}

	c.Log.Info("created snapshot", "name", name, "source", sourceID, "pool", desired.PoolGUID, "dataset", desired.Dataset)
	return &csi.CreateSnapshotResponse{Snapshot: snapshotMessage(snap)}, nil
}

// DeleteSnapshot removes the ZfsSnapshot; its finalizer drives `zfs destroy`.
func (c *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	id := req.GetSnapshotId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot id is required")
	}
	snap := &storagev1alpha1.ZfsSnapshot{ObjectMeta: metav1.ObjectMeta{Name: id}}
	if err := c.Client.Delete(ctx, snap); err != nil && !apierrors.IsNotFound(err) {
		return nil, status.Errorf(codes.Internal, "delete ZfsSnapshot %q: %v", id, err)
	}
	c.Log.Info("deleted snapshot", "name", id)
	return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots returns ZfsSnapshot objects, optionally filtered by snapshot id
// or source volume id, with offset-based pagination.
func (c *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	// Exact snapshot-id lookup short-circuits listing.
	if id := req.GetSnapshotId(); id != "" {
		snap := &storagev1alpha1.ZfsSnapshot{}
		if err := c.Client.Get(ctx, client.ObjectKey{Name: id}, snap); err != nil {
			if apierrors.IsNotFound(err) {
				return &csi.ListSnapshotsResponse{}, nil
			}
			return nil, status.Errorf(codes.Internal, "get ZfsSnapshot %q: %v", id, err)
		}
		return &csi.ListSnapshotsResponse{Entries: []*csi.ListSnapshotsResponse_Entry{{Snapshot: snapshotMessage(snap)}}}, nil
	}

	var list storagev1alpha1.ZfsSnapshotList
	if err := c.Client.List(ctx, &list); err != nil {
		return nil, status.Errorf(codes.Internal, "list ZfsSnapshots: %v", err)
	}

	items := make([]storagev1alpha1.ZfsSnapshot, 0, len(list.Items))
	sourceFilter := req.GetSourceVolumeId()
	for i := range list.Items {
		if sourceFilter != "" && list.Items[i].Spec.SourceVolume != sourceFilter {
			continue
		}
		items = append(items, list.Items[i])
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	start := 0
	if tok := req.GetStartingToken(); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 || n > len(items) {
			return nil, status.Errorf(codes.Aborted, "invalid starting_token %q", tok)
		}
		start = n
	}

	end := len(items)
	if max := int(req.GetMaxEntries()); max > 0 && start+max < end {
		end = start + max
	}

	entries := make([]*csi.ListSnapshotsResponse_Entry, 0, end-start)
	for i := start; i < end; i++ {
		entries = append(entries, &csi.ListSnapshotsResponse_Entry{Snapshot: snapshotMessage(&items[i])})
	}
	resp := &csi.ListSnapshotsResponse{Entries: entries}
	if end < len(items) {
		resp.NextToken = strconv.Itoa(end)
	}
	return resp, nil
}

// ensureSnapshot creates the ZfsSnapshot, or validates that an existing one with
// the same name refers to the same source (CSI idempotency).
func (c *ControllerServer) ensureSnapshot(ctx context.Context, name string, desired storagev1alpha1.ZfsSnapshotSpec) error {
	existing := &storagev1alpha1.ZfsSnapshot{}
	err := c.Client.Get(ctx, client.ObjectKey{Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		snap := &storagev1alpha1.ZfsSnapshot{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: desired}
		if err := c.Client.Create(ctx, snap); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return c.ensureSnapshot(ctx, name, desired)
			}
			return status.Errorf(codes.Internal, "create ZfsSnapshot %q: %v", name, err)
		}
		return nil
	case err != nil:
		return status.Errorf(codes.Internal, "get ZfsSnapshot %q: %v", name, err)
	default:
		if existing.Spec.PoolGUID != desired.PoolGUID ||
			existing.Spec.Dataset != desired.Dataset ||
			existing.Spec.SourceVolume != desired.SourceVolume {
			return status.Errorf(codes.AlreadyExists, "snapshot %q already exists for a different source", name)
		}
		return nil
	}
}

// waitSnapshotReady polls the ZfsSnapshot until it is ready to use, fails, or the
// deadline elapses (DeadlineExceeded so external-snapshotter retries).
func (c *ControllerServer) waitSnapshotReady(ctx context.Context, name string) (*storagev1alpha1.ZfsSnapshot, error) {
	interval := c.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	waitCtx := ctx
	if c.CreateTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, c.CreateTimeout)
		defer cancel()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		snap := &storagev1alpha1.ZfsSnapshot{}
		if err := c.Client.Get(waitCtx, client.ObjectKey{Name: name}, snap); err != nil {
			return nil, status.Errorf(codes.Internal, "get ZfsSnapshot %q: %v", name, err)
		}
		switch {
		case snap.Status.Phase == storagev1alpha1.SnapshotPhaseReady && snap.Status.ReadyToUse:
			return snap, nil
		case snap.Status.Phase == storagev1alpha1.SnapshotPhaseError:
			return nil, status.Errorf(codes.Internal, "snapshot %q failed: %s", name, snap.Status.Message)
		}
		select {
		case <-waitCtx.Done():
			return nil, status.Errorf(codes.DeadlineExceeded, "timed out waiting for snapshot %q to become ready", name)
		case <-ticker.C:
		}
	}
}

// snapshotMessage renders a ZfsSnapshot into the CSI Snapshot message.
func snapshotMessage(snap *storagev1alpha1.ZfsSnapshot) *csi.Snapshot {
	var size int64
	if snap.Status.RestoreSize != nil {
		size = snap.Status.RestoreSize.Value()
	}
	var ct *timestamppb.Timestamp
	if snap.Status.CreationTime != nil {
		ct = timestamppb.New(snap.Status.CreationTime.Time)
	}
	return &csi.Snapshot{
		SnapshotId:     snap.Name,
		SourceVolumeId: snap.Spec.SourceVolume,
		SizeBytes:      size,
		CreationTime:   ct,
		ReadyToUse:     snap.Status.ReadyToUse,
	}
}
