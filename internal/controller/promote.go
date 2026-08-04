package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// This file implements the delete-path half of the snapshot-lifecycle redesign
// (docs/snapshot-lifecycle-redesign.md): detaching everything that depends on a
// ZFS object before it is destroyed, so nothing ever needs `zfs destroy -r`
// (D11) and a volume's snapshots/clones survive its own deletion (D0, via
// `zfs promote`).
//
// D17: the live ZFS clone graph is the single source of truth for what depends
// on what. Dependents are discovered by asking ZFS — `zfs list -t snapshot` for
// what a dataset owns, the `clones` property for what depends on each of those
// snapshots — at the moment of deletion, never by replaying bookkeeping kept in
// Kubernetes. One `zfs promote` rewrites four edges of that graph at once (it
// relocates the origin snapshot and every older snapshot onto the promoted
// clone, re-parents sibling clones onto it, gives it the former parent's
// previous origin, and turns the former parent into a clone of it), and the
// process can crash between any two of them — so any mirror of that graph held
// elsewhere is permanently one interrupted reconcile away from being wrong.
// Re-deriving it makes the whole sequence idempotent and crash-safe: every
// reconcile starts from the truth.
//
// Kubernetes still decides the things ZFS cannot express — whether deletion may
// proceed at all — but those are reads of *desired* state (spec), never of
// derived bookkeeping.

// restoreSourceSnapshotName is the fixed, CSI-invisible self-snapshot name (D5)
// taken on every standalone-mode backing-clone ZfsDataset (D15) immediately
// after it is created. Restores always clone from
// "<backing-clone-dataset>@restore-source", never from the raw origin snapshot
// directly, so restores keep working whether the true source volume is still
// alive, deleted-but-not-yet-promoted, or already promoted away.
const restoreSourceSnapshotName = "restore-source"

// maxDetachRounds bounds the detach fixpoint. Each round performs exactly one
// `zfs promote`, and every promote strictly reduces the number of snapshots
// still owned by the dataset being destroyed, so the loop terminates in at most
// one round per snapshot. The cap only exists so a ZFS-side surprise degrades
// into a visible error rather than an unbounded loop.
const maxDetachRounds = 100

// driverSnapshotSuffix matches the snapshot short names (the part after "@")
// this driver creates, and only those:
//
//   - "restore-source" — a standalone backing clone's self-snapshot (D5);
//   - "clone-<16 hex>" — the ephemeral intermediate snapshot ADR-0009's direct
//     PVC-to-PVC clone path takes (see cloneSnapshotSuffix);
//   - "csi-snap-<uuid>" — a CSI-visible raw snapshot
//     (independent-resource-naming-redesign.md).
//
// It is deliberately an allow-list (D18): anything else living on a driver
// dataset was put there from outside the driver, and the delete path refuses to
// touch it rather than guessing. The "csi-snap-" arm matches on prefix rather
// than on UUID shape because that prefix is reserved for the driver by design
// (D1a); assertDriverSnapshot additionally refuses any such snapshot that a
// live ZfsSnapshot still claims.
var driverSnapshotSuffix = regexp.MustCompile(`^(restore-source|clone-[0-9a-f]{16}|csi-snap-.+)$`)

// effectiveSnapshotMode returns snap's resolved Mode, treating an empty value
// (snapshots created before Mode existed) as Integrated — today's original,
// pre-redesign behaviour — for backward compatibility (D8).
func effectiveSnapshotMode(snap *storagev1alpha1.ZfsSnapshot) storagev1alpha1.ZfsSnapshotMode {
	if snap.Spec.Mode == "" {
		return storagev1alpha1.SnapshotModeIntegrated
	}
	return snap.Spec.Mode
}

// splitSnapshot splits a full ZFS snapshot name into its dataset and short
// name, e.g. "tank/k8s/vol@restore-source" -> ("tank/k8s/vol", "restore-source").
// The suffix is empty when full is not a snapshot.
func splitSnapshot(full string) (dataset, suffix string) {
	if i := strings.Index(full, "@"); i >= 0 {
		return full[:i], full[i+1:]
	}
	return full, ""
}

// datasetPathOf converts a full ZFS name into the pool-relative path stored in
// ZfsDataset.Spec.Dataset, e.g. ("tank", "tank/k8s/vol") -> "k8s/vol".
func datasetPathOf(poolName, full string) string {
	return strings.Trim(strings.TrimPrefix(strings.TrimPrefix(full, poolName), "/"), "/")
}

