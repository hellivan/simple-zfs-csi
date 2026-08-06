# Lifecycle protection matrix: who stops you from deleting something that is still needed

**Status:** reference. Cross-verified 2026-08-06 against upstream source code and
this repository's source code.

This document answers one question for every volume/snapshot lifecycle flow we
support:

> When object A depends on object B, what stops a user from deleting B?
> Kubernetes? The CSI layer? Us? Nobody?

It exists because we were about to build our own in-use protection (a lease
object, plus a scan over pending clones) on the assumption that Kubernetes did
not protect these flows. That assumption was **half wrong**, and the half that
was wrong would have cost us a redundant, leaky mechanism. The half that was
right is real and is documented in [§6](#6-gaps-that-are-genuinely-ours).

---

## 1. Method

Everything below was verified against a primary source. Three kinds of evidence
are used, and each claim says which one it rests on:

| Marker | Meaning |
| --- | --- |
| **[src]** | Read from upstream Go source (external-snapshotter, external-provisioner). Quoted verbatim. |
| **[doc]** | Read from official kubernetes.io / kubernetes-csi.github.io documentation. Quoted verbatim. |
| **[ours]** | Read from this repository. File and function named. |
| **[?]** | Not verified. Explicitly flagged. Do not build on it. |

Where upstream documentation and upstream source disagree in specificity, the
**source wins** and the doc is cited only as corroboration. Documentation
routinely omits finalizer names; source does not.

Nothing in this document is inferred from memory of how Kubernetes "usually"
behaves.

---

## 2. Executive summary

**For every flow that goes through the normal Kubernetes API path, the source
object is already protected upstream. We must not build a second protection
layer for those flows.**

There are exactly three places where nothing upstream protects us, and all three
are outside the CO's model of the world:

1. **A PVC with a completed integrated-mode snapshot.** Kubernetes drops its
   protection the moment a snapshot becomes `readyToUse`. Destroying the ZFS
   dataset would take the user's snapshot with it. **We must block. We do.**
2. **Direct manipulation of our own CRDs** (`kubectl delete zfsdataset ...`).
   Kubernetes has no idea these represent ZFS datasets with clone dependents.
   **We must block. We do.**
3. **ZFS-level clone dependencies**, which have no Kubernetes counterpart at all.
   These are not blocked — they are *resolved*, by `zfs promote`.

Everything else is already handled, and the correct driver behaviour is to do
nothing extra.

**One defect was found while writing this document.** Case 1 above is not merely
our own policy — the CSI spec makes it a **MUST**, and our `DeleteVolume` does
not comply: it returns `OK` where the spec requires `FAILED_PRECONDITION`. No
data is at risk, but the volume silently disappears from the user's view while
the `ZfsDataset` blocks forever, with no diagnostic. See
[§6.2](#62-deviation-deletevolume-returns-ok-when-it-must-return-failed_precondition).

---

## 3. The three layers of protection

Understanding which layer owns a given guarantee is what prevents duplicated
mechanisms.

```
┌─ Layer 1 ── Container Orchestrator ────────────────────────────────┐
│  kube-controller-manager   → pvc-protection, pv-protection         │
│  snapshot-controller       → snapshot source/bound protection      │
│  external-provisioner      → clone + snapshot source protection    │
│                                                                     │
│  Mechanism: finalizers on PVC / PV / VolumeSnapshot / Content.      │
│  Effect: the DELETE is accepted, deletionTimestamp is set, but the  │
│          object is not removed and no downstream CSI call fires.    │
└─────────────────────────────────────────────────────────────────────┘
                              ↓ only if layer 1 lets it through
┌─ Layer 2 ── CSI RPC contract ──────────────────────────────────────┐
│  DeleteVolume / DeleteSnapshot may return 9 FAILED_PRECONDITION     │
│  ("in use by another resource"). The CO retries with backoff.       │
│                                                                     │
│  This is the SP's chance to refuse something the CO thought was OK. │
└─────────────────────────────────────────────────────────────────────┘
                              ↓ only if we accept the call
┌─ Layer 3 ── Driver-internal (ours) ────────────────────────────────┐
│  ZfsDataset / ZfsSnapshot finalizers + beforeDestroy() guards.      │
│  Source of truth: the live ZFS clone graph (ADR-0020).              │
│                                                                     │
│  Protects against: out-of-band CR deletion, ZFS-level dependencies, │
│  and the one gap layer 1 leaves open (integrated-mode snapshots).   │
└─────────────────────────────────────────────────────────────────────┘
```

**Layer 1 protection propagates downward.** A PVC that cannot be deleted means
its PV is never released, which means `DeleteVolume` is never called, which means
our `ZfsDataset` is never deleted. Blocking at the top blocks the whole chain.
This is why we do not need to re-implement the same check at layer 3.

---

## 4. Inventory of upstream finalizers

All names below are copied verbatim from upstream source. This table is the
factual core of the document.

> **Full upstream reference:**
> [kubernetes-volume-lifecycle-facts.md](kubernetes-volume-lifecycle-facts.md)
> carries the complete, driver-agnostic fact sheet — every finalizer, the exact
> window each one covers, the CSI contract, and the misconceptions this
> investigation corrected. What follows here is the subset we depend on.

### 4.1 kube-controller-manager

From `kubernetes/kubernetes`, `pkg/volume/util/finalizer.go` **[src]**:

```go
const (
	// PVCProtectionFinalizer is the name of finalizer on PVCs that have a running pod.
	PVCProtectionFinalizer = "kubernetes.io/pvc-protection"

	// PVProtectionFinalizer is the name of finalizer on PVs that are bound by PVCs
	PVProtectionFinalizer = "kubernetes.io/pv-protection"
)
```

> "Storage object in use protection [...] the PVC removal is postponed until the
> PVC is no longer actively used by any Pods." — kubernetes.io, *Persistent
> Volumes* **[doc]**

The finalizers docs page names `kubernetes.io/pv-protection` as the canonical
example of a finalizer used to **prevent** deletion, not to clean up:

> "Finalizers [...] tell Kubernetes to **wait until specific conditions are met
> before it fully deletes** resources marked for deletion. [...] **You can also
> use finalizers to prevent deletion of unmanaged resources.**" — kubernetes.io,
> *Finalizers* **[doc]**

This directly refutes the belief that finalizers are only a cleanup hook. Using
a finalizer as a deletion gate is the documented, intended pattern.

### 4.2 snapshot-controller (external-snapshotter)

From `pkg/utils/util.go` **[src]**:

```go
// Name of finalizer on VolumeSnapshotContents that are bound by VolumeSnapshots
VolumeSnapshotContentFinalizer = "snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"
// Name of finalizer on VolumeSnapshot that is being used as a source to create a PVC
VolumeSnapshotBoundFinalizer = "snapshot.storage.kubernetes.io/volumesnapshot-bound-protection"
// Name of finalizer on VolumeSnapshot that is used as a source to create a PVC
VolumeSnapshotAsSourceFinalizer = "snapshot.storage.kubernetes.io/volumesnapshot-as-source-protection"
// Name of finalizer on PVCs that is being used as a source to create VolumeSnapshots
PVCFinalizer = "snapshot.storage.kubernetes.io/pvc-as-source-protection"
// Name of finalizer on VolumeSnapshot that is a member of a VolumeGroupSnapshot
VolumeSnapshotInGroupFinalizer = "snapshot.storage.kubernetes.io/volumesnapshot-in-group-protection"
```

### 4.3 external-provisioner

From `pkg/controller/controller.go` **[src]**:

```go
pvcCloneFinalizer = "provisioner.storage.kubernetes.io/cloning-protection"

// snapshotSourceProtectionFinalizer is managed by the external-provisioner to track
// in-progress provisioning operations. It's distinct from the external-snapshotter's own
// "volumesnapshot-as-source-protection" finalizer, which will be deprecated and removed in a future release.
snapshotSourceProtectionFinalizer = "provisioner.storage.kubernetes.io/volumesnapshot-as-source-protection"
```

Note the migration in progress: the snapshot-as-source protection is moving from
the snapshot-controller to the external-provisioner. **Both** exist today; the
snapshot-controller's copy is documented as being on its way out. Any code we
write that matches on the exact finalizer string would break during this
migration — another reason not to depend on it.

---

## 5. Use-case matrix

### 5.1 Create PVC from PVC (clone)

**Flow:** `PVC-B.spec.dataSource → PersistentVolumeClaim/PVC-A`.

**Kubernetes protection: YES.** `provisioner.storage.kubernetes.io/cloning-protection`
is placed on the *source* PVC by external-provisioner. From
`pkg/controller/clone_controller.go` **[src]**:

> "PVC referenced as a data source is now updated with a finalizer
> `provisioner.storage.kubernetes.io/cloning-protection` during a `ProvisionExt`
> method call. The detection of cloning in-progress is based on the assumption
> that a PVC with `spec.DataSource` pointing on a another PVC will go into
> `Pending` state. The downside of this, is that fact that any other reason
> causing PVC to stay in the `Pending` state also blocks resource from deletion"

Finalizer removal is handled by a dedicated worker pool, sized by
`--cloning-protection-threads` (default `1`) **[src]**.

Note the upstream-acknowledged weakness: the heuristic is "target PVC is
Pending", so an unrelated reason for pending-ness over-blocks the source. It errs
toward safety.

**After the clone completes**, protection is dropped, and this is intentional
**[doc]**:

> "Users should be aware [...] the source PVC may also be modified or deleted
> without affecting the newly created clone" — kubernetes.io, *CSI Volume
> Cloning*

This is safe for us because a completed ZFS clone is detached by `zfs promote`
on the delete path — see [§6.4](#64-zfs-level-clone-dependencies).

**What we do [ours]:** `resolveContentSource` in
[internal/csi/clone.go](internal/csi/clone.go) builds a `DatasetSource`; the
delete path in [internal/controller/promote.go](internal/controller/promote.go)
promotes dependents away. We add **no** clone-source lock.

**Verdict: no driver-side protection needed.** Building one would duplicate
`cloning-protection` and inherit its over-blocking behaviour without its
sweeper.

---

### 5.2 Create snapshot from PVC

**Flow:** `VolumeSnapshot.spec.source.persistentVolumeClaimName → PVC-A`.

**Kubernetes protection: YES, but only during creation.**
`snapshot.storage.kubernetes.io/pvc-as-source-protection` is added to the source
PVC in `createSnapshotContent` **[src]**:

> "If PVC is not being deleted and finalizer is not added yet, a finalizer
> should be added to PVC until snapshot is created"

The scope is precisely defined by `isPVCBeingUsed` **[src]**:

```go
if snap.Spec.Source.PersistentVolumeClaimName != nil &&
   pvc.Name == *snap.Spec.Source.PersistentVolumeClaimName &&
   !utils.IsSnapshotReady(snap) {
        klog.V(2).Infof("Keeping PVC %s/%s, it is used by snapshot %s/%s", ...)
        return true
}
```

**`!utils.IsSnapshotReady(snap)` is the critical clause.** The instant the
snapshot becomes `readyToUse`, `checkandRemovePVCFinalizer` strips the finalizer
and the source PVC becomes freely deletable **even though the snapshot still
exists and still points at it**.

That is correct for storage systems where a snapshot is an independent object.
It is **not** correct for ZFS integrated-mode snapshots, where the snapshot is a
child of the live dataset. See [§6.1](#61-a-pvc-with-a-completed-integrated-mode-snapshot).

**What we do [ours]:** `CreateSnapshot` in
[internal/csi/snapshot.go](internal/csi/snapshot.go) creates a `ZfsSnapshot`.
We add no PVC-level protection (we could not — it is not our object).

**Verdict: no driver-side protection needed for the creation window.**
Driver-side protection **is** needed for the *post*-creation window in integrated
mode; that is §6.1.

---

### 5.3 Create PVC from snapshot (restore)

**Flow:** `PVC-B.spec.dataSource → VolumeSnapshot/S`.

**Kubernetes protection: YES — doubly.** Both the snapshot-controller's
`volumesnapshot-as-source-protection` and external-provisioner's newer
`provisioner.storage.kubernetes.io/volumesnapshot-as-source-protection` guard
this window.

The snapshot-controller's enforcement is explicit **[src]**:

```go
// check if the snapshot is being used for restore a PVC, if yes, return an error
// so the workqueue will requeue this snapshot and retry deletion when the PVC
// is no longer in use (e.g., binding completed or PVC deleted).
if content != nil && ctrl.isVolumeBeingCreatedFromSnapshot(snapshot) {
        ctrl.eventRecorder.Event(snapshot, v1.EventTypeWarning,
                "SnapshotDeletePending", "Snapshot is being used to restore a PVC")
        return fmt.Errorf("snapshot %s is in use (being used to restore a PVC), will retry deletion", ...)
}
```

and `isVolumeBeingCreatedFromSnapshot` **[src]** defines "in use" as: some PVC in
the namespace has `DataSource.Kind == "VolumeSnapshot"` naming this snapshot and
`pvc.Status.Phase == v1.ClaimPending`.

Note the deliberate asymmetry, documented in `checkandAddSnapshotFinalizers`
**[src]**:

> "Source finalizer will be added to snapshot if DeletionTimeStamp is nil and it
> is not set yet. This is because the logic to check whether a PVC is being
> created from the snapshot is expensive so we only go through it when we need to
> remove this finalizer"

i.e. the finalizer is added *eagerly and unconditionally*, and the expensive
in-use scan runs only at removal time. **A snapshot that could possibly be
restored from always carries the finalizer.**

#### 5.3.1 The closure argument

This is the reasoning that makes the guarantee total rather than probabilistic:

- The `snapshot.storage.k8s.io` CRDs and the snapshot-controller are installed
  and versioned as a single unit.
- Without them, no `VolumeSnapshot` object can exist.
- external-provisioner resolves a snapshot data source by fetching that object
  — `getSnapshotSource` in `pkg/controller/controller.go` **[src]**:

  ```go
  snapshotObj, err := p.snapshotClient.SnapshotV1().VolumeSnapshots(dataSource.Namespace).Get(ctx, dataSource.Name, metav1.GetOptions{})
  ```

  With no CRD, that `Get` fails and provisioning never proceeds.
- Therefore our `CreateVolume`-from-snapshot path is never invoked.

**Absence of the protection and absence of the risk are the same condition.**
There is no deployment in which we are exposed. This cluster is itself an
instance of the negative case: it has no `snapshot.storage.k8s.io` CRDs and no
snapshot-controller, so none of our snapshot code can run there at all.

**What we do [ours]:** `resolveContentSource` → `ensureVolume` →
`waitVolumeReady` in [internal/csi/controller.go](internal/csi/controller.go).

**Verdict: no driver-side protection needed.** Specifically, this retires the
proposed restore lease and removes the CO-driven justification for the pending
clone scan.

---

### 5.4 Delete PVC that is mounted by a Pod

**Kubernetes protection: YES** — `kubernetes.io/pvc-protection` **[doc]**.
Deletion is postponed until no Pod uses the PVC. Nothing reaches us.

**Verdict: no driver-side protection needed.**

---

### 5.5 Delete PV bound to a PVC

**Kubernetes protection: YES** — `kubernetes.io/pv-protection` (§4.1) **[src]**.

There is additionally a finalizer on CSI PVs which ensures the PV object
outlives the backing storage so that `DeleteVolume` is not lost. It is defined
in the shared provisioner library, **not** in external-provisioner itself —
`kubernetes-sigs/sig-storage-lib-external-provisioner`, `controller/controller.go`
**[src]**:

```go
// Finalizer for PVs so we know to clean them up
const finalizerPV = "external-provisioner.volume.kubernetes.io/finalizer"
```

external-provisioner enables it unconditionally — `controller.AddFinalizer(true)`
in `cmd/csi-provisioner/csi-provisioner.go` **[src]**. This is a *cleanup
ordering* finalizer, not an in-use gate.

**Verdict: no driver-side protection needed.**

---

### 5.6 Delete a snapshot while a restore is in flight

Covered by [§5.3](#53-create-pvc-from-snapshot-restore). Upstream blocks it and
emits a `SnapshotDeletePending` warning event.

**Verdict: no driver-side protection needed.**

---

### 5.7 Delete a snapshot with no dependents

**Flow:** `VolumeSnapshot` deleted → content deleted → `DeleteSnapshot` →
`ZfsSnapshot` deleted.

**What we do [ours]:** `reconcileDelete` in
[internal/controller/zfssnapshot_controller.go](internal/controller/zfssnapshot_controller.go)
calls `detachSnapshotClones` before destroying the raw ZFS snapshot, because a
ZFS snapshot with dependent clones cannot be destroyed. This is ZFS-level
dependency resolution, not policy — see §6.4.

**Verdict: correct as-is.**

---

### 5.8 Delete a PVC that has completed clones

**Kubernetes protection: NONE, deliberately** — see the quote in §5.1.

**What we do [ours]:** `detachAndCleanSnapshots` promotes every dependent clone
away, then destroys. Verified against a real pool (18/18 checks,
[docs/delete-path-verification-2026-08-03.md](docs/delete-path-verification-2026-08-03.md)).

**Verdict: no *blocking* needed. Resolution, not refusal.** This is the right
answer and it matches what the CO promises users.

---

### 5.9 Delete a PVC that has completed snapshots

This splits by snapshot mode and is the single most important row in this table.

| Mode | Kubernetes protects? | Must we? | Why |
| --- | --- | --- | --- |
| standalone | No (finalizer already removed) | **No** | The snapshot's data lives in a backing clone; promote detaches it and the snapshot survives. |
| integrated | No (finalizer already removed) | **YES** | The snapshot is a child of the live dataset. Destroying the dataset destroys the user's snapshot. |

See [§6.1](#61-a-pvc-with-a-completed-integrated-mode-snapshot).

---

### 5.10 Pre-provisioned snapshots

**Flow:** an admin creates a `VolumeSnapshotContent` with
`spec.source.snapshotHandle` and a `VolumeSnapshot` pointing at it.

The `volumesnapshotcontent-bound-protection` finalizer keeps the content alive
while bound **[src]**. Additionally, the controller refuses to cascade deletion
when the snapshot object is missing entirely **[src]**:

> "NOTE(xyang): Do not trigger content deletion if snapshot is nil. This is to
> avoid data loss if the user copied the yaml files and expect it to work in a
> different setup. In this case snapshot is nil. If we trigger content deletion,
> it will delete physical snapshot resource on the storage system and result in
> data loss!"

**[ours]** `assertDriverSnapshot` / the `driverSnapshotSuffix` regex
(`^(restore-source|clone-[0-9a-f]{16}|csi-snap-.+)$`) in
[internal/controller/promote.go](internal/controller/promote.go) refuses to touch
snapshots we did not create (D18). A hand-made ZFS snapshot on one of our
datasets is never destroyed by our delete path.

**Verdict: aligned. No change needed.**

---

### 5.11 Volume expansion

`ControllerExpandVolume` **[ours]**,
[internal/csi/controller.go](internal/csi/controller.go). Expansion does not
create or remove dependencies between objects, so it has no in-use protection
dimension.

**Verdict: out of scope for this document.**

---

### 5.12 Direct deletion of our CRDs

**Flow:** `kubectl delete zfsdataset <name>` / `kubectl delete zfssnapshot <name>`.

**Kubernetes protection: NONE.** The CO's finalizers live on PVCs, PVs and
VolumeSnapshots. It has no knowledge that a `ZfsDataset` is the clone origin of
another `ZfsDataset`, or that a `ZfsDataset` is the backing clone owned by a
live `ZfsSnapshot`.

This is the one entry point where every CO-layer guarantee is bypassed, and it is
why layer 3 exists at all. See [§6.3](#63-out-of-band-deletion-of-our-own-crds).

---

## 6. Gaps that are genuinely ours

### 6.1 A PVC with a completed integrated-mode snapshot

> **RESOLVED (2026-08-06): this gap no longer exists.** It existed only because
> `integrated` mode existed, and that mode was removed — ADR-0027, implemented.
> Every snapshot is now backed by its own clone, which puts the driver in the CSI
> spec's "supports deleting a volume without affecting its existing snapshots"
> branch, where no refusal is required at all. `checkSnapshotDependents` keeps
> only its "snapshot not yet Ready" clause.
>
> The section is kept because the *reasoning* still matters: it is the worked
> example of a storage backend whose snapshots are not independent of their
> source, and of what upstream does and does not protect in that case. The
> §6.2 deviation below is likewise now unreachable in practice, but remains a
> real spec deviation worth fixing.

**The gap:** `isPVCBeingUsed` returns false once the snapshot is ready (§5.2), so
Kubernetes will happily delete the PVC → PV → call `DeleteVolume`. In integrated
mode the `ZfsSnapshot` *is* a raw snapshot hanging off the live dataset. A
`zfs destroy` of the dataset takes the snapshot with it. The user's
`VolumeSnapshot` object would still exist, still claim `readyToUse: true`, and
the data would be gone.

**Why upstream does not cover it:** the CSI model assumes a snapshot is
independent of its source volume once created. That assumption holds for most
backends. It does not hold for a ZFS snapshot in integrated mode.

**The CSI spec mandates our behaviour here.** From `spec@v1.11.0/spec.md`,
`DeleteVolume` **[src]**:

> CSI plugins SHOULD treat volumes independent from their snapshots.
>
> If the Controller Plugin supports deleting a volume without affecting its
> existing snapshots, then these snapshots MUST still be fully operational and
> acceptable as sources for new volumes as well as appear on `ListSnapshot` calls
> once the volume has been deleted.
>
> **When a Controller Plugin does not support deleting a volume without affecting
> its existing snapshots, then the volume MUST NOT be altered in any way by the
> request and the operation must return the `FAILED_PRECONDITION` error code** and
> MAY include meaningful human-readable information in the `status.message` field.

The two branches map exactly onto our two snapshot modes: `standalone` is the
first, `integrated` is the second. Corroborated by the `DeleteVolume` error table
**[src]**, which defines "Volume in use" as `9 FAILED_PRECONDITION` when the volume
"is in use by another resource **or has snapshots and the plugin doesn't treat
them as independent entities**".

**What we do [ours]:** `checkSnapshotDependents` in
[internal/controller/promote.go](internal/controller/promote.go):

```go
if snap.Status.Phase != storagev1alpha1.SnapshotPhaseReady {
        return fmt.Errorf("volume %q has snapshot %q still in phase %q; requeue", ...)
}
if effectiveSnapshotMode(snap) != storagev1alpha1.SnapshotModeStandalone {
        return fmt.Errorf("volume %q has a live integrated-mode snapshot %q; delete it before deleting the volume", ...)
}
```

The first clause also covers the reverse window — a snapshot that is still being
taken, whose backing clone may not exist yet.

**Verdict: required, implemented — but not correctly surfaced. See §6.2.**

### 6.2 DEVIATION: `DeleteVolume` reports success for a delete that was refused

**Re-assessed 2026-08-06 after ADR-0027.** Removing `integrated` mode retired the
CSI *snapshot* clause that originally motivated this section (a plugin that cannot
delete a volume without affecting its snapshots MUST return `FAILED_PRECONDITION`)
— we are now in the compliant branch. **The defect itself survives, and its real
trigger turns out to be more mundane and more likely than the one first described.**

`DeleteVolume` in [internal/csi/controller.go](internal/csi/controller.go)
**[ours]** is fire-and-forget:

```go
vol := &storagev1alpha1.ZfsDataset{ObjectMeta: metav1.ObjectMeta{Name: id}}
if err := c.Client.Delete(ctx, vol); err != nil && !apierrors.IsNotFound(err) {
        return nil, status.Errorf(codes.Internal, "delete ZfsDataset %q: %v", id, err)
}

c.Log.Info("deprovisioned volume", "name", id)
return &csi.DeleteVolumeResponse{}, nil
```

It issues the `Delete` and returns `OK` without reading back whether the reconciler
accepted it.

#### What can still refuse, verified by execution

Two guards still refuse **permanently** — until a human intervenes — and both are
reachable through an ordinary `DeleteVolume`:

| Trigger | Guard | Observed error |
| --- | --- | --- |
| A snapshot on the PVC's dataset that the driver did not create | `assertDriverSnapshot` (D18) | `snapshot "tank/k8s/pvc-1@auto-2026-08-06-0300" was not created by this driver; refusing to destroy it — remove it manually to continue` |
| A clone of one of our snapshots with no `ZfsDataset` behind it | `assertKnownDatasets` (D18) | `snapshot "…@csi-snap-x" is cloned by "tank/k8s/hand-made-clone", which is not a known ZfsDataset on this pool; refusing to promote a dataset the driver does not manage — resolve it manually` |

**Both refusals are correct** — refusing is far better than destroying state the
driver does not own. The problem is entirely in how the refusal is (not) reported.

**The first row is not hypothetical for this deployment.** Periodic snapshots from
a replication or auto-snapshot tool (`zfs-auto-snapshot`, TrueNAS periodic tasks,
anything feeding the `zfs send -R` workflow of §2.5) land exactly there, named
exactly like that. Any PVC that has been captured by such a tool will refuse to
delete.

The remaining guards are transient by nature and self-resolve:
`checkSnapshotDependents` (a dependent snapshot not yet `Ready`) and
`checkPendingCloneDependents` (a declared-but-unprovisioned clone).

#### Consequences

1. **No data is lost.** The finalizer holds and the dataset survives. The guards do
   their job.
2. `OK` causes external-provisioner to delete the PV. **The volume disappears from
   the user's view while the `ZfsDataset` remains, blocked, indefinitely** — and
   nothing retries, because the CO was told the operation succeeded.
3. **The user gets no diagnostic.** The reason exists only in reconciler logs. The
   `status.message` channel the CSI spec offers for exactly this is unused, and
   `ZfsDataset.Status` is not updated on the refusal either.

The net effect is a silent storage leak with a manual, undocumented recovery path
— the operator has to know to go looking for a `Terminating` `ZfsDataset` and read
node-agent logs.

#### Direction (proposed, not implemented)

Make `DeleteVolume` symmetric with `CreateVolume`, which already blocks on
`waitVolumeReady`:

1. The delete path records its refusal on `ZfsDataset.Status` (phase + message)
   instead of only returning an error into the requeue loop.
2. `DeleteVolume` polls, bounded by a timeout, for the object to disappear. If a
   refusal is reported instead, it returns `codes.FailedPrecondition` carrying that
   message.

The CO then retries with exponential backoff — precisely the behaviour the spec
prescribes — the PV is not deleted, and `kubectl describe pvc` shows the real
reason.

**Status: not implemented, awaiting decision.**

### 6.3 Out-of-band deletion of our own CRDs

**The gap:** §5.12. Nothing upstream watches our CRDs.

**What we do [ours]**, all in
[internal/controller/promote.go](internal/controller/promote.go):

- `checkOwningSnapshotLive` — refuses to destroy a standalone backing clone while
  the `ZfsSnapshot` that owns it is still live. Garbage collection (owner already
  gone) and the reconciler's own explicit delete (owner terminating) both pass;
  a hand-run `kubectl delete zfsdataset csi-snap-<uuid>` does not.
- `checkPendingCloneDependents` — refuses to destroy a dataset that some other
  `ZfsDataset` has declared as its clone source but whose ZFS dataset does not
  exist yet. Such a dependent is **invisible in the ZFS clone graph**, because the
  CSI controller creates the object before the agent runs `zfs clone`.

On `checkPendingCloneDependents` specifically: §5.1 and §5.3 show the CO already
prevents this from being reachable via the normal path (the source PVC or source
VolumeSnapshot cannot be deleted while a dependent PVC is `Pending`). Its
remaining value is exactly this section's scenario — direct CR deletion — plus
the crash window in which a CO-layer finalizer has leaked. It is not redundant,
but its justification is narrower than originally stated, and the doc comment on
the function should be read with that scope in mind.

### 6.4 ZFS-level clone dependencies

**The gap:** ZFS refuses `zfs destroy` on a dataset that has snapshots, and on a
snapshot that has dependent clones. This is a storage-engine constraint with no
Kubernetes counterpart.

**We do not treat this as a protection problem.** We resolve it:
`detachAndCleanSnapshots` promotes dependents away until nothing depends on the
target, then destroys non-recursively. The source of truth is the live ZFS clone
graph queried at delete time (ADR-0020) — never a mirror of it held in
Kubernetes.

The reason that distinction matters is recorded in
[docs/known-pitfalls.md](docs/known-pitfalls.md) class 17: a single `zfs promote`
rewrites **four** edges at once, so any Kubernetes-side mirror of the graph is
wrong the moment a promote happens.

---

## 7. Conclusions

1. **Do not build a restore lease.** §5.3 plus the closure argument in §5.3.1
   show the window is fully covered upstream, in every cluster where it can
   occur.
2. **Do not build clone-source protection.** §5.1 shows
   `provisioner.storage.kubernetes.io/cloning-protection` already does it, and
   already handles the corner cases we would get wrong.
3. **Keep `checkSnapshotDependents`.** §6.1 is a real gap with a real data-loss
   consequence, and it is ours alone.
4. **Keep `checkOwningSnapshotLive` and `checkPendingCloneDependents`**, but
   understand their scope as out-of-band and crash-window protection (§6.3), not
   as the primary defence for CO-driven flows.
5. **Never match on upstream finalizer strings.** The snapshot-as-source
   protection is mid-migration from snapshot-controller to external-provisioner
   (§4.3); code keyed on the exact name would silently stop working.
6. **Do not mistake "Kubernetes allows it" for "it is safe".** §5.8 and §5.9 are
   both allowed by Kubernetes; one is safe because we resolve it, the other is
   unsafe and we block it. The CO's model is not ZFS's model.
7. **Fix `DeleteVolume` to return `FAILED_PRECONDITION`** (§6.2). This is the
   only actionable code defect this investigation produced.

---

## 8. Open questions and unverified items

- **[?]** Whether the external-provisioner snapshot-source finalizer fully
  replaces the snapshot-controller one in the version we deploy, or whether both
  are active simultaneously. Both exist in current master. This affects nothing
  we implement (per conclusion 5) but would matter for troubleshooting a stuck
  snapshot deletion.
- **[?]** Behaviour of `cloning-protection` when the source PVC and target PVC
  are in different namespaces (cross-namespace data sources,
  `AnyVolumeDataSource`). Not investigated; we do not support it today.
- **Known upstream fragility, verified:** these finalizers can leak. The
  external-provisioner ships `--snapshot-orphan-sweep-interval` (default `5m`)
  precisely for this **[src]**:

  > "How often to check for orphaned snapshot source-protection finalizers. These
  > finalizers can become stuck if the provisioner crashes during snapshot-based
  > provisioning. Set to `0` to disable."

  This is worth knowing for two reasons. First, if a user reports a
  `VolumeSnapshot` stuck in `Terminating`, a leaked upstream finalizer is a
  likelier cause than anything in our code. Second, it is direct evidence that
  the "just add a finalizer" approach we considered has a failure mode requiring
  its own reaper — additional support for not building one.

---

## 9. Sources

**Upstream source code (read directly):**

- `kubernetes-csi/external-snapshotter` — `pkg/utils/util.go`,
  `pkg/common-controller/snapshot_controller.go`
- `kubernetes-csi/external-provisioner` — `pkg/controller/controller.go`,
  `pkg/controller/clone_controller.go`,
  `pkg/controller/snapshot_finalizer_controller.go`,
  `cmd/csi-provisioner/csi-provisioner.go`, `README.md`, `CHANGELOG/CHANGELOG-1.6.md`

**Documentation:**

- kubernetes.io — *Finalizers*, *Persistent Volumes*, *Volume Snapshots*,
  *CSI Volume Cloning*
- kubernetes-csi.github.io — *Volume Cloning*, *Snapshot Restore Feature*
- CSI specification v1.11.0 (vendored) — `DeleteVolume`, `DeleteSnapshot` error
  code tables

**This repository:**

- [internal/controller/promote.go](internal/controller/promote.go)
- [internal/controller/zfssnapshot_controller.go](internal/controller/zfssnapshot_controller.go)
- [internal/csi/clone.go](internal/csi/clone.go),
  [internal/csi/snapshot.go](internal/csi/snapshot.go),
  [internal/csi/controller.go](internal/csi/controller.go)
- [docs/design-decisions.md](docs/design-decisions.md) — ADR-0020
- [docs/snapshot-lifecycle-redesign.md](docs/snapshot-lifecycle-redesign.md) — decision log
- [docs/known-pitfalls.md](docs/known-pitfalls.md) — class 17
- [docs/delete-path-verification-2026-08-03.md](docs/delete-path-verification-2026-08-03.md)
