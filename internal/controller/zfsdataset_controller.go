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

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// zfsDatasetFinalizer guards a ZfsDataset so the agent hosting its pool can run
// `zfs destroy` before the object is removed. Unlike ZfsShare (whose child
// NetworkExport is garbage-collected via owner references), destroying a dataset
// is a real external side-effect that must complete before we release the object.
const zfsDatasetFinalizer = "storage.simple-zfs-csi.io/zfsdataset"

// ZfsDatasetReconciler is the per-node agent that fulfils ZfsDataset allocations.
// It runs inside the privileged storage DaemonSet (one manager per node, no
// leader election) alongside pool discovery. Each agent reconciles every
// ZfsDataset but only acts on those whose pool GUID is currently hosted by its
// own node (ZfsPool.status.currentNode == NodeName): it creates the dataset/zvol
// idempotently, reports status.path + Ready, and on deletion runs `zfs destroy`
// before removing the finalizer. Watching ZfsPool re-drives volumes on takeover.
type ZfsDatasetReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NodeName is the node this agent runs on; only volumes on pools currently
	// hosted here are acted upon.
	NodeName string
	// ZFS performs the dataset/zvol create and destroy operations on the host.
	ZFS zpool.ZFS
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsdatasets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsdatasets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspools,verbs=get;list;watch

// Reconcile creates or destroys the ZFS object backing a ZfsDataset, but only on
// the node that currently hosts its pool.
func (r *ZfsDatasetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var vol storagev1alpha1.ZfsDataset
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
		if !controllerutil.ContainsFinalizer(&vol, zfsDatasetFinalizer) {
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

	if !controllerutil.ContainsFinalizer(&vol, zfsDatasetFinalizer) {
		controllerutil.AddFinalizer(&vol, zfsDatasetFinalizer)
		if err := r.Update(ctx, &vol); err != nil {
			return ctrl.Result{}, err
		}
		// The update re-enqueues; continue on the next pass with the finalizer set.
		return ctrl.Result{}, nil
	}

	full, err := datasetName(pool.Status.PoolName, vol.Spec.Dataset)
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.DatasetPhaseError, "",
			"InvalidDataset", err.Error())
	}

	// Idempotent create: only create when the object is absent.
	if _, err := r.ZFS.Get(ctx, full, "type"); err != nil {
		if !errors.Is(err, zpool.ErrNotExist) {
			return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.DatasetPhaseError, "",
				"LookupFailed", err.Error())
		}
		if err := r.create(ctx, &vol, pool.Status.PoolName, full); err != nil {
			return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.DatasetPhaseError, "",
				"CreateFailed", err.Error())
		}
		if err := r.applyRootOwnership(ctx, &vol, pool.Status.BaseMountPath, pool.Status.PoolName); err != nil {
			return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.DatasetPhaseError, "",
				"OwnershipFailed", err.Error())
		}
		logger.Info("created ZFS object", "dataset", full, "type", vol.Spec.Type)
	}

	// Converge the on-disk size toward the spec (volume expansion). The CSI
	// controller bumps spec.filesystem.quota / spec.volume.size; the agent applies
	// it here on the next reconcile.
	if err := r.ensureSize(ctx, &vol, full); err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.DatasetPhaseError, "",
			"ResizeFailed", err.Error())
	}

	volPath, err := deriveVolumePath(vol.Spec.Type, pool.Status.BaseMountPath, pool.Status.PoolName, vol.Spec.Dataset)
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.DatasetPhaseError, "",
			"PathDeriveFailed", err.Error())
	}

	return ctrl.Result{}, r.setStatus(ctx, &vol, storagev1alpha1.DatasetPhaseReady, volPath,
		"Ready", fmt.Sprintf("provisioned %s on %s", full, r.NodeName))
}

// releaseFinalizer removes the agent finalizer, allowing the API server to
// complete deletion.
func (r *ZfsDatasetReconciler) releaseFinalizer(ctx context.Context, vol *storagev1alpha1.ZfsDataset) error {
	controllerutil.RemoveFinalizer(vol, zfsDatasetFinalizer)
	return r.Update(ctx, vol)
}

