package controller

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
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

// zfsVolumeFinalizer guards a ZfsVolume so the agent hosting its pool can run
// `zfs destroy` before the object is removed. Unlike ZfsShare (whose child
// NetworkExport is garbage-collected via owner references), destroying a dataset
// is a real external side-effect that must complete before we release the object.
const zfsVolumeFinalizer = "storage.zfs-shares.io/zfsvolume"

// ZfsVolumeReconciler is the per-node agent that fulfils ZfsVolume allocations.
// It runs inside the privileged storage DaemonSet (one manager per node, no
// leader election) alongside pool discovery. Each agent reconciles every
// ZfsVolume but only acts on those whose pool GUID is currently hosted by its
// own node (ZfsPool.status.currentNode == NodeName): it creates the dataset/zvol
// idempotently, reports status.path + Ready, and on deletion runs `zfs destroy`
// before removing the finalizer. Watching ZfsPool re-drives volumes on takeover.
type ZfsVolumeReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NodeName is the node this agent runs on; only volumes on pools currently
	// hosted here are acted upon.
	NodeName string
	// ZFS performs the dataset/zvol create and destroy operations on the host.
	ZFS zpool.ZFS
}

// +kubebuilder:rbac:groups=storage.zfs-shares.io,resources=zfsvolumes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.zfs-shares.io,resources=zfsvolumes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.zfs-shares.io,resources=zfspools,verbs=get;list;watch

// Reconcile creates or destroys the ZFS object backing a ZfsVolume, but only on
// the node that currently hosts its pool.
func (r *ZfsVolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var vol storagev1alpha1.ZfsVolume
	if err := r.Get(ctx, req.NamespacedName, &vol); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	poolName := zpool.ResourceName(vol.Spec.PoolGUID)
	var pool storagev1alpha1.ZfsPool
	poolErr := r.Get(ctx, client.ObjectKey{Name: poolName}, &pool)
	if poolErr != nil && !apierrors.IsNotFound(poolErr) {
		return ctrl.Result{}, poolErr
	}
	poolFound := poolErr == nil
	hostedHere := poolFound &&
		pool.Status.CurrentNode == r.NodeName &&
		pool.Status.Health != storagev1alpha1.PoolHealthNodeOffline

	// Deletion: only the hosting node destroys the dataset and releases the
	// finalizer. Other nodes ignore it; if the pool CRD is gone entirely there is
	// nothing to destroy, so any agent may release the object.
	if !vol.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&vol, zfsVolumeFinalizer) {
			return ctrl.Result{}, nil
		}
		switch {
		case hostedHere:
			full, err := datasetName(pool.Status.PoolName, vol.Spec.Dataset)
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := r.ZFS.Destroy(ctx, full, true); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("destroyed ZFS object", "dataset", full)
			return ctrl.Result{}, r.releaseFinalizer(ctx, &vol)
		case !poolFound:
			return ctrl.Result{}, r.releaseFinalizer(ctx, &vol)
		default:
			// Pool is hosted by another (or currently offline) node; that node's
			// agent will run the destroy once it can reach the pool.
			return ctrl.Result{}, nil
		}
	}

	if !hostedHere {
		// Not our pool right now; leave the object untouched for the hosting agent.
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&vol, zfsVolumeFinalizer) {
		controllerutil.AddFinalizer(&vol, zfsVolumeFinalizer)
		if err := r.Update(ctx, &vol); err != nil {
			return ctrl.Result{}, err
		}
		// The update re-enqueues; continue on the next pass with the finalizer set.
		return ctrl.Result{}, nil
	}

	full, err := datasetName(pool.Status.PoolName, vol.Spec.Dataset)
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.VolumePhaseError, "",
			"InvalidDataset", err.Error())
	}

	// Idempotent create: only create when the object is absent.
	if _, err := r.ZFS.Get(ctx, full, "type"); err != nil {
		if !errors.Is(err, zpool.ErrNotExist) {
			return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.VolumePhaseError, "",
				"LookupFailed", err.Error())
		}
		if err := r.create(ctx, &vol, full); err != nil {
			return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.VolumePhaseError, "",
				"CreateFailed", err.Error())
		}
		logger.Info("created ZFS object", "dataset", full, "type", vol.Spec.Type)
	}

	volPath, err := deriveVolumePath(vol.Spec.Type, pool.Status.BaseMountPath, pool.Status.PoolName, vol.Spec.Dataset)
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.VolumePhaseError, "",
			"PathDeriveFailed", err.Error())
	}

	return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.VolumePhaseReady, volPath,
		"Ready", fmt.Sprintf("provisioned %s on %s", full, r.NodeName))
}

// releaseFinalizer removes the agent finalizer, allowing the API server to
// complete deletion.
func (r *ZfsVolumeReconciler) releaseFinalizer(ctx context.Context, vol *storagev1alpha1.ZfsVolume) error {
	controllerutil.RemoveFinalizer(vol, zfsVolumeFinalizer)
	return r.Update(ctx, vol)
}

