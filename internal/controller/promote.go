package controller

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// This file implements the ZfsDatasetReconciler side of the snapshot-lifecycle
// redesign (docs/snapshot-lifecycle-redesign.md): promoting away every tracked
// dependent of a ZfsDataset before it is destroyed, so DeleteVolume never has to
// use `zfs destroy -r` (D11) and a volume's snapshots/clones survive its own
// deletion (D0, via `zfs promote`).

// restoreSourceSnapshotName is the fixed, CSI-invisible self-snapshot name (D5)
// taken on every standalone-mode backing-clone ZfsDataset (D15) immediately
// after it is created. Restores always clone from
// "<backing-clone-dataset>@restore-source", never from the raw origin snapshot
// directly, so restores keep working whether the true source volume is still
// alive, deleted-but-not-yet-promoted, or already promoted away.
const restoreSourceSnapshotName = "restore-source"

// Finalizer prefixes for the generalized dependent-tracking mechanism (D12/D15).
// A "promoted-onto.<name>" or "restored-by.<name>" finalizer on a ZfsDataset
// means the *named* ZfsDataset currently depends on this one (its ZFS dataset is
// a clone whose origin lives here) and must be `zfs promote`d away before this
// one can be destroyed. The two prefixes exist for readability at the call site
// (D12's promote-chaining vs. D4's restore-tracking) but are handled completely
// uniformly by every helper below — D15 unifies D4 into D12.
const (
	promotedOntoFinalizerPrefix = "storage.simple-zfs-csi.io/promoted-onto."
	restoredByFinalizerPrefix   = "storage.simple-zfs-csi.io/restored-by."
)

func promotedOntoFinalizer(dependentName string) string {
	return promotedOntoFinalizerPrefix + dependentName
}

// restoredByFinalizer names the finalizer a restored PVC's ZfsDataset registers
// on the standalone-mode backing clone it was cloned from (D4/D15).
func restoredByFinalizer(dependentName string) string {
	return restoredByFinalizerPrefix + dependentName
}

// effectiveSnapshotMode returns snap's resolved Mode, treating an empty value
// (snapshots created before Mode existed) as Integrated — today's original,
// pre-redesign behaviour — for backward compatibility (D8).
func effectiveSnapshotMode(snap *storagev1alpha1.ZfsSnapshot) storagev1alpha1.ZfsSnapshotMode {
	if snap.Spec.Mode == "" {
		return storagev1alpha1.SnapshotModeIntegrated
	}
	return snap.Spec.Mode
}

// isOriginEmptyValue reports whether a `zfs get origin` value means "no
// origin" (never a clone, or already fully promoted/independent).
func isOriginEmptyValue(origin string) bool {
	origin = strings.TrimSpace(origin)
	return origin == "" || origin == "-"
}

// originDatasetPath extracts the pool-relative dataset path from a full ZFS
// origin value (e.g. "tank/k8s/csi-snap-x@restore-source" -> "k8s/csi-snap-x",
// given poolName "tank").
func originDatasetPath(origin, poolName string) string {
	ds := origin
	if i := strings.Index(ds, "@"); i >= 0 {
		ds = ds[:i]
	}
	return strings.TrimPrefix(strings.TrimPrefix(ds, poolName), "/")
}

// trackedDependentNames returns the object names of every ZfsDataset tracked as
// depending on vol via a promoted-onto.*/restored-by.* finalizer (D12/D15).
func trackedDependentNames(vol *storagev1alpha1.ZfsDataset) []string {
	var names []string
	for _, f := range vol.Finalizers {
		switch {
		case strings.HasPrefix(f, promotedOntoFinalizerPrefix):
			names = append(names, strings.TrimPrefix(f, promotedOntoFinalizerPrefix))
		case strings.HasPrefix(f, restoredByFinalizerPrefix):
			names = append(names, strings.TrimPrefix(f, restoredByFinalizerPrefix))
		}
	}
	return names
}

