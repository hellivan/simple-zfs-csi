# Code Review — Dataset Naming & Snapshot Lifecycle Redesign (2026-08-03)

**Reviewed range:** `ef6a39b..9db3041` (commits `922c157`, `f1f4825`, `8e9fcbe`, `2d2ec95`,
`e70cb53`, `5a73301`, `9db3041`).

**Scope of the review:** correctness, data integrity/safety, and internal consistency of
the two redesigns landed in this range:
- [independent-resource-naming-redesign.md](independent-resource-naming-redesign.md) —
  opaque `csi-vol-<uuid>` / `csi-snap-<uuid>` ZFS-facing identifiers.
- [snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md) — `standalone` /
  `integrated` snapshot modes, `zfs promote`-based dependent detachment, non-recursive
  `zfs destroy` (D11).

**Reference material used:** both redesign docs, the
[live-pool verification log](promote-order-verification-2026-07-31.md), the official
`zfs-promote.8` / `zfsconcepts.7` semantics, and the shipped Helm chart.

This document is written so that someone with no prior context on the review conversation
can reproduce, understand, and fix every finding independently.

---

## 0. Executive summary

**What is correct.** The direction is right and the biggest data-safety win is real:
`zfs destroy -r` has been eliminated from **every** call site (verified: all four
`ZFS.Destroy` calls in `internal/` pass `recursive=false`). `zfs promote` is the correct
ZFS-native primitive for detaching dependents, and the reasoning in the design docs
(D0–D16) is sound. The independent-naming change is small, well-scoped, and correctly
implemented (idempotent retries reuse the persisted `Spec.Dataset`/`Spec.SnapshotName`,
`ObjectMeta.Name` is untouched, no derived naming broke).

**What is wrong.** The implementation does not match what the project's *own* live-pool
test actually observed. There is one systemic defect with several distinct manifestations
(§2), plus a hard RBAC blocker that makes the new default snapshot mode non-functional as
shipped (§1).

**Failure mode of the defects.** Because `-r` is gone everywhere, the failure mode is
**permanently stuck `Terminating` objects and unreclaimable pool space**, *not* silent
destruction of data. No data-loss path was found in normal (CSI-driven) operation. One
data-loss path exists only via direct manual manipulation of an internal CRD (§4.4).

**Why CI is green.** The fake `ZFS` test double does not model the ZFS behaviours that
these defects depend on (§3). Every defect in §2 is invisible to the current test suite.

### Finding index

| # | Severity | Title | Section |
|---|----------|-------|---------|
| F1 | **Blocker** | Agent RBAC lacks `create`/`delete` on `zfsdatasets` — `standalone` mode cannot work | §1 |
| F2 | **Critical** | Promoted dependents inherit relocated snapshots that nothing cleans up | §2 |
| F2a | Critical | Direct PVC-to-PVC clone becomes permanently undeletable (regression) | §2.1 |
| F2b | Critical | `DeleteSnapshot` with a live restored PVC gets permanently stuck | §2.2 |
| F2c | Critical | Two restores from one snapshot produce stale/incorrect tracking | §2.3 |
| F2d | Critical | Sibling backing clones chain to each other with no tracking registered | §2.4 |
| F3 | High | Test double contradicts the empirically verified ZFS behaviour | §3 |
| F4 | Medium | Tracking finalizers cleared before the destroy actually succeeds | §4.1 |
| F5 | Medium | D6 cross-prefix rejection missing for `integrated` mode and for volume-clones | §4.2 |
| F6 | Medium | `promoteDirectCloneDependents` has no `PoolGUID` filter | §4.3 |
| F7 | Medium | Manually deleting a backing-clone `ZfsDataset` destroys the raw snapshot | §4.4 |
| F8 | Low | `FormatAndMount` doc comment describes the rejected (wrong) behaviour | §5.1 |
| F9 | Low | `backingCloneOwnerSnapshotName` doc comment contradicts its code | §5.2 |
| F10 | Low | "Immutable" spec fields have no CEL immutability rule | §5.3 |
| F11 | Low | `releaseSnapshotFinalizer` not hardened like `releaseFinalizer` | §5.4 |
| F12 | Low | D10 compatibility checks silently no-op when the source volume is gone | §5.5 |

