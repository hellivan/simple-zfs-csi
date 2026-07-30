# ZFS Snapshot/Clone Lifecycle Redesign — Research & Decision Log

**Status: design complete, implementation not yet started.** This document is a complete
record of the investigation, research, options considered, and final decisions for
reworking how `ZfsSnapshot`/clone/restore lifecycle interacts with `DeleteVolume` and
`DeleteSnapshot`. It exists so the work can resume from scratch even with zero prior
context (e.g. after losing chat history). Once implemented and tested, the final
decisions here should also be distilled into a proper ADR in
[design-decisions.md](design-decisions.md) (one ADR, referencing this doc for the full
rationale) — but *this* document stays as the detailed working record and should be kept
up to date as implementation proceeds (unlike ADRs, this file is meant to be edited).

---

## 1. The problem that started this

`internal/controller/zfsdataset_controller.go`'s `ZfsDatasetReconciler`, on `ZfsDataset`
deletion (i.e. CSI `DeleteVolume`), runs:

```go
if err := r.ZFS.Destroy(ctx, full, true); err != nil { // recursive=true → "zfs destroy -r"
```

`zfs destroy -r pool/ds` destroys `pool/ds` **and every one of its own snapshots** in one
atomic operation. Plain `zfs destroy` (no `-r`) refuses outright if the dataset still has
snapshots ("filesystem has snapshots"), so `-r` is what authorizes cascading through them.

**Consequence:** deleting a PVC also destroys all its ZFS snapshots, even if a
`VolumeSnapshot`/`ZfsSnapshot` object still exists and is marked `readyToUse: true`. This
violates the CSI contract that snapshots must be independent of their source volume
(a snapshot must survive the deletion of the volume it was taken from).

## 2. What the specs and other drivers actually require/do

### 2.1 CSI spec (`github.com/container-storage-interface/spec@v1.11.0/spec.md`, vendored locally)

`DeleteVolume` (lines ~1265-1304):

> CSI plugins SHOULD treat volumes independent from their snapshots.
> If the Controller Plugin supports deleting a volume without affecting its existing
> snapshots, then these snapshots MUST still be fully operational and acceptable as
> sources for new volumes ... once the volume has been deleted.
> When a Controller Plugin does **not** support deleting a volume without affecting its
> existing snapshots, then the volume MUST NOT be altered in any way by the request and
> the operation must return the `FAILED_PRECONDITION` error code.

Error table: `Volume in use | 9 FAILED_PRECONDITION | ... has snapshots and the plugin
doesn't treat them as independent entities ... retry with exponential back off.`

**Key takeaway:** the spec gives two explicitly valid strategies — true independence, or
refuse-and-let-the-CO-retry. Blocking is *not* a workaround, it's a named, spec-sanctioned
option. But we can do better (see below).

`DeleteSnapshot` (lines ~2040-2075): far less explicit than `DeleteVolume` — no "SHOULD
treat independent" framing sentence. `Snapshot in use | 9 FAILED_PRECONDITION` is listed
as just one of several possible error table entries, not a mandated response. A driver
that can make deletion succeed anyway is free to just succeed instead.

Kubernetes itself provides **no built-in protection** for this case at all: unlike PVCs
(which get a `kubernetes.io/pvc-protection` finalizer blocking deletion while a Pod uses
them), there is no equivalent for `VolumeSnapshot`s. `kubectl delete volumesnapshot` is
always accepted by the API server regardless of dependents — this is entirely the CSI
driver's responsibility.

### 2.2 Ceph-CSI's actual behavior (fetched from `/home/ivan/git/democratic-csi` sibling research + live RBD investigation doc supplied by user)

RBD hard rules:
- An image cannot be `rbd rm`'d while it has any **live** snapshot.
- A snapshot cannot be `rbd snap rm`'d while it has clone dependents (unless flattened).

RBD's `snap-trash` feature (`op_features: clone-parent, snap-trash`) lets a snapshot be
"trashed" (hidden, e.g. from `rbd snap ls`/Dashboard) while its clone dependents keep
working — this decouples snapshot lifecycle from source lifecycle. Verified via a live
cluster investigation (user-supplied doc): every ceph-csi "VolumeSnapshot" is actually
implemented as: snapshot the source → **immediately clone that snapshot into a new,
separate image** (`csi-snap-<uuid>`) → trash the original snapshot. The clone image also
immediately gets its **own** self-named snapshot (since `rbd clone` always needs a named
snapshot to clone from) so that a **future restore never needs to touch the original
source again** — it clones from the clone-image's own snapshot.

Fetched `ceph-csi` Go source (`internal/rbd/rbd_util.go`, `controllerserver.go`) confirms:
- `rbdImage.Delete()` = `Trash(0)` + `trashRemoveImage()` (schedules an async
  ceph-mgr background purge task, or falls back to a synchronous `TrashRemove(force=true)`).
  **Trash does not bypass "still has snapshots"** — it only defers the real purge until
  dependents clear. There is no second copy of data; the original object just lingers,
  hidden, until nothing needs it.