// create provisions the filesystem or volume described by the volume spec. When
// spec.Source is set it clones the dataset from an existing snapshot or volume
// (on the same pool) instead of creating it empty.
func (r *ZfsDatasetReconciler) create(ctx context.Context, vol *storagev1alpha1.ZfsDataset, poolName, full string) error {
	if vol.Spec.Source != nil {
		return r.clone(ctx, vol, poolName, full)
	}
	switch vol.Spec.Type {
	case storagev1alpha1.DatasetTypeFilesystem:
		return r.ZFS.CreateDataset(ctx, full, filesystemProps(vol))
	case storagev1alpha1.DatasetTypeVolume:
		if vol.Spec.Volume == nil {
			return fmt.Errorf("spec.volume is required for volume")
		}
		return r.ZFS.CreateZvol(ctx, full, vol.Spec.Volume.Size.Value(), volumeProps(vol))
	default:
		return fmt.Errorf("unknown volume type %q", vol.Spec.Type)
	}
}

// applyRootOwnership sets the provision-time POSIX owner/mode on a freshly
// created filesystem dataset's mountpoint (spec.filesystem.uid/gid/mode). It runs
// exactly once, in the create-absent branch, so it never re-chowns a dataset the
// application has since taken ownership of. Volume/zvol datasets and datasets
// without any ownership field set are a no-op. `zfs create`/`zfs clone`
// auto-mount the dataset, so the mountpoint exists by the time this runs.
func (r *ZfsDatasetReconciler) applyRootOwnership(ctx context.Context, vol *storagev1alpha1.ZfsDataset, baseMountPath, poolName string) error {
	if vol.Spec.Type != storagev1alpha1.DatasetTypeFilesystem {
		return nil
	}
	fs := vol.Spec.Filesystem
	if fs == nil || (fs.UID == nil && fs.GID == nil && fs.Mode == "") {
		return nil
	}
	mountpoint, err := deriveVolumePath(storagev1alpha1.DatasetTypeFilesystem, baseMountPath, poolName, vol.Spec.Dataset)
	if err != nil {
		return err
	}
	return r.ZFS.ApplyOwnership(ctx, mountpoint, fs.UID, fs.GID, fs.Mode)
}

// clone provisions the dataset by cloning a source snapshot or volume. Sizing is
// left to ensureSize (which runs right after) so the clone inherits the origin's
// size and then converges to the requested capacity.
func (r *ZfsDatasetReconciler) clone(ctx context.Context, vol *storagev1alpha1.ZfsDataset, poolName, dest string) error {
	src := vol.Spec.Source
	props := copyProps(vol.Spec.Properties)
	switch {
	case src.Snapshot != "":
		snapFull := poolName + "/" + strings.TrimLeft(src.Snapshot, "/")
		return r.ZFS.Clone(ctx, snapFull, dest, props)
	case src.Volume != "":
		srcFull := poolName + "/" + strings.Trim(src.Volume, "/")
		snapFull := srcFull + "@clone-" + vol.Name
		if err := r.ZFS.Snapshot(ctx, snapFull); err != nil {
			return err
		}
		return r.ZFS.Clone(ctx, snapFull, dest, props)
	default:
		return fmt.Errorf("spec.source has neither snapshot nor volume")
	}
}

// ensureSize converges the on-disk size of an existing object toward the spec:
// filesystem refquota (up or down) and zvol volsize (grow only, never shrink).
// This is what makes volume expansion work end to end.
func (r *ZfsDatasetReconciler) ensureSize(ctx context.Context, vol *storagev1alpha1.ZfsDataset, full string) error {
	switch vol.Spec.Type {
	case storagev1alpha1.DatasetTypeFilesystem:
		desired := "none"
		if cfg := vol.Spec.Filesystem; cfg != nil && cfg.Quota != nil && !cfg.Quota.IsZero() {
			desired = strconv.FormatInt(cfg.Quota.Value(), 10)
		}
		current, err := r.ZFS.Get(ctx, full, "refquota")
		if err != nil {
			return err
		}
		if !refquotaEqual(current, desired) {
			return r.ZFS.SetProperty(ctx, full, "refquota", desired)
		}
		return nil
	case storagev1alpha1.DatasetTypeVolume:
		if vol.Spec.Volume == nil {
			return nil
		}
		desired := alignUp(vol.Spec.Volume.Size.Value(), volblockBytes(vol.Spec.Volume.Volblocksize))
		current, err := r.ZFS.Get(ctx, full, "volsize")
		if err != nil {
			return err
		}
		cur, _ := strconv.ParseInt(strings.TrimSpace(current), 10, 64)
		if desired > cur {
			return r.ZFS.SetProperty(ctx, full, "volsize", strconv.FormatInt(desired, 10))
		}
		return nil
	}
	return nil
}