---

## 1. F1 — BLOCKER: agent RBAC lacks `create`/`delete` on `zfsdatasets`

### Evidence

D15 moved backing-clone provisioning into `ZfsSnapshotReconciler`, which now creates and
deletes `ZfsDataset` **objects**:

- Create: [zfssnapshot_controller.go](../internal/controller/zfssnapshot_controller.go)
  `reconcileStandaloneCreate` → `r.Create(ctx, desired)`.
- Delete: [zfssnapshot_controller.go](../internal/controller/zfssnapshot_controller.go)
  `reconcileDelete` → `r.Delete(ctx, backing)`.

Both reconcilers are registered on the agent manager under the **discovery** ServiceAccount
([cmd/zpool-discovery/main.go](../cmd/zpool-discovery/main.go), lines ~107 and ~117).

The discovery ClusterRole grants
([charts/simple-zfs-csi/templates/rbac.yaml](../charts/simple-zfs-csi/templates/rbac.yaml), ~line 71):

```yaml
  - apiGroups: ["storage.simple-zfs-csi.io"]
    resources: ["zfsdatasets"]
    verbs: ["get", "list", "watch", "update", "patch"]   # <-- no create, no delete
```

The kubebuilder marker that generates this
([internal/controller/zfsdataset_controller.go](../internal/controller/zfsdataset_controller.go), ~line 51)
has the same gap, and `ZfsSnapshotReconciler` carries **no** `zfsdatasets` RBAC marker at
all — so `make manifests` would not surface or fix this either.

### Impact

With the shipped chart, `standalone` (the **new default** snapshot mode):
- `CreateSnapshot` fails: the backing-clone `Create` returns 403, the `ZfsSnapshot` goes to
  `Error`/`BackingCloneCreateFailed`, and the CSI call fails.
- `DeleteSnapshot` fails: the backing-clone `Delete` returns 403, so `reconcileDelete`
  never advances and the `ZfsSnapshot` stays `Terminating` forever.

Effectively, snapshots are broken end-to-end on a fresh install.

### Fix

1. Add `create` and `delete` to the discovery role's `zfsdatasets` rule.
2. Add `+kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsdatasets,verbs=get;list;watch;create;update;patch;delete`
   to `ZfsSnapshotReconciler` (and update the `ZfsDatasetReconciler` marker to match what
   the chart ships, so generated and hand-written RBAC stay in sync).
3. Add `zfssnapshots/finalizers: ["update"]` to the discovery role. `metav1.NewControllerRef`
   sets `blockOwnerDeletion: true`, which the `OwnerReferencesPermissionEnforcement`
   admission plugin rejects without that permission. It is off by default in kubeadm but
   on in several managed distributions — this must not depend on cluster configuration.

### Acceptance test

Deploy the chart and run a full `VolumeSnapshot` create → restore → delete cycle in
`standalone` mode against a real cluster. A unit test cannot catch this class of defect;
an RBAC-assertion test over the rendered chart (verbs required by each reconciler) is the
cheap regression guard.

---

## 2. F2 — CRITICAL: promoted dependents inherit relocated snapshots that nothing cleans up

### The underlying mechanic (and where the design reasoning went wrong)

D11's argument is: "once every tracked dependent has been promoted away, the dataset being
deleted has zero snapshots of its own, so plain `zfs destroy` always succeeds." That
statement is **true for the dataset being deleted**.

What it misses: `zfs promote` does not make snapshots disappear — it **relocates them onto
the dependent**. Per `zfs-promote.8`:

> The snapshot that was cloned, and any snapshots previous to this snapshot, are now owned
> by the promoted clone.

So every promote *creates* snapshots on some other dataset. And `zfs destroy <fs>` **without
`-r` refuses** whenever the dataset has any snapshot of its own:

```
cannot destroy 'tank/x': filesystem has children
use '-r' to destroy the following datasets:
tank/x@snap
```

Before D11 the driver used `Destroy(full, true)`, which papered over this. With D11 it
becomes a hard, permanent failure.

### Where the cleanup exists, and why it is insufficient

