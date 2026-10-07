# VolumeGroupSnapshot Support — Design & Implementation Plan

**Status: design, not yet implemented.** This document is the reviewable plan
before any code is written; see the SQL-tracked `vgs-*` todos for the
implementation breakdown once approved.

> **Revised 2026-10-07 — read this first.** The decisions in
> [ADR-0038](design-decisions.md) and [ADR-0039](design-decisions.md) are the current
> design and override any conflicting text below. In short:
>
> - The resource is a plain **`ZfsGroupSnapshot`** (no `Job` in the name).
> - Order: members recorded in `Spec` -> one atomic exec -> `status.creationTime` read
>   once -> child `ZfsSnapshot`s created (they never run `zfs snapshot`) ->
>   **`provisionedAt` when all children are Ready for the first time**.
> - After `provisionedAt` the group creates nothing again. Before it, a missing child
>   may be recreated. Deletion always works.
> - Ready/Lost is **derived from the children** in `Spec.Members` (API cache, no ZFS).
> - The group has a **finalizer**; deletion deletes each child from `Spec.Members`
>   directly and waits until all are gone. **No `ownerReference`.**
> - Children keep `GroupSnapshotID` but are **never blocked from deletion**.
> - `snapshot_ids` on Get/Delete is compared with **`Spec.Members`**, not live children.
> - `snapshotTakenAt` is not needed now (possible later addition: the real on-disk
>   timestamp). `provisionedAt` is not `creation_time`.
>
> Sections superseded by this note are marked **SUPERSEDED** below: "Design decision: one
> thin new CRD" (ownerReference cascade), "No finalizer on the parent", "Flow: Create"
> step 6 (order), "Flow: Delete", "Flow: single-snapshot Delete rejection", and the
> section on the atomic snapshot being taken at most once.

## Motivation

Applications that split related state across multiple PVCs — the driving
example is PostgreSQL, whose data directory and WAL are frequently separate
volumes — need those volumes snapshotted at *exactly* the same instant.
Snapshotting them independently, even microseconds apart, can produce a WAL
that doesn't line up with the data directory it's meant to replay against:
silent corruption on restore. Kubernetes' `VolumeGroupSnapshot` API
(`groupsnapshot.storage.k8s.io`) exists specifically for this case, and CSI's
`GroupController` service is the vendor-facing mechanism for implementing it.

## Verified facts this design relies on (not assumptions)

- **ZFS multi-dataset snapshot is genuinely atomic**, verified against OpenZFS
  master source, not documentation prose:
  - `zfs_do_snapshot` (`cmd/zfs/zfs_main.c`) builds one nvlist across every
    `dataset@snap` argument and calls `zfs_snapshot_nvl` **once** → one ioctl.
  - `dsl_dataset_snapshot()` (`module/zfs/dsl_dataset.c`) wraps the whole
    multi-name request in **one `dsl_sync_task` → one transaction group
    (txg)**. Kernel comment: *"All-or-nothing: if there are any failures,
    nothing will be modified"*; *"All snapshots created by one request are
    taken atomically in a single txg."*
  - Cross-pool names in one request fail safely: `dsl_sync_task(firstname,...)`
    binds to a single `dsl_pool_t`; a name from a different pool can't resolve
    against it, aborting the whole txg — no partial state is possible.
  - **Load-bearing consequence:** the driver must issue exactly **one** exec
    with all dataset names as arguments. Looping per-dataset destroys the
    atomicity guarantee entirely — this is the single most important
    invariant in this entire feature.
- **CSI spec v1.11.0** (already vendored, no proto upgrade needed) defines the
  full `GroupController` service and `GROUP_CONTROLLER_SERVICE` capability.
  The regular `Snapshot` message has a `group_snapshot_id` field (field 6),
  and the spec text mandates that `DeleteSnapshot` **must reject**
  (`INVALID_ARGUMENT`) any snapshot with a non-empty `group_snapshot_id` — the
  CO must use `DeleteVolumeGroupSnapshot` instead.
- **Kubernetes' own `common-controller`** (verified in
  `pkg/common-controller/groupsnapshot_controller_helper.go` of
  `external-snapshotter`) automatically creates one real, standard, namespaced
  `VolumeSnapshot` + `VolumeSnapshotContent` pair **per group member**, wired
  back to the source PVC. **The driver never creates these itself** — it only
  needs to return correct `csi.Snapshot` entries in
  `CreateVolumeGroupSnapshotResponse`. Restore is the completely standard
  single-PVC `dataSource: VolumeSnapshot` path; no group-aware restore API
  exists.
- The CSI controller pod (`internal/csi.ControllerServer`) **has no ZFS access
  at all** — only `Client client.Client`. Only the per-node DaemonSet agent
  (`ZfsSnapshotReconciler`, gated on `pool.Status.CurrentNode == r.NodeName`)
  has a `zpool.ZFS` handle. So the one atomic `zfs snapshot ds1@x ds2@x ...`
  call **must** happen in a new per-node reconciler, never in the CSI
  controller RPC handler.

## CSI spec-conformance audit (full pass against vendored `spec.md`)

A dedicated pass against the vendored CSI spec
(`github.com/container-storage-interface/spec@v1.11.0/spec.md`, `GroupController`
service, lines ~392-3145), cross-checked against existing code precedent.
Findings that changed the plan:

1. **`GroupControllerGetCapabilities` is a required RPC, separate from the
   `Identity`-level flag.** Spec: *"A Plugin that implements GroupController
   MUST implement this RPC call"* (`spec.md:2892`). This is a **second**,
   independent capability-advertisement layer from `Identity.GetPluginCapabilities`
   — exactly mirroring how the existing `ControllerServer` already implements
   both `Identity.GetPluginCapabilities` (advertises `CONTROLLER_SERVICE`,
   `identity.go:34`) **and** its own `ControllerGetCapabilities` (advertises
   `CREATE_DELETE_VOLUME` etc., `controller.go:324`). `GroupControllerServer`
   needs the same second method, advertising
   `GroupControllerServiceCapability_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT`.
