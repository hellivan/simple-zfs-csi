package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/hellivan/zfs-shares/api/v1alpha1"
	"github.com/hellivan/zfs-shares/internal/nfsserver"
)

// NFSReconciler reconciles nfs-protocol ZfsShares for a single node into the
// kernel NFS export table.
type NFSReconciler struct {
	client.Client
	NodeName string
	Exports  *nfsserver.ExportManager
}

// +kubebuilder:rbac:groups=storage.zfs-shares.io,resources=zfsshares,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.zfs-shares.io,resources=zfsshares/status,verbs=get;update;patch

// Reconcile rebuilds /etc/exports from all nfs shares owned by this node.
func (r *NFSReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	shares, err := listOwnedShares(ctx, r.Client, r.NodeName, storagev1alpha1.ProtocolNFS)
	if err != nil {
		return ctrl.Result{}, err
	}

	exports := make([]nfsserver.Export, 0, len(shares))
	for i := range shares {
		s := &shares[i]
		if s.Spec.NFS == nil || len(s.Spec.NFS.Clients) == 0 {
			r.markInvalid(ctx, s, "missing spec.nfs.clients")
			continue
		}
		clients := make([]nfsserver.Client, 0, len(s.Spec.NFS.Clients))
		for _, c := range s.Spec.NFS.Clients {
			clients = append(clients, nfsserver.Client{Client: c.Client, Options: c.Options})
		}
		exports = append(exports, nfsserver.Export{Path: s.Spec.Path, Clients: clients})
	}

	if err := r.Exports.Apply(exports); err != nil {
		logger.Error(err, "failed to apply NFS exports")
		r.markErrorForRequest(ctx, req, err)
		return ctrl.Result{}, err
	}

	logger.Info("reconciled NFS exports", "count", len(exports))
	r.markExportedForRequest(ctx, req)
	return ctrl.Result{}, nil
}

func (r *NFSReconciler) markInvalid(ctx context.Context, s *storagev1alpha1.ZfsShare, msg string) {
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseError, "InvalidSpec", msg, ""); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", s.Name)
	}
}

func (r *NFSReconciler) markErrorForRequest(ctx context.Context, req ctrl.Request, cause error) {
	s, ok := r.getRequested(ctx, req)
	if !ok {
		return
	}
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseError, "ExportFailed", cause.Error(), ""); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", req.Name)
	}
}

func (r *NFSReconciler) markExportedForRequest(ctx context.Context, req ctrl.Request) {
	s, ok := r.getRequested(ctx, req)
	if !ok {
		return
	}
	if s.Spec.NFS == nil || len(s.Spec.NFS.Clients) == 0 {
		return // already marked invalid above
	}
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseExported, "Exported", fmt.Sprintf("exported %s over NFS", s.Spec.Path), ""); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", req.Name)
	}
}

func (r *NFSReconciler) getRequested(ctx context.Context, req ctrl.Request) (*storagev1alpha1.ZfsShare, bool) {
	var s storagev1alpha1.ZfsShare
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "get share for status", "share", req.Name)
		}
		return nil, false
	}
	if s.Spec.NodeName != r.NodeName || s.Spec.Protocol != storagev1alpha1.ProtocolNFS {
		return nil, false
	}
	return &s, true
}

// SetupWithManager wires the reconciler into the manager.
func (r *NFSReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsShare{}).
		WithEventFilter(nodeProtocolPredicate(r.NodeName, storagev1alpha1.ProtocolNFS)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("nfs-zfsshare").
		Complete(r)
}