[internal/controller/promote.go](../internal/controller/promote.go), `beforeDestroy`
(lines ~120–133):

```go
snapName, isBackingClone, err := r.backingCloneOwnerSnapshotName(ctx, vol)
if err != nil {
    return err
}
if isBackingClone {
    if err := r.ZFS.Destroy(ctx, full+"@"+restoreSourceSnapshotName, false); err != nil { ... }
    if snapName != "" {
        if err := r.ZFS.Destroy(ctx, full+"@"+snapName, false); err != nil { ... }
    }
}
```

`backingCloneOwnerSnapshotName` returns `ok=true` **only** when `vol` has an
`ownerReference` of kind `ZfsSnapshot` — i.e. only for standalone-mode backing clones.
Restored PVCs, direct PVC-to-PVC clones, and *sibling backing clones* never get this
cleanup, even though they are exactly the datasets the promote machinery dumps relocated
snapshots onto.

Additionally, only two fixed suffixes are handled (`@restore-source` and the owner's
`Spec.SnapshotName`). Real `zfs promote` can drag along an arbitrary set of older snapshots.

### 2.1 F2a — Direct PVC-to-PVC clone becomes permanently undeletable (regression)

The simplest and most likely-to-be-hit case. ADR-0009 direct clone path.

Reproduction:
1. `pvc-src` exists. Create `pvc-clone` with `dataSource: pvc-src`.
   `internal/controller/zfsdataset_controller.go`'s `clone()` takes
   `pvc-src@clone-<hash>` and clones it into `pvc-clone`.
2. Delete `pvc-src`. `promoteDirectCloneDependents`
   ([promote.go](../internal/controller/promote.go), ~line 181) calls
   `Promote(pvc-clone)`.
   Real ZFS result: `@clone-<hash>` **relocates onto** `pvc-clone` (now
   `pvc-clone@clone-<hash>`), and `pvc-src.origin` becomes `pvc-clone@clone-<hash>`.
3. `zfs destroy pvc-src` succeeds (it has no snapshots of its own). ✔
4. Later, delete `pvc-clone`. `beforeDestroy` does nothing (`isBackingClone == false`,
   no tracking finalizers), then `zfs destroy pvc-clone` (non-recursive) →
   **"filesystem has children"** → reconcile error → requeue → `ZfsDataset` stuck
   `Terminating` **forever**. The PV is never released and the space is never reclaimed.

This is a direct regression against the pre-D11 behaviour.

### 2.2 F2b — `DeleteSnapshot` with a live restored PVC gets permanently stuck

Reproduction:
1. `pvc-A` exists. Create standalone `VolumeSnapshot` `S` → raw `A@csi-snap-x`, backing
   clone `BC` (`ZfsDataset` named `csi-snap-x`), plus `BC@restore-source`.
2. Restore `pvc-R` from `S`. `resolveContentSource` clones from `BC@restore-source` and
   registers `restored-by.pvc-R` on `BC`. ✔
3. Delete `VolumeSnapshot` `S`.
   - `ZfsSnapshotReconciler.reconcileDelete` deletes the `BC` object.
   - `BC`'s `beforeDestroy` → `promoteTrackedDependents` promotes `pvc-R`.
     Real ZFS: `pvc-R` takes ownership of `@restore-source`; `pvc-R.origin` becomes `BC`'s
     old origin, i.e. `A@csi-snap-x`; `BC.origin` becomes `pvc-R@restore-source`.
   - The code correctly detects the non-empty origin and re-registers
     `promoted-onto.pvc-R` onto `pvc-A`. ✔
   - `BC@restore-source` / `BC@csi-snap-x` destroys are no-ops (relocated), `BC` itself
     destroys fine. ✔
   - `ZfsSnapshotReconciler` then runs the required raw-origin cleanup:
     `zfs destroy A@csi-snap-x` (non-recursive)
     ([zfssnapshot_controller.go](../internal/controller/zfssnapshot_controller.go), ~line 190)
     → **fails: "snapshot has dependent clones"**, because `pvc-R` is now a clone of it.
   - The `zfssnapshot` finalizer is deliberately gated on that destroy succeeding, so the
     `ZfsSnapshot` stays `Terminating` **forever**.