2. **Idempotent-retry compatibility check needs to be a set comparison, not a
   scalar one.** Spec error table: retrying `CreateVolumeGroupSnapshot` with the
   same `name` but different `source_volume_ids`/`parameters` MUST return
   `codes.AlreadyExists` (6). `ensureSnapshot`'s standalone equivalent compares
   scalar fields (`PoolGUID`/`Dataset`/`SourceVolume`/`SourceType`); the group
   version must compare `PoolGUID` plus the **set** of `Spec.Members[].SourceVolume`
   values against the incoming `source_volume_ids`, order-independent — the spec does
   not guarantee `source_volume_ids` list order is stable across separate
   retried calls, so an ordered-list comparison would wrongly reject a
   legitimate idempotent retry.
3. **`DeleteVolumeGroupSnapshotRequest`/`GetVolumeGroupSnapshotRequest` both
   carry a required `snapshot_ids` field the spec expects the SP to validate
   when it can:** *"If SP does not need to rely on this field... it SHOULD
   check this field and report an error if it has the ability to detect a
   mismatch."* Since both handlers already read `Spec.Members` to do their
   real work, comparing `set(req.GetSnapshotIds())` against the set of child CR
   names is nearly free — mismatch → `codes.InvalidArgument` ("snapshot list
   mismatch"), the exact condition/message the spec's error table names.
4. **`parameters` (from `VolumeGroupSnapshotClass`) needs the same treatment as
   standalone snapshots, not a stricter one.** Checked what standalone
   `CreateSnapshot` actually does with `req.GetParameters()`
   (`internal/csi/snapshot.go:44`): there is **no allow-list of real
   parameters at all** — the only check is `rejectRemovedModeParam`, a
   narrow, defensive rejection of one specific *leftover* key (`mode:
   integrated`, from a removed feature) with everything else silently
   ignored. Chosen: run that exact same check for
   `CreateVolumeGroupSnapshot` too (so a stale class pointed at group
   snapshots gets the same actionable error), and otherwise leave unknown
   parameters unrejected — parity with the standalone path, not a new,
   stricter blanket rule. No group-specific ZFS parameter is defined by this
   driver today; if one is added later it should be validated identically on
   both paths.
5. **The group's reported `creation_time` should be read from the raw atomic
   snapshot directly, not derived from a child's `Status.CreationTime`.**
   Verified: a standalone `ZfsSnapshot.Status.CreationTime` is populated from
   the **backing clone's** `@restore-source` self-snapshot
   (`snapshotCreationTime(ctx, r.ZFS, restoreSourceFull)`,
   `zfssnapshot_controller.go:325`), taken independently per member, slightly
   *after* the real atomic instant — already an accepted per-child cosmetic
   nuance (see below). But `VolumeGroupSnapshot.creation_time` is a
   spec-required, headline field, and this feature's entire purpose is
   precise same-instant timing — it should carry the *true* value. The group
   reconciler should read the raw snapshot's own ZFS `creation` property
   directly (any one member — provably identical across all of them, same
   txg) right after the atomic exec succeeds, and store that on the
   **parent's own** `Status.CreationTime`, entirely independent of the
   per-child cosmetic value.

Checked and confirmed to need **no** change (stated explicitly so the scope of
what was actually verified is clear): there is no `ListVolumeGroupSnapshots`
RPC in the CSI spec at all; the `ready_to_use` AND-rollup across members
already matches the spec's wording exactly; `Secrets` is unused by every
existing standalone RPC in this driver too, so ignoring it for groups is
consistent, not an oversight; the "in use" `FAILED_PRECONDITION` condition
doesn't apply since this project's delete path never blocks on dependents
(D17, promote-based reparenting handles that instead); mapping any node-side
exec failure straight to `Status.Phase=Error` → generic `codes.Internal` is
pre-existing standalone behavior (`zfssnapshot_controller.go:143-146`), not a
new asymmetry introduced for groups.

## Design decision: one thin new CRD, maximum reuse of existing machinery

> **SUPERSEDED (ADR-0039, 2026-10-07):** the group-liveness guard and the owner-reference cascade described here are replaced by a parent finalizer with explicit child deletion; the CRD is `ZfsGroupSnapshot`.

Three earlier variants were considered and rejected before this one:

1. *Persistent CRD that also duplicates full per-member ZFS state* — rejected
   as needless duplication; the child `ZfsSnapshot` already holds that state.
2. *No new CRD; membership derived from a shared label across independently
   created `ZfsSnapshot`s* — rejected because it reintroduces a real race
   (waiting for N siblings to become visible via list/label, needing an
   expected-member-count field and informer-lag handling) that a parent object
   with an explicit, known-upfront member list simply doesn't have.
3. *The CSI controller runs `zfs snapshot` directly* — rejected outright: the
   controller pod has no ZFS access and no guarantee of running on the node
   that hosts the pool (see verified facts above).

**Chosen design:** a new, thin, cluster-scoped `ZfsGroupSnapshot` CRD.
Its `Spec.Members` list is fully known at creation time — no sibling discovery
needed. Its only jobs: trigger the one atomic multi-name ZFS exec, then create
one ordinary child `ZfsSnapshot` per member. Every other mechanism — backing
clone, restore-source snapshot, promote/relocate handling, `zfs destroy`
ordering — is the **existing `ZfsSnapshotReconciler`**, with exactly **one**
small, targeted addition to its delete path (a group-liveness guard against
out-of-band `kubectl delete zfssnapshot`, see "Flow: single-snapshot Delete
rejection" below) — otherwise unmodified.

## Resources involved

| Layer | Resource | Created by |
|---|---|---|
| K8s snapshot API | `VolumeGroupSnapshotClass` | user |
| K8s snapshot API | `VolumeGroupSnapshot` (namespaced) | user |
| K8s snapshot API | `VolumeGroupSnapshotContent` (cluster) | `snapshot-controller` (common-controller) |
| K8s snapshot API | `VolumeSnapshot` × N (namespaced) | `snapshot-controller`, automatically, one per member |
| K8s snapshot API | `VolumeSnapshotContent` × N (cluster) | `snapshot-controller`, automatically |
| This driver | `ZfsGroupSnapshot` (cluster, **new**) | our `GroupControllerServer` |
| This driver | `ZfsSnapshot` × N (cluster, existing type, one new field) | our new `ZfsGroupSnapshotReconciler` |

### `ZfsGroupSnapshot` (new CRD)

Kept deliberately thin — identifying info + status rollup only, no
duplication of per-member ZFS properties (those live solely on the child
`ZfsSnapshot`, matching the shape of upstream's own
`VolumeGroupSnapshotContent.Status.VolumeSnapshotInfoList`).

```yaml
apiVersion: storage.simple-zfs-csi.io/v1alpha1
kind: ZfsGroupSnapshot
metadata:
  name: groupsnapshot-<uid>          # == CSI group_snapshot_id
spec:
  poolGUID: abcd1234
  members:
    - sourceVolume: pvc-111...
      dataset: k8s/pg-data
      sourceType: filesystem
      sourceFSType: ext4                    # D25 capture, same as standalone
      sourceProperties: {recordsize: "128K"} # D25 capture, same as standalone
      snapshotName: csi-snap-<uuid-A>        # independently random, own suffix
      childSnapshotName: groupsnapshot-<uid>-<uuid-A2>  # own random CR name
    - sourceVolume: pvc-222...
      dataset: k8s/pg-wal
      sourceType: filesystem
      sourceFSType: ext4                     # D25 capture, same as standalone
      sourceProperties: {recordsize: "8K"}   # D25 capture, same as standalone
      snapshotName: csi-snap-<uuid-B>        # independently random, own suffix
      childSnapshotName: groupsnapshot-<uid>-<uuid-B2>  # own random CR name
status:
  phase: Ready | Pending | Error | Lost   # derived from the children (ADR-0039)
  provisionedAt: ...   # first time all children were Ready; write-once (ADR-0038)
  readyToUse: true
  creationTime: ...  # from the raw ZFS `creation` property, read once right
                      # after the atomic exec — NOT copied from any child's
                      # own Status.CreationTime; see spec-conformance finding 5
  observedGeneration: 1
  message: "..."
  conditions: [...]
```

Note there is deliberately no `groupSnapshotName`/shared short-name field.
`snapshotName` and `childSnapshotName` per member are generated **exactly**
like a standalone `ZfsSnapshot`'s `Spec.SnapshotName` (`"csi-snap-" +
uuid.New().String()`, `independent-resource-naming-redesign.md`) — no new
naming scheme, no `csi-groupsnap-` prefix, no positional suffix. Each member
is, structurally, an ordinary independent snapshot; the only thing that
makes it a *group* member is that its raw `zfs snapshot` call was issued in
the same batch as its siblings', and its `ZfsSnapshot.Spec.GroupSnapshotID`
points back at the parent. Correlating a child back to its source volume or
its group is always done via explicit spec fields (`SourceVolume`,
`GroupSnapshotID`), never by parsing any name — consistent with every other
identifier in this codebase.

### Why not one shared short name across members (bug found during review)

An earlier draft gave every member the *same* raw ZFS snapshot short name
(e.g. `csi-groupsnap-<uuid>`), reasoning that the one atomic
`zfs snapshot ds1@x ds2@x ...` exec needed a shared suffix. Verified against
the actual `zfs-snapshot(8)` man page, this is false: *"Creates a snapshot of
a dataset or multiple snapshots of **different** datasets"* — the synopsis
is `dataset@snapname ...`, an independent `snapname` per positional
argument. Atomicity comes from issuing one exec (one txg per OpenZFS's
`dsl_sync_task`, already verified against `dsl_dataset.c`), never from
sharing a suffix.

Sharing a suffix anyway would have been an active bug: `internal/zpool/zfs.go`'s
`FindSnapshot(ctx, pool, suffix)` — used by `zfssnapshot_controller.go`'s
`reconcileDelete` to relocate a snapshot after a `zfs promote` — is
documented and implemented on the assumption that *"driver snapshot short
names are `csi-snap-<uuid>` ... so at most one match can exist"* pool-wide.
A shared group short name would have multiple simultaneous matches on the
same pool for as long as the group exists, breaking that invariant during a
group member's delete/promote-relocation handling. Giving every member its
own independently random short name (as above) keeps `FindSnapshot`'s
"at most one match" invariant true unconditionally — **zero changes needed**
to `FindSnapshot`, its tests, or its documented contract.

A second, independent piece of evidence points the same way:
`internal/controller/promote.go`'s `driverSnapshotSuffix` regex
(`^(restore-source|clone-.+|csi-snap-.+)$`) is a deliberate **allow-list**
(D18) used during delete/promote-relocation to decide whether a snapshot is
driver-owned at all — anything not matching is treated as foreign and left
untouched. A distinct `csi-groupsnap-` prefix would silently fail this
allow-list and make group snapshots invisible to that safety check. Reusing
`csi-snap-<uuid>` verbatim (not a new prefix) makes every group member
indistinguishable from a standalone snapshot to *every* downstream
ZFS-facing check — no special-casing required anywhere in the delete path.

> **SUPERSEDED (ADR-0039):** the group now has a finalizer and no ownerReference; the paragraph below is the historical reasoning.

**No finalizer on the parent** (this went back and forth during review —
history below, because the reasoning matters for whoever touches this next).
Cleanup relies entirely on `controllerutil.SetControllerReference` +
`Owns()`: deleting the parent (no finalizer ⇒ removed from etcd immediately)
triggers Kubernetes' own owner-reference garbage collector to asynchronously
`Delete` every child `ZfsSnapshot` in the background; each child's *own*
`zfsSnapshotFinalizer` still drives its real `zfs destroy` before it actually
disappears. Same pattern as `ZfsShare`→`NetworkExport`. The parent CRD does
**no ZFS work and no cleanup-orchestration work of its own** — it is purely a
declarative record of group membership plus a status rollup.

**Why this went back and forth:** an earlier draft of this doc had each
child's on-disk `SnapshotName` **and** its CR name (`ChildSnapshotName`)
*deterministically* derived from the parent's own name
(`<group-name>-0`, `<group-name>-1`) — reusing one shared raw ZFS snapshot
short name across all members. That draft *did* need a parent finalizer: the
parent's name is the CO-supplied idempotency token for
`CreateVolumeGroupSnapshot`, which CSI callers may retry with the same name;
with deterministic child names, a fast `Delete` → retried `Create` sees no
parent (it was removed instantly without a finalizer), builds *the same*
child names again, and collides (`409 AlreadyExists`) with the prior
incarnation's children still `Terminating`. That's exactly the race the
standalone `ZfsSnapshot`'s own `zfsSnapshotFinalizer` exists to prevent, one
level down — so the group parent seemed to need the same protection.

That deterministic-naming draft was itself wrong for an independent,
verified reason (see next section): sharing one short name across multiple
datasets on the same pool breaks `FindSnapshot`'s documented "at most one
match" invariant. Fixing *that* bug — giving every member its own
independently random `SnapshotName` and `ChildSnapshotName`, exactly like a
standalone snapshot — also happens to remove the collision race the parent
finalizer was added for: a retried `Create` now always mints **fresh** random
names, which can never collide with a still-`Terminating` prior incarnation's
(different) names. So the finalizer's original justification no longer
applies, and the simpler `ZfsShare`/`NetworkExport`-style no-finalizer design
is correct after all.

### `ZfsSnapshot` (existing CRD, one new field)

`Spec.GroupSnapshotID string` — empty for standalone snapshots, set to the
owning `ZfsGroupSnapshot`'s name for group members. Used for two things:
(1) making the single-snapshot `DeleteSnapshot` RPC reject group members per
the CSI spec's mandatory rule, and (2) letting `ZfsSnapshotReconciler`'s
delete path check whether the parent group still exists before proceeding
(see "Flow: single-snapshot Delete rejection" below). It is **not** used for
membership discovery — the parent's `Spec.Members` is already the single
source of truth for that.

## Code reuse / avoiding duplication

- `ZfsSnapshotReconciler.sourceDatasetPath`'s body is extracted into a free
  function `resolveDatasetPath(ctx, reader, sourceVolume, fallbackDataset
  string) (string, error)`; `sourceDatasetPath` becomes a one-line wrapper
  (zero behavior change). The new reconciler calls the free function directly.
- `snapshotFullName`, `snapshotCreationTime`, `snapshotRestoreSize`,
  `backingCloneProperties` are already package-level free functions in
  `internal/controller` — called as-is, no changes.
- `internal/csi/snapshot.go`'s `ensureSnapshot`/`waitSnapshotReady` idempotency
  and polling patterns are mirrored (not literally shared, different package
  and type) by the new `GroupControllerServer`.
- The entire backing-clone / restore-source-snapshot / promote-relocate /
  `zfs destroy`-ordering logic in `ZfsSnapshotReconciler` is reused unmodified. The
  only changes to it are the ADR-0038 freeze and "a child with `Spec.GroupSnapshotID`
  never runs `zfs snapshot`". (The group-liveness check once proposed for
  `reconcileDelete` is dropped, ADR-0039.)


## Critical ordering invariant (load-bearing, not a style preference)

`ZfsSnapshotReconciler.Reconcile` unconditionally performs its own idempotent
single-name `Get`-then-`Snapshot` on **every** `ZfsSnapshot` it sees, including
group members. If a group member's child `ZfsSnapshot` CR existed *before*
the atomic multi-name exec ran, that reconciler would race it with its own
single-name `zfs snapshot`, silently destroying the atomicity guarantee this
whole feature exists for.

**Therefore: child `ZfsSnapshot` CRs for a group must only ever be created
after the atomic multi-name group exec has already succeeded — never
before.** This will be called out with an explicit code comment at both call
sites, in the same style as this codebase's existing D-numbered/ADR-numbered
inline invariants.

Because both reconcilers run in the same per-node agent process, `zfs
snapshot` for group members is only ever issued by the new reconciler, and
`MaxConcurrentReconciles: 1` (set independently on each controller) is
sufficient — no cross-controller locking is needed, as long as the ordering
invariant above holds.

## Flow: Create

> **SUPERSEDED (ADR-0039, 2026-10-07):** step 6 now runs in this order: atomic exec, read `creationTime`, create children, `provisionedAt` at first all-Ready. Status is derived from the children.

1. User creates two PVCs (`pg-data`, `pg-wal`), both provisioned by this
   driver, both resolvable to `ZfsDataset`s on the **same** pool (the user's
   responsibility — typically true by design for co-located PG components).
2. User creates:
   ```yaml
   apiVersion: groupsnapshot.storage.k8s.io/v1beta2
   kind: VolumeGroupSnapshotClass
   metadata: {name: zfs-group-snap}
   driver: storage.simple-zfs-csi.io
   deletionPolicy: Delete
   ---
   apiVersion: groupsnapshot.storage.k8s.io/v1beta2
   kind: VolumeGroupSnapshot
   metadata: {name: pg-consistent-snap, namespace: pg}
   spec:
     volumeGroupSnapshotClassName: zfs-group-snap
     source:
       selector: {matchLabels: {app: postgres, role: primary}}
   ```
3. `snapshot-controller` resolves the selector to the two PVCs, creates a
   `VolumeGroupSnapshotContent` with `Spec.Source.VolumeHandles = [pvc-111,
   pvc-222]`.
4. The `csi-snapshotter` sidecar's group-controller part calls our driver:
   `CreateVolumeGroupSnapshot(name="groupsnapshot-<uid>",
   source_volume_ids=[pvc-111, pvc-222])`.
5. Our `GroupControllerServer.CreateVolumeGroupSnapshot`:
   - Runs the same `rejectRemovedModeParam` check as standalone
     `CreateSnapshot` (finding 4 above) — a stale leftover `mode: integrated`
     parameter gets the same actionable error; any other/unknown parameter is
     left unrejected, same as today's standalone behavior.
   - Resolves both volume IDs to `ZfsDataset`s; validates both share one
     `PoolGUID` — if not, `codes.FailedPrecondition` (the spec's "Cannot snapshot multiple
     volumes together" row, `spec.md:3033`), **nothing is created**.
   - Looks up any existing `ZfsGroupSnapshot` by name first. If found,
     compares `PoolGUID` plus the **set** of `Spec.Members[].SourceVolume`
     against the incoming `source_volume_ids` (order-independent — the spec
     does not guarantee list-order stability across retries). Mismatch →
     `codes.AlreadyExists` (finding 2); match → idempotent no-op, skip straight
     to polling.
   - Otherwise creates a fresh `ZfsGroupSnapshot` with `Spec.Members`
     fully populated.
   - Polls (same pattern as `waitSnapshotReady`, extended to N) until
     `Status.Phase == Ready` or timeout (`DeadlineExceeded`, sidecar retries).
   - Builds the response by `Get()`-ing each child `ZfsSnapshot` and rendering
     via the existing `snapshotMessage()`. The response's
     `group_snapshot.creation_time` is read from the **parent's own**
     `Status.CreationTime` (finding 5 below) — not from any child.
6. Meanwhile, on the node hosting the pool, `ZfsGroupSnapshotReconciler`:
   - Gated exactly like `ZfsSnapshotReconciler`
     (`pool.Status.CurrentNode == r.NodeName && Health != NodeOffline`).
   - Resolves each member's live dataset path via `resolveDatasetPath`,
     re-validates each still resolves to the declared `PoolGUID` (defense in
     depth against manual object surgery).
   - If not all raw snapshots exist yet: issues **one**
     `r.ZFS.Snapshot(ctx, full0, full1)` → one `zfs snapshot
     tank/k8s/pg-data@csi-snap-<uuid-A> tank/k8s/pg-wal@csi-snap-<uuid-B>`
     exec — the atomic, same-txg operation. Note the two full names carry
     *different* short names (each independently random); only the single
     exec matters for atomicity, not a shared suffix (verified against
     `zfs-snapshot(8)`: "creates a snapshot of a dataset or multiple
     snapshots of **different** datasets", one `snapname` per positional arg).
   - **Immediately after that exec succeeds**, queries the raw `creation`
     property of any one member (all provably identical — same txg) and
     writes it to the **parent's own** `Status.CreationTime` (finding 5) —
     independent of, and taken slightly earlier than, any child's own
     `Status.CreationTime` (see "Known, accepted cosmetic nuance" below).
   - **Only after that succeeds**, creates/adopts the two child `ZfsSnapshot`s
     (owned via `controllerutil.SetControllerReference`), each with
     `GroupSnapshotID` set and its dataset info prefilled.
   - These are picked up by the **existing, unmodified**
     `ZfsSnapshotReconciler`: it finds the raw snapshot already present (its
     own idempotent check no-ops) and proceeds straight into
     `reconcileBackingClone`, exactly like a standalone snapshot.
   - The group reconciler rolls up both children's `Status.ReadyToUse` (AND)
     into its own `Status`.
7. Kubernetes' `common-controller` takes the `CreateVolumeGroupSnapshotResponse`
   and automatically creates one ordinary, namespaced `VolumeSnapshot` +
   `VolumeSnapshotContent` pair per member, each pointing at the real PVC.

## Flow: Delete

> **SUPERSEDED (ADR-0039, 2026-10-07):** the group has a finalizer and deletes its children itself from `Spec.Members`; there is no ownerReference cascade, no child-liveness guard, and `snapshot_ids` is compared with `Spec.Members`. The "no finalizer on the parent" reasoning is historical.

**TL;DR — final protection model, three tiers, only one is a finalizer:**

| | Standalone `ZfsSnapshot` | Group-member `ZfsSnapshot` | `ZfsGroupSnapshot` (parent) |
|---|---|---|---|
| Finalizer? | Yes, `zfsSnapshotFinalizer` — orders/completes teardown, does **not** refuse deletion | Same finalizer, unmodified | **None** |
| "Don't destroy while claimed" check | **None.** `assertDriverSnapshot` does not run on this path at all — see correction below | **Same "none," deliberately (ADR-0035).** The CRD-layer parent-liveness guard once proposed here is deferred, not implemented in v1 | N/A — nothing else can claim it |
| Cascade mechanism | N/A | N/A | `ownerReference` → K8s GC, cascade-only use, never a business-logic check |

**Correction (second pass — the first version of this table was also wrong):**
the previous draft credited `assertDriverSnapshot` with protecting a standalone
`ZfsSnapshot`'s *own* direct deletion. Re-traced the exact call graph and that's
false: `reconcileDelete` calls `detachAndCleanSnapshots` (backing clone) and
`detachSnapshotClones` (raw snapshot) — the latter **never calls
`assertDriverSnapshot` at all** (confirmed by reading its full body), and the
former only calls it for *leftover, un-cloned artifacts left on the backing
clone itself*, not for "is something else still using the snapshot being
deleted." What actually happens to anything depending on the snapshot
(a restored PVC, i.e. a clone) is **promotion, not refusal** — ADR-0027's
stated consequence is literally "restores are unconditionally promote-safe."
So today, direct `kubectl delete zfssnapshot <name>` has **no** "something
still needs this, refuse" gate at all — only ordering. See the trust-boundary
note directly below for why this is a deliberate scope boundary, not an
oversight to close.

### Trust boundary: why "does anything else still reference this CR" is out of scope by design

This is the underlying reason the table above ends in "None" for the
standalone case, and it generalizes to every `zfs*` CRD, not just
`ZfsSnapshot`:

- **The assumption:** nothing other than the CSI sidecar (via our own RPC
  handlers) and our own reconcilers mutates `ZfsDataset`/`ZfsSnapshot`/
  `ZfsGroupSnapshot`/... directly. A human or script running
  `kubectl delete zfssnapshot <name>` is, by assumption, out of band —
  equivalent in kind to someone running `zfs destroy` on the pool directly
  over SSH. We do not defend against either, for the same reason: both bypass
  every layer we control.
- **Why we can't do better, not just choose not to:** the CSI spec's
  `group_snapshot_id` invariant (`spec.md:2045-2048`) is something *we*
  enforce, because *we* are the one authority that knows which snapshots
  belong to which group — it's a closed, fully-enumerable set we already
  hold in `Spec.Members`/`Spec.GroupSnapshotID`. "Does some other Kubernetes
  object still reference this `ZfsSnapshot`" is a fundamentally different,
  **open, unenumerable** question: a `VolumeSnapshotContent`, a completely
  unrelated third-party controller, a backup tool, a human's kubectl script —
  none of these register themselves with us, none are discoverable by any
  API we can watch, and Kubernetes provides no generic "what refers to this
  object" index. There is no scan we could write that would ever be complete.
  This is categorically different from `assertDriverSnapshot`'s check, which
  never asks "does anything reference this" — it only asks "is this artifact
  one *we* created" (a self-consistency check against our own known naming
  scheme, D18), which *is* a closed, answerable question.
- **The one place this looks like an exception, and why it isn't:** the new
  group-member guard (`Get()` parent by `GroupSnapshotID`) looks superficially
  like "checking if something else still uses this snapshot," but it isn't a
  scan for unknown referrers — it's enforcing an invariant over a closed set
  we fully own and already track (our own parent CR, by a field we wrote at
  creation time). It requires no discovery, just a direct lookup by a key we
  already have. That's why it's in scope while the general case is not.
- **Consequence for the previously-flagged `VolumeSnapshotContent` gap:**
  this reframes it. It is not an overlooked TODO to eventually close — it is
  the same, already-accepted, structural trust boundary
  (`lifecycle-protection-matrix.md` §5.12/§6.3: *"nothing upstream watches our
  CRDs"*) applied to one more object type. Watching `VolumeSnapshotContent`
  would only ever cover *that one* referrer type and give false confidence
  that "referrers are covered" while every other unknowable referrer remains
  exactly as unprotected as before. Not pursuing it is the correct call, not
  a shortfall.

Full reasoning below.

1. User deletes the `VolumeGroupSnapshot`.
2. Sidecar calls `DeleteVolumeGroupSnapshot(group_snapshot_id, snapshot_ids)`.
3. Our handler first compares `set(req.GetSnapshotIds())` against the set of
   child `ZfsSnapshot` CR names read from `Spec.Members` — mismatch →
   `codes.InvalidArgument` ("snapshot list mismatch", finding 3 above; cheap
   to check since we already read `Spec.Members` for this op regardless).
4. On match, deletes the one `ZfsGroupSnapshot` CR and returns —
   fire-and-forget, same semantics as today's `DeleteSnapshot` (it does not
   poll for actual removal; the CSI spec doesn't require synchronous
   completion here).
5. The parent has no finalizer, so it's removed from etcd immediately.
   Kubernetes' own owner-reference garbage collector then asynchronously
   issues `Delete` on every child `ZfsSnapshot` it owned (same
   `SetControllerReference`/`Owns()` pattern as `ZfsShare`→`NetworkExport`).
6. Each child's own `zfsSnapshotFinalizer` blocks *its* removal until
   `ZfsSnapshotReconciler.reconcileDelete` has actually run `zfs destroy` for
   its backing clone and raw snapshot (existing logic — correct ordering,
   promote/relocate handling included, unchanged). Before any of that,
   `reconcileDelete` now first checks `Spec.GroupSnapshotID`: if set, it
   `Get()`s the parent `ZfsGroupSnapshot` — if still found, this is an
   out-of-band delete (parent had no finalizer, so a legitimate cascade always
   means the parent is already gone by the time we get here); refuse and leave
   the finalizer in place. This is the CRD-level backstop described in "Flow:
   single-snapshot Delete rejection" below, and it only ever triggers on a
   direct `kubectl delete zfssnapshot` — a normal group deletion (step 5
   above) always clears the parent first, so this check is a no-op on every
   legitimate path.
7. Because every member's on-disk `SnapshotName` and CR `ChildSnapshotName`
   are independently random (never derived from the parent's name), a
   same-name retried `CreateVolumeGroupSnapshot` can never collide with a
   still-`Terminating` child from a previous incarnation — it always mints
   fresh names. This is what makes skipping the parent finalizer safe; see
   the CRD section above for the full reasoning and the bug that first made
   a finalizer look necessary.

`GetVolumeGroupSnapshot` follows the same `snapshot_ids`-mismatch check as
step 3 above (it carries the same required field, same spec-named error
condition), then simply renders the current `Spec.Members`/`Status` — no ZFS
access, no polling, mirroring the existing standalone `GetSnapshot`-equivalent
read path.



## Flow: Create — collision handling (no guessing, verified against kernel source)

> **SUPERSEDED (ADR-0039, 2026-10-07):** the "created once" gate is now `provisionedAt` (first all-Ready), and a post-provisioning loss is observed as Lost, never recreated (ADR-0038).

Read directly from OpenZFS `module/zfs/dsl_dataset.c`,
`dsl_dataset_snapshot_check_impl`: the per-dataset `check` phase looks up the
requested snapshot name and returns `EEXIST` if it already exists. This check
runs for **every** dataset in the batch, and if **any** one of them collides,
`dsl_sync_task`'s `check` returns non-zero and `sync` (the phase that actually
writes anything) **never runs for any dataset in the batch** — ZFS itself
guarantees zero snapshots are created if even one name collides. This is not
something the driver has to implement; it falls out of the verified
check-before-sync structure for free.

What the driver must still get right is never retrying with a **subset** of
names — the dangerous move would be "2 of 3 already exist, so just snapshot
the missing one," which would create that one at a different txg and quietly
break the same-instant guarantee this whole feature exists for. The
reconciler's idempotent-create step is therefore exactly three-way, not
two-way:

- **All N raw snapshots already exist** → no-op, proceed straight to child
  creation (this is the normal case on a requeue after a fully-succeeded prior
  attempt).
- **None exist** → issue the one atomic exec covering all N names.
- **Some exist, some don't** → a real, actionable error state (manual
  meddling, or an out-of-band name collision). The reconciler never attempts
  to fill the gap; it sets `Status.Phase = Error` naming exactly which
  member(s) collided, and this propagates to `CreateVolumeGroupSnapshot`
  (via the poll loop) as a hard error — never a silent retry loop, never a
  hang.

**One more state this three-way split must not be confused with (ADR-0034):**
the "none exist" branch above describes the *first-ever* successful creation.
Once the parent's `Status` has recorded that the group was successfully
created — i.e. this is no longer a fresh object, it previously reached the
success state — "none exist" must never again be read as "go ahead and
create them." A `ZfsGroupSnapshot` is a point-in-time record, exactly
like a standalone `ZfsSnapshot` (ADR-0034's reasoning applies identically
here, if anything more critically): if every member's raw snapshot has since
vanished from a group that was already successfully created once, re-issuing
the atomic exec would silently mint a *new*, different cross-member instant
under the same object identity. That is precisely the corruption this entire
feature exists to prevent. The group reconciler must therefore gate the
"none exist → create" branch on "has this group ever reached success before,"
not merely on "do the snapshots currently exist" — on a post-success
reconcile finding them gone, it must report a `Lost`-style failure (mirroring `SnapshotPhaseLost`,
which is an observation, not terminal — ADR-0034 amendment) instead of recreating.

## Flow: Restore

Entirely standard, unmodified by this feature: each auto-created
`VolumeSnapshot` is used as a normal single-PVC `dataSource`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: pg-data-restored}
spec:
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: <auto-generated per-member VolumeSnapshot for pg-data>
```

## Flow: single-snapshot Delete rejection — RPC-layer only (CRD-layer guard deferred, ADR-0035)

> **SUPERSEDED (ADR-0039, 2026-10-07):** the CRD-layer guard is dropped; the `DeleteSnapshot` RPC still refuses group members with INVALID_ARGUMENT (ADR-0039). The reasoning below is kept for history.

**Correction (ADR-0035):** an earlier pass over this section labeled this whole
section "spec conformance" and described two guards as if both were mandated.
That's wrong. The CSI spec (`spec.md:2045-2048`) states: *"The CO SHALL NOT
call this RPC [`DeleteSnapshot`] with a snapshot for which SP provided a
non-empty `group_snapshot_id`... The SP MAY refuse to delete such snapshots
**with this RPC call** and return an error instead."* — scoped entirely to the
`DeleteSnapshot` RPC. The spec has no concept of CRDs and says nothing about
protecting our internal representation from being manipulated directly; that's
purely an internal choice, not something conformance requires. Only the
RPC-layer rejection below is spec-mandated. The CRD-layer guard once proposed
here — `reconcileDelete` `Get()`ing the parent `ZfsGroupSnapshot` and
refusing if it still exists — is **deferred, not implemented in v1** (see
ADR-0035 for the full reasoning: it would only ever matter for a direct
`kubectl` bypass, already out of scope everywhere else in this driver, and
adding it just for this one relation while leaving every structurally
identical relation elsewhere undefended would be an arbitrary inconsistency).

**Historical note, kept for the record:** the CRD-layer guard described below
was, at one point, justified by citing `checkOwningSnapshotLive` (dead code,
deleted by ADR-0030) and later re-justified by citing `assertDriverSnapshot`
(a real, live function, but one that protects a different entry point
entirely — see the "Trust boundary" section above). Both citations were
corrected in earlier passes. This history is kept only so the guard is not
re-proposed later on the strength of either wrong citation; the actual reason
it's not implemented is simpler than either: it's optional, per ADR-0035.

**What v1 actually does:**

1. **CSI RPC layer (implemented, spec-mandated).** If someone calls the plain
   `DeleteSnapshot` RPC (or `kubectl delete volumesnapshot`, which the CO
   translates into one), our `ControllerServer` rejects it:
   `codes.InvalidArgument`, `"snapshot is part of a group, use
   DeleteVolumeGroupSnapshot"` — checked via `Spec.GroupSnapshotID` on the
   target `ZfsSnapshot`. This protects the path every conformant CO is
   expected to use, and is the only guard the spec requires.
2. **CRD layer — deferred (ADR-0035).** `kubectl delete zfssnapshot
   <group-member-name>` bypasses CSI entirely and, as of v1, is **not**
   guarded: `reconcileDelete` proceeds exactly as it would for a standalone
   snapshot (ADR-0034's ratchet still applies — a separate, unrelated
   concern), destroying the member's own primitives and leaving the parent
   `ZfsGroupSnapshot` referencing a child it no longer owns. This is a
   deliberate, accepted gap, in the same category as every other
   direct-CRD-tampering gap this driver already declines to defend against
   (trust-boundary section above). If a future revision decides this
   asymmetry is worth closing after all, the shape would be: `Get()` the
   parent by `Spec.GroupSnapshotID` at the top of `reconcileDelete`, refuse if
   still found — no new field needed, `GroupSnapshotID` already exists for the
   RPC-layer check above.

## Creation-time accuracy (was "known, accepted cosmetic nuance" — now fixed at the source, ADR-0036)

Each child's reported `creation_time` (in `snapshotMessage()`, i.e. the
per-member `VolumeSnapshot`'s timestamp) previously came from its own
backing-clone `@restore-source` self-snapshot, taken independently per member
slightly after the shared raw group snapshot — members' *reported* CSI
timestamps could differ even though the actual data-consistency point (the raw
snapshot's txg) was identical for all of them. **ADR-0036 fixes this at the
source**: `reconcileBackingClone` now reads `creation` from the raw origin
snapshot itself (`rawFull`), not the later self-snapshot. Since every member's
raw snapshot was created in the same atomic exec (same txg/sync pass), this
reads back as identical in every realistic case — without any new alignment
mechanism, because both members were already reading the correct, shared
underlying property, just from the wrong artifact.

**Caveat, stated precisely so it isn't oversold:** ZFS's `creation` property
is a whole-second wall-clock value (`gethrestime_sec()`), set independently
per dataset object at sync time, not copied from one shared value — so this is
not a byte-for-byte, code-enforced guarantee for an arbitrarily large batch.
The actual, code-enforced same-instant guarantee is the shared txg
(`ds_creation_txg`), which nothing CSI-facing reads directly. See ADR-0036 for
the full reasoning, including why forcing a literal copy across members was
considered and rejected as unneeded complexity.

This is deliberately **not** the same field as the group-level
`VolumeGroupSnapshot.creation_time`, which is sourced independently by the
group reconciler at the moment the atomic exec succeeds — see spec-conformance
finding 5 above. Both now derive from the same underlying ZFS property, on
purpose; they are simply captured at two different layers (group reconciler
vs. per-child `reconcileBackingClone`).

## Decision: the atomic snapshot is taken at most once (never re-taken)

*(Revised 2026-10-07, ADR-0038/0039; replaces the earlier `snapshotTakenAt` wording.)*

- The raw snapshot names are fixed in `Spec.Members` **before** the exec. A retry can
  therefore ask ZFS whether they exist: all exist -> adopt; none -> exec; some -> `Error`.
- The group's `provisionedAt` is stamped when all children are Ready for the first time,
  the same meaning as on every other kind. From then on the exec never runs again, even if
  the raw snapshots are later lost (they are then observed as Lost, nothing is created).
- Before `provisionedAt`, a retry is harmless: nothing outside has seen the instant. A raw
  snapshot destroyed by hand in that window leaves a child unfulfilled, the same as any
  source going missing during provisioning.
- No separate "exec happened" marker (`snapshotTakenAt`) is kept for now.
- A child created for a group member never runs `zfs snapshot` (`Spec.GroupSnapshotID`); it
  adopts the raw snapshot and builds the backing clone and `@restore-source`.

## Decision: deletion, status and the CSI Get/Delete contract (ADR-0039)

- **Delete:** finalizer on the group. The group controller deletes each child named in
  `Spec.Members` (NotFound = already gone), waits until all are gone, then releases the
  finalizer. No `ownerReference`.
- **`snapshot_ids`** on `GetVolumeGroupSnapshot` and `DeleteVolumeGroupSnapshot` is compared
  with `Spec.Members`, not live children, so a group with one deleted member stays deletable.
  The sidecar sends the list returned at create time (external-snapshotter
  `sidecar-controller/groupsnapshot_helper.go`, `DeleteGroupSnapshot`).
- **Get:** returns the group with all `Spec.Members` children and
  `ready_to_use=false` while any is not ready. If a member's CR is gone, fail with
  `FAILED_PRECONDITION` naming it (Ceph-CSI and host-path also fail; no partial list, no
  placeholder). `INVALID_ARGUMENT` if `snapshot_ids` differs from `Spec.Members`.
  `NOT_FOUND` only when the group object is gone. Full decision table with Ceph-CSI and
  csi-driver-host-path references: ADR-0039.
- **`creation_time`** is `status.creationTime`, read once from a raw snapshot right after the
  exec. It is not `provisionedAt` and survives deletion of any child.
- **Delete before `provisionedAt`:** for a member with no child CR the finalizer destroys the
  raw `csi-snap-*` snapshot if it exists. After `provisionedAt` it touches no ZFS object. A
  `Terminating` group never creates or recreates a child.
- **Create/Delete RPC error codes:** table in ADR-0039 (cross-pool = `FAILED_PRECONDITION`,
  not `INVALID_ARGUMENT`, per the spec's error table).
- **Retried create while the group is `Terminating`:** return `Aborted`.
- **`DeleteSnapshot` RPC for a group member:** refused with `INVALID_ARGUMENT` when the
  `ZfsSnapshot` CR has `Spec.GroupSnapshotID` (spec error table, `spec.md:2073`); missing CR = OK.
  Direct CR deletion stays allowed. Upstream never sends it for members.
- **Known limitation (accepted, no deletion guard):** between the atomic exec and the child CR
  creation the raw snapshots have no CR. Deleting the source PVC then destroys them (nothing waits: D3 is
  dropped, ADR-0041; upstream adds no source-PVC protection for group snapshots). Short window, loud and safe failure (never
  provisioned). Details and the rejected guard: ADR-0039.

## New surface area (net)

- 1 new CRD (`ZfsGroupSnapshot`) **with a finalizer** (ADR-0039): membership record,
  derived status, explicit child deletion. No `ownerReference`.
- 1 new per-node reconciler (`ZfsGroupSnapshotReconciler`).
- 1 new field on the existing `ZfsSnapshot` (`GroupSnapshotID`); a child that has it never
  runs `zfs snapshot`.
- `ControllerServer.DeleteSnapshot` refuses members (`INVALID_ARGUMENT`); `snapshotMessage`
  sets `group_snapshot_id` for members.
- 1 new CSI service implementation (`GroupControllerServer`):
  Create/Delete/Get/GroupControllerGetCapabilities.
- `internal/zpool.ZFS.Snapshot` becomes variadic.
- New RBAC: `create`/`delete` on `zfssnapshots` for the DaemonSet agent
  (additive); the CSI controller gets create/get/list/watch/delete on `zfsgroupsnapshots`;
  the agent gets get/list/watch/update/patch on `zfsgroupsnapshots` and its `/status` and
  `/finalizers`.
- Helm: `--enable-volume-group-snapshots` sidecar flag,
  `groupsnapshot.storage.k8s.io` CRDs/webhook as a documented prerequisite.

Everything else — backing clone creation, restore-source snapshotting,
promote/relocate handling, `zfs destroy` ordering, restore/clone compatibility
checks — is reused **completely unmodified**.

## Implementation breakdown

Tracked as SQL todos (`vgs-*`), in dependency order:

1. `vgs-zfs-variadic-snapshot` — make `ZFS.Snapshot` variadic.
2. `vgs-crd-type` — add the `ZfsGroupSnapshot` CRD (with finalizer, ADR-0039).
3. `vgs-group-membership-invariant` — `GroupSnapshotID` field +
   `DeleteSnapshot` rejection.
4. `vgs-group-controller` — `ZfsGroupSnapshotReconciler` +
   `resolveDatasetPath` extraction.
5. `vgs-csi-group-controller-server` — `GroupControllerServer`.
6. `vgs-csi-wiring` — capability advertisement + gRPC registration.
7. `vgs-helm-chart` — RBAC + sidecar flag + CRD prerequisite docs.
8. `vgs-docs-adr` — ADR + walkthrough doc.
9. `vgs-tests` — unit tests across all new/changed components.
10. `vgs-e2e-validation` — manual/CI end-to-end validation.
