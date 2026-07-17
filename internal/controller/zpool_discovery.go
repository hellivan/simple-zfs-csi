package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// PoolReporter is the Tier 1 (per-node) discovery component. It runs as a
// manager Runnable inside the privileged discovery DaemonSet, periodically
// enumerating the ZFS pools imported on its node and publishing their identity,
// routing and health into cluster-scoped ZfsPool objects.
//
// Discovery is a poll loop rather than an event-driven reconcile because pool
// health is host state (drive failures, HBA crashes) with no Kubernetes event
// source. Every tick fully re-derives state from the live `zpool` view, so the
// CRDs are self-healing and a node that imports a pool automatically takes over
// its ZfsPool (overwriting a stale NODE_OFFLINE left by the central watcher).
type PoolReporter struct {
	client.Client

	// NodeName is the node this reporter runs on; written to status.currentNode.
	NodeName string
	// NodeIP is the routable node address written to status.currentIP.
	NodeIP string
	// Discoverer enumerates local pools.
	Discoverer *zpool.Discoverer
	// Interval is the poll period; defaults to 30s.
	Interval time.Duration
}

// Start runs the discovery loop until ctx is cancelled. It satisfies
// manager.Runnable.
func (r *PoolReporter) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	logger := log.FromContext(ctx).WithName("zpool-reporter")

	// Report once immediately so the CRDs are populated without waiting a tick.
	r.reportOnce(ctx, logger)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.reportOnce(ctx, logger)
		}
	}
}

// reportOnce discovers local pools and upserts one ZfsPool per pool. Errors are
// logged and swallowed so a transient CLI/API failure never crashes the loop.
func (r *PoolReporter) reportOnce(ctx context.Context, logger logr.Logger) {
	pools, err := r.Discoverer.Discover(ctx)
	if err != nil {
		logger.Error(err, "failed to discover local pools")
		return
	}
	for i := range pools {
		if err := r.upsert(ctx, &pools[i]); err != nil {
			logger.Error(err, "failed to publish pool", "pool", pools[i].Name, "guid", pools[i].GUID)
		}
	}
	logger.Info("published local pools", "count", len(pools), "node", r.NodeName)
}

// upsert creates the ZfsPool if missing, then patches its status to the freshly
// observed values.
func (r *PoolReporter) upsert(ctx context.Context, p *zpool.Pool) error {
	name := zpool.ResourceName(p.GUID)

	var pool storagev1alpha1.ZfsPool
	err := r.Get(ctx, client.ObjectKey{Name: name}, &pool)
	switch {
	case apierrors.IsNotFound(err):
		pool = storagev1alpha1.ZfsPool{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}
		if err := r.Create(ctx, &pool); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return err
			}
			if err := r.Get(ctx, client.ObjectKey{Name: name}, &pool); err != nil {
				return err
			}
		}
	case err != nil:
		return err
	}

	patched := pool.DeepCopy()
	now := metav1.Now()
	patched.Status.GUID = p.GUID
	patched.Status.PoolName = p.Name
	patched.Status.CurrentNode = r.NodeName
	patched.Status.CurrentIP = r.NodeIP
	patched.Status.BaseMountPath = p.Mountpoint
	patched.Status.Health = p.Health
	patched.Status.LastUpdated = &now
	patched.Status.Message = "reported by " + r.NodeName
	return r.Status().Patch(ctx, patched, client.MergeFrom(&pool))
}
