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

// legacyRestoreSourceSnapshot is the self-snapshot older drivers took on every
// backing clone (and restored from). Nothing creates or reads it any more
// (strategy D, docs/snapshot-delete-redesign-plan.md); it stays on the delete
// allow-list so objects created by those drivers can still be cleaned up until
// the migration script has retired them.
const legacyRestoreSourceSnapshot = "restore-source"

// maxDetachRounds bounds the detach fixpoint. Each round performs exactly one
// `zfs promote`, and every promote strictly reduces the number of snapshots
// still owned by the dataset being destroyed, so the loop terminates in at most
// one round per snapshot. The cap only exists so a ZFS-side surprise degrades
// into a visible error rather than an unbounded loop.
const maxDetachRounds = 100

// driverSnapshotSuffix matches the snapshot short names (the part after "@")
// this driver creates, and only those:
//
//   - "restore-source" — legacy self-snapshot of a backing clone (see legacyRestoreSourceSnapshot);
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
	// P5, first of all: see releaseCloneOrigin.
	if err := r.releaseCloneOrigin(ctx, vol, full); err != nil {
		return err
	}
	return detachAndCleanSnapshots(ctx, r.gateReader(), r.ZFS, vol.Spec.PoolGUID, poolName, full)
}

// detachAndCleanSnapshots leaves `full` with zero snapshots of its own, which
// is exactly the precondition a non-recursive `zfs destroy` needs (D11/D22).
//
// Each round asks ZFS which snapshots `full` still owns and which datasets
// clone them. Every snapshot is first checked against the driver name
// allow-list (D18); a foreign one refuses the whole delete. Every snapshot that
// nothing clones is then destroyed with `-d` (P2). If any snapshot is still
// cloned, one of its clones is promoted away, which relocates the snapshot and
// every older one onto the clone, and the round restarts from freshly read
// state, because one promote can move several snapshots and re-parent several
// clones at once.
//
// Which clone (P4a): the oldest cloned snapshot first, since a promote moves it
// and everything older, so this moves the least and avoids name conflicts; among
// the clones of those snapshots the one labelled as a backing clone is preferred,
// so a raw snapshot stays on its source or on its backing clone.
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

		type candidate struct{ snap, clone string }
		var cands []candidate
		clonesOf := map[string][]string{}
		for _, snap := range snaps {
			clones, err := z.Clones(ctx, snap)
			if err != nil {
				if errors.Is(err, zpool.ErrNotExist) {
					continue
				}
				return err
			}
			if len(clones) == 0 {
				if err := z.DestroySnapshotDeferred(ctx, snap); err != nil {
					return fmt.Errorf("destroy snapshot %q: %w", snap, err)
				}
				logger.Info("destroyed snapshot with no clones", "snapshot", snap, "destroying", full)
				continue
			}
			clonesOf[snap] = clones
			for _, cl := range clones {
				cands = append(cands, candidate{snap, cl})
			}
		}
		if len(cands) == 0 {
			return nil
		}

		pick := cands[0]
		for _, cd := range cands {
			role, err := z.Get(ctx, cd.clone, zpool.PropRole)
			if err != nil && !errors.Is(err, zpool.ErrNotExist) {
				return err
			}
			if strings.TrimSpace(role) == zpool.RoleBacking {
				pick = cd
				break
			}
		}
		if err := assertKnownDatasets(ctx, c, poolGUID, poolName, pick.snap, clonesOf[pick.snap]); err != nil {
			return err
		}
		if err := z.Promote(ctx, pick.clone); err != nil {
			return fmt.Errorf("promote %q away from %q: %w", pick.clone, pick.snap, err)
		}
		logger.Info("promoted dependent away", "dependent", pick.clone, "detachedFrom", pick.snap, "destroying", full)
	}
	return fmt.Errorf("detaching dependents of %q did not converge after %d rounds", full, maxDetachRounds)
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