// refquotaEqual compares a ZFS refquota value against a desired one, treating the
// several "unlimited" spellings ("", "-", "0", "none") as equivalent.
func refquotaEqual(current, desired string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" || s == "-" || s == "0" || s == "none" {
			return "none"
		}
		return s
	}
	return norm(current) == norm(desired)
}

// alignUp rounds size up to the next multiple of block (ZFS requires zvol volsize
// to be a multiple of volblocksize). A non-positive block leaves size unchanged.
func alignUp(size, block int64) int64 {
	if block <= 0 || size <= 0 {
		if size < 0 {
			return 0
		}
		return size
	}
	if r := size % block; r != 0 {
		return size + (block - r)
	}
	return size
}

// volblockBytes parses a ZFS volblocksize string (e.g. "16k", "8192") into bytes,
// defaulting to 16 KiB (the modern OpenZFS zvol default) when empty/unparseable.
func volblockBytes(s string) int64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 16384
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k':
		mult, s = 1024, s[:len(s)-1]
	case 'm':
		mult, s = 1024*1024, s[:len(s)-1]
	case 'g':
		mult, s = 1024*1024*1024, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 16384
	}
	return n * mult
}

// filesystemProps renders the ZFS properties for a filesystem dataset: the user
// properties, plus refquota derived from the filesystem quota when set.
func filesystemProps(vol *storagev1alpha1.ZfsDataset) map[string]string {
	props := copyProps(vol.Spec.Properties)
	if cfg := vol.Spec.Filesystem; cfg != nil && cfg.Quota != nil && !cfg.Quota.IsZero() {
		props["refquota"] = strconv.FormatInt(cfg.Quota.Value(), 10)
	}
	return props
}

// volumeProps renders the ZFS properties for a volume/zvol: the user properties,
// plus volblocksize when set.
func volumeProps(vol *storagev1alpha1.ZfsDataset) map[string]string {
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
func deriveVolumePath(volType storagev1alpha1.DatasetType, baseMountPath, poolName, dataset string) (string, error) {
	ds := strings.Trim(dataset, "/")
	if ds == "" {
		return "", fmt.Errorf("dataset is empty")
	}
	switch volType {
	case storagev1alpha1.DatasetTypeFilesystem:
		if baseMountPath == "" {
			return "", fmt.Errorf("pool baseMountPath is unknown")
		}
		return path.Join(baseMountPath, ds), nil
	case storagev1alpha1.DatasetTypeVolume:
		if poolName == "" {
			return "", fmt.Errorf("pool name is unknown")
		}
		return path.Join("/dev/zvol", poolName, ds), nil
	default:
		return "", fmt.Errorf("unknown volume type %q", volType)
	}
}

// setStatus patches the volume's status subresource.
func (r *ZfsDatasetReconciler) setStatus(ctx context.Context, vol *storagev1alpha1.ZfsDataset, phase storagev1alpha1.ZfsDatasetPhase, volPath, reason, message string) error {
	patched := vol.DeepCopy()
	patched.Status.Phase = phase
	patched.Status.Path = volPath
	patched.Status.ObservedGeneration = vol.Generation
	patched.Status.Message = message

	condStatus := metav1.ConditionTrue
	if phase != storagev1alpha1.DatasetPhaseReady {
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

// volumesForPool maps a ZfsPool event to reconcile requests for every ZfsDataset
// that references its GUID, so volumes are re-driven when a pool moves nodes or
// changes health (e.g. this node takes over hosting).
func (r *ZfsDatasetReconciler) volumesForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*storagev1alpha1.ZfsPool)
	if !ok {
		return nil
	}
	guid := pool.Status.GUID
	if guid == "" {
		guid = strings.TrimPrefix(pool.Name, "zpool-")
	}

	var volumes storagev1alpha1.ZfsDatasetList
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
func (r *ZfsDatasetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsDataset{}).
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.volumesForPool)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfsdataset").
		Complete(r)
}
