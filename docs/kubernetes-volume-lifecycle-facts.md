# Kubernetes volume lifecycle: fact sheet

**Purpose:** a standalone, driver-agnostic reference for how Kubernetes actually
manages the PVC / PV / VolumeSnapshot lifecycle, and precisely which finalizers
gate which deletions.

**Scope:** upstream behaviour only. Nothing about this driver appears here. For
how these facts apply to *us*, see
[lifecycle-protection-matrix.md](lifecycle-protection-matrix.md).

**Compiled:** 2026-08-06.

---

## 0. Rules this document follows

1. **Every claim is quoted from a primary source** — upstream Go source, or
   official documentation. The source is named for each claim.
2. **Source outranks documentation.** Documentation routinely omits finalizer
   names and timing details; source does not. Where only documentation was
   available, it is marked.
3. **Nothing is written from recollection of "how Kubernetes usually behaves".**
   Several statements that felt obvious turned out to be wrong when checked; the
   ones that were corrected are listed in [§9](#9-common-misconceptions-corrected).
4. **Unverified items are marked [?] and must not be built on.**

**Versions read:** `kubernetes/kubernetes` master; `external-snapshotter` v8
(import path `github.com/kubernetes-csi/external-snapshotter/v8`);
`external-provisioner` master; `sig-storage-lib-external-provisioner` master;
CSI specification v1.11.0. Behaviour differs across versions — see
[§10](#10-version-caveats).

---

## 1. The components

| Component | Ships with | Watches | Owns finalizers on |
| --- | --- | --- | --- |
| `pvc-protection-controller` | kube-controller-manager (in-tree) | PVCs, Pods | PVC |
| `pv-protection-controller` | kube-controller-manager (in-tree) | PVs | PV |
| `snapshot-controller` | external-snapshotter (**optional add-on**) | VolumeSnapshot, VolumeSnapshotContent, PVC | PVC, VolumeSnapshot, VolumeSnapshotContent |
| `csi-snapshotter` sidecar | external-snapshotter | VolumeSnapshotContent | — (calls CSI RPCs) |
| `external-provisioner` sidecar | external-provisioner | PVC, PV, VolumeSnapshot | PV, PVC, VolumeSnapshot |

The critical structural fact: **`snapshot-controller` and the
`snapshot.storage.k8s.io` CRDs are an optional add-on installed as one unit.** A
cluster without them cannot have a `VolumeSnapshot` object at all, and
`external-provisioner` cannot resolve a snapshot data source, because it fetches
the object directly (`pkg/controller/controller.go`, `getSnapshotSource`):

```go
snapshotObj, err := p.snapshotClient.SnapshotV1().VolumeSnapshots(dataSource.Namespace).Get(ctx, dataSource.Name, metav1.GetOptions{})
```

Consequence: **the snapshot protections and the snapshot risks are present or
absent together.** There is no configuration in which snapshots work but their
protections are missing.

---

## 2. Complete finalizer reference

Every string below is copied verbatim from source. This is the factual core of
the document.

### 2.1 On PersistentVolumeClaim

| Finalizer | Added by | Held while | Source |
| --- | --- | --- | --- |
| `kubernetes.io/pvc-protection` | kube-controller-manager | a Pod is using the PVC | `pkg/volume/util/finalizer.go` |
| `snapshot.storage.kubernetes.io/pvc-as-source-protection` | snapshot-controller | a snapshot of this PVC is **not yet ready** | `pkg/utils/util.go` |
| `provisioner.storage.kubernetes.io/cloning-protection` | external-provisioner | a PVC cloning from this one is **Pending** | `pkg/controller/controller.go` |

```go
// kubernetes/kubernetes, pkg/volume/util/finalizer.go
const (
	// PVCProtectionFinalizer is the name of finalizer on PVCs that have a running pod.
	PVCProtectionFinalizer = "kubernetes.io/pvc-protection"

	// PVProtectionFinalizer is the name of finalizer on PVs that are bound by PVCs
	PVProtectionFinalizer = "kubernetes.io/pv-protection"
)
```

### 2.2 On PersistentVolume

| Finalizer | Added by | Held while | Source |
| --- | --- | --- | --- |
| `kubernetes.io/pv-protection` | kube-controller-manager | the PV is bound to a PVC | `pkg/volume/util/finalizer.go` |
| `external-provisioner.volume.kubernetes.io/finalizer` | external-provisioner (via shared lib) | backing storage has not been deleted yet | `sig-storage-lib-external-provisioner`, `controller/controller.go` |

```go
// sig-storage-lib-external-provisioner, controller/controller.go
// Finalizer for PVs so we know to clean them up
const finalizerPV = "external-provisioner.volume.kubernetes.io/finalizer"
```

Enabled unconditionally by external-provisioner
(`cmd/csi-provisioner/csi-provisioner.go`):

```go
controller.RetryIntervalMax(*retryIntervalMax),
controller.AddFinalizer(true),
```

This one is **cleanup ordering, not an in-use gate**: it guarantees the PV object
outlives the storage so `DeleteVolume` is not lost.

### 2.3 On VolumeSnapshot

| Finalizer | Added by | Held while | Source |
| --- | --- | --- | --- |
| `snapshot.storage.kubernetes.io/volumesnapshot-as-source-protection` | snapshot-controller | a PVC is being restored from it | `pkg/utils/util.go` |
| `snapshot.storage.kubernetes.io/volumesnapshot-bound-protection` | snapshot-controller | bound to a content with `DeletionPolicy: Delete` | `pkg/utils/util.go` |
| `snapshot.storage.kubernetes.io/volumesnapshot-in-group-protection` | snapshot-controller | it is a member of a VolumeGroupSnapshot | `pkg/utils/util.go` |
| `provisioner.storage.kubernetes.io/volumesnapshot-as-source-protection` | external-provisioner | provisioning from it is in progress | `pkg/controller/controller.go` |

```go
// external-snapshotter, pkg/utils/util.go
// Name of finalizer on VolumeSnapshotContents that are bound by VolumeSnapshots
VolumeSnapshotContentFinalizer = "snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"
// Name of finalizer on VolumeSnapshot that is being used as a source to create a PVC
VolumeSnapshotBoundFinalizer = "snapshot.storage.kubernetes.io/volumesnapshot-bound-protection"
// Name of finalizer on VolumeSnapshot that is used as a source to create a PVC
VolumeSnapshotAsSourceFinalizer = "snapshot.storage.kubernetes.io/volumesnapshot-as-source-protection"
// Name of finalizer on PVCs that is being used as a source to create VolumeSnapshots
PVCFinalizer = "snapshot.storage.kubernetes.io/pvc-as-source-protection"
```

```go
// external-provisioner, pkg/controller/controller.go
pvcCloneFinalizer = "provisioner.storage.kubernetes.io/cloning-protection"

// snapshotSourceProtectionFinalizer is managed by the external-provisioner to track
// in-progress provisioning operations. It's distinct from the external-snapshotter's own
// "volumesnapshot-as-source-protection" finalizer, which will be deprecated and removed in a future release.
snapshotSourceProtectionFinalizer = "provisioner.storage.kubernetes.io/volumesnapshot-as-source-protection"
```

**Note the duplication and the migration.** Two different components currently
place a snapshot-as-source protection finalizer, under two different prefixes.
Upstream states the snapshot-controller's copy "will be deprecated and removed in
a future release". Any code that matches on the exact string will break.

### 2.4 On VolumeSnapshotContent

| Finalizer | Added by | Held while | Source |
| --- | --- | --- | --- |
| `snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection` | snapshot-controller | bound to a VolumeSnapshot | `pkg/utils/util.go` |

---

## 3. What "protection" actually means

From kubernetes.io, *Finalizers*:

> "Finalizers [...] tell Kubernetes to **wait until specific conditions are met
> before it fully deletes** resources marked for deletion. [...] **You can also
> use finalizers to prevent deletion of unmanaged resources.**"

The same page names `kubernetes.io/pv-protection` as its worked example.

Mechanically:

1. `DELETE` is **accepted**. The API server does not reject it.
2. `metadata.deletionTimestamp` is set. The object enters `Terminating`.
3. The object is **not removed** while `metadata.finalizers` is non-empty.
4. No downstream action fires. A PVC that cannot finish deleting never releases
   its PV, so `DeleteVolume` is never called.

**Protection propagates downward through the binding chain.** Blocking at the PVC
blocks the PV, which blocks the CSI call, which blocks whatever the driver would
have done.

**A finalizer is not a lock and carries no data.** It is a boolean gate. Encoding
information in a finalizer's *name* and parsing it back out is an anti-pattern —
the API guarantees nothing about it beyond presence/absence.

---

## 4. Provisioning flows

### 4.1 Empty volume

```
PVC created → external-provisioner → CreateVolume → PV created → bound
```

Finalizers involved: `pv-protection` once bound;
`external-provisioner.volume.kubernetes.io/finalizer` on the PV.

### 4.2 Clone from another PVC

```
PVC-B.spec.dataSource = {kind: PersistentVolumeClaim, name: PVC-A}
  → external-provisioner adds cloning-protection to PVC-A
  → CreateVolume(volume_content_source: volume{PVC-A's handle})
  → PV-B created, PVC-B Bound
  → cloning-protection removed from PVC-A
```

From `pkg/controller/clone_controller.go` (verbatim):

> "This package introduces a way to handle finalizers, related to in-progress PVC
> cloning. This is a two-step approach:
>
> 1) PVC referenced as a data source is now updated with a finalizer
> `provisioner.storage.kubernetes.io/cloning-protection` during a `ProvisionExt`
> method call. The detection of cloning in-progress is based on the assumption
> that a PVC with `spec.DataSource` pointing on a another PVC will go into
> `Pending` state. The downside of this, is that fact that any other reason
> causing PVC to stay in the `Pending` state also blocks resource from deletion"

Removal is handled by a dedicated worker pool
(`cmd/csi-provisioner/csi-provisioner.go`):

```go
finalizerThreads = flag.Uint("cloning-protection-threads", 1,
	"Number of simultaneously running threads, handling cloning finalizer removal")
```

Introduced in external-provisioner v1.6 (`CHANGELOG/CHANGELOG-1.6.md`).

**Detection is a heuristic** — "target PVC is Pending" — and upstream says so. It
over-blocks (any unrelated cause of pending-ness holds the source) rather than
under-blocking.

**After completion, protection is intentionally dropped.** From kubernetes.io,
*CSI Volume Cloning*:

> "Users should be aware [...] the source PVC may also be modified or deleted
> without affecting the newly created clone"

### 4.3 Snapshot a PVC

```
VolumeSnapshot created (source: PVC-A)
  → snapshot-controller adds pvc-as-source-protection to PVC-A
  → VolumeSnapshotContent created
  → csi-snapshotter → CreateSnapshot
  → content.status.readyToUse = true → snapshot.status.readyToUse = true
  → pvc-as-source-protection REMOVED from PVC-A
```

The finalizer is added in `createSnapshotContent`:

> "If PVC is not being deleted and finalizer is not added yet, a finalizer should
> be added to PVC until snapshot is created"

**The removal condition is the important part** (`isPVCBeingUsed`):

```go
if snap.Spec.Source.PersistentVolumeClaimName != nil &&
   pvc.Name == *snap.Spec.Source.PersistentVolumeClaimName &&
   !utils.IsSnapshotReady(snap) {
        klog.V(2).Infof("Keeping PVC %s/%s, it is used by snapshot %s/%s", ...)
        return true
}
```

`!utils.IsSnapshotReady(snap)` bounds the protection to the **creation window
only**. Once the snapshot is ready, the source PVC is freely deletable even
though the snapshot still exists and still names it.

Ordering is enforced deliberately (`removeSnapshotFinalizer`):

> "NOTE(xyang): We have to make sure PVC finalizer is deleted before the
> VolumeSnapshot API object is deleted. Once the VolumeSnapshot API object is
> deleted, there won't be any VolumeSnapshot update event that can trigger the
> PVC finalizer removal any more."

### 4.4 Restore a PVC from a snapshot

```
PVC-B.spec.dataSource = {kind: VolumeSnapshot, name: S}
  → snapshot S carries volumesnapshot-as-source-protection
  → CreateVolume(volume_content_source: snapshot{S's handle})
  → PV-B created, PVC-B Bound
  → finalizer released
```

The source finalizer is added **eagerly and unconditionally**
(`checkandAddSnapshotFinalizers`):

> "NOTE: Source finalizer will be added to snapshot if DeletionTimeStamp is nil
> and it is not set yet. This is because the logic to check whether a PVC is
> being created from the snapshot is expensive so we only go through it when we
> need to remove this finalizer and make sure it is removed when it is not needed
> any more."

So **any snapshot that could be restored from always carries the finalizer**;
the expensive in-use scan runs only at removal time.

---

## 5. Deletion flows: what blocks what

### 5.1 Delete a PVC in use by a Pod

Blocked by `kubernetes.io/pvc-protection` until no Pod uses it.

kubelet also refuses to build state for a terminating PVC that has lost its
finalizer (`pkg/kubelet/volumemanager/populator/desired_state_of_world_populator.go`):

```go
if pvc.ObjectMeta.DeletionTimestamp != nil && !slices.Contains(pvc.Finalizers, util.PVCProtectionFinalizer) {
	return nil, errors.New("PVC is being deleted")
}
```

### 5.2 Delete a PVC being snapshotted

Blocked by `pvc-as-source-protection` **until the snapshot is ready** (§4.3).

### 5.3 Delete a PVC being cloned from

Blocked by `cloning-protection` **while the target PVC is Pending** (§4.2).

### 5.4 Delete a snapshot being restored from

Blocked. From `checkandRemoveSnapshotFinalizersAndCheckandDeleteContent`:

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

"In use" is defined by `isVolumeBeingCreatedFromSnapshot` as: some PVC in the
same namespace has `DataSource.Kind == "VolumeSnapshot"` naming this snapshot
**and** `pvc.Status.Phase == v1.ClaimPending`.

Users see a `SnapshotDeletePending` warning event.

### 5.5 Delete a snapshot that is a group member

Blocked, with an explanatory event:

> "deletion of the individual volume snapshot %s is not allowed as it belongs to
> group snapshot %s. Deleting the group snapshot will trigger the deletion of all
> the individual volume snapshots that are part of the group."

### 5.6 Delete a VolumeSnapshotContent whose VolumeSnapshot is missing

**Deliberately does not cascade** (`syncContent`):

> "NOTE(xyang): Do not trigger content deletion if snapshot is nil. This is to
> avoid data loss if the user copied the yaml files and expect it to work in a
> different setup. In this case snapshot is nil. If we trigger content deletion,
> it will delete physical snapshot resource on the storage system and result in
> data loss!"

### 5.7 Delete a PV bound to a PVC

Blocked by `kubernetes.io/pv-protection`.

---

## 6. The CSI contract

Verbatim from CSI specification v1.11.0, `spec.md`.

### 6.1 `DeleteVolume` and snapshots

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

This is a **MUST**, and it is the only place in the lifecycle where the driver is
*required* to refuse an operation the CO considered legitimate.

### 6.2 Relevant error codes

| RPC | Condition | Code | Caller behaviour (verbatim) |
| --- | --- | --- | --- |
| `DeleteVolume` | Volume in use | `9 FAILED_PRECONDITION` | "could not be deleted because it is in use by another resource **or has snapshots and the plugin doesn't treat them as independent entities**. Caller SHOULD ensure that there are no other resources using the volume and that it has no snapshots, and then retry with exponential back off." |
| `DeleteSnapshot` | Snapshot in use | `9 FAILED_PRECONDITION` | "could not be deleted because it is in use by another resource. Caller SHOULD ensure that there are no other resources using the snapshot, and then retry with exponential back off." |
| `ControllerPublishVolume` | Volume published to another node | `9 FAILED_PRECONDITION` | "Caller SHOULD ensure the specified volume is not published at any other node before retrying with exponential back off." |
| `DeleteVolume` | volume does not exist | `0 OK` | "If a volume corresponding to the specified `volume_id` does not exist [...] the Plugin MUST reply `0 OK`." |

`DeleteVolume` and `DeleteSnapshot` **MUST be idempotent**.

---

## 7. Timing summary

The single most useful table in this document: **how long each protection lasts.**

| Protection | Starts | Ends | Therefore does NOT cover |
| --- | --- | --- | --- |
| `pvc-protection` | Pod references PVC | no Pod references it | anything after the Pod stops |
| `pv-protection` | PV bound | PV unbound | — |
| `pvc-as-source-protection` | snapshot creation begins | **snapshot becomes ready** | a completed snapshot whose source PVC is then deleted |
| `cloning-protection` | clone provisioning begins | target PVC leaves Pending | a completed clone whose source PVC is then deleted |
| `volumesnapshot-as-source-protection` | snapshot exists and is not terminating | no Pending PVC restores from it | — |
| `volumesnapshot-bound-protection` | bound to a Delete-policy content | content removed | — |
| `volumesnapshotcontent-bound-protection` | content bound | snapshot gone | — |

**The two "ends" in bold are where storage backends most often need their own
logic**, because Kubernetes assumes snapshots and clones become independent of
their sources once complete. For backends where that is not true (copy-on-write
snapshots that are children of the live volume, for instance), §6.1's MUST
applies.

---

## 8. What Kubernetes does *not* protect

Verified absences — nothing upstream covers these:

1. **A completed snapshot's source PVC.** §4.3, §7. Deleting it is permitted.
2. **A completed clone's source PVC.** §4.2. Explicitly documented as permitted.
3. **Backend-internal dependencies.** Copy-on-write chains, clone origins,
   reference counts inside the storage system are invisible to Kubernetes.
4. **Custom resources belonging to a driver.** Finalizers exist on PVC/PV/
   VolumeSnapshot/VolumeSnapshotContent. A driver's own CRDs are unmanaged, and
   `kubectl delete <driver-crd>` bypasses every protection listed here.
5. **Leaked finalizers.** See §9.4.

---

## 9. Common misconceptions, corrected

Each of these was believed before verification and proved wrong.

**9.1 "Finalizers are only for cleanup; using one to block deletion is a misuse."**
False. kubernetes.io explicitly documents blocking as an intended use, and names
`kubernetes.io/pv-protection` as the example (§3).

**9.2 "A PVC used as a clone source has no protection; only `pvc-protection`
exists and it only covers Pod mounts."**
False. `provisioner.storage.kubernetes.io/cloning-protection` exists. The error
came from searching only in-tree controllers; the protection lives in the
external-provisioner **sidecar** (§4.2).

**9.3 "The snapshot-controller is optional, so a driver cannot rely on its
protections."**
True but irrelevant. The CRDs and controller install together, and provisioning
from a snapshot requires fetching the `VolumeSnapshot` object (§1). Where the
protection is absent, the operation it protects cannot occur.

**9.4 "Adding a finalizer is a complete solution."**
False. These finalizers leak, and upstream ships a reaper. From
external-provisioner's `README.md`:

> `--snapshot-orphan-sweep-interval <duration>`: How often to check for orphaned
> snapshot source-protection finalizers. These finalizers can become stuck if the
> provisioner crashes during snapshot-based provisioning. Set to `0` to disable.
> Defaults to `5m`.

**Operational consequence:** a `VolumeSnapshot` or PVC stuck in `Terminating` is
more often a leaked upstream finalizer than a driver bug. Check
`metadata.finalizers` first.

**9.5 "There is one snapshot-as-source finalizer."**
False. There are currently two, from different components, with different
prefixes, mid-migration (§2.3).

---

## 10. Version caveats

- The `provisioner.storage.kubernetes.io/volumesnapshot-as-source-protection`
  finalizer and `--snapshot-orphan-sweep-interval` are **recent**. Older
  external-provisioner releases have only the snapshot-controller's copy.
- `cloning-protection` dates from external-provisioner v1.6.
- **[?]** Exact version boundaries were not determined. If behaviour must be
  guaranteed for a specific deployment, read that release's source rather than
  relying on this sheet.
- **[?]** Cross-namespace data sources (`AnyVolumeDataSource`,
  `ReferenceGrant`) were not investigated. The protections above are described
  for same-namespace sources only.

---

## 11. Sources

**Source code read directly:**

| Repo | Files |
| --- | --- |
| `kubernetes/kubernetes` | `pkg/volume/util/finalizer.go`, `pkg/kubelet/volumemanager/populator/desired_state_of_world_populator.go` |
| `kubernetes-csi/external-snapshotter` | `pkg/utils/util.go`, `pkg/common-controller/snapshot_controller.go` |
| `kubernetes-csi/external-provisioner` | `pkg/controller/controller.go`, `pkg/controller/clone_controller.go`, `pkg/controller/snapshot_finalizer_controller.go`, `cmd/csi-provisioner/csi-provisioner.go`, `README.md`, `CHANGELOG/CHANGELOG-1.6.md` |
| `kubernetes-sigs/sig-storage-lib-external-provisioner` | `controller/controller.go`, `CHANGELOG/CHANGELOG-9.0.md` |
| `container-storage-interface/spec` v1.11.0 | `spec.md` |

**Documentation:**

- kubernetes.io — *Finalizers*, *Persistent Volumes*, *Volume Snapshots*, *CSI Volume Cloning*
- kubernetes-csi.github.io — *Volume Cloning*, *Snapshot Restore Feature*, *external-snapshotter*
