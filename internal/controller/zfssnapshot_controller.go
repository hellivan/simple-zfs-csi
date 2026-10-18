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
// objects it looks up are a different kind than the ZfsSnapshot that triggered
// the reconcile, so the two informers have no ordering relationship and a stale
// read could let state be torn down while something still depends on it
// (known-pitfalls.md #19).
func (r *ZfsSnapshotReconciler) gateReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfssnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfssnapshots/status,verbs=get;update;patch
// Reads the source volume to resolve its dataset path; authors no objects.
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsdatasets,verbs=get;list;watch
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

	// A snapshot that has been Ready is never (re-)created (ADR-0034). The phase
	// checks are the one-off migration for objects that predate ProvisionedAt;
	// reconcileSettled backfills them and the checks are removed afterwards
	// (TODO.md).
	if snap.Status.ProvisionedAt != nil || snap.Status.Phase == storagev1alpha1.SnapshotPhaseReady || snap.Status.Phase == storagev1alpha1.SnapshotPhaseLost {
		return r.reconcileSettled(ctx, &snap, &pool, datasetPath)
	}

	full, err := snapshotFullName(pool.Status.PoolName, datasetPath, snap.Spec.SnapshotName)
	if err != nil {
		return ctrl.Result{}, r.setSnapshotStatus(ctx, &snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
			"InvalidSnapshot", err.Error())
	}

	// Idempotent create: only snapshot when it is absent (never for a group member).
	if _, err := r.ZFS.Get(ctx, full, "type"); err != nil {
		if !errors.Is(err, zpool.ErrNotExist) {
			return ctrl.Result{}, r.setSnapshotStatus(ctx, &snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"LookupFailed", err.Error())
		}
		// Both outcomes below are decisive (a new snapshot, or a loud failure), so
		// confirm the source path with a direct read: a lagging cache after a rename
		// would otherwise snapshot or condemn the wrong address.
		if direct, derr := r.sourceDatasetPath(ctx, r.gateReader(), &snap); derr != nil {
			return ctrl.Result{}, derr
		} else if direct != datasetPath {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if snap.Spec.GroupSnapshotID != "" {
			// A group member never takes its own snapshot: a new one would be cut at a
			// different instant than the rest of the group (ADR-0039). Fail loudly.
			return ctrl.Result{}, r.setSnapshotStatus(ctx, &snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"GroupRawSnapshotMissing", fmt.Sprintf("raw snapshot %s of group %q not found; group snapshots are never re-taken", full, snap.Spec.GroupSnapshotID))
		}
		if err := r.ZFS.Snapshot(ctx, full); err != nil {
			return ctrl.Result{}, r.setSnapshotStatus(ctx, &snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"SnapshotFailed", err.Error())
		}
		logger.Info("created ZFS snapshot", "snapshot", full)
	}

	// The raw snapshot is labelled right after it exists. A group member's raw
	// snapshot is taken by the group reconciler in one atomic call, so this is
	// also where it gets its owner.
	if err := zpool.EnsureLabels(ctx, r.ZFS, full, zpool.RoleRaw, string(snap.UID)); err != nil {
		return ctrl.Result{}, err
	}

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
	return resolveDatasetPath(ctx, reader, snap.Spec.SourceVolume, snap.Spec.Dataset)
}

// resolveDatasetPath returns the source ZfsDataset's current dataset path, or
// the recorded one when sourceVolume is empty or no longer exists.
func resolveDatasetPath(ctx context.Context, reader client.Reader, sourceVolume, recorded string) (string, error) {
	if sourceVolume == "" {
		return recorded, nil
	}
	src := &storagev1alpha1.ZfsDataset{}
	err := reader.Get(ctx, client.ObjectKey{Name: sourceVolume}, src)
	switch {
	case apierrors.IsNotFound(err):
		return recorded, nil
	case err != nil:
		return "", err
	}
	return src.Spec.Dataset, nil
}

