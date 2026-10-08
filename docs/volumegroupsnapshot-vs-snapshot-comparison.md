# Single `ZfsSnapshot` vs. `VolumeGroupSnapshot`: side-by-side comparison

> **Note (2026-10-07):** the group lifecycle was revised, see ADR-0038/0039 in
> [design-decisions.md](design-decisions.md). Where this comparison says a group uses an
> ownerReference cascade or has no finalizer, or that a child delete is guarded, the ADRs win:
> the group has a finalizer, deletes its children from `Spec.Members`, and children are never
> blocked from deletion.

This document exists purely for review: a full end-to-end walkthrough of the
**existing, shipped** single-snapshot mechanism next to the **planned**
group-snapshot mechanism (see
[volumegroupsnapshot-design.md](volumegroupsnapshot-design.md) for the full
rationale), using one realistic scenario throughout — a PostgreSQL instance
with a `pg-data` PVC and a `pg-wal` PVC — so every actor, resource, and step
can be compared directly. Goal: confirm the new mechanism is additive and
doesn't contradict or duplicate anything the existing one already does.

## The scenario

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: pg-data, namespace: prod}
spec: {storageClassName: zfs-fs, resources: {requests: {storage: 100Gi}}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: pg-wal, namespace: prod}
spec: {storageClassName: zfs-fs, resources: {requests: {storage: 20Gi}}}
```

External-provisioner names the resulting `ZfsDataset` (and CSI volume id)
after the PV, e.g.:

- `pg-data` → `ZfsDataset/pvc-3f9a7e12-88b1-4e2a-9c0d-1a2b3c4d5e6f`,
  `Spec.Dataset: k8s/pvc-3f9a7e12-...`, `Spec.PoolGUID: 7fa2c9e1...` (pool `tank`)
- `pg-wal` → `ZfsDataset/pvc-9b4d21a0-55e3-4a10-8e7f-2c3d4e5f6a7b`,
  `Spec.Dataset: k8s/pvc-9b4d21a0-...`, same `Spec.PoolGUID` (same pool `tank`
  — required for the group case, and true here since both PVCs use the same
  `StorageClass`/pool in this example).

---

## Part 1 — Existing mechanism: single `ZfsSnapshot` (e.g. snapshotting `pg-data` alone)

### Actors

| Actor | Runs where | Role |
|---|---|---|
| `snapshot-controller` | cluster-wide, unprivileged | Watches `VolumeSnapshot`/`VolumeSnapshotContent`, drives binding |
| `csi-snapshotter` sidecar | alongside `ControllerServer`, in the CSI controller Deployment | Translates `VolumeSnapshotContent` into `CreateSnapshot`/`DeleteSnapshot` gRPC calls |
| `ControllerServer` | CSI controller Deployment (unprivileged, **no ZFS access**) | Implements `CreateSnapshot`/`DeleteSnapshot`/`ListSnapshots`; only talks to the K8s API |
| `ZfsSnapshotReconciler` | per-node DaemonSet (privileged, **has `zpool.ZFS`**) | Only one instance acts: the one on the node currently hosting the pool (`pool.Status.CurrentNode`) |

### Resources

| Resource | Scope | Created by |
|---|---|---|
| `VolumeSnapshotClass` | cluster | user |
| `VolumeSnapshot` (`prod/pg-data-snap`) | namespaced | user |
| `VolumeSnapshotContent` | cluster | `snapshot-controller` |
| `ZfsSnapshot` (cluster, **this driver**) | cluster | our `ControllerServer.CreateSnapshot` |

### Flow: Create

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: pg-data-snap, namespace: prod}
spec:
  volumeSnapshotClassName: zfs-snap
  source: {persistentVolumeClaimName: pg-data}
```

1. `snapshot-controller` creates `VolumeSnapshotContent`
   (`Spec.Source.VolumeHandle: pvc-3f9a7e12-...`).
2. `csi-snapshotter` calls
   `CreateSnapshot(name="snapshot-<uid>", source_volume_id="pvc-3f9a7e12-...")`.
