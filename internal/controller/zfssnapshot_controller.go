package controller

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
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
	// APIReader reads straight from the API server, bypassing the manager's
	// informer cache. SetupWithManager wires it; it is used only by the delete
	// path's gating reads (see gateReader).
	APIReader client.Reader
}

// gateReader returns the reader used for decisions that guard an irreversible
// `zfs destroy`, for the same reason as ZfsDatasetReconciler.gateReader: the
// backing-clone ZfsDataset it looks up is a different kind than the ZfsSnapshot
// that triggered the reconcile, so the two informers have no ordering
// relationship and a stale NotFound would let the raw origin snapshot be torn
// down while its backing clone is still live (known-pitfalls.md #19).
func (r *ZfsSnapshotReconciler) gateReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfssnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfssnapshots/status,verbs=get;update;patch
// Every snapshot is backed by a real, owned child ZfsDataset (D15), so this
// reconciler — unlike ZfsDatasetReconciler, which only ever fulfils objects the
// csi-controller authored — creates and deletes ZfsDataset objects itself (D26).
// The finalizers rule covers the backing clone's ownerReference, which sets
// blockOwnerDeletion: true.
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsdatasets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfssnapshots/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspools,verbs=get;list;watch

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
			return r.reconcileDelete(ctx, &snap, &pool)
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

	datasetPath, err := r.sourceDatasetPath(ctx, r.Client, &snap)
	if err != nil {
		return ctrl.Result{}, err
	}

	full, err := snapshotFullName(pool.Status.PoolName, datasetPath, snap.Spec.SnapshotName)
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

	// Provision/await the owned backing-clone ZfsDataset, then take its
	// "@restore-source" self-snapshot (D5) — restores always clone from that,
	// never from the raw snapshot above directly (D0/§3.1).
	return r.reconcileBackingClone(ctx, &snap, &pool, datasetPath, full)
}

// releaseSnapshotFinalizer removes the agent finalizer, allowing deletion. It
// re-reads the object under conflict retry rather than updating a possibly
// stale in-memory copy: the delete path calls helpers that may themselves write
// this object, and a plain Update on the stale copy fails with "object was
// modified" (known-pitfalls.md class 17). Tolerates the object already being
// gone.
func (r *ZfsSnapshotReconciler) releaseSnapshotFinalizer(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.ZfsSnapshot{}
		if err := r.Get(ctx, client.ObjectKey{Name: snap.Name}, cur); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if !controllerutil.RemoveFinalizer(cur, zfsSnapshotFinalizer) {
			return nil
		}
		return r.Update(ctx, cur)
	})
}

// sourceDatasetPath returns the dataset path this snapshot's raw ZFS snapshot
// lives on.
//
// Spec.Dataset is a *record* taken at CreateSnapshot time, not a pointer: the
// authoritative answer, while the source still exists, is that ZfsDataset's own
// Spec.Dataset. Location fields are deliberately mutable (api-conventions §5) —
// an operator may `zfs rename` a dataset and repoint its CR, and the ZFS
// snapshot moves with its dataset, but the copy recorded here cannot follow.
// Trusting the copy is the same mistake as trusting the CSI volume_context
// (ADR-0022), one layer down.
//
// Snapshots deliberately outlive their source (D15), so the recorded copy
// remains the fallback for when SourceVolume is unset (snapshots predating that
// field) or the source object is already gone.
func (r *ZfsSnapshotReconciler) sourceDatasetPath(ctx context.Context, reader client.Reader, snap *storagev1alpha1.ZfsSnapshot) (string, error) {
	if snap.Spec.SourceVolume == "" {
		return snap.Spec.Dataset, nil
	}
	src := &storagev1alpha1.ZfsDataset{}
	err := reader.Get(ctx, client.ObjectKey{Name: snap.Spec.SourceVolume}, src)
	switch {
	case apierrors.IsNotFound(err):
		return snap.Spec.Dataset, nil
	case err != nil:
		return "", err
	}
	return src.Spec.Dataset, nil
}