This directly violates the design's own guarantee ("`DeleteSnapshot` always succeeds,
never blocks", §3.1 / D4).

4. Compounding it: `pvc-R` now permanently owns `@restore-source`, so deleting `pvc-R`
   later hits F2a's failure and it too becomes permanently undeletable.

### 2.3 F2c — Two restores from one snapshot produce stale/incorrect tracking

`promoteTrackedDependents`
([promote.go](../internal/controller/promote.go), ~line 215) iterates the tracked
dependents and, for each, promotes it and *then* reads its origin. With two dependents the
second promote invalidates the first one's already-recorded result.

Reproduction: `pvc-R1` and `pvc-R2` both restored from snapshot `S` (both clones of
`BC@restore-source`). Delete `S`:
- Promote `pvc-R1` → `R1` owns `@restore-source`; its origin reads `A@csi-snap-x`;
  code writes `promoted-onto.pvc-R1` onto `pvc-A`.
- Promote `pvc-R2` → `R2` **steals** `@restore-source` from `R1`; `R1.origin` becomes
  `R2@restore-source`. Code reads `R2`'s origin (`A@csi-snap-x`) and writes
  `promoted-onto.pvc-R2` onto `pvc-A`.
- Net result: the real `R1 → R2` dependency is **untracked**, and `promoted-onto.pvc-R1`
  on `pvc-A` is **wrong** (R1 no longer depends on A).
- Deleting `pvc-R2` then fails permanently (it owns `@restore-source`, which `R1` clones).

### 2.4 F2d — Sibling backing clones chain to each other with no tracking registered

This one is proven by the project's own live-pool log. Final converged state from
[promote-order-verification-2026-07-31.md §3.6](promote-order-verification-2026-07-31.md):

```
csi-snap-t1   origin = -
csi-snap-t2   origin = csi-snap-t1@snap_t1
csi-snap-t3   origin = csi-snap-t2@snap_t2
csi-snap-t4   origin = csi-snap-t3@snap_t3
csi-snap-t5   origin = csi-snap-t4@snap_t4
csi-snap-t6   origin = csi-snap-t5@snap_t5
vol1          origin = csi-snap-t6@snap_t6
```

After `DeleteVolume` on a source with N standalone snapshots, the backing clones are
**chained to one another**, and each owns a real snapshot (`csi-snap-tN@snap_tN`) that the
next one in the chain clones.

`promoteSnapshotDependents` ([promote.go](../internal/controller/promote.go), ~line 142)
promotes each backing clone and then **never re-reads `origin` and never registers a
`promoted-onto` finalizer** — unlike `promoteTrackedDependents`, which does exactly that.
So none of these chain relationships are tracked.

Consequence: later deleting `VolumeSnapshot` `t1`:
- `BC1`'s `beforeDestroy` finds no tracked dependents.
- It runs `zfs destroy csi-snap-t1@snap_t1` (the `snapName` branch) →
  **"snapshot has dependent clones"** (`csi-snap-t2` clones it) → stuck permanently.

`promoteDirectCloneDependents` has the identical omission.

### Recommended fix for F2 (all four manifestations)

Two complementary changes; both are needed.

**(a) Make post-promote tracking uniform.** Extract the "promote, re-read `origin`,
resolve the new owner, re-register the tracking finalizer there" logic currently inside
`promoteTrackedDependents` into a shared helper, and use it from **all three** promote
helpers (`promoteSnapshotDependents`, `promoteDirectCloneDependents`,
`promoteTrackedDependents`). For F2c specifically, re-read each dependent's origin *after
the whole batch* (or re-drive the loop) rather than immediately after its own promote,
since a later sibling promote can invalidate an earlier reading.

**(b) Make leftover-artifact cleanup generic, not backing-clone-specific.** Replace the
`isBackingClone`-gated fixed-suffix destroys in `beforeDestroy` with:
1. Enumerate the dataset's **actual** snapshots (`zfs list -H -o name -t snapshot -r -d 1 <ds>`
   — this requires a new `ZFS.ListSnapshots(ctx, dataset)` method; `zpool.DatasetKind` has
   no snapshot kind today).