// sourceDependsOn reports whether dep's clone source lives on the dataset at
// datasetPath — either a snapshot of it (restore, or a standalone backing
// clone) or the dataset itself (direct PVC-to-PVC clone, ADR-0009).
func sourceDependsOn(dep *storagev1alpha1.ZfsDataset, datasetPath string) bool {
	src := dep.Spec.Source
	if src == nil || datasetPath == "" {
		return false
	}
	if src.Volume != "" && strings.Trim(src.Volume, "/") == datasetPath {
		return true
	}
	if src.Snapshot != "" {
		ds, _ := splitSnapshot(src.Snapshot)
		return strings.Trim(ds, "/") == datasetPath
	}
	return false
}

// beforeDestroy prepares vol for a non-recursive `zfs destroy` (D11/D22).
//
// It first applies the policies ZFS cannot express — all reads of Kubernetes
// *desired* state, never of derived bookkeeping — and then detaches whatever
// ZFS actually reports as depending on vol.
func (r *ZfsDatasetReconciler) beforeDestroy(ctx context.Context, vol *storagev1alpha1.ZfsDataset, poolName, full string) error {
	if err := r.checkSnapshotDependents(ctx, vol); err != nil {
		return err
	}
	if err := r.checkOwningSnapshotLive(ctx, vol); err != nil {
		return err
	}
	if err := r.checkPendingCloneDependents(ctx, vol, poolName); err != nil {
		return err
	}
	return detachAndCleanSnapshots(ctx, r.gateReader(), r.ZFS, vol.Spec.PoolGUID, poolName, full)
}

// checkSnapshotDependents implements D3/§3.2: the two situations where a
// volume's deletion must block rather than proceed, because there is nothing
// safe to promote yet.
//
//   - A dependent ZfsSnapshot that is not Ready is still being taken, so its
//     backing clone may not exist yet.
//   - A live integrated-mode snapshot has no backing clone at all, and so no
//     promote mechanism to fall back on — destroying the volume would take the
//     user's snapshot with it.
func (r *ZfsDatasetReconciler) checkSnapshotDependents(ctx context.Context, vol *storagev1alpha1.ZfsDataset) error {
	var snaps storagev1alpha1.ZfsSnapshotList
	if err := r.gateReader().List(ctx, &snaps); err != nil {
		return err
	}
	for i := range snaps.Items {
		snap := &snaps.Items[i]
		if snap.Spec.SourceVolume != vol.Name || !snap.DeletionTimestamp.IsZero() {
			continue
		}
		if snap.Status.Phase != storagev1alpha1.SnapshotPhaseReady {
			return fmt.Errorf("volume %q has snapshot %q still in phase %q; requeue", vol.Name, snap.Name, snap.Status.Phase)
		}
		if effectiveSnapshotMode(snap) != storagev1alpha1.SnapshotModeStandalone {
			return fmt.Errorf("volume %q has a live integrated-mode snapshot %q; delete it before deleting the volume", vol.Name, snap.Name)
		}
	}
	return nil
}

// checkOwningSnapshotLive refuses to destroy a standalone-mode backing clone
// while the ZfsSnapshot that owns it is still live (F7).
//
// The only sanctioned ways a backing clone is deleted are garbage collection
// after its owner went away, and the explicit Delete in
// ZfsSnapshotReconciler.reconcileDelete — both imply the owner is already
// terminating. A `kubectl delete zfsdataset csi-snap-<uuid>` run by hand does
// not, and proceeding would destroy the snapshot's only copy of the data while
// the user's VolumeSnapshot still claims to hold it.
func (r *ZfsDatasetReconciler) checkOwningSnapshotLive(ctx context.Context, vol *storagev1alpha1.ZfsDataset) error {
	for _, ref := range vol.OwnerReferences {
		if ref.Kind != "ZfsSnapshot" {
			continue
		}
		owner := &storagev1alpha1.ZfsSnapshot{}
		if err := r.gateReader().Get(ctx, client.ObjectKey{Name: ref.Name}, owner); err != nil {
			if apierrors.IsNotFound(err) {
				continue // owner already gone: the normal garbage-collection path
			}
			return err
		}
		if owner.DeletionTimestamp.IsZero() {
			return fmt.Errorf("refusing to destroy backing clone %q: its ZfsSnapshot %q is still live; delete the snapshot instead",
				vol.Name, owner.Name)
		}
	}
	return nil
}

