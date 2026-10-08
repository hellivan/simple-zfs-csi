# TODO

> Progress tracking for the group snapshot work: [IMPLEMENTATION_PROGRESS.md](IMPLEMENTATION_PROGRESS.md).

## Implement ADR-0038 in the existing code: DONE (see IMPLEMENTATION_PROGRESS.md Phase A)

## Implement `ZfsGroupSnapshot` (ADR-0039)

See [docs/volumegroupsnapshot-design.md](docs/volumegroupsnapshot-design.md) (the
revised note at the top). The SQL `vgs-*` todos carry the breakdown. Open details:

- Rename note: the doc and todos used `ZfsVolumeGroupSnapshot`; the kind is now
  `ZfsGroupSnapshot`.
- `Spec.GroupSnapshotID` on `ZfsSnapshot`: a child with it set never runs
  `zfs snapshot`; it adopts the raw snapshot.
- Group finalizer + explicit child deletion from `Spec.Members`; no `ownerReference`.
- `GetVolumeGroupSnapshot` / `DeleteVolumeGroupSnapshot` compare `snapshot_ids` with
  `Spec.Members`.
- RPC error codes: see the ADR-0039 tables (cross-pool Create is `FAILED_PRECONDITION`;
  `DeleteSnapshot` on a member and any `snapshot_ids` mismatch are `INVALID_ARGUMENT`;
  Get with a lost member is `FAILED_PRECONDITION`).
- Finalizer deleting before `provisionedAt`: destroy the raw snapshot of any member
  that has no child CR.
- Optional later: record `createtxg` (see FUTURE_OPTMIZATIONS.md) and `snapshotTakenAt`
  (the real on-disk timestamp).

### `ZfsGroupSnapshot` implementation checklist (authoritative; design in ADR-0038/0039/0040)

Order: 1 -> 9. Each item lists what must be true when it is done.

1. **Variadic `ZFS.Snapshot(ctx, names ...string)`** (`internal/zpool/zfs.go`): one `zfs snapshot`
   exec with all names; all exist = no-op; some exist = distinct error (never fill the gap).
   Update every fake `ZFS` and call site.
2. **`ZfsGroupSnapshot` CRD** (cluster-scoped, finalizer, no `ownerReference`). Spec: `poolGUID`,
   `members[]` fixed at creation (`sourceVolume`, `dataset`, `sourceType`, `sourceFSType`,
   `sourceVolblocksize`, `sourceProperties`, `snapshotName` = own random `csi-snap-<uuid>`,
   `childSnapshotName` = own random, equals the CSI `snapshot_id`, persisted once). Status:
   `phase` Ready/Pending/Error/Lost (derived), `creationTime` (once), `provisionedAt`
   (write-once, first all-Ready), `readyToUse`, `message`, `conditions`. Regenerate CRD and deepcopy.
