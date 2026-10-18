package zpool

import (
	"context"
	"strings"
)

// Labels are ZFS user properties the driver puts on every dataset and snapshot
// it creates. Delete and promote logic selects objects by them instead of
// matching names (docs/snapshot-delete-redesign-plan.md, P1). User properties
// are not inherited by clones, move with a promoted snapshot, and are
// readable on snapshots that are marked for deferred destroy.
const (
	// PropRole says what the object is for, one of the Role* values.
	PropRole = "simple-zfs-csi:role"
	// PropOwner is the UID of the Kubernetes object that owns the ZFS object
	// (ZfsDataset or ZfsSnapshot). It tells a live object from an orphan.
	PropOwner = "simple-zfs-csi:owner"
)

// PropOriginReleased is set on a clone (a directly cloned volume, or a
// snapshot's backing clone) once the snapshot it was made from has been marked
// for destruction during the clone's delete. The promote loop that follows moves
// snapshots around, so a retry can no longer find that snapshot from the clone;
// this tells the retry the step is done, so that "not found" can otherwise be an
// error.
const PropOriginReleased = "simple-zfs-csi:origin-released"

const (
	// RoleVolume is a volume dataset, including one created as a restore or clone.
	RoleVolume = "volume"
	// RoleRaw is the raw snapshot taken for a CSI snapshot.
	RoleRaw = "raw"
	// RoleBacking is the dataset cloned from a raw snapshot so the snapshot
	// can outlive its source volume.
	RoleBacking = "backing"
	// RoleCloneOrigin is the intermediate snapshot a direct volume clone is made from.
	RoleCloneOrigin = "clone-origin"
)

// LabelProps returns the label properties for create-time use (`-o`). An empty
// owner is left out rather than stored as an empty value.
func LabelProps(role, owner string) map[string]string {
	props := map[string]string{PropRole: role}
	if owner != "" {
		props[PropOwner] = owner
	}
	return props
}

// MergeLabels adds the label properties to props, which is not modified, and
// returns the result. The labels win over a user-supplied property of the same name.
func MergeLabels(props map[string]string, role, owner string) map[string]string {
	out := make(map[string]string, len(props)+2)
	for k, v := range props {
		out[k] = v
	}
	for k, v := range LabelProps(role, owner) {
		out[k] = v
	}
	return out
}

// EnsureLabels sets the label properties on an existing dataset or snapshot
// when they are missing or different, and does nothing when they are right.
// Idempotent, so it doubles as the repair for an object whose labels could not
// be set at create time (a snapshot is labelled right after `zfs snapshot`).
func EnsureLabels(ctx context.Context, z ZFS, name, role, owner string) error {
	for prop, want := range LabelProps(role, owner) {
		got, err := z.Get(ctx, name, prop)
		if err != nil {
			return err
		}
		if got = strings.TrimSpace(got); got == want {
			continue
		}
		if err := z.SetProperty(ctx, name, prop, want); err != nil {
			return err
		}
	}
	return nil
}