3. `ControllerServer.CreateSnapshot` (`internal/csi/snapshot.go`):
   - `Get()`s the source `ZfsDataset` `pvc-3f9a7e12-...`.
   - Captures its identity into a `desired` spec **right now**, because a
     snapshot must outlive its source (D25): `PoolGUID: 7fa2c9e1...`,
     `Dataset: k8s/pvc-3f9a7e12-...`, `SourceType: filesystem`,
     `SourceFSType: xfs`, `SourceProperties: {...}`.
   - Generates an opaque `SnapshotName: csi-snap-<uuid-A>` — never the CO name.
   - `ensureSnapshot`: creates
     `ZfsSnapshot/snapshot-<uid>` with that spec (idempotent by object name =
     the CO-supplied `name`).
   - `waitSnapshotReady`: polls `ZfsSnapshot/snapshot-<uid>` until
     `Status.Phase == Ready`.
4. On `tank`'s hosting node, `ZfsSnapshotReconciler.Reconcile`:
   - Confirms `pool.Status.CurrentNode == r.NodeName`.
   - Adds `zfsSnapshotFinalizer`.
   - Resolves the live dataset path (`resolveDatasetPath`, via `SourceVolume`).
   - Idempotent check: `ZFS.Get(tank/k8s/pvc-3f9a7e12-...@csi-snap-<uuid-A>, "type")`
     → `NotExist` → `ZFS.Snapshot(ctx, "tank/k8s/pvc-3f9a7e12-...@csi-snap-<uuid-A>")`
     (**one name**, single exec).
   - `reconcileBackingClone`: clones that raw snapshot into a flat sibling
     `tank/k8s/csi-snap-<uuid-A>` (`canmount=off`), then takes its own
     `@restore-source` self-snapshot.
   - Sets `Status = {Phase: Ready, ReadyToUse: true, CreationTime: ..., RestoreSize: ...}`.
5. `CreateSnapshot` returns `csi.Snapshot{SnapshotId: "snapshot-<uid>",
   SourceVolumeId: "pvc-3f9a7e12-...", ReadyToUse: true}`.
6. `snapshot-controller` marks `VolumeSnapshot/pg-data-snap` bound.

### Flow: Restore

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: pg-data-restored, namespace: prod}
spec:
  dataSource: {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: pg-data-snap}