- `DeleteSnapshot` calls `rbdSnap.Delete(ctx)`. `rbdSnapshot` **embeds** `rbdImage`, so via
  Go method promotion this calls the *exact same* `Trash()`-based `Delete()` used for
  `DeleteVolume`. **Ceph's `DeleteSnapshot` never blocks on "in use" either** — it's fully
  symmetric with `DeleteVolume`, always succeeds immediately, purge is deferred.

**Correction to an earlier assumption in this conversation:** it was initially unclear
whether Ceph "really" keeps two copies or just defers deletion. Confirmed: **no second
copy** — it's the same data, deletion is deferred until no live dependents remain.

### 2.3 democratic-csi's actual behavior (`/home/ivan/git/democratic-csi`, ZFS-CLI based — most directly comparable to this project)

`src/driver/controller-zfs/index.js`, `DeleteVolume` (~line 1285), right before
`zb.zfs.destroy(datasetName, { recurse: true, force: true })`:

```js
// Explicitly check if we have any managed snapshots
// If a clone has been created from a snapshot, it will fail anyway but if no clones
// have been created the destroy will succeed undesirably
let hasManagedSnapshot = false;
let snapshots = await zb.zfs.list(datasetName, ["name", MANAGED_PROPERTY_NAME], { types: ["snapshot"] });
hasManagedSnapshot = snapshots.indexed.some(s => s[MANAGED_PROPERTY_NAME].toLowerCase() == "true");
if (hasManagedSnapshot) {
  throw new GrpcError(grpc.status.FAILED_PRECONDITION, "filesystem has dependent snapshots");
}
```

This is **exactly the bug we found**, called out in their own comment. Their fix is
Option 1 (block, `FAILED_PRECONDITION`), filtered to snapshots *they* created (a
`democratic-csi:managed_resource` ZFS property), not any incidental ZFS snapshot. They do
**not** implement Ceph-style true independence for the ZFS backend.

### 2.4 The critical ZFS mechanic that makes true independence possible: `zfs promote`

`zfs promote <clone>` reverses the parent/child relationship between a clone and its
origin snapshot:
- The clone stops being a clone: it now **owns** the snapshot history up to (and
  including) the snapshot it was cloned from — that snapshot **physically relocates**
  to live under the clone's own name.
- The former origin dataset loses that snapshot and instead gets a **new** `origin`
  pointer *to the promoted clone*.
- Once a dataset has no more snapshots pinned by external clones, `zfs destroy -r` on it
  always succeeds — the data the clone needs was never touched, it just lives under a
  different name now.

This is structurally identical to what Ceph's snap-trash achieves, via a completely
different, ZFS-native mechanism. No ZFS "trash" feature is needed or exists — `promote`
is the equivalent primitive.

### 2.5 The `zfs send -R` backup-replication constraint (raised by the user; this is *their* production workflow, not a hypothetical)

User's environment: pools are periodically pulled to a second TrueNAS via
`zfs send -R <parent-dataset> | ssh ... zfs receive`, using a chosen parent (e.g.
`pool/k8s/secure`) as a single self-contained backup entry point for everything under it.

From the official `zfs-send` man page (openzfs.github.io, fetched live):

> `-R, --replicate`: Generate a replication stream package, which will replicate the
> specified filesystem, and all descendent filesystems, up to the named snapshot. **When
> received, all properties, snapshots, descendent file systems, and clones are
> preserved.** ... `-p` (send properties) is implicit when `-R` is specified.

Preserving a clone relationship on the receiving side requires the clone's **origin
snapshot to also be part of the replicated set**. A pool-global hidden namespace for
backing clones (e.g. `pool/.zfs-csi-snapshots/<name>`, sitting *outside* any
`datasetPrefix` subtree like `k8s/secure`) would break this: any PVC restored from a
snapshot would be a clone whose origin lives outside the replicated subtree, so
`zfs send -R k8s/secure` would fail to preserve it (or fail outright).

**Fix:** place the backing clone as a **flat sibling inside the same parent/prefix as the
source dataset**, not pool-global, distinguished by a name prefix rather than a nested
folder (revised after further discussion — see §2.8 and D1):

```
source dataset:  pool/k8s/secure/pvc-1
backing clone:   pool/k8s/secure/csi-snap-<name>   (prefix default: "csi-snap-")
```

Computed as `dirname(sourceDataset) + "/" + namePrefix + snapshotName` — purely from
data already on the spec, no cross-component absolute-path config needed, just a shared
name-prefix constant. (This was originally designed as a nested subfolder; revised to a
flat name-prefix scheme after further discussion — see §2.8.)

