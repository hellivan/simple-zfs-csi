package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// zfsSnapshotFinalizer guards a ZfsSnapshot so the agent hosting its pool can run
// `zfs destroy pool/ds@snap` before the object is removed. Like ZfsDataset,
// destroying a snapshot is a real external side-effect that must complete before
// we release the object.
const zfsSnapshotFinalizer = "storage.simple-zfs-csi.io/zfssnapshot"

// ZfsSnapshotReconciler is the per-node agent that fulfils ZfsSnapshot requests.
// It runs inside the privileged storage DaemonSet alongside pool discovery and
// the ZfsDataset reconciler. It acts only on snapshots whose pool GUID is
// currently hosted by its own node: it takes the ZFS snapshot idempotently,
// reports readiness/creation-time/restore-size, and on deletion runs
// `zfs destroy` before removing the finalizer.
type ZfsSnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NodeName is the node this agent runs on; only snapshots on pools currently
	// hosted here are acted upon.
	NodeName string
	// ZFS performs the snapshot create and destroy operations on the host.
	ZFS zpool.ZFS
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfssnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfssnapshots/status,verbs=get;update;patch

// Reconcile takes or destroys the ZFS snapshot backing a ZfsSnapshot, but only on
// the node that currently hosts its pool.
func (r *ZfsSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var snap storagev1alpha1.ZfsSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	poolName := zpool.ResourceName(snap.Spec.PoolGUID)
	var pool storagev1alpha1.ZfsPool
	poolErr := r.Get(ctx, client.ObjectKey{Name: poolName}, &pool)
	if poolErr != nil && !apierrors.IsNotFound(poolErr) {
		return ctrl.Result{}, poolErr
	}
	poolFound := poolErr == nil
	hostedHere := poolFound &&
		pool.Status.CurrentNode == r.NodeName &&
		pool.Status.Health != storagev1alpha1.PoolHealthNodeOffline

	// Deletion: only the hosting node destroys the snapshot and releases the
	// finalizer. If the pool CRD is gone entirely there is nothing to destroy.
	if !snap.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&snap, zfsSnapshotFinalizer) {
			return ctrl.Result{}, nil
		}
		switch {
		case hostedHere:
			full, err := snapshotFullName(pool.Status.PoolName, snap.Spec.Dataset, snap.Spec.SnapshotName)
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := r.ZFS.Destroy(ctx, full, false); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("destroyed ZFS snapshot", "snapshot", full)
			return ctrl.Result{}, r.releaseSnapshotFinalizer(ctx, &snap)
		case !poolFound:
			return ctrl.Result{}, r.releaseSnapshotFinalizer(ctx, &snap)
		default:
			// Pool hosted elsewhere/offline; that node's agent will destroy it.
			return ctrl.Result{}, nil
		}
	}

	if !hostedHere {
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&snap, zfsSnapshotFinalizer) {
		controllerutil.AddFinalizer(&snap, zfsSnapshotFinalizer)
		if err := r.Update(ctx, &snap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	full, err := snapshotFullName(pool.Status.PoolName, snap.Spec.Dataset, snap.Spec.SnapshotName)
	if err != nil {
		return ctrl.Result{}, r.setSnapshotStatus(ctx, &snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
			"InvalidSnapshot", err.Error())
	}

	// Idempotent create: only snapshot when it is absent.
	if _, err := r.ZFS.Get(ctx, full, "type"); err != nil {
		if !errors.Is(err, zpool.ErrNotExist) {
			return ctrl.Result{}, r.setSnapshotStatus(ctx, &snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"LookupFailed", err.Error())
		}
		if err := r.ZFS.Snapshot(ctx, full); err != nil {
			return ctrl.Result{}, r.setSnapshotStatus(ctx, &snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"SnapshotFailed", err.Error())
		}
		logger.Info("created ZFS snapshot", "snapshot", full)
	}

	creation := snapshotCreationTime(ctx, r.ZFS, full)
	restore := snapshotRestoreSize(ctx, r.ZFS, full)

	return ctrl.Result{}, r.setSnapshotStatus(ctx, &snap, storagev1alpha1.SnapshotPhaseReady, true, creation, restore,
		"Ready", fmt.Sprintf("snapshot %s ready on %s", full, r.NodeName))
}

// releaseSnapshotFinalizer removes the agent finalizer, allowing deletion.
func (r *ZfsSnapshotReconciler) releaseSnapshotFinalizer(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot) error {
	controllerutil.RemoveFinalizer(snap, zfsSnapshotFinalizer)
	return r.Update(ctx, snap)
}

// snapshotFullName joins the observed pool name, the source dataset path and the
// snapshot short name into a full ZFS snapshot name, e.g.
// ("tank", "k8s/pvc-1", "snapshot-x") -> "tank/k8s/pvc-1@snapshot-x".
func snapshotFullName(poolName, dataset, snapName string) (string, error) {
	if poolName == "" {
		return "", fmt.Errorf("pool name is unknown")
	}
	ds := strings.Trim(dataset, "/")
	if ds == "" {
		return "", fmt.Errorf("dataset is empty")
	}
	if snapName == "" {
		return "", fmt.Errorf("snapshot name is empty")
	}
	return poolName + "/" + ds + "@" + snapName, nil
}

// snapshotCreationTime reads the ZFS `creation` property (unix seconds with -p)
// and returns it as a metav1.Time, falling back to now when unavailable.
func snapshotCreationTime(ctx context.Context, z zpool.ZFS, full string) *metav1.Time {
	val, err := z.Get(ctx, full, "creation")
	if err == nil {
		if sec, perr := strconv.ParseInt(strings.TrimSpace(val), 10, 64); perr == nil && sec > 0 {
			t := metav1.NewTime(time.Unix(sec, 0).UTC())
			return &t
		}
	}
	t := metav1.Now()
	return &t
}

// snapshotRestoreSize reads the ZFS `referenced` property (bytes with -p) — the
// minimum size needed to restore the snapshot — as a resource.Quantity.
func snapshotRestoreSize(ctx context.Context, z zpool.ZFS, full string) *resource.Quantity {
	val, err := z.Get(ctx, full, "referenced")
	if err != nil {
		return nil
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
	if perr != nil || n < 0 {
		return nil
	}
	return resource.NewQuantity(n, resource.BinarySI)
}

// setSnapshotStatus patches the snapshot's status subresource.
func (r *ZfsSnapshotReconciler) setSnapshotStatus(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot, phase storagev1alpha1.ZfsSnapshotPhase, ready bool, creation *metav1.Time, restore *resource.Quantity, reason, message string) error {
	patched := snap.DeepCopy()
	patched.Status.Phase = phase
	patched.Status.ReadyToUse = ready
	if creation != nil {
		patched.Status.CreationTime = creation
	}
	if restore != nil {
		patched.Status.RestoreSize = restore
	}
	patched.Status.ObservedGeneration = snap.Generation
	patched.Status.Message = message

	condStatus := metav1.ConditionTrue
	if phase != storagev1alpha1.SnapshotPhaseReady {
		condStatus = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&patched.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: snap.Generation,
	})

	return r.Status().Patch(ctx, patched, client.MergeFrom(snap))
}

// snapshotsForPool maps a ZfsPool event to reconcile requests for every
// ZfsSnapshot that references its GUID, so snapshots are re-driven on takeover.
func (r *ZfsSnapshotReconciler) snapshotsForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*storagev1alpha1.ZfsPool)
	if !ok {
		return nil
	}
	guid := pool.Status.GUID
	if guid == "" {
		guid = strings.TrimPrefix(pool.Name, "zpool-")
	}

	var snaps storagev1alpha1.ZfsSnapshotList
	if err := r.List(ctx, &snaps); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range snaps.Items {
		if snaps.Items[i].Spec.PoolGUID == guid {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&snaps.Items[i])})
		}
	}
	return reqs
}

// SetupWithManager wires the reconciler into the manager.
func (r *ZfsSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsSnapshot{}).
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.snapshotsForPool)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfssnapshot").
		Complete(r)
}