```
`CreateVolume` clones the backing clone's `@restore-source` snapshot — never
the raw snapshot directly (D0/§3.1) — into a fresh `ZfsDataset`.

### Flow: Delete

1. User deletes `VolumeSnapshot/pg-data-snap`.
2. `csi-snapshotter` calls `DeleteSnapshot(snapshot_id="snapshot-<uid>")`.
3. `ControllerServer.DeleteSnapshot`: `client.Delete` on
   `ZfsSnapshot/snapshot-<uid>` — returns immediately, does **not** wait.
4. `zfsSnapshotFinalizer` blocks actual removal until
   `ZfsSnapshotReconciler.reconcileDelete`:
   - Promotes away any dependent clones of the backing clone, destroys it.
   - Finds the raw snapshot's *actual* current location (handles a prior
     `zfs promote` relocation, ADR-0028), destroys it.
   - Releases the finalizer.
5. Object fully gone; name `snapshot-<uid>` is now reusable.

---

## Part 2 — Planned mechanism: `VolumeGroupSnapshot` for `pg-data` + `pg-wal` together

### Actors — what's new vs. reused

| Actor | Status | Role |
|---|---|---|
| `snapshot-controller` | **reused, unmodified** | Also watches `VolumeGroupSnapshot`/`VolumeGroupSnapshotContent`; auto-creates one `VolumeSnapshot`/`VolumeSnapshotContent` pair **per member** |
| `csi-snapshotter` sidecar | **reused**, new sidecar flag | Its group-controller code path translates to `CreateVolumeGroupSnapshot`/`DeleteVolumeGroupSnapshot` |
| `GroupControllerServer` | **new**, same Deployment as `ControllerServer`, **no ZFS access** (verified: `ControllerServer` today has only `Client client.Client`) | Implements the 3 group RPCs |
| `ZfsGroupSnapshotReconciler` | **new**, same DaemonSet as `ZfsSnapshotReconciler`, **has `zpool.ZFS`** | Only the node hosting the pool acts (identical gating) |
| `ZfsSnapshotReconciler` | **reused, unmodified** | Drives each child exactly like a standalone snapshot |

### Resources — what's new vs. reused

| Resource | Scope | Created by | New? |
|---|---|---|---|
| `VolumeGroupSnapshotClass` | cluster | user | k8s-native, not ours |
| `VolumeGroupSnapshot` (`prod/pg-consistent-snap`) | namespaced | user | k8s-native |
| `VolumeGroupSnapshotContent` | cluster | `snapshot-controller` | k8s-native |
| `VolumeSnapshot` × 2 (auto) | namespaced | `snapshot-controller` | k8s-native, **auto-created, we never touch these** |
| `VolumeSnapshotContent` × 2 (auto) | cluster | `snapshot-controller` | k8s-native |
| `ZfsGroupSnapshot` (**new CRD**) | cluster | our `GroupControllerServer` | **new** |
| `ZfsSnapshot` × 2 | cluster | our new `ZfsGroupSnapshotReconciler` | **existing type**, +1 new field |

### Flow: Create

```yaml
apiVersion: groupsnapshot.storage.k8s.io/v1beta2
kind: VolumeGroupSnapshotClass
metadata: {name: zfs-group-snap}
driver: storage.simple-zfs-csi.io
deletionPolicy: Delete
---
apiVersion: groupsnapshot.storage.k8s.io/v1beta2
kind: VolumeGroupSnapshot
metadata: {name: pg-consistent-snap, namespace: prod}
spec:
  volumeGroupSnapshotClassName: zfs-group-snap
  source:
    selector: {matchLabels: {app: postgres, role: primary}}
    # pg-data and pg-wal PVCs both carry this label