This means: replicating `k8s/secure` recursively naturally captures the backing clones
and (post-promotion) the relocated origin snapshots, because they physically live inside
that subtree. Restoring a snapshot into the **same** prefix as its source stays fully
self-contained under replication forever, including after the original source is deleted.

**Known limitation (accepted, see D6):** if you restore a snapshot into a *different*
`datasetPrefix` than its source, the new PVC's clone-origin still lives under the
*source's* prefix (not the new PVC's) — not self-contained if only the new prefix gets
replicated. This is an inherent consequence of ZFS clone/origin mechanics, not engineerable
around. **Decision: reject cross-prefix restores outright** rather than let this footgun
exist silently (see D6).

### 2.6 Existing, already-correct behavior confirmed (no changes needed)

- **Cross-pool restore is already rejected** (`resolveContentSource`, ADR-0009):
  `snap.Spec.PoolGUID != rp.PoolGUID` → `InvalidArgument`. ZFS clones/promotes are
  same-pool only; this was already enforced before this investigation started.
- **Cross-type (filesystem vs. zvol) restore is already rejected** (`resolveContentSource`,
  ADR-0009 + our `SourceType` hardening earlier this session):
  `srcType != rp.DatasetType` → `InvalidArgument`. `zfs clone` cannot cross dataset kinds
  at all (physically different on-disk structures) — this is a hard ZFS constraint, not
  a policy choice, and is unrelated to whether the driver is split by protocol.
- **This is not an argument for splitting into separate CSI driver names** (see
  [ADR-0017](design-decisions.md)). Ceph's rbd/cephfs split exists because they're
  genuinely different *storage backends*; our nfs/nvmeof split is one backend (ZFS)
  producing two *dataset kinds*, much closer to how a single CSI driver commonly serves
  both `Filesystem` and `Block` `VolumeCapability`/`volumeMode` (EBS, GCE PD, even Ceph's
  own RBD driver). The type-mismatch rejection is a normal, already-tested
  `CreateVolume` business rule, not evidence of an architecture problem.

---

### 2.7 Property/fsType compatibility on clone and restore (raised by the user, separately from the snapshot-lifecycle work above, but folded into the same redesign)

**The concern:** `zfs clone`/`zfs send`+`receive` copy *content*, not the freedom to
retroactively re-decide structural properties. Three concrete cases, verified against
this codebase and ZFS semantics:

1. **`volblocksize` (zvols) — hard ZFS wall.** A clone's block layout must exactly match
   its origin; `-o volblocksize=X` differing from the source is rejected by ZFS outright.
   `internal/zpool/zfs.go`'s `Clone()` already has a comment acknowledging this
   ("a clone inherits read-only properties such as volblocksize from its origin"), and
   `zfsdataset_controller.go`'s `clone()` path already avoids passing it (`volumeProps()`,
   which adds `volblocksize`, is only used by the plain-create path, never by `clone()`).
   **Gap: this means a target StorageClass's `volblocksize` is silently ignored for a
   cloned/restored volume today — no error, no warning.**
2. **`recordsize` (filesystems) and other mutable `property.*` overrides — soft gotcha,
   not a ZFS error.** `-o recordsize=X` *is* accepted on a clone even if different from
   the origin (recordsize is always mutable), but it only governs **new** writes; blocks
   shared with the origin at clone time keep the **original** record size until
   rewritten. `zfs send`/`receive` has the identical characteristic for the identical
   reason (the stream reproduces blocks exactly as originally written). Not an error, but
   silently not doing what a user restoring into a differently-tuned StorageClass would
   expect.
