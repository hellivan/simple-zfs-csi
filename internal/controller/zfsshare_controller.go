package controller

import (
	"context"
	"fmt"
	"path"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/hellivan/zfs-shares/api/v1alpha1"
	"github.com/hellivan/zfs-shares/internal/zpool"
)

// ZfsShareReconciler is the cluster-wide translator that compiles a ZFS-centric
// ZfsShare (keyed on pool GUID + dataset) into a node-pinned NetworkExport. It
// runs in the operator manager (leader-elected, unprivileged): it resolves the
// pool GUID to the pool's current node, name and mount root via ZfsPool.status,
// derives the node-local export path, and owns a child NetworkExport that the
// per-node aggregators execute. Watching ZfsPool re-targets shares on takeover.
type ZfsShareReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=storage.zfs-shares.io,resources=zfsshares,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.zfs-shares.io,resources=zfsshares/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.zfs-shares.io,resources=zfspools,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.zfs-shares.io,resources=networkexports,verbs=get;list;watch;create;update;patch;delete

// Reconcile resolves the share's pool and renders its child NetworkExport.
func (r *ZfsShareReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var share storagev1alpha1.ZfsShare
	if err := r.Get(ctx, req.NamespacedName, &share); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !share.DeletionTimestamp.IsZero() {
		// The child NetworkExport is garbage-collected via its owner reference.
		return ctrl.Result{}, nil
	}

	poolName := zpool.ResourceName(share.Spec.PoolGUID)
	var pool storagev1alpha1.ZfsPool
	if err := r.Get(ctx, client.ObjectKey{Name: poolName}, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.setStatus(ctx, &share, storagev1alpha1.SharePhasePending, "", "",
				"PoolNotFound", fmt.Sprintf("ZfsPool %s not found", poolName))
		}
		return ctrl.Result{}, err
	}

	if pool.Status.CurrentNode == "" || pool.Status.Health == storagev1alpha1.PoolHealthNodeOffline {
		// Leave any existing child in place; it will be re-targeted once the pool
		// is imported (or re-imported) on a reachable node.
		return ctrl.Result{}, r.setStatus(ctx, &share, storagev1alpha1.SharePhasePending, pool.Status.CurrentNode, "",
			"PoolUnavailable", "pool has no reachable current node")
	}

	exportPath, err := derivePath(share.Spec.Protocol, pool.Status.BaseMountPath, pool.Status.PoolName, share.Spec.Dataset)
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &share, storagev1alpha1.SharePhaseError, pool.Status.CurrentNode, "",
			"PathDeriveFailed", err.Error())
	}

	export := &storagev1alpha1.NetworkExport{ObjectMeta: metav1.ObjectMeta{Name: share.Name}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, export, func() error {
		export.Spec.NodeName = pool.Status.CurrentNode
		export.Spec.Protocol = share.Spec.Protocol
		export.Spec.Path = exportPath
		export.Spec.NFS = share.Spec.NFS.DeepCopy()
		export.Spec.NVMeoF = share.Spec.NVMeoF.DeepCopy()
		return controllerutil.SetControllerReference(&share, export, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &share, storagev1alpha1.SharePhaseError, pool.Status.CurrentNode, exportPath,
			"RenderFailed", err.Error())
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("rendered NetworkExport", "op", op, "export", export.Name, "node", export.Spec.NodeName, "path", exportPath)
	}

	return ctrl.Result{}, r.setStatus(ctx, &share, storagev1alpha1.SharePhaseBound, pool.Status.CurrentNode, exportPath,
		"Bound", fmt.Sprintf("exported %s on %s", exportPath, pool.Status.CurrentNode))
}

// derivePath computes the node-local source path of the export from the resolved
// pool. NFS exports the dataset mountpoint under the pool's base mount path;
// NVMe-oF exports the zvol device node under /dev/zvol/<poolName>.
func derivePath(protocol storagev1alpha1.Protocol, baseMountPath, poolName, dataset string) (string, error) {
	ds := strings.Trim(dataset, "/")
	if ds == "" {
		return "", fmt.Errorf("dataset is empty")
	}
	switch protocol {
	case storagev1alpha1.ProtocolNFS:
		if baseMountPath == "" {
			return "", fmt.Errorf("pool baseMountPath is unknown")
		}
		return path.Join(baseMountPath, ds), nil
	case storagev1alpha1.ProtocolNVMeoF:
		if poolName == "" {
			return "", fmt.Errorf("pool name is unknown")
		}
		return path.Join("/dev/zvol", poolName, ds), nil
	default:
		return "", fmt.Errorf("unknown protocol %q", protocol)
	}
}

// setStatus patches the share's status subresource.
func (r *ZfsShareReconciler) setStatus(ctx context.Context, share *storagev1alpha1.ZfsShare, phase storagev1alpha1.ZfsSharePhase, nodeName, exportPath, reason, message string) error {
	patched := share.DeepCopy()
	patched.Status.Phase = phase
	patched.Status.NodeName = nodeName
	patched.Status.Path = exportPath
	patched.Status.ObservedGeneration = share.Generation
	patched.Status.Message = message
	if phase == storagev1alpha1.SharePhaseBound {
		patched.Status.NetworkExportName = share.Name
	}

	condStatus := metav1.ConditionTrue
	if phase != storagev1alpha1.SharePhaseBound {
		condStatus = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&patched.Status.Conditions, metav1.Condition{
		Type:               "Bound",
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: share.Generation,
	})

	return r.Status().Patch(ctx, patched, client.MergeFrom(share))
}

// sharesForPool maps a ZfsPool event to reconcile requests for every ZfsShare
// that references its GUID, so shares are re-rendered when a pool moves nodes,
// changes mount path, or is offlined.
func (r *ZfsShareReconciler) sharesForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*storagev1alpha1.ZfsPool)
	if !ok {
		return nil
	}
	guid := pool.Status.GUID
	if guid == "" {
		guid = strings.TrimPrefix(pool.Name, "zpool-")
	}

	var shares storagev1alpha1.ZfsShareList
	if err := r.List(ctx, &shares); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range shares.Items {
		if shares.Items[i].Spec.PoolGUID == guid {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&shares.Items[i])})
		}
	}
	return reqs
}

// SetupWithManager wires the reconciler into the manager.
func (r *ZfsShareReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsShare{}).
		Owns(&storagev1alpha1.NetworkExport{}).
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.sharesForPool)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfsshare").
		Complete(r)
}
