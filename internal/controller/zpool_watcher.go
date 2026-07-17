package controller

import (
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// PoolWatcher is the Tier 2 (cluster-wide) monitoring component. It runs as a
// single-replica Deployment and detects storage-node deaths that the per-node
// discovery DaemonSet cannot self-report: a node that loses power or panics
// takes its reporter down with it, leaving its ZfsPool objects falsely claiming
// ONLINE at a dead IP.
//
// The watcher reconciles core Node objects. When a node is unreachable
// (NotReady or deleted) it forcibly marks every ZfsPool that node last reported
// as NODE_OFFLINE, so CSI node plugins fail fast instead of hanging on a dead
// target. Once the pool is imported elsewhere, that node's reporter overwrites
// the status back to ONLINE with the new routing.
type PoolWatcher struct {
	client.Client
}

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspools,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspools/status,verbs=get;update;patch

// Reconcile checks a Node's readiness and offlines its pools when it is down.
func (r *PoolWatcher) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var node corev1.Node
	err := r.Get(ctx, req.NamespacedName, &node)
	switch {
	case apierrors.IsNotFound(err):
		// Node object removed entirely: treat as offline.
		return ctrl.Result{}, r.offlinePoolsForNode(ctx, logger, req.Name)
	case err != nil:
		return ctrl.Result{}, err
	}

	if nodeReady(&node) {
		// Healthy node: the per-node reporter owns its pools' status.
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, r.offlinePoolsForNode(ctx, logger, node.Name)
}

// offlinePoolsForNode marks every ZfsPool whose status.currentNode is nodeName
// as NODE_OFFLINE, unless it is already offline or has since been taken over by
// a different node.
func (r *PoolWatcher) offlinePoolsForNode(ctx context.Context, logger logr.Logger, nodeName string) error {
	var pools storagev1alpha1.ZfsPoolList
	if err := r.List(ctx, &pools); err != nil {
		return err
	}
	for i := range pools.Items {
		p := &pools.Items[i]
		if p.Status.CurrentNode != nodeName {
			continue
		}
		if p.Status.Health == storagev1alpha1.PoolHealthNodeOffline {
			continue
		}
		patched := p.DeepCopy()
		now := metav1.Now()
		patched.Status.Health = storagev1alpha1.PoolHealthNodeOffline
		patched.Status.LastUpdated = &now
		patched.Status.Message = "node " + nodeName + " is unreachable; offlined by watcher"
		if err := r.Status().Patch(ctx, patched, client.MergeFrom(p)); err != nil {
			return err
		}
		logger.Info("offlined pool for dead node", "pool", p.Name, "node", nodeName)
	}
	return nil
}

// nodeReady reports whether the node's Ready condition is True.
func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager wires the watcher into the manager.
func (r *PoolWatcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Named("zfspool-node-watcher").
		Complete(r)
}