3. **`fsType` (block/zvol volumes only) — confirmed real bug via the node plugin's own
   code.** `internal/csi/mount.go`'s `FormatAndMount`:
   ```go
   existing, err := m.detectFS(device)   // blkid
   if existing == "" {
       // mkfs.<fsType> only runs if the device has NO filesystem yet
   }
   mount -t <fsType> device target        // always mounts with whatever fsType was requested
   ```
   A cloned/restored zvol is a byte-for-byte copy of the source, filesystem included. If
   the target StorageClass/PV requests a different `fsType` than what the source was
   actually formatted with, `mkfs` is correctly skipped (device isn't empty) but `mount -t
   <requested>` is then attempted against the *actual* on-disk filesystem — this fails at
   `NodeStageVolume` time with a wrong-fs-type/bad-superblock error. Unlike the ZFS
   properties above, **`fsType` isn't tracked anywhere in our CRDs today** — it only
   exists as a per-mount argument at `NodeStageVolume` time, invisible to the controller
   at `CreateVolume`/restore time.

**Is this scenario something a real user could actually trigger, or a contrived edge
case?** Checked against the official Kubernetes docs
(`kubernetes.io/docs/concepts/storage/volume-pvc-datasource/`, fetched live):

> **Cloning is supported with a different Storage Class.** Destination volume can be the
> same or a different storage class as the source. ... Cloning can only be performed
> between two volumes that use the same `VolumeMode` setting (if you request a block mode
> volume, the source MUST also be block mode).

**Confirmed: yes, this is normal, documented, spec-compliant Kubernetes usage, not a
misuse.** The *only* K8s-level restrictions on PVC-to-PVC cloning are: same namespace,
source `Bound` and not in use, same `VolumeMode` (already covered by our existing
`SourceType`/dataset-kind check), and destination capacity ≥ source capacity. **Nothing**
about matching `fsType`, `recordsize`, `volblocksize`, or any other driver-specific
property is checked by Kubernetes at all — consistent with the pattern found everywhere
else in this investigation (the CSI/K8s layer pushes essentially all data-integrity
enforcement onto the driver). This fully justifies treating it as a real gap to close, not
a hypothetical.

**Decision: see D10.**

### 2.8 Backing-clone naming: flat name-prefix, not a nested subfolder (revised from the
initial design)

Initially designed as a nested subfolder (`<prefix>/<leafName>/<name>`, requiring a
separate hidden container dataset per prefix — see the original D2). Revised, for two
reasons raised by the user:

1. **This is actually how Rook/ceph-csi does it, but as a structural necessity, not a
   preference** — RBD images (`csi-snap-<uuid>`, `csi-vol-<uuid>`) live in a **flat**
   pool namespace; RBD has no folder/hierarchy concept at all, so ceph-csi's flat,
   prefixed naming isn't a design choice, it's the only option RBD offers. ZFS is
   hierarchical, so nesting was an *option* for us, not a requirement — but there's no
   strong reason to use it if a flat prefix works just as well.
2. **A flat prefix already matches this project's own existing convention** for
   internal/bookkeeping ZFS objects: ADR-0009's direct-volume-clone path already does
   `srcFull + "@clone-" + vol.Name` — a flat, prefixed snapshot name, no subfolder.
   Matching that convention is more consistent than introducing a new nested-folder
   pattern.

**Revised scheme:** `dirname(sourceDataset)/<namePrefix><snapshotName>`, e.g.
`pool/k8s/secure/csi-snap-<name>` — flat, no separate container dataset needed at all
(the original D2 "auto-create hidden container" step is eliminated entirely, since
there's no container anymore, just a sibling dataset under the already-existing prefix).
Collision risk against real PVC dataset names is negligible in practice: dynamically
provisioned PV names are always Kubernetes-generated and start with `pvc-`, so a
`csi-snap-` prefix cannot realistically collide — the same risk profile already accepted
for `@clone-<name>`.

**Clarifying "hidden":** ZFS has no feature to make a dataset invisible from `zfs list`
(unlike RBD's `snap-trash`, which specifically hides a snapshot from `rbd snap ls`) —
every backing clone will always show up in `zfs list`/`zfs list -t all` regardless of
naming scheme. What "hidden" actually refers to is `canmount=off` (filesystem) /
`volmode=none` (zvol) — properties that suppress auto-mounting / block-device exposure,
an orthogonal, purely operational concern kept regardless of the naming scheme.

## 3. Chosen design

Two selectable **modes**, per `VolumeSnapshotClass` (see D8), because they have genuinely
different trade-offs and the user's own backup-replication use case benefits from being
able to choose:

### 3.1 `standalone` mode (Ceph-style, via `zfs promote`) — **new default**

**On `CreateSnapshot`** (source `pool/k8s/secure/pvc-src`, CSI name `snap-1`):
1. `zfs snapshot pool/k8s/secure/pvc-src@snap-1` — raw point-in-time snapshot (unchanged
   from today).
2. `zfs clone pool/k8s/secure/pvc-src@snap-1 pool/k8s/secure/csi-snap-snap-1` — the
   "backing clone", a flat sibling of the source directly under the same prefix (no
   separate container dataset needed, see §2.8). Matches Ceph's `csi-snap-<uuid>`
   clone-image equivalent, just ZFS-flavored. Use `canmount=off` (filesystem) or
   `volmode=none` (zvol) so it never auto-mounts/exposes a device — pick based on
   `ZfsSnapshotSpec.SourceType` (already added to the CRD earlier this session).
3. `zfs snapshot pool/k8s/secure/csi-snap-snap-1@restore-source` — fixed-name
   self-snapshot on the backing clone, created immediately (mirrors Ceph pre-creating the
   clone-image's own snapshot). **Fixed suffix name, distinct from the CSI-visible
   snapshot name**, so it never collides with the relocated raw-origin snapshot after
   promotion (see step below — the raw origin keeps its own CSI-visible name,
   `@snap-1`, and after promotion both `@snap-1` and `@restore-source` coexist on the same
   backing-clone dataset without collision).
4. `CreationTime`/`RestoreSize` status fields are read from
   `csi-snap-snap-1@restore-source` (not the raw source snapshot) — stable regardless of
   what happens to the source later.

**On `DeleteVolume`** (deleting `pvc-src`), in `ZfsDatasetReconciler`, before
`zfs destroy -r`:
1. List live `ZfsSnapshot`s with `Spec.SourceVolume == pvc-src`.
2. **Block** (return error, requeue) if any of them are not yet `Ready` (see D3 — avoids
   destroying the source out from under an in-flight `CreateSnapshot`).
3. For each `Ready` one, `zfs promote pool/k8s/secure/csi-snap-<name>` (idempotent: skip
   if no `origin` property — i.e. already promoted, or never was a clone).
4. **Also** (D7/D9): list live `ZfsDataset`s with `Spec.Source.Volume == pvc-src` (direct
   PVC-to-PVC clones, not via any `VolumeSnapshot`) and unconditionally `zfs promote` each
   of *those* clone datasets too, detaching the intermediate `pvc-src@clone-<name>`
   snapshot from ADR-0009's volume-clone path. No mode toggle here — always-on, since
   there's no `VolumeSnapshotClass` involved in a direct volume clone to attach a toggle
   to, and blocking here would be confusing (no visible object the user is managing).
5. Only after all of the above, `zfs destroy -r pool/k8s/secure/pvc-src` — now guaranteed
   zero remaining snapshot/clone dependents, **always succeeds**.

**Restores** (`resolveContentSource`) always clone from
`csi-snap-<name>@restore-source`, never from the original source path directly — stable
whether the source is alive, deleted-but-not-yet-promoted, or promoted away.

**`DeleteSnapshot`, final design — finalizer-based tracking + reverse-promote** (see D4
evolution below): always succeeds, never blocks.
1. When `CreateVolume` restores a PVC from `snap-1`, it (synchronously, as part of the
   restore, fetch-check-patch with resourceVersion-conflict retry) adds a finalizer to
   `snap-1` like `storage.simple-zfs-csi.io/restored-by.<newPvcName>`. If `snap-1` already
   has a `deletionTimestamp` at that point, the restore is rejected (snapshot is going
   away).
2. When that restored PVC is later deleted, its own `ZfsDatasetReconciler` teardown path
   removes that same finalizer from `snap-1` as part of its own cleanup (in addition to
   its own `zfsDatasetFinalizer`).
3. `snap-1`'s own delete path (`ZfsSnapshotReconciler`): reads its **own** finalizer list
   directly (no cluster-wide `List` needed — an efficiency win over an earlier considered
   design) to know exactly which datasets still depend on it, `zfs promote`s every one of
   them (relocating `@restore-source`, and `@snap-1` if not already promoted, onto each
   dependent — each dependent ends up owning its own copy of the shared history), then
   destroys the backing clone (now dependent-free) and removes its own `zfssnapshot`
   finalizer. **Always succeeds**, matching Ceph's real (confirmed via source) behavior.
4. Best-effort cleanup of the raw origin snapshot on the source dataset too, if the source
   still exists and it wasn't already relocated by an earlier promote.

### 3.2 `integrated` mode (today's model, hardened only with blocking)

- `CreateSnapshot`: plain `zfs snapshot` only — no backing clone, no hidden folder, no
  extra ZFS objects. Exactly today's behavior.
- `DeleteVolume`: **blocks** (`FAILED_PRECONDITION`-style error/requeue) while any
  `integrated`-mode dependent `ZfsSnapshot` still exists — there's nothing to promote in
  this mode, so Option 1 (the democratic-csi-style fix) applies as-is.
- `DeleteSnapshot`: plain `zfs destroy` on the raw snapshot; ZFS's own "has dependent
  clones" protection naturally blocks/retries if something was restored from it.
- Cheaper (no extra clone dataset per snapshot), simpler `zfs list` output, but the
  trade-off from the original bug report returns: you must delete all snapshots before
  deleting the source PVC.

### 3.3 Selecting the mode

`CreateSnapshot` currently ignores `req.GetParameters()` entirely. New parameter:
`mode: standalone|integrated`, resolved with the same values.yaml-default →
`VolumeSnapshotClass`-parameter-override inheritance pattern already used for
StorageClass parameters. Stored once, immutably, on new `ZfsSnapshotSpec.Mode` field
(same pattern as the existing immutable `SourceType` field). Chart value:
`csiController.snapshotter.defaultMode`, **default `standalone`** (no existing snapshots
in the user's cluster yet, so no migration concern — go straight to the better default).

---

## 4. Full decision log

| # | Topic | Final decision |
|---|---|---|
| D0 | Core mechanism | `zfs promote` to relocate a snapshot onto a pre-created backing clone — ZFS-native equivalent of Ceph's snap-trash |
| D1 | Backing clone naming/location | **Revised (§2.8):** flat sibling `dirname(sourceDataset)/<namePrefix><snapName>` (e.g. `pool/k8s/secure/csi-snap-<name>`) — **not** pool-global (required for `zfs send -R` backup compatibility, §2.5), and **not** a nested subfolder (matches Rook/ceph-csi's own naming convention and this project's existing `@clone-<name>` convention, §2.8) |
| D1a | Name prefix | Configurable via values.yaml, default **`csi-snap-`** |
| D2 | ~~Hidden container creation~~ | **Removed — no longer applicable.** Only existed for the original nested-subfolder design; the flat-prefix revision (§2.8, D1) needs no separate container dataset at all, just a sibling under the already-existing prefix. |
| D3 | In-flight-snapshot vs. concurrent `DeleteVolume` race | `ZfsDatasetReconciler`'s delete path blocks (error+requeue) on any dependent `ZfsSnapshot` not yet `Ready` (`Pending` or `Error` phase) — bounded wait, not a deadlock (verified: both reconcilers run on the same node/manager per pool, underlying ZFS object isn't destroyed yet so the in-flight snapshot can still complete) |
| D4 | `DeleteSnapshot` "in use" detection + policy | **Final: finalizer-based dependency tracking** (`restored-by.<pvcName>` finalizers added at restore time, removed at the dependent's own teardown) **+ reverse-`zfs promote`** of every tracked dependent before destroying the backing clone. Always succeeds, matches Ceph's actual (source-code-confirmed) behavior. Superseded two earlier, less-good candidates: (a) discover "in use" only via the async ZFS destroy error inside the finalizer loop (bad UX, no immediate/standard error signal); (b) synchronous check via `ZfsDataset.Spec.Source.Snapshot` + block with `FAILED_PRECONDITION` (better UX than (a), race-prone via TOCTOU, and doesn't match Ceph's real always-succeeds behavior) |
| D5 | Self-snapshot suffix name | Fixed `@restore-source`, distinct from the CSI-visible snapshot name (avoids collision with the relocated raw-origin snapshot after promotion) |
| D6 | Cross-prefix restore (different `datasetPrefix` than the source) | **Reject** (`InvalidArgument`) in the controller, not just documented — applies identically to both modes (the backup-locality problem exists in `integrated` mode too, via the raw snapshot's own location) |
| D7 | Direct PVC-to-PVC clone (`VolumeContentSource_Volume`, no `VolumeSnapshot` involved) — must the *source* PVC's deletion be blocked while a clone exists? | **No — always promote instead, unconditionally** (no mode toggle possible/needed, since there's no `VolumeSnapshotClass` in this path). Same mechanism as D3, applied to ADR-0009's intermediate `<src>@clone-<name>` snapshot. Re-confirmed explicitly as its own question later in the conversation — same answer. |
| D8 | Dual-mode selectability | `VolumeSnapshotClass` parameter `mode: standalone\|integrated`; values.yaml default `csiController.snapshotter.defaultMode: standalone` (chosen over `integrated` because there are no existing snapshots yet in the target cluster — no migration risk) |
| D9 | (alias of D7 — the user re-asked this question in different words; recorded here to make clear it was re-verified, not newly introduced) | Same as D7 |
| D10 | Property/fsType compatibility on clone and restore (§2.7) | **Reject on any mismatch, for both `resolveContentSource` code paths (restore-from-snapshot and clone-from-volume), both modes.** Specifically: (a) `volblocksize` — reject if the target's resolved value differs from the source's actual value (today it's silently ignored instead); (b) any `property.*` override (`recordsize`, `compression`, etc.) — reject if the target's resolved value differs from the source's recorded `Spec.Properties`/`Spec.Volume.Volblocksize`, rather than allowing a technically-valid-but-confusing partial override; (c) `fsType` (block/zvol only) — reject if the target's resolved `fsType` differs from the source's actual formatted filesystem. Chosen over allowing selective, per-property overrides: simpler, matches "clone copies content, not the freedom to redecide structure," and closes a real, confirmed, silently-broken case (Kubernetes explicitly permits cross-StorageClass cloning/restore with no compatibility checking of its own, per §2.7). Comparison for (a)/(b) is Spec-to-Spec (no live ZFS query needed, consistent with the D4 preference for K8s-native checks over ZFS round-trips); (c) requires new state — see task list. |

## 5. Rejected alternatives (and why)

- **Just implement Option 1 everywhere (block `DeleteVolume` via `FAILED_PRECONDITION`,
  democratic-csi style) and stop there.** Rejected as the *final* answer (though it's
  still used for `integrated` mode) because the user's real requirement is "deleting a
  volume must always be possible, snapshots must survive" — Option 1 doesn't achieve that,
  it just fails safely instead of corrupting data.
- **Ceph-style "trash" reimplemented literally in ZFS.** ZFS has no trash/hidden-snapshot
  feature; `zfs promote` achieves the same *end state* (source deletable, dependent data
  intact) via a completely different mechanism (relocation instead of hiding). No trash
  subsystem needed.
- **Pool-global hidden namespace for backing clones** (`pool/.zfs-csi-snapshots/<name>`).
  Rejected: breaks `zfs send -R <prefix>` backup/replication for any restored PVC, since
  the clone's origin would sit outside the replicated subtree (§2.5). Replaced by placing
  the backing clone under the source's own prefix (D1).
- **Nested subfolder for the backing clone** (`<prefix>/<leafName>/<name>`, with a
  separate hidden container dataset). Rejected in favor of a flat name-prefix
  (`<prefix>/csi-snap-<name>`, D1/§2.8): matches how Rook/ceph-csi actually names things
  (a structural necessity for RBD, adopted here for consistency) and this project's own
  existing `@clone-<name>` convention; also eliminates the need for a separate
  container-dataset creation step entirely.
- **Synchronous `List(ZfsDataset)` + block for D4.** Rejected in favor of finalizer-based
  tracking + reverse-promote: the `List`-based check has a TOCTOU race (a concurrent
  restore could land between the check and the delete), requires a cluster-wide scan on
  every `DeleteSnapshot`, and — critically — still just *blocks* rather than achieving
  Ceph-parity always-succeeds behavior.
- **`ownerReferences` + `blockOwnerDeletion` (Kubernetes' built-in cascade-delete
  protection) for tracking snapshot-in-use-by-PVC.** Rejected: that mechanism models
  parent/child cascade-delete semantics (deleting the owner can cascade-delete or be
  blocked by children), which is backwards from what we want — a restored PVC must stay
  fully independent forever, never subject to cascade deletion if its source snapshot is
  later removed. Plain custom finalizers (which this codebase already uses extensively)
  are the correct tool, not owner references.
- **Splitting into separate CSI driver names for zvol vs. filesystem** (raised twice: once
  generally, once specifically re: cross-type restore rejection). Rejected both times —
  see ADR-0017 and §2.6 above. Not reopened by anything found in this investigation.

## 6. Implementation task list (order of work)

1. `internal/zpool`: add `ZFS.Promote(ctx, dataset) error` (+ CLI impl `zfs promote`, +
   fake/test double). Idempotent: no-op if the dataset has no `origin` property.
2. `api/v1alpha1/zfssnapshot_types.go`: add `Spec.Mode` (enum `Standalone`/`Integrated`,
   immutable, default resolved at creation — same pattern as the existing `SourceType`
   field). Regenerate CRDs/deepcopy (`make manifests`).
3. `internal/controller/zfssnapshot_controller.go`:
   - Create path: branch on `Mode`. `standalone` → raw snapshot + flat-sibling clone
     (`csi-snap-<name>`, no separate container dataset needed) + `@restore-source`
     self-snapshot (with `canmount=off`/`volmode=none` per `SourceType`). `integrated` →
     unchanged (raw snapshot only).
   - Delete path: branch on `Mode`. `standalone` → read own finalizer list, `zfs promote`
     each tracked dependent, destroy backing clone, best-effort cleanup of the raw origin
     snapshot on the source if still present, release own finalizer. `integrated` →
     unchanged (destroy raw snapshot; rely on ZFS's natural "has dependent clones"
     refusal + reconcile retry if something depends on it).
4. `internal/controller/zfsdataset_controller.go` (`ZfsDatasetReconciler` delete path):
   - D3: list dependent `ZfsSnapshot`s (`Spec.SourceVolume == this`); block/requeue if any
     are not yet `Ready`.
   - For `Ready`, `standalone`-mode dependents: `zfs promote` their backing clone.
   - For `Ready`, `integrated`-mode dependents: block/requeue (`FAILED_PRECONDITION`-style)
     until they're gone.
   - D7/D9: list dependent `ZfsDataset`s (`Spec.Source.Volume == this`, direct clones);
     always `zfs promote` each one, unconditionally.
   - Only then proceed to `zfs destroy -r`.
5. `internal/csi/clone.go` (`resolveContentSource`):
   - Restore-from-snapshot: clone from `csi-snap-<name>@restore-source` in `standalone`
     mode, from `snap.Spec.Dataset + "@" + snap.Spec.SnapshotName` in `integrated` mode
     (branch on `snap.Spec.Mode`).
   - D6: reject cross-prefix restores (`dirname(targetDataset) != dirname(snap.Spec.Dataset)`)
     with `InvalidArgument`, for both modes.
6. `internal/csi/snapshot.go` (`CreateSnapshot`):
   - Read/resolve the new `mode` parameter (values.yaml default → `VolumeSnapshotClass`
     parameter override), validate it's one of the two allowed values, store on
     `ZfsSnapshotSpec.Mode`.
   - Restore-side finalizer add (D4): when `CreateVolume` resolves a `standalone`-mode
     snapshot content source, patch the `ZfsSnapshot` to add
     `storage.simple-zfs-csi.io/restored-by.<newPvcName>` (fetch-check-`deletionTimestamp`
     -patch-with-conflict-retry). Reject the restore if the snapshot is already
     terminating.
   - `ZfsDatasetReconciler`'s own delete path removes that same finalizer from the source
     `ZfsSnapshot` (if any) as part of its teardown, regardless of clone/restore kind.
7. Chart: `charts/simple-zfs-csi/values.yaml` — add
   `csiController.snapshotter.defaultMode: standalone` and a name-prefix value
   (default `csi-snap-`). Thread both into `cmd/csi-controller` (parameter
   resolution + restore path) and `cmd/zpool-discovery` (agent — creates the backing
   clone) via chart-templated flags/args so they can never drift out of sync.
8. Tests (both `internal/controller/*_test.go` and `internal/csi/*_test.go`):
   - `standalone` create → clone + self-snapshot with correct properties per `SourceType`.
   - `DeleteVolume` promotes all `Ready` `standalone` dependents and always succeeds.
   - `DeleteVolume` blocks on a non-`Ready` dependent snapshot (D3) and on any
     `integrated`-mode dependent.
   - `DeleteVolume` promotes direct-clone dependents (D7/D9) unconditionally.
   - Restore clones from `@restore-source` (standalone) / raw snapshot (integrated).
   - Cross-prefix restore rejected (D6), both modes.
   - `DeleteSnapshot` (standalone): finalizer added on restore, removed on dependent
     teardown, reverse-promote + destroy always succeeds even with live dependents.
   - `DeleteSnapshot` (integrated): unaffected, still relies on ZFS's natural protection.
9. `api/v1alpha1/zfsdataset_types.go`: add `Status.FSType` (string, set once). Node plugin's
   `FormatAndMount` (`internal/csi/mount.go`) needs to report the fsType it actually used
   the first time it formats an empty device (`existing == ""` branch) back up so the
   `ZfsDatasetReconciler`/node code path can persist it on the `ZfsDataset` status. An
   empty `Status.FSType` (never formatted yet) means nothing to conflict with — restores
   from/clones of a never-mounted volume remain unconstrained until first format.
10. `internal/csi/clone.go` (`resolveContentSource`), D10: before accepting a
    restore/clone, compare the target's resolved `volblocksize` and `property.*`
    overrides against the source `ZfsDataset.Spec.Properties`/`Spec.Volume.Volblocksize`
    (Spec-to-Spec, no ZFS call), and the target's resolved `fsType` (if block/zvol)
    against the source's `Status.FSType` (if set) — reject any mismatch with
    `InvalidArgument`.
11. Once implemented and passing: add a single ADR to `design-decisions.md` distilling
   this document's final decisions (per the repo's one-decision-per-ADR, append-only
   convention) — this document stays as the detailed backing record, referenced from
   the ADR.

## 7. Future work (deliberately out of scope for this redesign)

- **Prefix regular volume dataset names too, for naming consistency** (e.g.
  `csi-vol-<name>`), mirroring the new `csi-snap-` prefix on backing clones and
  Ceph-csi's `csi-vol-`/`csi-snap-` convention. Raised by the user, explicitly deferred
  as future work, not part of this redesign. Rationale for treating it separately:
  - Ceph's `csi-vol-`/`csi-snap-` prefixes are type-tags on top of a self-generated
    opaque UUID (Ceph never reuses the PVC's own name as the RBD image name — it
    maintains a separate volume-journal mapping). This project deliberately does the
    opposite: the ZFS dataset name **is** the CSI volume name directly
    (`rp.Dataset(name)`), no separate ID journal, which already gets Kubernetes'
    `pvc-<uuid>` naming for free as a de facto unique/opaque identifier.
  - `csi-snap-` was introduced to solve a **one-sided** disambiguation problem (backing
    clones vs. real PVC datasets sharing the same flat prefix namespace) — marking one
    side is sufficient; marking volumes too would be redundant, not additive.
  - Much larger blast radius than the snapshot work: it would touch every PVC dataset
    name, present and future, across the whole driver, not just the new, isolated
    snapshot/clone mechanism this redesign covers.
  - Desired purely for aesthetic/naming-consistency reasons ("I like to have things
    clean"), not for a functional gap — worth doing later as its own, separately-scoped
    change (its own ADR/plan), not bundled into this one.

## 8. Open items / not yet decided

None outstanding as of writing — every decision point raised (D0-D10) has a final answer
recorded above. If new questions come up during implementation, add them here as `D11`,
`D12`, etc., following the same table format, before resolving them.
