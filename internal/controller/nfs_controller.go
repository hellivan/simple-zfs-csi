package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/nfsserver"
)

// NFSReconciler reconciles nfs-protocol NetworkExports for a single node into the
// kernel NFS export table.
type NFSReconciler struct {
	client.Client
	NodeName string
	Exports  *nfsserver.ExportManager
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=networkexports,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=networkexports/status,verbs=get;update;patch

// Reconcile rebuilds /etc/exports from all nfs exports owned by this node.
//
// Deletion goes through networkExportFinalizer (see its doc comment): the
// object is re-rendered — which already excludes it, since listOwnedExports
// skips anything with a DeletionTimestamp — before the finalizer is released,
// so the export is guaranteed to be dropped from /etc/exports even if this
// pod was down when the object was deleted and only sees it on a later List.
func (r *NFSReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	export, ok := r.getRequested(ctx, req)
	if !ok {
		return ctrl.Result{}, nil
	}

	if !export.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(export, networkExportFinalizer) {
			if err := r.renderExports(ctx); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(export, networkExportFinalizer)
			if err := r.Update(ctx, export); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(export, networkExportFinalizer) {
		controllerutil.AddFinalizer(export, networkExportFinalizer)
		if err := r.Update(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
		// The update re-enqueues; continue on the next pass with the finalizer set.
		return ctrl.Result{}, nil
	}

	if err := r.renderExports(ctx); err != nil {
		r.markError(ctx, export, err)
		return ctrl.Result{}, err
	}
	r.markExported(ctx, export)
	return ctrl.Result{}, nil
}

// renderExports re-lists every current NetworkExport for this node and applies
// the resulting desired state to /etc/exports.
func (r *NFSReconciler) renderExports(ctx context.Context) error {
	logger := log.FromContext(ctx)

	shares, err := listOwnedExports(ctx, r.Client, r.NodeName, storagev1alpha1.ProtocolNFS)
	if err != nil {
		return err
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
		return err
	}

	logger.Info("reconciled NFS exports", "count", len(exports))
	return nil
}

func (r *NFSReconciler) markInvalid(ctx context.Context, s *storagev1alpha1.NetworkExport, msg string) {
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseError, "InvalidSpec", msg, ""); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", s.Name)
	}
}

func (r *NFSReconciler) markError(ctx context.Context, s *storagev1alpha1.NetworkExport, cause error) {
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseError, "ExportFailed", cause.Error(), ""); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", s.Name)
	}
}

func (r *NFSReconciler) markExported(ctx context.Context, s *storagev1alpha1.NetworkExport) {
	if s.Spec.NFS == nil || len(s.Spec.NFS.Clients) == 0 {
		return // already marked invalid above
	}
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseExported, "Exported", fmt.Sprintf("exported %s over NFS", s.Spec.Path), ""); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", s.Name)
	}
}

func (r *NFSReconciler) getRequested(ctx context.Context, req ctrl.Request) (*storagev1alpha1.NetworkExport, bool) {
	var s storagev1alpha1.NetworkExport
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
		For(&storagev1alpha1.NetworkExport{}).
		WithEventFilter(nodeProtocolPredicate(r.NodeName, storagev1alpha1.ProtocolNFS)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("nfs-networkexport").
		Complete(r)
}
