package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

// zfsGroupSnapshotFinalizer lets the group delete its member ZfsSnapshots (and,
// before provisioning finished, any leftover raw snapshot) before it goes away.
const zfsGroupSnapshotFinalizer = "storage.simple-zfs-csi.io/zfsgroupsnapshot"

// groupRequeue is how soon a group that is waiting on something outside this
// reconciler (a missing source, children being deleted) looks again.
const groupRequeue = 5 * time.Second

// ZfsGroupSnapshotReconciler is the per-node agent that fulfils ZfsGroupSnapshot
// requests (ADR-0039). On the node hosting the pool it takes ONE atomic
// multi-name `zfs snapshot` over every member, records the creation time once,
// and creates the member ZfsSnapshots, which adopt the raw snapshots. Once
// every member has been Ready (provisionedAt) nothing is created again
// (ADR-0038); Ready/Lost is then derived from the members and only observed.
type ZfsGroupSnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NodeName is the node this agent runs on.
	NodeName string
	// ZFS performs the atomic snapshot on the host.
	ZFS zpool.ZFS
	// APIReader bypasses the informer cache for the delete path's decisions.
	APIReader client.Reader
}

func (r *ZfsGroupSnapshotReconciler) gateReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsgroupsnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsgroupsnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsgroupsnapshots/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfssnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsdatasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspools,verbs=get;list;watch

// Reconcile drives one ZfsGroupSnapshot, but only on the node hosting its pool.
func (r *ZfsGroupSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var grp storagev1alpha1.ZfsGroupSnapshot
	if err := r.Get(ctx, req.NamespacedName, &grp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var pool storagev1alpha1.ZfsPool
	poolErr := r.Get(ctx, client.ObjectKey{Name: zpool.ResourceName(grp.Spec.PoolGUID)}, &pool)
	if poolErr != nil && !apierrors.IsNotFound(poolErr) {
		return ctrl.Result{}, poolErr
	}
	poolFound := poolErr == nil
	hostedHere := poolFound &&
		pool.Status.CurrentNode == r.NodeName &&
		pool.Status.Health != storagev1alpha1.PoolHealthNodeOffline

	if !grp.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&grp, zfsGroupSnapshotFinalizer) {
			return ctrl.Result{}, nil
		}
		switch {
		case hostedHere, !poolFound:
			return r.reconcileDelete(ctx, &grp, &pool, hostedHere)
		default:
			return ctrl.Result{}, nil // the hosting node's agent handles it
		}
	}

	if !hostedHere {
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&grp, zfsGroupSnapshotFinalizer) {
		controllerutil.AddFinalizer(&grp, zfsGroupSnapshotFinalizer)
		return ctrl.Result{}, r.Update(ctx, &grp)
	}

	// After provisionedAt nothing is created or re-read: the group only observes
	// its members (ADR-0038).
	if grp.Status.ProvisionedAt != nil {
		return r.observe(ctx, &grp)
	}
	return r.provision(ctx, &grp, &pool)
}

// memberRaw returns the full name of a member's raw snapshot and of its source
// dataset, resolving the source's current path (it may have been renamed).
func (r *ZfsGroupSnapshotReconciler) memberRaw(ctx context.Context, reader client.Reader, poolName string, m *storagev1alpha1.ZfsGroupSnapshotMember) (rawFull, datasetFull string, err error) {
	path, err := resolveDatasetPath(ctx, reader, m.SourceVolume, m.Dataset)
	if err != nil {
		return "", "", err
	}
	if rawFull, err = snapshotFullName(poolName, path, m.SnapshotName); err != nil {
		return "", "", err
	}
	if datasetFull, err = datasetName(poolName, path); err != nil {
		return "", "", err
	}
	return rawFull, datasetFull, nil
}