3. **`ZfsSnapshot.Spec.GroupSnapshotID`** (immutable). `snapshotMessage` sets `group_snapshot_id`.
   A child with it never runs `zfs snapshot`. If its raw snapshot or source dataset cannot be
   found it sets `phase=Error` with a clear message ("raw snapshot X not found; group snapshots
   are never re-taken"), never staying silently Pending; the group derives Error from it.
   `ControllerServer.DeleteSnapshot` returns `INVALID_ARGUMENT` for a member (missing CR = OK).
   No CRD-layer deletion guard.
4. **`ZfsGroupSnapshotReconciler`** (per node, gated like `ZfsSnapshotReconciler`; extract
   `resolveDatasetPath` from `sourceDatasetPath`). Order: three-way check of the raw snapshots
   (all exist: adopt; none: one variadic exec; some: `Error`) -> read `creationTime` once ->
   create children (only after the exec succeeded; comment at both call sites) -> `provisionedAt`
   at the first all-Ready. Before `provisionedAt` a missing child may be recreated; after it
   nothing is created. Refuse the exec if a source volume is `Terminating`. Status derived from
   `Spec.Members` children (cache, no ZFS). Finalizer: delete each child from `Spec.Members`
   (NotFound = done), wait until all are gone, release. Before `provisionedAt`, destroy the raw
   snapshot of any member with no child CR. A `Terminating` group creates nothing.
5. **`GroupControllerServer`** (`internal/csi/groupcontroller.go`): the Create/Delete/Get tables in
   ADR-0039. Cross-pool = `FAILED_PRECONDITION`; Get with a lost member = `FAILED_PRECONDITION`;
   `snapshot_ids` mismatch (set comparison against `Spec.Members`) = `INVALID_ARGUMENT`.
6. **Wiring:** `GROUP_CONTROLLER_SERVICE` in `Identity.GetPluginCapabilities`,
   `GroupControllerGetCapabilities` advertising `CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT`, optional
   group server in `csi.Serve`, constructed in the controller Deployment entrypoint only.
7. **Helm/RBAC:** agent gets `create;delete` on `zfssnapshots` and get/list/watch/update/patch on
   `zfsgroupsnapshots` (+ `/status`, `/finalizers`); controller gets get/list/watch/create/delete on
   `zfsgroupsnapshots`; csi-snapshotter `--enable-volume-group-snapshots`; document the
   `groupsnapshot.storage.k8s.io` CRDs and snapshot-controller flag; CRD install (Helm never
   upgrades `crds/`).
8. **Tests:** variadic exec (single process, mixed existence); reconciler (gating, atomic create
   with all raw snapshots before any child, `creationTime` once, `provisionedAt` once and never
   again, nothing recreated after it, Lost derivation, finalizer waits and orphan raw-snapshot
   cleanup, Terminating creates nothing, pool takeover); child never snapshots and fails loud;
   RPC tables (Create codes, set comparison and reorder, Terminating `ABORTED`, Delete mismatch,
   Get decision table, `DeleteSnapshot` refusal).
9. **End-to-end** (k8s with group snapshot CRDs, csi-snapshotter v7+): one exec with both dataset
   names in the agent log, restore both members, delete tears down cleanly, cross-pool group gets
   `FAILED_PRECONDITION`, deleting one member `VolumeSnapshot` is blocked by upstream while the
   group exists, a member restore still works after another member is deleted.

## Implement ADR-0041: the volume delete looks only at ZFS

- Principle: Ready means complete; the volume delete never waits for unfinished work.
- Remove `checkSnapshotDependents` (D3) and its call in the `ZfsDatasetReconciler` delete path.
- Remove the live-CR clause from `assertDriverSnapshot` (keep the name allow-list, D18).
- In `detachAndCleanSnapshots`, each round: first destroy every driver-named snapshot with
  no clones (still passing the allow-list), then promote the clones of what remains.
- Tests: clone-free `csi-snap-*` destroyed before the promote; cloned snapshot promoted, not
  destroyed; snapshot of a non-Ready or `Error` `ZfsSnapshot` no longer blocks the volume delete;
  foreign snapshot still refused. Update docs that cite D3 (`snapshot-lifecycle-redesign.md` is
  historical; update `lifecycle-protection-matrix.md` and `runbooks.md` if they describe the wait).
- Update the comments that cite D3/D21/the claim check: `promote.go` (~70, 123-170, 350-373) and
  `zfsdataset_controller.go:111`; update `csi-technical-reference.md` tables (89-94, 124) and
  mark `redesign-strategy.md` as decided. Group snapshots: upstream adds no source-PVC protection.
- Remove `checkPendingCloneDependents` (D21) and its call too, with its tests; a pending
  restore whose source is gone fails loudly instead of the source delete waiting.
  Check `ZfsDatasetReconciler` for the error a clone reports when its source is missing and make
  sure it is a clear `Error`, not an endless silent retry.

## Remove the `provisionedAt` migration fallback

`status.provisionedAt` (ADR-0034 snapshots, ADR-0037 datasets) is the single
marker for "this object has existed, never create it again". Objects created
before it existed do not carry it. They are handled by a one-off fallback that
treats phase `Ready`/`Lost` as provisioned and backfills the field on the next
reconcile:

- `datasetProvisioned` in `internal/controller/zfsdataset_controller.go`
- the `snap.Status.Phase == Ready || Lost` check in `ZfsSnapshotReconciler.Reconcile`
  and the backfill branch in `reconcileSettled`

There is no separate migration job: the agents backfill when they reconcile each
object after upgrade. `provisionedAt` comes from the Ready condition's
`lastTransitionTime` (the real first-Ready time for a continuously Ready object),
and `creationTime` is read from ZFS, which only the agent on the pool's node can do.

### Upgrade order

1. `make install-crd` (Helm never upgrades `crds/`); otherwise the API server
   prunes `provisionedAt` and it is never persisted.
2. Upgrade the workload; agents backfill on their first reconcile.
3. Verify every object has it:

   ```sh
   kubectl get zfsdatasets  -o jsonpath='{range .items[?(!@.status.provisionedAt)]}{.metadata.name}{"\n"}{end}'
   kubectl get zfssnapshots -o jsonpath='{range .items[?(!@.status.provisionedAt)]}{.metadata.name}{"\n"}{end}'
   ```

   Both must print nothing. Anything listed is either not yet reconciled, hosted
   on an offline pool, or (a `Lost`/`Error` object) not recoverable
   automatically — patch `status.provisionedAt` by hand or delete the object.
4. Then delete the fallback code above and its legacy tests
   (`*LegacyReadyIsBackfilled`).

State at the time of writing (2026-09-30, `admin@kube-sl-home`): 91 datasets and
30 snapshots, all `Ready`, so all are backfilled automatically.

## Possible follow-up: act on a `provisionedAt` mismatch

Nothing compares `creationTime` with `provisionedAt` yet (later = replaced,
earlier = adopted). A ZFS `guid` would be
a stronger identity than a timestamp (it survives `zfs rename`); decide what a
mismatch should do (warn only? `Lost`?) before wiring it in.

## Decide: adoption of an already-existing dataset

A new `ZfsDataset` whose ZFS dataset already exists is adopted, not created: no
properties or ownership are applied, but `ensureSize` still sets `refquota` (and
grows a zvol). For a clone-sourced spec the existing dataset is not verified to
be a clone of `spec.source`. `creationTime` earlier than `provisionedAt` marks an
adopted dataset. Decide whether adoption should skip resizing, verify the origin,
or need an explicit marker.

## Note: per-object reconcile rate

Objects are reconciled about every 30s, not every 10h: the discovery agent
rewrites `ZfsPool.status.lastUpdated` every 30s and the dataset/snapshot
controllers watch `ZfsPool` unfiltered. This is what detects a vanished
primitive quickly, and it means any per-reconcile ZFS call is multiplied by
every object (about 120 today). Adding a predicate that ignores
`lastUpdated`-only pool changes would cut the load but also that detection.