```

1. `snapshot-controller` resolves the selector to both PVCs, creates
   `VolumeGroupSnapshotContent`
   (`Spec.Source.VolumeHandles: [pvc-3f9a7e12-..., pvc-9b4d21a0-...]`).
2. `csi-snapshotter` calls
   `CreateVolumeGroupSnapshot(name="groupsnapshot-<uid>",
   source_volume_ids=["pvc-3f9a7e12-...", "pvc-9b4d21a0-..."])`.
3. `GroupControllerServer.CreateVolumeGroupSnapshot` — **directly parallel to
   step 3 above, done twice plus one extra check**:
   - `Get()`s both source `ZfsDataset`s.
   - **New check with no single-snapshot analogue**: both must share one
     `PoolGUID` (`7fa2c9e1...` here) — else `codes.InvalidArgument`, nothing
     created. (If `pg-wal` lived on a second pool, this is where it's caught.)
   - Captures each source's identity into its own member entry — **the exact
     same D25 capture as step 3 above, just done per-member**:
     ```yaml
     members:
       - sourceVolume: pvc-3f9a7e12-...
         dataset: k8s/pvc-3f9a7e12-...
         sourceType: filesystem
         sourceFSType: xfs
         snapshotName: csi-snap-<uuid-A>
         zfsSnapshotRef: groupsnapshot-<uid>-<uuid-A2>
       - sourceVolume: pvc-9b4d21a0-...
         dataset: k8s/pvc-9b4d21a0-...
         sourceType: filesystem
         sourceFSType: xfs
         snapshotName: csi-snap-<uuid-B>
         zfsSnapshotRef: groupsnapshot-<uid>-<uuid-B2>
     ```
   - Each member gets its **own** independently random `SnapshotName`
     (`"csi-snap-" + uuid.New().String()`) and `ZfsSnapshotRef` — generated
     **exactly** like a standalone `CreateSnapshot`, not a scheme unique to
     groups. (An earlier draft used one shared name across members; verified
     against `zfs-snapshot(8)` that atomicity never required a shared suffix,
     and a shared name would have broken `FindSnapshot`'s documented "at most
     one match per pool" invariant used during promote-relocation — so every
     member keeps its own name, just like today.)
   - Get-or-creates `ZfsGroupSnapshot/groupsnapshot-<uid>` (idempotent by
     object name = CO-supplied `name`, same pattern as `ensureSnapshot`).
   - Polls until `Status.Phase == Ready` (same shape as `waitSnapshotReady`,
     extended to N members, AND-ed).
4. On `tank`'s hosting node, `ZfsGroupSnapshotReconciler.Reconcile` —
   **the one genuinely new piece of ZFS-facing logic**:
   - Same hosted-here gate as `ZfsSnapshotReconciler`.
   - **No finalizer** — cleanup is owner-reference GC, same as
     `ZfsShare`→`NetworkExport` (see design doc for why this is safe now that
     names are independently random).
   - Resolves both members' live dataset paths (`resolveDatasetPath`, shared
     free function).
   - Idempotent 3-way check (see collision-handling below): neither
     `tank/k8s/pvc-3f9a7e12-...@csi-snap-<uuid-A>` nor
     `tank/k8s/pvc-9b4d21a0-...@csi-snap-<uuid-B>` exists yet →
   - `ZFS.Snapshot(ctx, "tank/k8s/pvc-3f9a7e12-...@csi-snap-<uuid-A>",
     "tank/k8s/pvc-9b4d21a0-...@csi-snap-<uuid-B>")` — **one exec, two
     different names, one dataset each** — this is the single atomic txg
     call; contrast with step 4 above, which passes exactly one name because
     there is only one dataset. (Atomicity comes from one exec/one txg, not
     from the names matching — verified against `zfs-snapshot(8)`: "creates a
     snapshot of a dataset or multiple snapshots of **different** datasets".)
   - **Only now**, creates the two child `ZfsSnapshot`s
     (`groupsnapshot-<uid>-<uuid-A2>`, `groupsnapshot-<uid>-<uuid-B2>`), each
     with `GroupSnapshotID: groupsnapshot-<uid>` set and the same
     `PoolGUID`/`Dataset`/`SourceType`/`SourceFSType`/`SnapshotName` fields a
     standalone `CreateSnapshot` would have written (`csi-snap-<uuid-A>` and
     `csi-snap-<uuid-B>` respectively — each member's own name, not shared).
   - **From here on, each child is indistinguishable from a standalone
     snapshot**: picked up by the existing, unmodified
     `ZfsSnapshotReconciler`, whose own idempotent check finds the raw
     snapshot already present (no-op) and proceeds straight into
     `reconcileBackingClone` — **identical to step 4 above**, run twice,
     independently, no coordination needed between the two (the hard part —
     same-instant raw snapshot creation — is already done).
   - Once both children report `Ready`, rolls that up into its own `Status`.
5. `GroupControllerServer` returns both members via the existing
   `snapshotMessage()` helper (unmodified), each with
   `GroupSnapshotId: "groupsnapshot-<uid>"` set.
6. `snapshot-controller`'s common-controller (not our code) auto-creates:
   - `VolumeSnapshot/<auto>` + `VolumeSnapshotContent/<auto>` for `pg-data`
     (`Spec.Source.VolumeHandle: pvc-3f9a7e12-...`)
   - `VolumeSnapshot/<auto>` + `VolumeSnapshotContent/<auto>` for `pg-wal`

### Flow: Restore

**Identical to Part 1, done twice, completely independently** — no
group-aware restore API exists:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: pg-data-restored, namespace: prod}
spec:
  dataSource: {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: <auto pg-data VolumeSnapshot>}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: pg-wal-restored, namespace: prod}
spec:
  dataSource: {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: <auto pg-wal VolumeSnapshot>}
```

The consistency guarantee that mattered — both raw snapshots came from the
same txg — is already baked into each backing clone's own `@restore-source`
snapshot by the time restore happens; restore itself needs no group awareness
at all.

### Flow: Delete

1. User deletes `VolumeGroupSnapshot/pg-consistent-snap`.
2. `csi-snapshotter` calls
   `DeleteVolumeGroupSnapshot(group_snapshot_id="groupsnapshot-<uid>", ...)`.