// beforeDestroy prepares vol for a non-recursive `zfs destroy` (D11) by
// promoting away every tracked dependent first, so vol is guaranteed to have
// zero snapshots/clones of its own left by the time destroy runs. Returns an
// error (causing a requeue) when it isn't yet safe to proceed: an in-flight
// standalone-mode snapshot (D3), or a still-live integrated-mode dependent
// (§3.2), neither of which have anything to promote.
func (r *ZfsDatasetReconciler) beforeDestroy(ctx context.Context, vol *storagev1alpha1.ZfsDataset, poolName, full string) error {
	if err := r.promoteSnapshotDependents(ctx, vol, poolName); err != nil {
		return err
	}
	if err := r.promoteDirectCloneDependents(ctx, vol, poolName); err != nil {
		return err
	}
	if err := r.promoteTrackedDependents(ctx, vol, poolName); err != nil {
		return err
	}

	// D15: if vol is itself a standalone-mode backing clone, its own
	// "@restore-source" self-snapshot (and, if the true source volume was
	// deleted earlier and this backing clone was promoted as a result, the raw
	// origin snapshot relocated here by that promote) are internal artifacts,
	// never CSI-visible, and safe to destroy directly now that every clone
	// dependent of them has been promoted away above (a snapshot with no
	// remaining clones can be destroyed directly, no -r needed). Destroy() is
	// idempotent/NotExist-tolerant, so this is a no-op wherever it doesn't apply.
	snapName, isBackingClone, err := r.backingCloneOwnerSnapshotName(ctx, vol)
	if err != nil {
		return err
	}
	if isBackingClone {
		if err := r.ZFS.Destroy(ctx, full+"@"+restoreSourceSnapshotName, false); err != nil {
			return fmt.Errorf("destroy backing-clone self-snapshot %s@%s: %w", full, restoreSourceSnapshotName, err)
		}
		if snapName != "" {
			if err := r.ZFS.Destroy(ctx, full+"@"+snapName, false); err != nil {
				return fmt.Errorf("destroy relocated origin snapshot %s@%s: %w", full, snapName, err)
			}
		}
	}
	return nil
}

// promoteSnapshotDependents implements D3/§3.1-3.2: blocks (returns an error,
// causing a requeue) on any dependent ZfsSnapshot not yet Ready, and on any
// live integrated-mode dependent (which has no promote mechanism to fall back
// on). Ready standalone-mode dependents get their backing clone promoted away
// unconditionally.
func (r *ZfsDatasetReconciler) promoteSnapshotDependents(ctx context.Context, vol *storagev1alpha1.ZfsDataset, poolName string) error {
	var snaps storagev1alpha1.ZfsSnapshotList
	if err := r.List(ctx, &snaps); err != nil {
		return err
	}
	for i := range snaps.Items {
		snap := &snaps.Items[i]
		if snap.Spec.SourceVolume != vol.Name {
			continue
		}
		if snap.Status.Phase != storagev1alpha1.SnapshotPhaseReady {
			return fmt.Errorf("volume %q has snapshot %q still in phase %q; requeue", vol.Name, snap.Name, snap.Status.Phase)
		}
		if effectiveSnapshotMode(snap) != storagev1alpha1.SnapshotModeStandalone {
			return fmt.Errorf("volume %q has a live integrated-mode snapshot %q; delete it before deleting the volume", vol.Name, snap.Name)
		}
		backing := &storagev1alpha1.ZfsDataset{}
		if err := r.Get(ctx, client.ObjectKey{Name: snap.Spec.SnapshotName}, backing); err != nil {
			if apierrors.IsNotFound(err) {
				continue // backing clone already gone; nothing to promote
			}
			return err
		}
		backingFull, err := datasetName(poolName, backing.Spec.Dataset)
		if err != nil {
			return err
		}
		if err := r.ZFS.Promote(ctx, backingFull); err != nil {
			return fmt.Errorf("promote backing clone %q for snapshot %q: %w", backingFull, snap.Name, err)
		}
	}
	return nil
}

