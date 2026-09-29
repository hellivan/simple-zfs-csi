# TODO

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