// reconcileDelete tears down a ZfsSnapshot on the node hosting its pool.
//
// All promote/dependent-chaining complexity is delegated to
// ZfsDatasetReconciler: delete the owned backing-clone ZfsDataset, wait for it
// to be fully gone, then perform the required (not best-effort)
// raw-origin-snapshot cleanup. The order matters because the backing clone is
// itself a dependent clone of the raw snapshot, so the raw snapshot cannot be
// destroyed while it exists (D11) — though D19's detachSnapshotClones below
// also handles a dependent that appeared some other way.
func (r *ZfsSnapshotReconciler) reconcileDelete(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot, pool *storagev1alpha1.ZfsPool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	datasetPath, err := r.sourceDatasetPath(ctx, r.gateReader(), snap)
	if err != nil {
		return ctrl.Result{}, err
	}
	rawFull, err := snapshotFullName(pool.Status.PoolName, datasetPath, snap.Spec.SnapshotName)
	if err != nil {
		return ctrl.Result{}, err
	}

	backing := &storagev1alpha1.ZfsDataset{}
	getErr := r.gateReader().Get(ctx, client.ObjectKey{Name: snap.Spec.SnapshotName}, backing)
	switch {
	case apierrors.IsNotFound(getErr):
		// Backing clone is fully gone; fall through to the raw-origin cleanup.
	case getErr != nil:
		return ctrl.Result{}, getErr
	default:
		if backing.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, backing); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		// ZfsDatasetReconciler owns all promote/dependent-chaining complexity
		// for the backing clone (D3/D7/D9/D12/D13); poll until it's confirmed
		// gone rather than adding a second watch/queueing path here.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Where the raw snapshot *actually* is, which need not be where this object
	// recorded it: `zfs promote` relocates the origin snapshot and everything
	// older than it onto the promoted clone, and nothing rewrites Spec.Dataset
	// when that happens. Trusting the record here used to leave the snapshot
	// behind — Destroy is NotExist-tolerant, so a stale address silently no-ops
	// and the finalizer is released anyway, orphaning the real snapshot on
	// whatever dataset now holds it (ADR-0028).
	//
	// Deliberately done after the backing-clone wait above, so the pool-wide
	// listing is paid once at the end rather than on every requeue.
	if found, findErr := r.ZFS.FindSnapshot(ctx, pool.Status.PoolName, snap.Spec.SnapshotName); findErr != nil {
		return ctrl.Result{}, findErr
	} else if found != "" && found != rawFull {
		logger.Info("raw snapshot was relocated by an earlier promote; destroying it where it actually is",
			"recorded", rawFull, "actual", found)
		rawFull = found
	}

	// Required, not best-effort (D11): a no-op (Destroy is NotExist-tolerant)
	// when the snapshot is genuinely gone already — real work in the common case
	// (a snapshot created and later deleted). This is the invariant that makes it
	// safe for DeleteVolume to assume zero live ZfsSnapshots referencing a volume
	// means zero raw snapshots remain on it either.
	//
	// D19: a restored PVC that was promoted while the backing clone above was
	// being torn down is now a clone of this raw snapshot, and ZFS refuses to
	// destroy a snapshot that still has dependent clones. Promote those away
	// first — that relocates the snapshot onto the dependent and turns the
	// destroy into a NotExist no-op — rather than deadlocking here forever.
	if err := detachSnapshotClones(ctx, r.gateReader(), r.ZFS, snap.Spec.PoolGUID, pool.Status.PoolName, rawFull); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ZFS.Destroy(ctx, rawFull, false); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("destroyed ZFS snapshot", "snapshot", rawFull)
	return ctrl.Result{}, r.releaseSnapshotFinalizer(ctx, snap)
}