2. For each snapshot, destroy it **only** if it is driver-owned — i.e. its suffix matches
   a known internal pattern (`restore-source`, `clone-<hex>`, `csi-snap-<uuid>`) **and**
   it has no remaining clones. Prefer an explicit allow-list over a deny-list.
3. **Fail loud** (error + requeue, object stays visibly `Terminating`) on any snapshot
   that is not recognised — consistent with ADR-0013 and with D11's stated intent. Never
   fall back to `-r`.
4. Only then run the plain `zfs destroy`.

Note this cleanup is safe by construction in every case identified: a relocated
`@restore-source` / `@csi-snap-<uuid>` / `@clone-<hex>` only ever lands on a dependent
*because* the object that owned it is itself being destroyed, so it is by definition no
longer needed by anyone.

**(c) Re-check F2b's raw-origin destroy.** Even with (a) and (b),
`ZfsSnapshotReconciler.reconcileDelete`'s `zfs destroy A@csi-snap-x` must not run while a
promoted restored PVC still clones it. Either the raw snapshot must also be promoted away
onto a dependent first, or the destroy must be gated on the snapshot having no clones and
the `ZfsSnapshot` must tolerate deferring it (which then re-opens the D11 invariant — see
§6, "design questions to re-settle").

---

## 3. F3 — HIGH: the test double contradicts the empirically verified ZFS behaviour

[internal/controller/zfsdataset_controller_test.go](../internal/controller/zfsdataset_controller_test.go), `fakeZFS.Promote` (~lines 99–131):

```go
func (f *fakeZFS) Promote(_ context.Context, dataset string) error {
    ...
    origin, ok := f.origin[dataset]
    if !ok || origin == "" { return nil }
    ...
    relocated := dataset + suffix
    delete(f.origin, dataset)                       // (i)
    for other, o := range f.origin {
        if o == origin { f.origin[other] = relocated }
    }
    return nil
}
```

The fake models **none** of the following real behaviours:

| Real ZFS behaviour | Modelled? | Evidence it is real |
|---|---|---|
| Promoted dataset **acquires** the relocated snapshot | ✗ (no snapshot table at all) | verification log §3.3 |
| Former parent gets a **reverse** `origin` link to the promoted clone | ✗ | verification log §3.3, `zfsconcepts.7` |
| Promote drags along **all older** snapshots, not just the origin one | ✗ | verification log §3.3, `zfs-promote.8` |
| A successfully promoted dataset can retain a **non-empty** `origin` | ✗ — line (i) always clears it | verification log §3.6 / §4.2 |
| `Destroy` refuses on "filesystem has children" | ✗ — `fakeZFS.Destroy` never fails | ZFS semantics |
| `Destroy` refuses on "snapshot has dependent clones" | ✗ | ZFS semantics |

Every defect in §2 is therefore structurally invisible to the current suite, which is why
`go test ./...` is green.

Worse, the suite actively asserts the behaviour the project's own errata **corrected**:
[zfsdataset_controller_test.go](../internal/controller/zfsdataset_controller_test.go)
(~lines 909–913) asserts `origin` is cleared after promotion, while
[verification log §4.2](promote-order-verification-2026-07-31.md) states plainly that "a
cleanly and successfully promoted dataset does not necessarily end up with an empty
`origin`".

### Fix

Upgrade `fakeZFS` to a minimally faithful model before fixing §2, so the fixes are actually
verified:
1. Add `snapshots map[string][]string` (dataset → ordered snapshot suffixes, creation
   order preserved).
2. `Snapshot()` appends; `Clone()` records `origin`; `Destroy()` on a dataset **errors**
   if `snapshots[ds]` is non-empty; `Destroy()` on a snapshot **errors** if any dataset's
   `origin` references it.
3. `Promote(d)`: move the origin snapshot **and all snapshots created before it** from the
   parent to `d`; set `parent.origin = d@<originSuffix>`; set `d.origin =` the parent's
   previous origin; reparent sibling clones that referenced any moved snapshot.
4. Re-run the six-snapshot scrambled-order scenario from the verification log against the
   fake and assert the final state matches the log **verbatim** (including the non-empty
   chained origins). That makes the fake's fidelity itself a test.

