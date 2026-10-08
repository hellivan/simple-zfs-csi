package controller

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

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
// taken on every backing clone immediately
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
//   - "restore-source" — a backing clone's self-snapshot (D5);
//   - "clone-<dest-name>" — the ephemeral intermediate snapshot ADR-0009's
//     direct PVC-to-PVC clone path takes on the source dataset, named after the
//     destination ZfsDataset's own object name (see cloneSnapshotSuffix);
//   - "csi-snap-<uuid>" — a CSI-visible raw snapshot
//     (independent-resource-naming-redesign.md).
//
// It is deliberately an allow-list (D18): anything else living on a driver
// dataset was put there from outside the driver, and the delete path refuses to
// touch it rather than guessing. The "clone-" and "csi-snap-" arms match on
// prefix rather than a fixed shape because both suffixes are reserved for the
// driver by design (D1a).
var driverSnapshotSuffix = regexp.MustCompile(`^(restore-source|clone-.+|csi-snap-.+)$`)

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

// beforeDestroy prepares vol for a non-recursive `zfs destroy` (D11/D22).
//
// It looks only at ZFS (ADR-0041): detach whatever ZFS reports as depending on
// vol and destroy the driver snapshots nothing clones. A snapshot, restore or
// group snapshot is complete only when it is Ready; deleting its source earlier
// is the caller's responsibility, so nothing here waits for unfinished work.
func (r *ZfsDatasetReconciler) beforeDestroy(ctx context.Context, vol *storagev1alpha1.ZfsDataset, poolName, full string) error {
	return detachAndCleanSnapshots(ctx, r.gateReader(), r.ZFS, vol.Spec.PoolGUID, poolName, full)
}

// detachAndCleanSnapshots leaves `full` with zero snapshots of its own, which
// is exactly the precondition a non-recursive `zfs destroy` needs (D11/D22).
//
// Each round asks ZFS which snapshots `full` still owns and which datasets
// clone them. Every snapshot is first checked against the driver name
// allow-list (D18); a foreign one refuses the whole delete. Every snapshot that
// nothing clones is then destroyed (ADR-0041: with no clone it is free to go,
// and a ZfsSnapshot that still wanted it fails loudly). If any snapshot is
// still cloned, that clone is promoted away — which relocates the snapshot, and
// every snapshot older than it, onto the clone — and the round restarts from
// freshly read state, because one promote can move several snapshots and
// re-parent several clones at once.
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

		// Verify every snapshot is ours (D18) before touching anything: failing
		// loud leaves the object visibly Terminating, which is strictly better
		// than deleting data the driver did not create.
		for _, snap := range snaps {
			if err := assertDriverSnapshot(snap); err != nil {
				return err
			}
		}

		cloned := map[string][]string{}
		var order []string
		for _, snap := range snaps {
			clones, err := z.Clones(ctx, snap)
			if err != nil {
				if errors.Is(err, zpool.ErrNotExist) {
					continue
				}
				return err
			}
			if len(clones) == 0 {
				if err := z.Destroy(ctx, snap, false); err != nil {
					return fmt.Errorf("destroy snapshot %q: %w", snap, err)
				}
				logger.Info("destroyed snapshot with no clones", "snapshot", snap, "destroying", full)
				continue
			}
			cloned[snap] = clones
			order = append(order, snap)
		}
		if len(order) == 0 {
			return nil
		}

		snap := order[0]
		clones := cloned[snap]
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
	// Backing clones are not Kubernetes objects (ADR-0030): they are an
	// implementation detail of a ZfsSnapshot, named after its Spec.SnapshotName
	// and living as a flat sibling of the source dataset. Without this they would
	// look exactly like a dataset the driver does not manage, and deleting any
	// volume that has a snapshot would refuse.
	backing := map[string]bool{}
	var snaps storagev1alpha1.ZfsSnapshotList
	if err := c.List(ctx, &snaps); err != nil {
		return err
	}
	for i := range snaps.Items {
		if s := &snaps.Items[i]; s.Spec.PoolGUID == poolGUID && s.Spec.SnapshotName != "" {
			backing[s.Spec.SnapshotName] = true
		}
	}
	for _, clone := range clones {
		p := datasetPathOf(poolName, clone)
		if known[p] || backing[path.Base(p)] {
			continue
		}
		return fmt.Errorf("snapshot %q is cloned by %q, which is neither a known ZfsDataset "+
			"nor a live ZfsSnapshot's backing clone on this pool; "+
			"refusing to promote a dataset the driver does not manage — resolve it manually", snap, clone)
	}
	return nil
}

// assertDriverSnapshot refuses to destroy a snapshot the driver did not create
// (D18). It is a pure name allow-list: whether a ZfsSnapshot still wants the
// snapshot is not asked (ADR-0041), because that object fails loudly on its own
// when its snapshot is gone.
func assertDriverSnapshot(full string) error {
	_, suffix := splitSnapshot(full)
	if !driverSnapshotSuffix.MatchString(suffix) {
		return fmt.Errorf("snapshot %q was not created by this driver; refusing to destroy it — remove it manually to continue", full)
	}
	return nil
}