// reconcileDelete tears down a ZfsSnapshot on the node hosting its pool.
//
// The raw snapshot is marked for deferred destruction first, then the backing
// clone is emptied and destroyed (see the steps below, strategy D).
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

	backingDataset := path.Join(path.Dir(strings.Trim(datasetPath, "/")), snap.Spec.SnapshotName)
	backingFull, err := datasetName(pool.Status.PoolName, backingDataset)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Strategy D (P2/P3): this object created the raw snapshot, so it defers its
	// destruction with `zfs destroy -d`: gone at once when nothing clones it,
	// otherwise ZFS removes it when the last clone (the backing clone, live
	// restores) is gone. Nothing is ever promoted onto a restore to get rid of
	// the snapshot, and no restore can end up pinning it forever.
	//
	// Where the snapshot is is not where this object recorded it: a promote
	// relocates it and nothing rewrites Spec.Dataset (ADR-0028). The backing
	// clone answers that: its origin, or one of its own snapshots.
	if err := r.releaseRaw(ctx, snap, backingFull, rawFull); err != nil {
		return ctrl.Result{}, err
	}

	// The backing clone is a vessel with no snapshots of its own, unless a promote
	// moved the raw snapshot (and older ones) onto it. The same loop as for a
	// volume empties it.
	if err := detachAndCleanSnapshots(ctx, r.gateReader(), r.ZFS, snap.Spec.PoolGUID, pool.Status.PoolName, backingFull); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ZFS.Destroy(ctx, backingFull, false); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("destroyed backing clone", "dataset", backingFull)
	return ctrl.Result{}, r.releaseSnapshotFinalizer(ctx, snap)
}

// releaseRaw marks the raw snapshot for deferred destruction, before anything is
// promoted. While the backing clone exists the snapshot is its origin or one of
// its own snapshots. Not finding it means an earlier attempt of this delete got
// past this step (the promote loop then moved or destroyed it) or the snapshot
// was never created, so the delete resumes. What still fails loudly is the
// destroy of the backing clone, which ZFS refuses while it has snapshots or
// dependent clones.
func (r *ZfsSnapshotReconciler) releaseRaw(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot, backingFull, rawFull string) error {
	raw, err := locateRawSnapshot(ctx, r.ZFS, backingFull, rawFull, snap.Spec.SnapshotName)
	if err != nil || raw == "" {
		return err
	}
	if raw != rawFull {
		log.FromContext(ctx).Info("raw snapshot was relocated by an earlier promote; destroying it where it actually is",
			"recorded", rawFull, "actual", raw)
	}
	if err := r.ZFS.DestroySnapshotDeferred(ctx, raw); err != nil {
		return err
	}
	log.FromContext(ctx).Info("destroyed ZFS snapshot (deferred)", "snapshot", raw)
	return nil
}

// reconcileSettled handles a snapshot that already reached Ready (or Lost).
//
// Unlike a ZfsDataset (ADR-0026), a snapshot is not a standing declaration: it
// records one instant, and there is no supported way to reset it. If its
// primitives vanish, re-creating them would fabricate a different instant under
// the same identity — and, for a group member, silently break the guarantee
// that every member was cut at the same time. So this only observes and records
// (ADR-0034): it never creates anything. Missing on disk means Lost; present
// again means Ready. The recorded creation time is kept (ADR-0038).
//
// Only the raw snapshot is checked: restores clone it, and it is what the
// backing clone's origin points at.
func (r *ZfsSnapshotReconciler) reconcileSettled(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot, pool *storagev1alpha1.ZfsPool, datasetPath string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	backingFull := backingCloneFullName(pool.Status.PoolName, datasetPath, snap.Spec.SnapshotName)
	recorded, _ := snapshotFullName(pool.Status.PoolName, datasetPath, snap.Spec.SnapshotName)
	raw, err := locateRawSnapshot(ctx, r.ZFS, backingFull, recorded, snap.Spec.SnapshotName)
	if err != nil {
		return ctrl.Result{}, err
	}
	present := raw != ""
	switch {
	case present && snap.Status.Phase == storagev1alpha1.SnapshotPhaseReady && snap.Status.ProvisionedAt != nil:
		return ctrl.Result{}, nil
	case present:
		logger.Info("lost snapshot's raw snapshot is back", "snapshot", raw)
		// creationTime is recorded once (ADR-0038) and never re-read. Only an
		// object that never recorded one (legacy) reads it now.
		var creation *metav1.Time
		if snap.Status.CreationTime == nil {
			creation = snapshotCreationTime(ctx, r.ZFS, raw)
		}
		message := fmt.Sprintf("raw snapshot %s is present again", raw)
		if snap.Status.Phase == storagev1alpha1.SnapshotPhaseReady {
			message = snap.Status.Message // backfilling ProvisionedAt only
		}
		return ctrl.Result{}, r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseReady, true, creation, nil,
			"Ready", message)
	case snap.Status.Phase == storagev1alpha1.SnapshotPhaseLost:
		return ctrl.Result{RequeueAfter: lostRecheckInterval}, nil
	}

	logger.Info("ready snapshot lost its raw snapshot; not re-creating", "snapshot", snap.Spec.SnapshotName)
	if err := r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseLost, false, nil, nil,
		"PrimitivesLost", fmt.Sprintf("raw snapshot %q no longer exists; a snapshot is never re-created in place", snap.Spec.SnapshotName)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: lostRecheckInterval}, nil
}

