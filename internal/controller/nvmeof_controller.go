package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/nvmet"
)

// NVMeoFReconciler reconciles nvmeof-protocol NetworkExports for a single node into
// the kernel NVMe target (nvmet) configfs tree.
type NVMeoFReconciler struct {
	client.Client
	NodeName  string
	Target    *nvmet.Target
	NQNPrefix string
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=networkexports,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=networkexports/status,verbs=get;update;patch

// Reconcile rebuilds the nvmet target from all nvmeof exports owned by this node.
func (r *NVMeoFReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	shares, err := listOwnedExports(ctx, r.Client, r.NodeName, storagev1alpha1.ProtocolNVMeoF)
	if err != nil {
		return ctrl.Result{}, err
	}

	desired := make([]nvmet.Subsystem, 0, len(shares))
	nqnByName := make(map[string]string, len(shares))
	for i := range shares {
		s := &shares[i]
		nqn := r.effectiveNQN(s)
		nqnByName[s.Name] = nqn
		var allowed []string
		if s.Spec.NVMeoF != nil {
			allowed = s.Spec.NVMeoF.AllowedHosts
		}
		desired = append(desired, nvmet.Subsystem{
			NQN:          nqn,
			DevicePath:   s.Spec.Path,
			AllowedHosts: allowed,
		})
	}

	if err := r.Target.Reconcile(desired); err != nil {
		logger.Error(err, "failed to reconcile nvmet target")
		r.markErrorForRequest(ctx, req, err)
		return ctrl.Result{}, err
	}

	logger.Info("reconciled NVMe-oF target", "subsystems", len(desired))
	r.markExportedForRequest(ctx, req, nqnByName)
	return ctrl.Result{}, nil
}

// effectiveNQN returns the explicit NQN or a deterministic derived one.
func (r *NVMeoFReconciler) effectiveNQN(s *storagev1alpha1.NetworkExport) string {
	if s.Spec.NVMeoF != nil && s.Spec.NVMeoF.NQN != "" {
		return s.Spec.NVMeoF.NQN
	}
	return fmt.Sprintf("%s:%s:%s", r.NQNPrefix, s.Spec.NodeName, s.Name)
}

func (r *NVMeoFReconciler) markErrorForRequest(ctx context.Context, req ctrl.Request, cause error) {
	s, ok := r.getRequested(ctx, req)
	if !ok {
		return
	}
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseError, "ExportFailed", cause.Error(), ""); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", req.Name)
	}
}

func (r *NVMeoFReconciler) markExportedForRequest(ctx context.Context, req ctrl.Request, nqnByName map[string]string) {
	s, ok := r.getRequested(ctx, req)
	if !ok {
		return
	}
	nqn := nqnByName[s.Name]
	if nqn == "" {
		nqn = r.effectiveNQN(s)
	}
	msg := fmt.Sprintf("exported %s as %s", s.Spec.Path, nqn)
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseExported, "Exported", msg, nqn); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", req.Name)
	}
}

func (r *NVMeoFReconciler) getRequested(ctx context.Context, req ctrl.Request) (*storagev1alpha1.NetworkExport, bool) {
	var s storagev1alpha1.NetworkExport
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "get share for status", "share", req.Name)
		}
		return nil, false
	}
	if s.Spec.NodeName != r.NodeName || s.Spec.Protocol != storagev1alpha1.ProtocolNVMeoF {
		return nil, false
	}
	return &s, true
}

// SetupWithManager wires the reconciler into the manager.
func (r *NVMeoFReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.NetworkExport{}).
		WithEventFilter(nodeProtocolPredicate(r.NodeName, storagev1alpha1.ProtocolNVMeoF)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("nvmeof-networkexport").
		Complete(r)
}