func (r *ZfsGroupSnapshotReconciler) exists(ctx context.Context, name string) (bool, error) {
	if _, err := r.ZFS.Get(ctx, name, "type"); err != nil {
		if errors.Is(err, zpool.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// provision runs the one-time steps, in this order: three-way check of the raw
// snapshots, one atomic exec, read creationTime once, create the children.
// Children are created only after the exec succeeded: a child never takes its
// own snapshot.
func (r *ZfsGroupSnapshotReconciler) provision(ctx context.Context, grp *storagev1alpha1.ZfsGroupSnapshot, pool *storagev1alpha1.ZfsPool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	poolName := pool.Status.PoolName

	// Everything below creates something, so the cached copy that routed us here
	// is not good enough: a lagging cache could still show "not provisioned" or
	// "not deleting" and make us re-create a frozen group's children or snapshot
	// for a group that is being deleted. A stale view just retries.
	fresh := &storagev1alpha1.ZfsGroupSnapshot{}
	if err := r.gateReader().Get(ctx, client.ObjectKey{Name: grp.Name}, fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !fresh.DeletionTimestamp.IsZero() || fresh.Status.ProvisionedAt != nil {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	grp.Status = fresh.Status

	raws := make([]string, len(grp.Spec.Members))
	datasets := make([]string, len(grp.Spec.Members))
	present := 0
	for i := range grp.Spec.Members {
		raw, ds, err := r.memberRaw(ctx, r.Client, poolName, &grp.Spec.Members[i])
		if err != nil {
			return ctrl.Result{}, r.setStatus(ctx, grp, storagev1alpha1.GroupSnapshotPhaseError, "InvalidGroupSnapshot", err.Error())
		}
		raws[i], datasets[i] = raw, ds
		ok, err := r.exists(ctx, raw)
		if err != nil {
			return ctrl.Result{}, err // transient: retried with backoff
		}
		if ok {
			present++
		}
	}

	// Nothing has been taken yet: every member needs a live source. A missing or
	// terminating source leaves the group unfulfilled until it is back
	// (ADR-0038); it is never snapshotted around.
	if present == 0 {
		for i := range grp.Spec.Members {
			msg, err := r.sourceBlocker(ctx, &grp.Spec.Members[i], datasets[i])
			if err != nil {
				return ctrl.Result{}, err
			}
			if msg != "" {
				return ctrl.Result{RequeueAfter: groupRequeue}, r.setStatus(ctx, grp, storagev1alpha1.GroupSnapshotPhasePending, "SourceUnavailable", msg)
			}
		}
	}

	switch {
	case present == len(raws):
		// All raw snapshots exist already (a previous pass, or a retry): adopt.
	case present == 0:
		if err := r.ZFS.Snapshot(ctx, raws...); err != nil {
			if errors.Is(err, zpool.ErrSnapshotsPartiallyExist) {
				return ctrl.Result{}, r.setStatus(ctx, grp, storagev1alpha1.GroupSnapshotPhaseError, "PartialRawSnapshots", err.Error())
			}
			// Nothing was taken (zfs snapshot is all-or-nothing): keep the group
			// Pending, show why, and retry with backoff.
			if serr := r.setStatus(ctx, grp, storagev1alpha1.GroupSnapshotPhasePending, "SnapshotFailed", err.Error()); serr != nil {
				logger.Error(serr, "record snapshot failure")
			}
			return ctrl.Result{}, err
		}
		logger.Info("created atomic group snapshot", "members", len(raws), "first", raws[0])
	default:
		// Never fill the gap: a member cut later would not share the instant.
		return ctrl.Result{}, r.setStatus(ctx, grp, storagev1alpha1.GroupSnapshotPhaseError, "PartialRawSnapshots",
			fmt.Sprintf("%d of %d raw snapshots exist; refusing to take the rest at a different instant", present, len(raws)))
	}

	var creation *metav1.Time
	if grp.Status.CreationTime == nil {
		creation = snapshotCreationTime(ctx, r.ZFS, raws[0])
		if err := r.patchStatus(ctx, grp, func(st *storagev1alpha1.ZfsGroupSnapshotStatus) { st.CreationTime = creation }); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureChildren(ctx, grp); err != nil {
		return ctrl.Result{}, err
	}
	return r.derive(ctx, grp, true)
}

// sourceBlocker explains why a member's source cannot be snapshotted now, or "".
func (r *ZfsGroupSnapshotReconciler) sourceBlocker(ctx context.Context, m *storagev1alpha1.ZfsGroupSnapshotMember, datasetFull string) (string, error) {
	src := &storagev1alpha1.ZfsDataset{}
	switch err := r.Get(ctx, client.ObjectKey{Name: m.SourceVolume}, src); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return "", err
	case !src.DeletionTimestamp.IsZero():
		return fmt.Sprintf("source volume %q is being deleted", m.SourceVolume), nil
	}
	ok, err := r.exists(ctx, datasetFull)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("source dataset %s of volume %q does not exist", datasetFull, m.SourceVolume), nil
	}
	return "", nil
}

// ensureChildren creates the ZfsSnapshot of every member that has none. It runs
// only after the atomic exec and only before provisionedAt.
func (r *ZfsGroupSnapshotReconciler) ensureChildren(ctx context.Context, grp *storagev1alpha1.ZfsGroupSnapshot) error {
	for i := range grp.Spec.Members {
		m := &grp.Spec.Members[i]
		child := &storagev1alpha1.ZfsSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: m.ChildSnapshotName},
			Spec: storagev1alpha1.ZfsSnapshotSpec{
				PoolGUID:           grp.Spec.PoolGUID,
				Dataset:            m.Dataset,
				SnapshotName:       m.SnapshotName,
				SourceVolume:       m.SourceVolume,
				SourceType:         m.SourceType,
				SourceFSType:       m.SourceFSType,
				SourceVolblocksize: m.SourceVolblocksize,
				SourceProperties:   m.SourceProperties,
				GroupSnapshotID:    grp.Name,
			},
		}
		if err := r.Create(ctx, child); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create member ZfsSnapshot %q: %w", m.ChildSnapshotName, err)
		}
	}
	return nil
}

// observe records Ready/Lost from the members and never acts.
func (r *ZfsGroupSnapshotReconciler) observe(ctx context.Context, grp *storagev1alpha1.ZfsGroupSnapshot) (ctrl.Result, error) {
	return r.derive(ctx, grp, false)
}

// derive computes the group phase from the member objects (cache, no ZFS call).
func (r *ZfsGroupSnapshotReconciler) derive(ctx context.Context, grp *storagev1alpha1.ZfsGroupSnapshot, provisioning bool) (ctrl.Result, error) {
	var missing, notReady, failed []string
	for _, m := range grp.Spec.Members {
		child := &storagev1alpha1.ZfsSnapshot{}
		if err := r.Get(ctx, client.ObjectKey{Name: m.ChildSnapshotName}, child); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, m.ChildSnapshotName)
				continue
			}
			return ctrl.Result{}, err
		}
		switch {
		case child.Status.Phase == storagev1alpha1.SnapshotPhaseError:
			failed = append(failed, fmt.Sprintf("%s: %s", child.Name, child.Status.Message))
		case !(child.Status.Phase == storagev1alpha1.SnapshotPhaseReady && child.Status.ReadyToUse):
			notReady = append(notReady, child.Name)
		}
	}

	switch {
	case len(failed) > 0 && provisioning:
		return ctrl.Result{}, r.setStatus(ctx, grp, storagev1alpha1.GroupSnapshotPhaseError, "MemberFailed", strings.Join(failed, "; "))
	case len(missing)+len(notReady)+len(failed) == 0:
		return ctrl.Result{}, r.setStatus(ctx, grp, storagev1alpha1.GroupSnapshotPhaseReady, "Ready",
			fmt.Sprintf("all %d member snapshots are ready", len(grp.Spec.Members)))
	case provisioning:
		return ctrl.Result{RequeueAfter: groupRequeue}, r.setStatus(ctx, grp, storagev1alpha1.GroupSnapshotPhasePending, "WaitingForMembers",
			fmt.Sprintf("waiting for member snapshots: %s", strings.Join(append(append([]string{}, missing...), notReady...), ", ")))
	default:
		all := append(append(append([]string{}, missing...), notReady...), failed...)
		return ctrl.Result{}, r.setStatus(ctx, grp, storagev1alpha1.GroupSnapshotPhaseLost, "MembersLost",
			fmt.Sprintf("member snapshots missing or not ready: %s; nothing is re-created", strings.Join(all, ", ")))
	}
}