3. `GroupControllerServer.DeleteVolumeGroupSnapshot`: `client.Delete` on
   `ZfsGroupSnapshot/groupsnapshot-<uid>` — returns immediately, same
   fire-and-forget style as `DeleteSnapshot` above.
4. The parent has **no finalizer**, so it's removed from etcd immediately.
   Kubernetes' owner-reference garbage collector then asynchronously deletes
   both owned children in the background (`SetControllerReference`/`Owns()`,
   same pattern as `ZfsShare`→`NetworkExport` — no new controller code).
5. Each child's `zfsSnapshotFinalizer` blocks *its* removal exactly as in
   step 4 of Part 1's delete flow — **same code path, same ordering, same
   promote/relocate handling, run twice, independently**.
6. Because each member's on-disk name and CR name were independently random
   (never derived from the parent's name), a same-name retried
   `CreateVolumeGroupSnapshot` while old children are still `Terminating`
   always mints fresh names for the new incarnation — no collision, so
   skipping the parent finalizer is safe. (An earlier draft shared one name
   across members and *did* need a parent finalizer for exactly this reason —
   see the design doc for the full back-and-forth.)

---

## Direct comparison table

| Aspect | Single `ZfsSnapshot` (existing) | `VolumeGroupSnapshot` (planned) |
|---|---|---|
| Raw ZFS operation | `zfs snapshot ds@x` (1 name) | `zfs snapshot ds1@x1 ds2@x2` (N independent names, **1 exec**, same primitive) |
| Who issues it | `ZfsSnapshotReconciler` | **new** `ZfsGroupSnapshotReconciler` (sibling, not a fork) |
| Who does backing clone / restore-source / destroy-ordering | `ZfsSnapshotReconciler.reconcileBackingClone` / `reconcileDelete` | **same code, unmodified**, driven per-child |
| CSI controller ZFS access | none (`Client` only) | none (**same constraint, respected**) |
| Idempotency key | object name = CO `name` | object name = CO `name` (**same pattern**) |
| Source-identity capture timing | at `CreateSnapshot`, before object exists (D25) | at `CreateVolumeGroupSnapshot`, before object exists (**same D25 reasoning, per member**) |
| Cross-pool handling | N/A (single dataset) | **new**: rejected with `InvalidArgument` before anything is created (verified against kernel: ZFS itself would refuse this too, at the `dsl_sync_task`/`dsl_pool_t` level) |
| Pre-existing-name collision handling | ZFS `EEXIST` on the one name, idempotent check no-ops or errors | ZFS `EEXIST` verified to abort the *entire* multi-name batch (kernel `check`-before-`sync`); driver adds a 3-way idempotent check (all/none/mixed) so it never retries a subset |
| Finalizer | `zfsSnapshotFinalizer`, does real `zfs destroy` work | **none** — cleanup via owner-reference GC, same as `ZfsShare`→`NetworkExport` (safe because member names are independently random, never derived from the parent's name) |
| `DeleteSnapshot` (single RPC) behavior on a member | N/A | **new**: rejected (`InvalidArgument`) via new `GroupSnapshotID` field — mandatory CSI spec rule |
| Auto-created per-PVC `VolumeSnapshot` | is the primary object | **same objects**, just auto-created by k8s' own common-controller instead of the user |
| Restore path | clone backing clone's `@restore-source` | **identical**, per member, independently |

## Conclusion

Every piece of the existing single-snapshot mechanism — object model,
idempotency pattern, source-identity capture, per-member independent naming,
backing-clone/restore-source mechanics, destroy ordering — is reused as-is or
mirrored with the same shape one level up. The only genuinely new ZFS-facing
behavior is the one atomic multi-name `zfs snapshot` exec and the thin,
finalizer-free parent CRD that triggers it and records membership. Nothing in
the planned design changes the meaning, behavior, or code path of a standalone
`ZfsSnapshot` created via plain `CreateSnapshot` — those keep working exactly
as they do today, `GroupSnapshotID` empty.