// checkPendingCloneDependents implements D21: block while some ZfsDataset has
// declared vol as its clone source but its own ZFS dataset does not exist yet.
//
// Such a dependent is invisible in the ZFS clone graph — the object is created
// by the CSI controller before the agent runs `zfs clone` — so without this
// check the detach below would find nothing, destroy vol, and leave the pending
// restore permanently unable to complete. Spec is desired state, written once
// at creation and never recomputed, so reading it here does not reintroduce the
// mirror D17 removed.
func (r *ZfsDatasetReconciler) checkPendingCloneDependents(ctx context.Context, vol *storagev1alpha1.ZfsDataset, poolName string) error {
	var list storagev1alpha1.ZfsDatasetList
	if err := r.gateReader().List(ctx, &list); err != nil {
		return err
	}
	for i := range list.Items {
		dep := &list.Items[i]
		if dep.Name == vol.Name || dep.Spec.PoolGUID != vol.Spec.PoolGUID || !dep.DeletionTimestamp.IsZero() {
			continue
		}
		if !sourceDependsOn(dep, strings.Trim(vol.Spec.Dataset, "/")) {
			continue
		}
		depFull, err := datasetName(poolName, dep.Spec.Dataset)
		if err != nil {
			return err
		}
		if _, err := r.ZFS.Get(ctx, depFull, "type"); err != nil {
			if errors.Is(err, zpool.ErrNotExist) {
				return fmt.Errorf("volume %q is the clone source of %q, which has not been provisioned yet; "+
					"waiting for it before destroying (delete %q instead if it is stuck)", vol.Name, dep.Name, dep.Name)
			}
			return err
		}
	}
	return nil
}

// detachAndCleanSnapshots leaves `full` with zero snapshots of its own, which
// is exactly the precondition a non-recursive `zfs destroy` needs (D11/D22).
//
// Each round asks ZFS which snapshots `full` still owns and which datasets
// clone them. If any snapshot is still cloned, that clone is promoted away —
// which relocates the snapshot, and every snapshot older than it, onto the
// clone — and the round restarts from freshly read state, because one promote
// can move several snapshots and re-parent several clones at once. Once nothing
// clones anything any more, whatever remains is leftover driver artifacts that
// an earlier promote relocated here, and they are destroyed directly.
//
// For the overwhelmingly common case of a dataset with no snapshots at all this
// is a single `zfs list` and done.
func detachAndCleanSnapshots(ctx context.Context, c client.Reader, z zpool.ZFS, poolGUID, poolName, full string) error {
	logger := log.FromContext(ctx)
	for round := 0; round < maxDetachRounds; round++ {
		snaps, err := z.ListSnapshots(ctx, full)
		if err != nil {
			if errors.Is(err, zpool.ErrNotExist) {
				return nil // dataset already gone; nothing to detach
			}
			return err
		}
		if len(snaps) == 0 {
			return nil
		}

		promoted := false
		for _, snap := range snaps {
			clones, err := z.Clones(ctx, snap)
			if err != nil {
				if errors.Is(err, zpool.ErrNotExist) {
					continue
				}
				return err
			}
			if len(clones) == 0 {
				continue
			}
			if err := assertKnownDatasets(ctx, c, poolGUID, poolName, snap, clones); err != nil {
				return err
			}
			// Promoting any one clone detaches the snapshot from all of them:
			// ZFS re-parents the siblings onto the promoted clone as part of the
			// same operation. The next round picks up whatever is left.
			if err := z.Promote(ctx, clones[0]); err != nil {
				return fmt.Errorf("promote %q away from %q: %w", clones[0], snap, err)
			}
			logger.Info("promoted dependent away", "dependent", clones[0], "detachedFrom", snap, "destroying", full)
			promoted = true
			break
		}
		if promoted {
			continue
		}

		// Nothing clones any of the remaining snapshots, so they are leftover
		// driver artifacts relocated here by an earlier promote. Verify every one
		// of them is ours (D18) before destroying anything: failing loud leaves
		// the object visibly Terminating, which is strictly better than deleting
		// data the driver did not create.
		for _, snap := range snaps {
			if err := assertDriverSnapshot(ctx, c, snap); err != nil {
				return err
			}
		}
		for _, snap := range snaps {
			if err := z.Destroy(ctx, snap, false); err != nil {
				return fmt.Errorf("destroy leftover snapshot %q: %w", snap, err)
			}
			logger.Info("destroyed leftover snapshot artifact", "snapshot", snap, "destroying", full)
		}
		return nil
	}
	return fmt.Errorf("detaching dependents of %q did not converge after %d rounds", full, maxDetachRounds)
}

