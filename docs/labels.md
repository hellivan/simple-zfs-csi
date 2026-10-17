# ZFS labels

The driver tags every ZFS object it creates with two ZFS user properties. Later logic
(restore, delete, promote, cleanup, migration) selects objects by these labels and never by
name pattern. Code: `internal/zpool/labels.go`.

| Property | Value | Purpose |
|---|---|---|
| `simple-zfs-csi:role` | one of the roles below | what the object is for |
| `simple-zfs-csi:owner` | UID of the owning Kubernetes object | which CR owns it; omitted when unknown |

## Roles

| Role | Applied to | Owner (`metadata.uid` of) | Meaning |
|---|---|---|---|
| `volume` | dataset or zvol | `ZfsDataset` | A volume's dataset (new, restored or cloned). |
| `raw` | snapshot `<src>@csi-snap-<uuid>` | `ZfsSnapshot` | The point-in-time snapshot taken for a CSI snapshot. |
| `backing` | clone `<parent>/csi-snap-<uuid>` | `ZfsSnapshot` | Snapshot-less vessel dataset cloned from the raw snapshot; it is promoted away from when the snapshot is deleted. |
| `clone-origin` | snapshot `<src>@clone-<name>` | `ZfsDataset` | Origin snapshot of a direct volume clone. |

Group snapshot members: the raw snapshot is taken atomically by the group reconciler (no UID
yet) and is labelled `raw` with the member `ZfsSnapshot` as owner on that object's first
reconcile.

## Examples

Plain volume `pvc-A`:

| Object | role | owner |
|---|---|---|
| `pool/csi-vol-A` | `volume` | UID of `ZfsDataset pvc-A` |

Snapshot `snap-1` of `pvc-A`:

| Object | role | owner |
|---|---|---|
| `pool/csi-vol-A` | `volume` | UID of `ZfsDataset pvc-A` |
| `pool/csi-vol-A@csi-snap-S` | `raw` | UID of `ZfsSnapshot snap-1` |
| `pool/csi-snap-S` | `backing` | UID of `ZfsSnapshot snap-1` |

Restore `pvc-B` from `snap-1` (a clone of the raw snapshot):

| Object | role | owner |
|---|---|---|
| `pool/csi-vol-B` (origin `...@csi-snap-S`) | `volume` | UID of `ZfsDataset pvc-B` |

Direct clone `pvc-C` of `pvc-A`:

| Object | role | owner |
|---|---|---|
| `pool/csi-vol-A@clone-pvc-C` | `clone-origin` | UID of `ZfsDataset pvc-C` |
| `pool/csi-vol-C` | `volume` | UID of `ZfsDataset pvc-C` |

Group snapshot with two members (`data`, `wal`): each member is as in "Snapshot" above, with
its own `raw` snapshot and `backing` clone, owned by its own `ZfsSnapshot`.

After deleting `snap-1` while `pvc-B` still exists: the raw snapshot is marked for deferred
destroy and promoted onto `pvc-B`'s lineage; it keeps `raw` and its owner. Labels move with
the snapshot on promote, so `raw` is found by label wherever it ends up.

## Behaviour and caveats (verified on ZFS 2.4.3)

- `EnsureLabels` writes only when the value is missing or different, so it is idempotent. It
  runs only while an object is being provisioned: provisioned objects are never touched again
  (ADR-0038), so **objects created before labels existed are labelled only by the migration
  script** (see the S6 section of `snapshot-delete-redesign-plan.md`).
- `zfs get` prints `-` for an unset property; the driver treats `-` and empty as unset.
- Labels survive `zfs destroy -d` (deferred destroy) and `zfs promote`; they move with the snapshot.
- A clone does not inherit the origin snapshot's labels; it gets its own at creation.
- **Snapshots inherit user properties from their dataset when read.** An unlabelled
  snapshot of a `volume` dataset reads back `role=volume`. Select snapshots by label only
  together with the expected role, and use `-o source` / `zfs get -s local` when "is it
  really labelled" matters.
- Labels are plain ZFS properties; inspect with
  `zfs list -t all -r -o name,simple-zfs-csi:role,simple-zfs-csi:owner <pool>`.