Then extend the redesign's task-list item 8 tests to cover: delete-after-promote for a
direct clone, delete-snapshot-with-live-restore, two-simultaneous-restores, and
delete-snapshot-after-source-deleted-with-N-snapshots.

---

## 4. Medium findings

### 4.1 F4 — Tracking finalizers cleared before the destroy actually succeeds

[zfsdataset_controller.go](../internal/controller/zfsdataset_controller.go) (~lines 95–106):

```go
if err := r.clearTrackingFinalizersReferencing(ctx, vol.Name); err != nil { ... }   // (1)
if err := r.beforeDestroy(ctx, &vol, pool.Status.PoolName, full); err != nil { ... } // (2)
if err := r.ZFS.Destroy(ctx, full, false); err != nil { ... }                        // (3)
return ctrl.Result{}, r.releaseFinalizer(ctx, &vol)
```

Step (1) removes every `promoted-onto.<vol>` / `restored-by.<vol>` finalizer that other
objects hold **before** `vol` is actually gone from ZFS. If (2) or (3) fails and requeues
(which, per §2, is now a realistic and possibly permanent state), the dependency has been
untracked while it is still physically real. A concurrent deletion of the object that used
to track `vol` would then skip promoting `vol` away and fail its own destroy — turning one
stuck object into two.

**Fix:** move `clearTrackingFinalizersReferencing` to immediately before
`releaseFinalizer`, after the destroy has succeeded.

### 4.2 F5 — D6 cross-prefix rejection missing for `integrated` mode and for volume-clones

D6 states the cross-prefix rejection "applies identically to both modes". The
implementation returns early for `integrated` **before** the check:

[internal/csi/clone.go](../internal/csi/clone.go) (~lines 74–96):

```go
if effectiveMode(snap.Spec) != storagev1alpha1.SnapshotModeStandalone {
    // integrated mode: returns here — D6 check below is never reached
    return &storagev1alpha1.DatasetSource{Snapshot: snap.Spec.Dataset + "@" + snap.Spec.SnapshotName}, nil
}
...
if path.Dir(strings.Trim(backing.Spec.Dataset, "/")) != normalizedDatasetPrefix(rp.DatasetPrefix) { ... }
```

Separately, the `VolumeContentSource_Volume` branch (direct PVC-to-PVC clone) has **no**
cross-prefix check at all, although it has the identical `zfs send -R` locality problem
(the clone's origin snapshot lives under the source's prefix).