// detachSnapshotClones promotes away every clone of a single snapshot so that
// snapshot can be destroyed on its own (D19).
//
// Used by ZfsSnapshotReconciler for the raw origin snapshot, which lives on the
// still-live source volume and is therefore never reached by that volume's own
// delete path. Promoting the first clone relocates the snapshot onto it and
// re-parents the rest, so one pass normally suffices and the destroy that
// follows becomes a NotExist no-op; the loop only guards against a clone
// appearing concurrently.
func detachSnapshotClones(ctx context.Context, c client.Reader, z zpool.ZFS, poolGUID, poolName, snap string) error {
	logger := log.FromContext(ctx)
	for round := 0; round < maxDetachRounds; round++ {
		clones, err := z.Clones(ctx, snap)
		if err != nil {
			if errors.Is(err, zpool.ErrNotExist) {
				return nil // already relocated elsewhere, or already destroyed
			}
			return err
		}
		if len(clones) == 0 {
			return nil
		}
		if err := assertKnownDatasets(ctx, c, poolGUID, poolName, snap, clones); err != nil {
			return err
		}
		if err := z.Promote(ctx, clones[0]); err != nil {
			return fmt.Errorf("promote %q away from %q: %w", clones[0], snap, err)
		}
		logger.Info("promoted dependent away", "dependent", clones[0], "detachedFrom", snap)
	}
	return fmt.Errorf("detaching clones of %q did not converge after %d rounds", snap, maxDetachRounds)
}

// assertKnownDatasets refuses to promote anything the driver does not manage
// (D18). `zfs promote` is not destructive, but it rewrites which dataset owns a
// shared snapshot history, which would surprise an administrator or an external
// tool that created the clone. The datasetPrefix is designated to the driver,
// so a clone with no corresponding ZfsDataset means something outside
// Kubernetes put it there and a human should decide what happens to it.
func assertKnownDatasets(ctx context.Context, c client.Reader, poolGUID, poolName, snap string, clones []string) error {
	var list storagev1alpha1.ZfsDatasetList
	if err := c.List(ctx, &list); err != nil {
		return err
	}
	known := map[string]bool{}
	for i := range list.Items {
		if d := &list.Items[i]; d.Spec.PoolGUID == poolGUID {
			known[strings.Trim(d.Spec.Dataset, "/")] = true
		}
	}
	for _, clone := range clones {
		if !known[datasetPathOf(poolName, clone)] {
			return fmt.Errorf("snapshot %q is cloned by %q, which is not a known ZfsDataset on this pool; "+
				"refusing to promote a dataset the driver does not manage — resolve it manually", snap, clone)
		}
	}
	return nil
}

// assertDriverSnapshot refuses to destroy a snapshot the driver did not create
// (D18), and — for a CSI-visible raw snapshot — refuses to destroy one whose
// ZfsSnapshot object is still live.
//
// The second check cannot fail through any driver-driven sequence, because a
// raw snapshot only ever relocates onto a dataset whose own deletion is already
// under way. It is kept as a cheap assertion that the allow-list can never be
// turned against real snapshot data.
func assertDriverSnapshot(ctx context.Context, c client.Reader, full string) error {
	_, suffix := splitSnapshot(full)
	if !driverSnapshotSuffix.MatchString(suffix) {
		return fmt.Errorf("snapshot %q was not created by this driver; refusing to destroy it — remove it manually to continue", full)
	}
	if !strings.HasPrefix(suffix, "csi-snap-") {
		return nil
	}
	var snaps storagev1alpha1.ZfsSnapshotList
	if err := c.List(ctx, &snaps); err != nil {
		return err
	}
	for i := range snaps.Items {
		s := &snaps.Items[i]
		if s.Spec.SnapshotName == suffix && s.DeletionTimestamp.IsZero() {
			return fmt.Errorf("refusing to destroy snapshot %q: ZfsSnapshot %q still claims it", full, s.Name)
		}
	}
	return nil
}
