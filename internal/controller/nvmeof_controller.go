package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/nvmeauth"
	"github.com/hellivan/simple-zfs-csi/internal/nvmet"
)

// NVMeoFReconciler reconciles nvmeof-protocol NetworkExports for a single node into
// the kernel NVMe target (nvmet) configfs tree.
type NVMeoFReconciler struct {
	client.Client
	// SecretReader reads DH-CHAP secrets directly from the API server (uncached).
	// Using a non-cached reader avoids starting a cluster-wide Secret informer,
	// so a namespaced secrets Role suffices (ADR-0011) and the controller never
	// caches every Secret in the cluster. Defaults to r.Client when nil.
	SecretReader client.Reader
	NodeName     string
	Target       *nvmet.Target
	NQNPrefix    string
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=networkexports,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=networkexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile rebuilds the nvmet target from all nvmeof exports owned by this node.
//
// Deletion goes through networkExportFinalizer (see its doc comment): the
// object is re-rendered — which already excludes it, since listOwnedExports
// skips anything with a DeletionTimestamp — before the finalizer is released,
// so the export is guaranteed to be dropped from the nvmet target even if this
// pod was down when the object was deleted and only sees it on a later List.
func (r *NVMeoFReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	export, ok := r.getRequested(ctx, req)
	if !ok {
		return ctrl.Result{}, nil
	}

	if !export.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(export, networkExportFinalizer) {
			if _, err := r.renderTarget(ctx); err != nil {
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

	nqnByName, err := r.renderTarget(ctx)
	if err != nil {
		r.markError(ctx, export, err)
		return ctrl.Result{}, err
	}
	r.markExported(ctx, export, nqnByName)
	return ctrl.Result{}, nil
}

// renderTarget re-lists every current NetworkExport for this node and
// reconciles the resulting desired state into the nvmet target. Returns the
// NQN each export was rendered with, keyed by NetworkExport name.
func (r *NVMeoFReconciler) renderTarget(ctx context.Context) (map[string]string, error) {
	logger := log.FromContext(ctx)

	shares, err := listOwnedExports(ctx, r.Client, r.NodeName, storagev1alpha1.ProtocolNVMeoF)
	if err != nil {
		return nil, err
	}

	desired := make([]nvmet.Subsystem, 0, len(shares))
	nqnByName := make(map[string]string, len(shares))
	for i := range shares {
		s := &shares[i]
		nqn := r.effectiveNQN(s)
		nqnByName[s.Name] = nqn
		var allowed []string
		var dhchapKey string
		if s.Spec.NVMeoF != nil {
			allowed = s.Spec.NVMeoF.AllowedHosts
			if s.Spec.NVMeoF.DHChapSecretName != "" {
				key, err := r.dhchapKey(ctx, s.Spec.NVMeoF.DHChapSecretNamespace, s.Spec.NVMeoF.DHChapSecretName, s.Spec.NVMeoF.DHChapSecretKey)
				if err != nil {
					// The target is not fully programmed until the key is available.
					logger.Error(err, "resolve dhchap key", "export", s.Name)
					return nil, err
				}
				dhchapKey = key
			}
		}
		desired = append(desired, nvmet.Subsystem{
			NQN:          nqn,
			DevicePath:   s.Spec.Path,
			AllowedHosts: allowed,
			DHChapKey:    dhchapKey,
		})
	}

	if err := r.Target.Reconcile(desired); err != nil {
		logger.Error(err, "failed to reconcile nvmet target")
		return nil, err
	}

	logger.Info("reconciled NVMe-oF target", "subsystems", len(desired))
	return nqnByName, nil
}

// effectiveNQN returns the explicit NQN or a deterministic derived one.
func (r *NVMeoFReconciler) effectiveNQN(s *storagev1alpha1.NetworkExport) string {
	if s.Spec.NVMeoF != nil && s.Spec.NVMeoF.NQN != "" {
		return s.Spec.NVMeoF.NQN
	}
	return fmt.Sprintf("%s:%s:%s", r.NQNPrefix, s.Spec.NodeName, s.Name)
}

// dhchapKey reads the DH-CHAP secret referenced by an nvmeof export.
func (r *NVMeoFReconciler) dhchapKey(ctx context.Context, namespace, name, dataKey string) (string, error) {
	if namespace == "" {
		return "", fmt.Errorf("dhchap secret %q has no namespace", name)
	}
	reader := client.Reader(r.Client)
	if r.SecretReader != nil {
		reader = r.SecretReader
	}
	var sec corev1.Secret
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec); err != nil {
		return "", fmt.Errorf("get dhchap secret %s/%s: %w", namespace, name, err)
	}
	k := nvmeauth.ResolveSecretKey(dataKey)
	key := sec.Data[k]
	if len(key) == 0 {
		return "", fmt.Errorf("dhchap secret %s/%s missing data key %q", namespace, name, k)
	}
	return string(key), nil
}

func (r *NVMeoFReconciler) markError(ctx context.Context, s *storagev1alpha1.NetworkExport, cause error) {
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseError, "ExportFailed", cause.Error(), ""); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", s.Name)
	}
}

func (r *NVMeoFReconciler) markExported(ctx context.Context, s *storagev1alpha1.NetworkExport, nqnByName map[string]string) {
	nqn := nqnByName[s.Name]
	if nqn == "" {
		nqn = r.effectiveNQN(s)
	}
	msg := fmt.Sprintf("exported %s as %s", s.Spec.Path, nqn)
	if err := updateStatus(ctx, r.Client, s, storagev1alpha1.PhaseExported, "Exported", msg, nqn); err != nil {
		log.FromContext(ctx).Error(err, "status update failed", "share", s.Name)
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