**Fix:** hoist the prefix comparison so it runs for both modes (comparing against
`snap.Spec.Dataset`'s dir for `integrated`), and add the equivalent check to the
volume-clone branch against `src.Spec.Dataset`.

### 4.3 F6 — `promoteDirectCloneDependents` has no `PoolGUID` filter

[promote.go](../internal/controller/promote.go) (~line 181):

```go
if dep.Name == vol.Name || dep.Spec.Source == nil || dep.Spec.Source.Volume != vol.Spec.Dataset {
    continue
}
depFull, err := datasetName(poolName, dep.Spec.Dataset)   // built against *this* pool
```

The match is on dataset **path** only. A `ZfsDataset` on a different pool with the same
logical path would be selected and then promoted against the wrong pool. UUID-based naming
makes a collision very unlikely in practice, but the guard is one line and the failure mode
(blocked deletion, or acting on an unrelated dataset) is disproportionate.

Compare with `findDatasetByPath` in the same file, which *does* filter on `PoolGUID` —
the inconsistency suggests this is an oversight rather than a decision.

**Fix:** add `dep.Spec.PoolGUID != vol.Spec.PoolGUID → continue`.

### 4.4 F7 — Manually deleting a backing-clone `ZfsDataset` destroys the raw snapshot

`beforeDestroy` unconditionally destroys `full@<owner.Spec.SnapshotName>` whenever `vol`
is a backing clone. If an operator runs `kubectl delete zfsdataset csi-snap-<uuid>` while
the owning `ZfsSnapshot` is still alive and not being deleted, the raw snapshot is
destroyed and the snapshot's data is **irrecoverably lost**; `reconcileStandaloneCreate`
will then try to re-clone from a snapshot that no longer exists and park the `ZfsSnapshot`
in `Error`.

This is the only genuine data-loss path found, and it requires direct manipulation of an
internal CRD — but given the project's stated "accidental dataset deletions are
UNACCEPTABLE" priority, it is worth closing.

**Fix:** in `beforeDestroy`, if `vol` has a `ZfsSnapshot` ownerReference and that owner
still exists with a zero `deletionTimestamp`, return an error (refuse to proceed) instead
of destroying anything. The GC-driven path (owner deleted first) is unaffected.

---

## 5. Low findings

### 5.1 F8 — `FormatAndMount` doc comment describes the rejected behaviour

[internal/csi/mount.go](../internal/csi/mount.go) (~lines 46–52), on the `NodeMounter`
interface:

> ... If device already carries a filesystem, that on-disk type is used for the mount (and
> returned) instead of fsType, so a mismatched request can never fail with a bad-superblock
> error.

That is the behaviour commit `e70cb53` explicitly **reverted** (and which the redesign doc's
§2.7 addendum documents as wrong). The implementation now correctly fails loud. Fix the
comment — it is actively misleading on a data-integrity-relevant function, and a future
reader could "restore" the documented behaviour.

### 5.2 F9 — `backingCloneOwnerSnapshotName` doc comment contradicts its code

[promote.go](../internal/controller/promote.go) (~line 296): the comment says it returns
"a zero value with `ok=false` (not an error) if the owning `ZfsSnapshot` is already gone".
The code returns `("", true, nil)` in that branch. The code's behaviour is the intended
one (still clean up `@restore-source`); the comment is wrong.

### 5.3 F10 — "Immutable" spec fields have no CEL immutability rule

[api/v1alpha1/zfssnapshot_types.go](../api/v1alpha1/zfssnapshot_types.go): `SnapshotName`,
`SourceType` and the new `Mode` are all documented as immutable but carry no
`+kubebuilder:validation:XValidation:rule="self == oldSelf"`. Flipping `Mode` on a live
object would switch its teardown semantics (`standalone` promote path ⇄ `integrated`
blocking path) against ZFS state built for the other mode. Same applies to
`ZfsDataset.Spec.Dataset`, which is now an opaque generated identifier and must never
change.

### 5.4 F11 — `releaseSnapshotFinalizer` not hardened like `releaseFinalizer`

Commit `9db3041` correctly fixed `ZfsDatasetReconciler.releaseFinalizer` to re-read and
retry on conflict. `ZfsSnapshotReconciler.releaseSnapshotFinalizer` still does the stale
`RemoveFinalizer` + `Update`. The risk is lower there (nothing else mutates the
`ZfsSnapshot` object during the delete path today), but the asymmetry is a latent trap.

### 5.5 F12 — D10 compatibility checks silently no-op when the source volume is gone

[internal/csi/clone.go](../internal/csi/clone.go), the snapshot-restore variant of
`checkCloneCompatibility` looks up `snap.Spec.SourceVolume` live and returns `nil` if the
source `ZfsDataset` is gone. That is precisely the headline scenario for standalone
snapshots (snapshot outlives the volume). So a restore into a StorageClass with a different
`fsType` or `volblocksize` passes the controller check and instead surfaces much later as a
`NodeStageVolume` failure (`fsType`) or as a silently-ignored parameter (`volblocksize` —
`clone()` never passes `volblocksize`, so ZFS raises no error either).

**Fix:** capture `SourceFSType` and `SourceVolblocksize` (and any structural
`property.*` values) onto `ZfsSnapshotSpec` at `CreateSnapshot` time, mirroring what
`SourceType` already does for the same reason, and compare against those.

---

## 6. Design questions to re-settle while fixing

These are not defects on their own, but the §2 fixes cannot be completed without answering
them explicitly. Per the repo's append-only convention, resolve them as new decisions
(`D17`, `D18`, …) appended to
[snapshot-lifecycle-redesign.md §4](snapshot-lifecycle-redesign.md), not by editing D11/D16.

1. **Does D11's "zero snapshots" invariant survive?** It holds for the dataset being
   deleted but not for its promoted dependents. Either the invariant needs restating
   ("zero *untracked* snapshots, all driver-owned artifacts explicitly destroyed first"),
   or the generic cleanup in §2(b) must be treated as part of the invariant rather than an
   add-on.

2. **Is D16 really "no design change needed"?** The live test confirmed *convergence and
   order-independence of the promote batch*. It did **not** test any subsequent `zfs
   destroy` (the run deliberately executed none — see its §5), which is exactly where §2
   fails. D16's conclusion should be narrowed to what was actually verified.

3. **Should `DeleteSnapshot`'s raw-origin cleanup stay hard-gated?** §2.2 shows the current
   gating can deadlock. Options: promote the raw snapshot's dependents away first; or
   relax the gate and re-derive D11's safety from the generic artifact cleanup instead of
   from "no live `ZfsSnapshot` ⇒ no raw snapshot".

---

## 7. Suggested order of work

1. **F1 (RBAC).** Blocks all functional verification of everything else.
2. **F3 (test double).** Upgrade `fakeZFS` first, then confirm §2's scenarios *fail*
   before fixing them. Fixing §2 against the current fake proves nothing.
3. **F2 (generic artifact cleanup + uniform post-promote tracking).** The core fix.
4. **F4 (finalizer clearing order).**
5. **F5, F6, F7.**
6. **F8–F12** (documentation/consistency/hardening).
7. **Real-pool end-to-end re-verification.** Repeat the 2026-07-31 scenario, this time
   *including* the deletes: after the promote batch, delete each `csi-snap-tN` and `vol1`
   in scrambled order and confirm every object goes away cleanly with non-recursive
   destroys only. Also run: (a) direct clone + delete source + delete clone; (b) restore +
   delete snapshot + delete restored PVC; (c) two restores + delete snapshot + delete both
   in either order. Record the log the same way, as a new dated document.

---

## 8. What was checked and found correct (no action needed)

Recorded so a subsequent reviewer does not re-derive it:

- `zfs destroy -r` is gone everywhere — all four `ZFS.Destroy` call sites pass
  `recursive=false`.
- `isNotExist` / `isExists` string matching does **not** swallow "has dependent clones" or
  "filesystem has children" — those correctly surface as hard errors.
- `CLI.Promote`'s D13 verification (compare `origin` before/after, retry only if
  *unchanged*) correctly implements the corrected check from the verification log §4.2.
