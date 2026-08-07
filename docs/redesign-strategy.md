# Redesign strategy: dependency guards (`checkSnapshotDependents`, `checkPendingCloneDependents`)

Status: **discussion baseline**, not a decision.

This document captures the current decision frame so we can continue later
without losing context.

---

## Scope

Two guards in [internal/controller/promote.go](../internal/controller/promote.go):

- `checkSnapshotDependents`
- `checkPendingCloneDependents`

Both are runtime guard functions, **not** Kubernetes finalizers.

---

## What they do today

### `checkSnapshotDependents`

Blocks `ZfsDataset` teardown while a dependent `ZfsSnapshot` exists for that
volume and is not yet `Ready`.

Simple intent: *do not destroy the source while snapshot creation is still in
flight*.

### `checkPendingCloneDependents`

Blocks `ZfsDataset` teardown while another `ZfsDataset` declares this volume as
clone source in `spec.source`, but its physical ZFS dataset does not exist yet.

Simple intent: *do not destroy the source during the intent→materialization
window for clones*.

---

## Keep vs remove: decision frame

### If we remove both guards

We rely entirely on upstream Kubernetes/sidecar protection:

- snapshot source protection during snapshot creation
- cloning protection during PVC-from-PVC provisioning

Potential gains:

- simpler delete path in `beforeDestroy`
- less overlap with upstream mechanisms

Potential losses:

- less defense-in-depth for non-standard/internal CR flows
- weaker local diagnostics at the exact reconcile point
- stronger coupling to upstream timing/behavior assumptions

### If we keep both guards

We keep a local safety net for the intent→materialization windows, even when
upstream protections are present.

Potential gains:

- robust under internal/manual CR manipulations and odd sequencing
- explicit local fail-fast behavior

Potential costs:

- extra logic that may duplicate standard-flow protection
- broader internal contract surface

---

## Contract question that decides everything

Are we explicitly supporting only **standard Kubernetes CSI flows**, or also
robustness under **internal/manual CR workflows** and out-of-band sequences?

- Standard-flow-only contract → removal is plausible.
- Broader robustness contract → keeping the guards is preferable.

No decision yet.

---

## Related references

- [csi-technical-reference.md](csi-technical-reference.md)
- [kubernetes-volume-lifecycle-facts.md](kubernetes-volume-lifecycle-facts.md)
- [lifecycle-protection-matrix.md](lifecycle-protection-matrix.md)
- [snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md)