// promoteDirectCloneDependents implements D7/D9: direct PVC-to-PVC clones
// (ADR-0009, no VolumeSnapshot involved) are always promoted away
// unconditionally — no mode toggle applies since there is no
// VolumeSnapshotClass in this path, and blocking here would be confusing (no
// visible object the user is managing).
func (r *ZfsDatasetReconciler) promoteDirectCloneDependents(ctx context.Context, vol *storagev1alpha1.ZfsDataset, poolName string) error {
	var list storagev1alpha1.ZfsDatasetList
	if err := r.List(ctx, &list); err != nil {
		return err
	}
	for i := range list.Items {
		dep := &list.Items[i]
		if dep.Name == vol.Name || dep.Spec.Source == nil || dep.Spec.Source.Volume != vol.Spec.Dataset {
			continue
		}
		depFull, err := datasetName(poolName, dep.Spec.Dataset)
		if err != nil {
			return err
		}
		if err := r.ZFS.Promote(ctx, depFull); err != nil {
			return fmt.Errorf("promote direct clone %q of %q: %w", depFull, vol.Name, err)
		}
	}
	return nil
}

// promoteTrackedDependents promotes away every ZfsDataset tracked (via
// promoted-onto.*/restored-by.* finalizers on vol, D12/D15) as depending on it,
// removing each finalizer once its dependent is confirmed independent. If
// promoting one dependent doesn't fully detach it — it's now a clone of a
// sibling dependent instead, real documented ZFS "move any clone references"
// behaviour when multiple clones share one snapshot (§2.9) — the tracking
// finalizer is re-registered on whichever ZfsDataset now owns that dependency
// instead of being dropped. Runs as a bounded fixpoint (D16): re-reads vol's
// finalizers fresh at the start of every round (earlier rounds' removals must
// be visible, or this would never converge) and repeats until nothing is left
// to track, capped at a small number of rounds — degrading to a single cheap
// pass whenever nothing needs reparenting, which D16's live-pool verification
// found to be the common case.
func (r *ZfsDatasetReconciler) promoteTrackedDependents(ctx context.Context, vol *storagev1alpha1.ZfsDataset, poolName string) error {
	const maxRounds = 10
	for round := 0; round < maxRounds; round++ {
		cur := &storagev1alpha1.ZfsDataset{}
		if err := r.Get(ctx, client.ObjectKey{Name: vol.Name}, cur); err != nil {
			if apierrors.IsNotFound(err) {
				return nil // vol itself is already gone
			}
			return err
		}
		names := trackedDependentNames(cur)
		if len(names) == 0 {
			return nil
		}
		for _, depName := range names {
			dep := &storagev1alpha1.ZfsDataset{}
			if err := r.Get(ctx, client.ObjectKey{Name: depName}, dep); err != nil {
				if apierrors.IsNotFound(err) {
					if err := r.removeTrackingFinalizer(ctx, vol.Name, depName); err != nil {
						return err
					}
					continue
				}
				return err
			}
			depFull, err := datasetName(poolName, dep.Spec.Dataset)
			if err != nil {
				return err
			}
			if err := r.ZFS.Promote(ctx, depFull); err != nil {
				return fmt.Errorf("promote tracked dependent %q: %w", depFull, err)
			}
			origin, err := r.ZFS.Get(ctx, depFull, "origin")
			if err != nil {
				return err
			}
			if isOriginEmptyValue(origin) {
				if err := r.removeTrackingFinalizer(ctx, vol.Name, depName); err != nil {
					return err
				}
				continue
			}
			// Still a clone of something (D12/§2.9): find whichever ZfsDataset now
			// owns that origin dataset and move the tracking finalizer there instead.
			ownerPath := originDatasetPath(origin, poolName)
			owner, err := r.findDatasetByPath(ctx, vol.Spec.PoolGUID, ownerPath)
			if err != nil {
				return err
			}
			if owner == nil {
				return fmt.Errorf("promote tracked dependent %q: new origin %q does not belong to any known ZfsDataset", depFull, origin)
			}
			if owner.Name != vol.Name {
				if err := r.addTrackingFinalizer(ctx, owner.Name, promotedOntoFinalizer(depName)); err != nil {
					return err
				}
			}
			if err := r.removeTrackingFinalizer(ctx, vol.Name, depName); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("promoteTrackedDependents: did not converge for %q after %d rounds", vol.Name, maxRounds)
}

// findDatasetByPath returns the ZfsDataset on poolGUID whose Spec.Dataset
// equals datasetPath, or nil if none is found.
func (r *ZfsDatasetReconciler) findDatasetByPath(ctx context.Context, poolGUID, datasetPath string) (*storagev1alpha1.ZfsDataset, error) {
	var list storagev1alpha1.ZfsDatasetList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		d := &list.Items[i]
		if d.Spec.PoolGUID == poolGUID && d.Spec.Dataset == datasetPath {
			return d, nil
		}
	}
	return nil, nil
}

// backingCloneOwnerSnapshotName reports whether vol is a standalone-mode
// backing clone (D15: it has an ownerReference to a ZfsSnapshot), returning
// that ZfsSnapshot's Spec.SnapshotName (the CSI-visible raw-snapshot suffix
// that may have relocated onto vol via an earlier promote of the true source
// volume). Returns ok=false when vol isn't a backing clone, and a zero value
// with ok=false (not an error) if the owning ZfsSnapshot is already gone.
func (r *ZfsDatasetReconciler) backingCloneOwnerSnapshotName(ctx context.Context, vol *storagev1alpha1.ZfsDataset) (name string, ok bool, err error) {
	for _, ref := range vol.OwnerReferences {
		if ref.Kind != "ZfsSnapshot" {
			continue
		}
		owner := &storagev1alpha1.ZfsSnapshot{}
		if getErr := r.Get(ctx, client.ObjectKey{Name: ref.Name}, owner); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return "", true, nil
			}
			return "", false, getErr
		}
		return owner.Spec.SnapshotName, true, nil
	}
	return "", false, nil
}