- Independent naming: `ensureVolume` / `ensureSnapshot` correctly treat an existing
  object's `Spec.Dataset` / `Spec.SnapshotName` as authoritative and drop them from the
  idempotency comparison; `ObjectMeta.Name` remains the CSI id; existing PVs keep working
  because `Spec.Dataset` is read from the object rather than recomputed.
- `cloneSnapshotSuffix` (sha256, 16 hex chars) is a sound sanitisation for the ephemeral
  `@clone-<hash>` suffix.
- `resolveContentSource` adds the `restored-by` finalizer **before** the `ZfsDataset` is
  created — correct ordering; a leaked finalizer from a failed `CreateVolume` is later
  garbage-collected by `promoteTrackedDependents`' `IsNotFound` branch.
- `addRestoredByFinalizer` correctly rejects a restore whose backing clone is already
  terminating (`FailedPrecondition`).
- Both CRDs are cluster-scoped, so the `ZfsSnapshot → ZfsDataset` ownerReference is valid
  (a namespaced/cluster-scoped mismatch would have caused immediate GC of the backing
  clone).
- `setStatus` / `recordFSType` both use `Status().Patch(..., client.MergeFrom(...))`, so
  the agent's status writes do not clobber the node plugin's `Status.FSType`.
- `reconcileStandaloneCreate`'s `path.Join(path.Dir(...))` and `normalizedDatasetPrefix`'s
  `"."` handling agree for the empty-prefix case.
- All reconcilers run with `MaxConcurrentReconciles: 1`, so the promote/destroy sequences
  are not racing themselves within a single agent.
- `FormatAndMount`'s implementation correctly fails loud on an fsType mismatch, matching
  `k8s.io/mount-utils` and ceph-csi (only the doc comment is stale — F8).