// create provisions the filesystem or volume described by the volume spec.
func (r *ZfsVolumeReconciler) create(ctx context.Context, vol *storagev1alpha1.ZfsVolume, full string) error {
	switch vol.Spec.Type {
	case storagev1alpha1.VolumeTypeFilesystem:
		return r.ZFS.CreateDataset(ctx, full, filesystemProps(vol))
	case storagev1alpha1.VolumeTypeVolume:
		if vol.Spec.Volume == nil {
			return fmt.Errorf("spec.volume is required for volume")
		}
		return r.ZFS.CreateZvol(ctx, full, vol.Spec.Volume.Size.Value(), volumeProps(vol))
	default:
		return fmt.Errorf("unknown volume type %q", vol.Spec.Type)
	}
}

// filesystemProps renders the ZFS properties for a filesystem dataset: the user
// properties, plus refquota derived from the filesystem quota when set.
func filesystemProps(vol *storagev1alpha1.ZfsVolume) map[string]string {
	props := copyProps(vol.Spec.Properties)
	if cfg := vol.Spec.Filesystem; cfg != nil && cfg.Quota != nil && !cfg.Quota.IsZero() {
		props["refquota"] = strconv.FormatInt(cfg.Quota.Value(), 10)
	}
	return props
}

// volumeProps renders the ZFS properties for a volume/zvol: the user properties,
// plus volblocksize when set.
func volumeProps(vol *storagev1alpha1.ZfsVolume) map[string]string {
	props := copyProps(vol.Spec.Properties)
	if cfg := vol.Spec.Volume; cfg != nil && cfg.Volblocksize != "" {
		props["volblocksize"] = cfg.Volblocksize
	}
	return props
}

// copyProps returns a shallow copy so callers can add derived properties without
// mutating the spec map.
func copyProps(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// datasetName joins the observed pool name with the logical dataset path into a
// full ZFS name, e.g. ("tank", "k8s/pvc-123") -> "tank/k8s/pvc-123".
func datasetName(poolName, dataset string) (string, error) {
	if poolName == "" {
		return "", fmt.Errorf("pool name is unknown")
	}
	ds := strings.Trim(dataset, "/")
	if ds == "" {
		return "", fmt.Errorf("dataset is empty")
	}
	return poolName + "/" + ds, nil
}

// deriveVolumePath computes the node-local path reported in status.path: a
// dataset's mountpoint under the pool base mount path, or a zvol's device node
// under /dev/zvol.
func deriveVolumePath(volType storagev1alpha1.VolumeType, baseMountPath, poolName, dataset string) (string, error) {
	ds := strings.Trim(dataset, "/")
	if ds == "" {
		return "", fmt.Errorf("dataset is empty")
	}
	switch volType {
	case storagev1alpha1.VolumeTypeFilesystem:
		if baseMountPath == "" {
			return "", fmt.Errorf("pool baseMountPath is unknown")
		}
		return path.Join(baseMountPath, ds), nil
	case storagev1alpha1.VolumeTypeVolume:
		if poolName == "" {
			return "", fmt.Errorf("pool name is unknown")
		}
		return path.Join("/dev/zvol", poolName, ds), nil
	default:
		return "", fmt.Errorf("unknown volume type %q", volType)
	}
}

// setStatus patches the volume's status subresource.
func (r *ZfsVolumeReconciler) setStatus(ctx context.Context, vol *storagev1alpha1.ZfsVolume, phase storagev1alpha1.ZfsVolumePhase, volPath, reason, message string) error {
	patched := vol.DeepCopy()
	patched.Status.Phase = phase
	patched.Status.Path = volPath
	patched.Status.ObservedGeneration = vol.Generation
	patched.Status.Message = message

	condStatus := metav1.ConditionTrue
	if phase != storagev1alpha1.VolumePhaseReady {
		condStatus = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&patched.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: vol.Generation,
	})

	return r.Status().Patch(ctx, patched, client.MergeFrom(vol))
}

// volumesForPool maps a ZfsPool event to reconcile requests for every ZfsVolume
// that references its GUID, so volumes are re-driven when a pool moves nodes or
// changes health (e.g. this node takes over hosting).
func (r *ZfsVolumeReconciler) volumesForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*storagev1alpha1.ZfsPool)
	if !ok {
		return nil
	}
	guid := pool.Status.GUID
	if guid == "" {
		guid = strings.TrimPrefix(pool.Name, "zpool-")
	}

	var volumes storagev1alpha1.ZfsVolumeList
	if err := r.List(ctx, &volumes); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range volumes.Items {
		if volumes.Items[i].Spec.PoolGUID == guid {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&volumes.Items[i])})
		}
	}
	return reqs
}

// SetupWithManager wires the reconciler into the manager.
func (r *ZfsVolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsVolume{}).
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.volumesForPool)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfsvolume").
		Complete(r)
}