// addTrackingFinalizer adds finalizer to the named ZfsDataset, retrying on
// update conflicts. A no-op if already present.
func (r *ZfsDatasetReconciler) addTrackingFinalizer(ctx context.Context, name, finalizer string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.ZfsDataset{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, cur); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(cur, finalizer) {
			return nil
		}
		controllerutil.AddFinalizer(cur, finalizer)
		return r.Update(ctx, cur)
	})
}

// removeTrackingFinalizer removes any promoted-onto.<dependentName>/
// restored-by.<dependentName> finalizer from the named ZfsDataset, retrying on
// update conflicts. A no-op if the object is gone or neither finalizer is
// present.
func (r *ZfsDatasetReconciler) removeTrackingFinalizer(ctx context.Context, name, dependentName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &storagev1alpha1.ZfsDataset{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, cur); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		removed := controllerutil.RemoveFinalizer(cur, promotedOntoFinalizer(dependentName))
		removed = controllerutil.RemoveFinalizer(cur, restoredByFinalizer(dependentName)) || removed
		if !removed {
			return nil
		}
		return r.Update(ctx, cur)
	})
}

// clearTrackingFinalizersReferencing best-effort removes any
// promoted-onto.<depName>/restored-by.<depName> finalizer that references
// depName, wherever it currently lives (D12/D15). Called when depName's own
// ZfsDataset is being destroyed, so a stale reference never lingers forever on
// whichever object happened to be tracking it (that object may otherwise never
// be deleted itself, and so never get a chance to notice depName is gone).
func (r *ZfsDatasetReconciler) clearTrackingFinalizersReferencing(ctx context.Context, depName string) error {
	var list storagev1alpha1.ZfsDatasetList
	if err := r.List(ctx, &list); err != nil {
		return err
	}
	for i := range list.Items {
		owner := &list.Items[i]
		if controllerutil.ContainsFinalizer(owner, promotedOntoFinalizer(depName)) ||
			controllerutil.ContainsFinalizer(owner, restoredByFinalizer(depName)) {
			if err := r.removeTrackingFinalizer(ctx, owner.Name, depName); err != nil {
				return err
			}
		}
	}
	return nil
}