// reconcileBackingClone provisions (D15) the owned backing-clone ZfsDataset
// for a snapshot — a flat sibling of the source dataset named
// after Spec.SnapshotName (already an independent, opaque identifier per
// independent-resource-naming-redesign.md, so it needs no extra prefixing here,
// D1/D1a) — waits for it to become Ready, then takes its fixed-name
// "@restore-source" self-snapshot (D5). Restores always clone from that, never
// from the raw snapshot directly (D0/§3.1), so they keep working regardless of
// what later happens to the true source volume.
func (r *ZfsSnapshotReconciler) reconcileBackingClone(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot, pool *storagev1alpha1.ZfsPool, datasetPath, rawFull string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	backingName := snap.Spec.SnapshotName
	backingDataset := path.Join(path.Dir(strings.Trim(datasetPath, "/")), backingName)

	backing := &storagev1alpha1.ZfsDataset{}
	err := r.Get(ctx, client.ObjectKey{Name: backingName}, backing)
	switch {
	case apierrors.IsNotFound(err):
		sourceType := snap.Spec.SourceType
		if sourceType == "" {
			// Legacy fallback only: SourceType is always set for snapshots created
			// after ADR-0017. Default to filesystem rather than fail outright.
			sourceType = storagev1alpha1.DatasetTypeFilesystem
		}
		desired := &storagev1alpha1.ZfsDataset{
			ObjectMeta: metav1.ObjectMeta{
				Name: backingName,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(snap, storagev1alpha1.GroupVersion.WithKind("ZfsSnapshot")),
				},
			},
			Spec: storagev1alpha1.ZfsDatasetSpec{
				PoolGUID:   snap.Spec.PoolGUID,
				Dataset:    backingDataset,
				Type:       sourceType,
				Properties: backingCloneProperties(sourceType),
				Source:     &storagev1alpha1.DatasetSource{Snapshot: datasetPath + "@" + snap.Spec.SnapshotName},
			},
		}
		if createErr := r.Create(ctx, desired); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return ctrl.Result{}, r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"BackingCloneCreateFailed", createErr.Error())
		}
		return ctrl.Result{Requeue: true}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	if backing.Status.Phase != storagev1alpha1.DatasetPhaseReady {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	backingFull, err := datasetName(pool.Status.PoolName, backing.Spec.Dataset)
	if err != nil {
		return ctrl.Result{}, r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
			"InvalidBackingClone", err.Error())
	}
	restoreSourceFull := backingFull + "@" + restoreSourceSnapshotName
	if _, err := r.ZFS.Get(ctx, restoreSourceFull, "type"); err != nil {
		if !errors.Is(err, zpool.ErrNotExist) {
			return ctrl.Result{}, r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"LookupFailed", err.Error())
		}
		if err := r.ZFS.Snapshot(ctx, restoreSourceFull); err != nil {
			return ctrl.Result{}, r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"SelfSnapshotFailed", err.Error())
		}
		logger.Info("created backing-clone self-snapshot", "snapshot", restoreSourceFull)
	}

	creation := snapshotCreationTime(ctx, r.ZFS, restoreSourceFull)
	restore := snapshotRestoreSize(ctx, r.ZFS, restoreSourceFull)
	return ctrl.Result{}, r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseReady, true, creation, restore,
		"Ready", fmt.Sprintf("snapshot %s ready on %s (backing clone %s)", rawFull, r.NodeName, backingFull))
}

// backingCloneProperties suppresses auto-mounting/block-device exposure for a
// backing clone (§2.8): it's never CSI-visible/mountable, so
// there's no reason for the agent to mount it (filesystem) or expose a device
// node for it (zvol).
func backingCloneProperties(t storagev1alpha1.DatasetType) map[string]string {
	if t == storagev1alpha1.DatasetTypeVolume {
		return map[string]string{"volmode": "none"}
	}
	return map[string]string{"canmount": "off"}
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
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsSnapshot{}).
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.snapshotsForPool)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfssnapshot").
		Complete(r)
}