// reconcileDelete deletes every member from Spec.Members, waits until they are
// gone, and releases the finalizer. Before provisionedAt a member with no child
// also has its leftover raw snapshot destroyed (nothing clones it yet).
func (r *ZfsGroupSnapshotReconciler) reconcileDelete(ctx context.Context, grp *storagev1alpha1.ZfsGroupSnapshot, pool *storagev1alpha1.ZfsPool, hostedHere bool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	// provisionedAt decides whether an unclaimed raw snapshot may be destroyed, so
	// read it directly rather than from a possibly lagging cache.
	fresh := &storagev1alpha1.ZfsGroupSnapshot{}
	if err := r.gateReader().Get(ctx, client.ObjectKey{Name: grp.Name}, fresh); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	grp = fresh
	remaining := 0
	for i := range grp.Spec.Members {
		m := &grp.Spec.Members[i]
		child := &storagev1alpha1.ZfsSnapshot{}
		err := r.gateReader().Get(ctx, client.ObjectKey{Name: m.ChildSnapshotName}, child)
		switch {
		case err == nil:
			remaining++
			if child.DeletionTimestamp.IsZero() {
				if derr := r.Delete(ctx, child); derr != nil && !apierrors.IsNotFound(derr) {
					return ctrl.Result{}, derr
				}
			}
		case apierrors.IsNotFound(err):
			if grp.Status.ProvisionedAt != nil || !hostedHere {
				continue
			}
			raw, _, rerr := r.memberRaw(ctx, r.gateReader(), pool.Status.PoolName, m)
			if rerr != nil {
				return ctrl.Result{}, rerr
			}
			if derr := r.ZFS.Destroy(ctx, raw, false); derr != nil {
				return ctrl.Result{}, derr
			}
			logger.Info("destroyed unclaimed raw snapshot of group member", "snapshot", raw)
		default:
			return ctrl.Result{}, err
		}
	}
	if remaining > 0 {
		return ctrl.Result{RequeueAfter: groupRequeue}, nil
	}
	return ctrl.Result{}, r.releaseFinalizer(ctx, grp)
}