// lostRecheckInterval is how often a Lost snapshot is re-observed, so a
// primitive that comes back (e.g. a dataset moved back) is noticed.
const lostRecheckInterval = time.Minute

// reconcileBackingClone clones the raw snapshot into a flat sibling of the
// source dataset named after Spec.SnapshotName. The clone exists only to pin the
// raw snapshot (it is its origin) and to tell where it is; it takes no snapshot
// of its own. Restores clone the raw snapshot directly (P4).
func (r *ZfsSnapshotReconciler) reconcileBackingClone(ctx context.Context, snap *storagev1alpha1.ZfsSnapshot, pool *storagev1alpha1.ZfsPool, datasetPath, rawFull string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	backingName := snap.Spec.SnapshotName
	backingDataset := path.Join(path.Dir(strings.Trim(datasetPath, "/")), backingName)
	backingFull, err := datasetName(pool.Status.PoolName, backingDataset)
	if err != nil {
		return ctrl.Result{}, r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
			"InvalidBackingClone", err.Error())
	}

	sourceType := snap.Spec.SourceType
	if sourceType == "" {
		// Legacy fallback only: SourceType is always set for snapshots created
		// after ADR-0017. Default to filesystem rather than fail outright.
		sourceType = storagev1alpha1.DatasetTypeFilesystem
	}

	if _, err := r.ZFS.Get(ctx, backingFull, "type"); err != nil {
		if !errors.Is(err, zpool.ErrNotExist) {
			return ctrl.Result{}, r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"LookupFailed", err.Error())
		}
		if err := r.ZFS.Clone(ctx, rawFull, backingFull, zpool.MergeLabels(backingCloneProperties(sourceType), zpool.RoleBacking, string(snap.UID))); err != nil {
			return ctrl.Result{}, r.setSnapshotStatus(ctx, snap, storagev1alpha1.SnapshotPhaseError, false, nil, nil,
				"BackingCloneFailed", err.Error())
		}
		logger.Info("created backing clone", "dataset", backingFull, "origin", rawFull)
	}

	if err := zpool.EnsureLabels(ctx, r.ZFS, backingFull, zpool.RoleBacking, string(snap.UID)); err != nil {
		return ctrl.Result{}, err
	}

	creation := snapshotCreationTime(ctx, r.ZFS, rawFull)
	restore := snapshotRestoreSize(ctx, r.ZFS, rawFull)
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
	if phase == storagev1alpha1.SnapshotPhaseReady && patched.Status.ProvisionedAt == nil {
		patched.Status.ProvisionedAt = firstReadyTime(patched.Status.Conditions)
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
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.snapshotsForPool), poolChanged()).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfssnapshot").
		Complete(r)
}

// firstReadyTime is the value for a new ProvisionedAt. An object that is Ready
// but predates the field is backfilled from its Ready condition's transition
// time, which for a continuously Ready object is the real first-Ready time; in
// every other case it is now.
func firstReadyTime(conds []metav1.Condition) *metav1.Time {
	if c := meta.FindStatusCondition(conds, "Ready"); c != nil && c.Status == metav1.ConditionTrue && !c.LastTransitionTime.IsZero() {
		t := c.LastTransitionTime
		return &t
	}
	now := metav1.Now()
	return &now
}