func (r *ZfsGroupSnapshotReconciler) releaseFinalizer(ctx context.Context, grp *storagev1alpha1.ZfsGroupSnapshot) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.ZfsGroupSnapshot{}
		if err := r.Get(ctx, client.ObjectKey{Name: grp.Name}, cur); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !controllerutil.RemoveFinalizer(cur, zfsGroupSnapshotFinalizer) {
			return nil
		}
		return r.Update(ctx, cur)
	})
}

// setStatus records a phase. ProvisionedAt is stamped on the first Ready and
// never changed (ADR-0038); CreationTime is not touched here.
func (r *ZfsGroupSnapshotReconciler) setStatus(ctx context.Context, grp *storagev1alpha1.ZfsGroupSnapshot, phase storagev1alpha1.ZfsGroupSnapshotPhase, reason, message string) error {
	return r.patchStatus(ctx, grp, func(st *storagev1alpha1.ZfsGroupSnapshotStatus) {
		st.Phase = phase
		st.ReadyToUse = phase == storagev1alpha1.GroupSnapshotPhaseReady
		st.ObservedGeneration = grp.Generation
		st.Message = message
		if phase == storagev1alpha1.GroupSnapshotPhaseReady && st.ProvisionedAt == nil {
			now := metav1.Now()
			st.ProvisionedAt = &now
		}
		status := metav1.ConditionTrue
		if phase != storagev1alpha1.GroupSnapshotPhaseReady {
			status = metav1.ConditionFalse
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: "Ready", Status: status, Reason: reason, Message: message, ObservedGeneration: grp.Generation,
		})
	})
}

// patchStatus applies mutate to a copy of the status and patches only when it
// changed, so steady-state reconciles cost no API write.
func (r *ZfsGroupSnapshotReconciler) patchStatus(ctx context.Context, grp *storagev1alpha1.ZfsGroupSnapshot, mutate func(*storagev1alpha1.ZfsGroupSnapshotStatus)) error {
	patched := grp.DeepCopy()
	mutate(&patched.Status)
	if equality.Semantic.DeepEqual(grp.Status, patched.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(grp)); err != nil {
		return err
	}
	grp.Status = patched.Status
	return nil
}

// groupsForPool maps a ZfsPool event to its groups.
func (r *ZfsGroupSnapshotReconciler) groupsForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*storagev1alpha1.ZfsPool)
	if !ok {
		return nil
	}
	guid := pool.Status.GUID
	if guid == "" {
		guid = strings.TrimPrefix(pool.Name, "zpool-")
	}
	var list storagev1alpha1.ZfsGroupSnapshotList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.PoolGUID == guid {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// groupForMember maps a member ZfsSnapshot event to its group.
func (r *ZfsGroupSnapshotReconciler) groupForMember(_ context.Context, obj client.Object) []reconcile.Request {
	snap, ok := obj.(*storagev1alpha1.ZfsSnapshot)
	if !ok || snap.Spec.GroupSnapshotID == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: snap.Spec.GroupSnapshotID}}}
}

// SetupWithManager wires the reconciler into the manager.
func (r *ZfsGroupSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsGroupSnapshot{}).
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.groupsForPool), poolChanged()).
		Watches(&storagev1alpha1.ZfsSnapshot{}, handler.EnqueueRequestsFromMapFunc(r.groupForMember)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfsgroupsnapshot").
		Complete(r)
}
