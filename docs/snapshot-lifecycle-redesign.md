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

### 2.9 Multiple simultaneous restores from one snapshot — chained-dependency problem (found while re-verifying "does this really fit the pattern")

**The scenario:** CSI/Kubernetes explicitly allow creating any number of PVCs from the
*same* `VolumeSnapshot` (`CreateVolume` with a snapshot content source has no
"already used once" restriction — this is standard, expected usage, e.g. spinning up
several dev copies from one snapshot). Our design must keep every one of them, and the
snapshot itself, fully and independently deletable in **any order**, per the user's
explicit requirement: *"whenever I delete something it must be possible to delete it and
not influence others."*

**Why the simple "promote every finalizer-tracked dependent" loop (§3.1 `DeleteSnapshot`
step 3) is not sufficient once there are 2+ simultaneous dependents:**

`zfs promote` physically relocates the shared snapshot history onto **one** dataset —
there is only ever one physical copy. When a snapshot has multiple sibling clones (e.g.
`pvc-restored-1` and `pvc-restored-2`, both cloned from `csi-snap-snap-1@restore-source`),
promoting `pvc-restored-1` does not make `pvc-restored-2` independent too — real ZFS
**reparents** `pvc-restored-2`'s `origin` to point at `pvc-restored-1@restore-source`
instead. **This is intentional, documented ZFS design, not a bug or a quirk we're relying
on**: verified directly in the OpenZFS source
([`dsl_dataset.c`, `dsl_dataset_promote_sync`](https://github.com/openzfs/zfs/blob/master/module/zfs/dsl_dataset.c)) —
there is an explicit block, commented `"move any clone references"`, that walks the
promoted snapshot's `ds_next_clones_obj` list and deliberately reassigns every *other*
sibling clone to point at the newly-promoted dataset. ZFS is designed to keep sibling
clones correctly attached when one of them is promoted; D12 relies on this intentional,
maintained mechanic — the same way it relies on `zfs promote` doing its documented job at
all.

**A second, separate, and genuinely confirmed real defect** — *not* relied upon, actively
defended against (D13): promoting a dataset that is itself part of a clone-of-clone chain
can require **multiple** `zfs promote` calls before `origin` actually becomes empty — the
first call can leave the dataset still reporting a (different) non-empty `origin`. This
is a distinct, narrower issue from the sibling-reparenting mechanic above, confirmed via a
live reproduction filed upstream
([openzfs/zfs#15587](https://github.com/openzfs/zfs/issues/15587), OpenZFS 2.1.5,
labeled "Defect"). A real downstream project (`bemgr`, a ZFS boot-environment manager)
worked around this by looping `zfs promote` (capped at 100 attempts) until `origin` is
confirmed empty, precisely because a single call isn't reliably sufficient. **We do not
depend on this defect for correctness — we defend against it**: D13 makes `Promote`
verify `origin` actually cleared and retry (bounded) if not, and hard-errors (fail loud,
`ZfsDataset`/`ZfsSnapshot` stays `Terminating`) rather than silently trusting a single
call. If a future OpenZFS release fixes this outright, the retry loop simply becomes a
harmless single-pass no-op every time — zero downside either way.

**Consequence for this design:** naively calling `zfs promote` once per finalizer-tracked
dependent (as originally worded in §3.1 step 3) can leave dependents silently
cross-referencing **each other** (an untracked `ZfsDataset`-to-`ZfsDataset` clone
relationship our reconciler doesn't know about) instead of the intended
fully-independent end state — which would later make one of those dependents'
own, ordinary `DeleteVolume` fail unexpectedly (D11's plain `zfs destroy` refuses if it
turns out to unknowingly have a dependent clone), violating the exact "deleting X must
never be influenced by/block on Y" requirement this whole redesign exists to satisfy.

**Rejected fix: always fully duplicate ("flatten") every dependent's data instead of
promoting.** Would guarantee true mutual independence unconditionally, but requires a
`zfs send | zfs receive` round-trip per extra dependent (real storage/time cost, not
just metadata) **and** — because the dataset being replaced may currently be an actively
mounted PVC (NFS-exported or NVMe-oF-attached to a running Pod) — would require briefly
quiescing/detaching and re-attaching the live workload to swap the dataset in place.
Rejected as disproportionate for what is a comparatively rare case (multiple simultaneous
restores from one snapshot, followed by deleting that snapshot while more than one of
those restores is still alive). See D12 for the accepted, metadata-only alternative.

**Decision: see D12 (generalized dependent-tracking, avoids needing flatten) and D13
(defensive, verify-and-retry `Promote` primitive, closing the confirmed upstream
reliability gap).**

### 2.10 Is `ZfsSnapshot` still the right CRD name, given `standalone` mode's implementation? (raised by the user; see D14)

Raised because, in `standalone` mode, the object tracked by a `ZfsSnapshot` isn't "just"
a plain ZFS snapshot under the hood — it's a raw snapshot plus a backing clone plus that
clone's own self-snapshot (§3.1). Does the CRD name/kind need to change, or split into a
"typed" CRD (the user's own suggestion, drawing a direct parallel to `ZfsDataset`)?

**Resolution (D14): no split needed — this is already the `ZfsDataset` pattern, just
using different terminology.** `ZfsDataset` already separates two independent concerns
via **fields**, not CRD kinds:
- `DatasetType` (`filesystem`/`volume`) — a *structural* distinction (what kind of ZFS
  object this fundamentally is).
- `DatasetSource` (`Spec.Source.Snapshot`/`Spec.Source.Volume`) — a *provenance/mechanism*
  distinction (plain-create vs. clone vs. restore). A cloned `ZfsDataset` is still a
  `ZfsDataset`; it isn't renamed `ZfsClone`.

`ZfsSnapshot`'s planned `Mode` field (`standalone`/`integrated`, D8) is the exact same
category as `DatasetSource`: it describes *mechanism*, not the CSI/K8s-facing *contract*.
At the CSI/`VolumeSnapshot` contract level, both modes mean exactly the same thing (a
point-in-time, read-only, restorable capture of a volume) — which is what "snapshot"
means at that layer, independent of ZFS-level implementation. Ceph-CSI sets the same
precedent from the other direction: its own "snapshot" is *also* actually a clone-image
plus a self-snapshot under the hood (§2.2), and Ceph doesn't rename their concept over
it — "snapshot" is a contract-level name, not an implementation-detail name.

Splitting into a second CRD kind was considered and rejected: it would require CSI-facing
code (`ListSnapshots`, `resolveContentSource`, etc.) to branch on CRD *kind* instead of a
field, complicate listing (two kinds to `List` and merge instead of one), and add RBAC/
controller-duplication overhead — all for a distinction (`Mode`) that a single field
already expresses cleanly, matching the existing `ZfsDataset` convention. Also: renaming
the CRD kind itself (as opposed to adding a field) would be a large, mostly-cosmetic
refactor touching every existing reference across controllers/CSI/charts/CRDs, for no
functional gain — the same category of cost as the naming-consistency item already
deferred as future work in
[independent-resource-naming-redesign.md](independent-resource-naming-redesign.md)
(§7 there).

**Decision: keep `ZfsSnapshot` as the CRD name/kind; add `Mode` as a field (D8), not a
new kind.**

### 2.11 Does promote iteration order matter when a source has multiple, independent snapshots? (raised by the user; see D16 — corrected after further review)

**Scenario:** `vol1` has six `standalone`-mode snapshots taken over time (`snap_t1`
oldest ... `snap_t6` newest), each with its own backing clone (`csi-snap-t1`...
`csi-snap-t6`, per-snapshot, D0/D15). On `DeleteVolume`, D3's loop calls `zfs promote`
on all six, unconditionally, in whatever order `List()` happens to return them (not
necessarily chronological). **Does promoting them out of order cause them to "steal"
each other's snapshots, the way D12's same-snapshot-multiple-clones case does?**

**Corrected answer: yes, more aggressively than first analyzed — this needed a second
pass, caught by the user re-checking the trace by hand.** The first version of this
section under-traced the drag-along: it assumed promoting `csi-snap-t6` would only pull
along snapshots *still physically on* `vol1` (`@snap_t4`/`@snap_t5`), stopping at
whatever `csi-snap-t3` had already claimed. That's wrong. Re-reading `snaplist_make`
in `dsl_dataset.c` precisely:

```c
error = snaplist_make(dp, 0, dsl_dir_phys(dd)->dd_origin_obj, &ddpa->shared_snaps, tag);
```

This walks backward via each snapshot's own `ds_prev_snap_obj` — an intrinsic property of
the snapshot *object*, set once, that does **not** care which dataset currently
dir-lists it — all the way back to the very first snapshot ever taken (object `0`).
Directory ownership (`ds_dir_obj`) is a separate field that changes when a snapshot is
relocated, but the chain link between snapshot objects survives the relocation. So
promoting `csi-snap-t6` (*after* `csi-snap-t3` already claimed `t1`/`t2`/`t3`) walks
`t6 → t5 → t4 → t3 (now under csi-snap-t3) → t2 (ditto) → t1 (ditto) → stop` — **all
six**, not just the two still on `vol1`. Worse: this isn't bounded to snapshots claimed
by *other* clones — even in a clean, chronological (oldest-first) order, promoting
`csi-snap-t2` right after `csi-snap-t1` has already been fully promoted would still walk
back through `t1`'s object, since that walk has no "stop, this now belongs to an
independent dataset" condition visible in the fragments read so far.

**Where this remains genuinely uncertain (stated plainly, not glossed over):** whether
ZFS actually permits relocating a snapshot out from under a dataset that structurally
depends on it as *its own* most-recent snapshot (`ds_prev_snap_obj` normally should match
`ds_dir_obj`) is not confirmed from the source fragments read here. This needs either a
full read of the real implementation (not fragments pulled via web fetch) or hands-on
testing against a real ZFS pool before being trusted — **not** asserted with the
confidence the first draft of this section wrongly had.

**Why this is fundamentally different from D12:** D12's problem is physical — multiple
simultaneous clones of the *exact same* snapshot can't all end up independently owning
it, since there's only one copy of that shared history. Here, every snapshot is a
distinct ZFS object with exactly one clone each — there's no *inherent* physical
conflict, just (at minimum) far more aggressive transient drag-along than first modeled,
and (at most, unconfirmed) a possible "steal-back" of an already-independent clone's
snapshot by a later, unrelated promote in the same batch.

**Mitigation, chosen to be correct regardless of which way the open question resolves
(D16, revised):** the D3 promote loop must not be a single pass over the tracked
dependents. It must be a **bounded fixpoint loop**: repeat the full pass (promote every
still-non-independent tracked dependent) until no dependent's `origin` flips from empty
back to non-empty anymore, capped at a bounded number of rounds (same spirit as D13's
per-call retry cap, just applied at the list level). If "steal-back" turns out to be
impossible, this degrades to exactly one pass with zero extra cost. If it is possible,
this detects and corrects it instead of silently leaving a backing clone incorrectly
re-attached. Only once the fixpoint is reached (every tracked dependent's `origin` is
simultaneously confirmed empty) does `vol1`'s own destroy proceed.

**Decision (D16, revised): no permanent design change beyond strengthening the promote
loop into a fixpoint** — order-independence for the *final* state is still very likely
true (every backing clone's own promote call is still individually correct once issued,
per D13), but getting there safely requires re-verifying the whole batch, not trusting a
single pass. **Flagged as needing hands-on ZFS verification before implementation** (§8)
— this is not a documentation-only question, and should not be treated as fully closed
until confirmed against a real pool.

> **Errata (2026-07-31): empirically verified on a live pool — see full test log in
> [promote-order-verification-2026-07-31.md](promote-order-verification-2026-07-31.md).**
> The hands-on test requested above was run (real ZFS pool `spinning-archive`, isolated
> scratch dataset, exact scrambled order t3, t1, t6, t2, t4, t5). Result: **the original
> concern above (possible "steal-back" of an already-independent, previously-promoted
> clone) was NOT observed** — `csi-snap-t1` stayed at `origin = -` for the rest of the
> batch, and `csi-snap-t6`'s promote stopped exactly at `csi-snap-t3`'s boundary instead
> of reaching through it. Every backing clone ended up owning exactly its own snapshot;
> `vol1` ended with zero snapshots of its own. **D16 is confirmed correct as originally
> (first) analyzed — no fixpoint loop is required after all**; the more cautious
> re-analysis above, based on source-reading alone without a live test, turned out to be
> overly pessimistic. The bounded-fixpoint mitigation described above is **not required**
> but is harmless to keep as defense-in-depth if preferred — see the test log's §4.1 for
> the full reasoning. **The same test also found and fixed a real, separate bug in D13's
> verification check** (a cleanly promoted dataset does not necessarily end up with an
> empty `origin` — see the test log's §4.2 and D13's table row, corrected accordingly).
> This errata supersedes the "flagged as needing verification" status above for D16, but
> the original analysis is left in place, unmodified, per this document's append-only
> correction convention.


### 2.12 Re-examined from scratch: is Ceph's trash approach actually more robust/reliable than `zfs promote`? (raised by the user; re-affirms D0)

The user explicitly asked to re-open **every** design decision here and seriously
consider discarding `zfs promote` (D0) entirely in favor of a literal Ceph-style trash
mechanism, if trash turns out simpler/more robust/more reliable — while independently
already suspecting trash leaks storage. **That suspicion is correct, and it turns out to
be the decisive reason to keep `zfs promote`, not a minor tradeoff.**

**What Ceph's trash actually does (re-confirmed from §2.2's source-level research):**
`rbdImage.Delete()` = `Trash(0)` + deferred `TrashRemove`. There is **no second copy**
of data — the image keeps existing under its original identity (hidden from normal
listings), and the real purge is retried later, only actually running `rbd rm`-equivalent
once no snapshot/clone still depends on it. Applied literally to ZFS, this would mean:
on `DeleteVolume`, if the source has live snapshot dependents, **rename it out of the way
(or just leave it in place, hidden via `canmount=off`) and defer the real
`zfs destroy -r` until every dependent is gone**, retried on an ordinary reconcile
requeue — structurally, this is **Option 1 (block/retry, §1/§5) with the CSI response
lied about**: instead of `DeleteVolume` returning `FAILED_PRECONDITION` and making the CO
retry, it returns success immediately while our own controller keeps the real cleanup
pending internally. (This *is* CSI-legal — Ceph does exactly this, and CSI doesn't
require the underlying bytes to be reclaimed the instant `DeleteVolume` returns, only
that the volume can't reappear/be reused.)

**The decisive flaw, quantified precisely — this is exactly the user's own suspicion,
confirmed:** a ZFS dataset with live snapshots cannot have *only* its own private/unique
data (the bytes written since the most recent snapshot — literally the reason someone
deletes a volume, to reclaim its space) freed while leaving its snapshots intact and
attached to it. ZFS gives exactly two options for a dataset with live snapshots: refuse
outright (plain `zfs destroy`), or cascade-destroy the dataset *and* every one of its own
snapshots (`zfs destroy -r`) — there is **no third "free my own data, keep my snapshots
attached to me" operation**. So under trash, the *entire* renamed/hidden source dataset —
including all of its own private data — **must stay fully allocated for as long as any
snapshot derived from it survives, which can be indefinitely** (keeping snapshots for
long-term backup/retention, exactly this project's own stated use case, §2.5, is a
completely normal pattern). `DeleteVolume` would report success while the pool silently
keeps the entire deleted volume's storage allocated — a real, unbounded, and **silent**
leak relative to what the CO/user believes happened, arguably worse than Option 1's
honest, visible block-and-retry (at least Option 1 doesn't claim space was reclaimed when
it wasn't).

**Why Ceph is forced into trash, and why that isn't evidence trash is inherently
"better":** RBD clones have no reverse-parentage primitive — once image B is cloned from
image A's snapshot, RBD has no operation to make A's data live under B instead. Trash is
Ceph's *only available compensating mechanism* given that structural limitation, not a
first-choice design born from trash being more robust. **ZFS is not similarly
constrained — `zfs promote` is exactly the "reverse the parent/child relationship
without copying data" primitive RBD lacks.** Using promote isn't "reinventing Ceph's
wheel differently for its own sake" — it's using a strictly *stronger* tool that happens
to be available here and isn't available there. Confirmed concretely: after `zfs promote
csi-snap-<name>` + `zfs destroy pvc-src` (no `-r`, D11), `pvc-src`'s own private/unique
data is freed **immediately and unconditionally** the moment `DeleteVolume` completes —
regardless of how many snapshots exist or how long they're kept — with the only
remaining allocated space being data a *live* snapshot still legitimately needs, which no
design (trash, promote, or any other real CSI driver) can avoid freeing early without
literally copying data. The backing clone (`csi-snap-<name>`) itself adds no meaningful
space overhead either: as a `canmount=off`/`volmode=none` clone that's never written to,
it shares 100% of its blocks with the (still-needed) snapshot data — there is nothing
"extra" being retained beyond what any correct design must retain regardless.

**What trash *would* have bought us, for honesty:** a substantial complexity reduction —
D0 (promote), D1/D1a (backing clone naming), D5 (self-snapshot suffix), D8 (dual-mode
toggle — trash would work identically well/poorly in both "modes," so there'd be no
reason for two modes at all), D12 (multi-restore promote-chaining), D13 (promote-retry
defensiveness), and D15 (backing clone as owned child `ZfsDataset`) would **all become
unnecessary** — `CreateSnapshot` would revert to today's plain `zfs snapshot`, universally,
for every case. That's a lot of surface area. It was seriously weighed against the
storage-leak flaw above, and rejected: given this project's explicit, repeated priority
this session ("whatever it takes," full correctness, "must be possible to delete and not
influence others" with **no** silent leaks), unbounded silent storage leakage on a very
common, explicitly-supported usage pattern (long-lived retained snapshots) is not an
acceptable trade for less code. **D0 stands: `zfs promote` is kept, Ceph-style trash is
rejected as the core mechanism** (see updated §5 entry).

## 3. Chosen design

Two selectable **modes**, per `VolumeSnapshotClass` (see D8), because they have genuinely
different trade-offs and the user's own backup-replication use case benefits from being
able to choose:

### 3.1 `standalone` mode (Ceph-style, via `zfs promote`) — **new default**

**On `CreateSnapshot`** (source `pool/k8s/secure/pvc-src`, CSI name `snap-1`):
1. `zfs snapshot pool/k8s/secure/pvc-src@snap-1` — raw point-in-time snapshot (unchanged
   from today).
2. **Create a real, owned child `ZfsDataset` object for the backing clone (D15)** —
   `pool/k8s/secure/csi-snap-snap-1`, a flat sibling of the source directly under the
   same prefix (no separate container dataset needed, see §2.8), matching Ceph's
   `csi-snap-<uuid>` clone-image equivalent. Set `Spec.Source.Snapshot =
   "pool/k8s/secure/pvc-src@snap-1"` and `Spec.Type = SourceType`; the object carries an
   `ownerReference` (`blockOwnerDeletion: true`) back to this `ZfsSnapshot` — appropriate
   here since it's a genuine, strict 1:1 lifecycle-bound parent/child (unlike the
   restored-PVC relationship below, which stays finalizer-based, not owner-reference
   based — see D4's rejected alternatives). **This is not a raw `zfs clone` call** —
   provisioning is entirely delegated to `ZfsDatasetReconciler`'s existing ADR-0009
   clone-creation code path (`canmount=off`/`volmode=none` derivation included), so
   `ZfsSnapshotReconciler` doesn't need a second, parallel clone implementation.
   `ZfsSnapshotReconciler` requeues until the child object reports `Ready`.
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
`zfs destroy`:
1. List live `ZfsSnapshot`s with `Spec.SourceVolume == pvc-src`.
2. **Block** (return error, requeue) if any of them are not yet `Ready` (see D3 — avoids
   destroying the source out from under an in-flight `CreateSnapshot`).
3. For each `Ready` one, `zfs promote pool/k8s/secure/csi-snap-<name>` **using the
   verify-and-retry `Promote` primitive (D13)** — re-reads `origin` after promoting and
   retries (bounded) until it's confirmed empty, per the confirmed upstream ZFS
   reliability gap (§2.9); idempotent no-op if `origin` was already empty.
4. **Also** (D7/D9): list live `ZfsDataset`s with `Spec.Source.Volume == pvc-src` (direct
   PVC-to-PVC clones, not via any `VolumeSnapshot`) and unconditionally `zfs promote` each
   of *those* clone datasets too, detaching the intermediate `pvc-src@clone-<name>`
   snapshot from ADR-0009's volume-clone path. No mode toggle here — always-on, since
   there's no `VolumeSnapshotClass` involved in a direct volume clone to attach a toggle
   to, and blocking here would be confusing (no visible object the user is managing).
5. **Also** (D12): list any `promoted-onto.<name>` finalizers on **this** `ZfsDataset`
   (registered by another dependent's `DeleteSnapshot`/`DeleteVolume` when a multi-way
   promote chain — §2.9 — landed on *this* dataset instead of fully detaching), and
   `zfs promote` each of those tracked datasets away too, same verify-and-retry
   primitive. Then destroy any of this dataset's own leftover relocated snapshot
   artifacts (e.g. a relocated `@restore-source`) that are now dependent-free — required,
   not best-effort, mirroring D11's fix for the `ZfsSnapshot` case.
6. Only after all of the above, **`zfs destroy pool/k8s/secure/pvc-src` — no `-r`** (D11):
   by this point every tracked dependent (§3.1 steps 3-5) has been promoted away and any
   of this dataset's own leftover snapshot artifacts destroyed, so it has zero remaining
   snapshots/children of its own — plain destroy always succeeds. If an
   untracked/manually-created snapshot or child dataset unexpectedly exists, this now
   fails loud (reconcile error/requeue, `ZfsDataset` stays `Terminating`) instead of
   silently destroying it the way `-r` would have.

**Restores** (`resolveContentSource`) always clone from
`csi-snap-<name>@restore-source`, never from the original source path directly — stable
whether the source is alive, deleted-but-not-yet-promoted, or promoted away.

**`DeleteSnapshot`, final design — delegates to `ZfsDatasetReconciler` (D15), which
still uses finalizer-based tracking + reverse-promote (D4/D12 unified)**: always
succeeds, never blocks.
1. When `CreateVolume` restores a PVC from `snap-1`'s backing clone, it (synchronously,
   as part of the restore, fetch-check-patch with resourceVersion-conflict retry) adds a
   finalizer **to the backing-clone `ZfsDataset` object itself** (not `ZfsSnapshot`) —
   `storage.simple-zfs-csi.io/restored-by.<newPvcName>` — the same generalized mechanism
   as D12's `promoted-onto` tracking, just applied to a snapshot-restore dependency
   instead of a promote-chain one; **D4 is now a special case of D12**, not a separate
   mechanism. If the backing-clone object already has a `deletionTimestamp` at that
   point, the restore is rejected (its source is going away).
2. When that restored PVC is later deleted, its own `ZfsDatasetReconciler` teardown path
   removes that same finalizer from the backing-clone `ZfsDataset` as part of its own
   cleanup (in addition to its own `zfsDatasetFinalizer`).
3. `snap-1`'s own delete path (`ZfsSnapshotReconciler`) **no longer runs its own
   promote/dependent loop.** It simply deletes the backing-clone `ZfsDataset` object
   (`ownerReference`-driven cascade, or an explicit `client.Delete`) and requeues until
   that object is confirmed gone.
4. **All promote/dependent-chaining complexity is delegated to `ZfsDatasetReconciler`'s
   own delete path** (already built for D3/D7/D9/D12/D13, generalized here to also read
   `restored-by.*` finalizers on itself, not just `promoted-onto.*`): it promotes every
   tracked dependent (single restored PVC, or the D12 multi-way chain if there were
   several simultaneous restores from this snapshot — verify-and-retry primitive, D13),
   destroys any of its own leftover relocated snapshot artifacts (e.g. a relocated
   `@restore-source`) once confirmed dependent-free, and only then destroys itself
   (plain `zfs destroy`, no `-r` — D11's invariant applies here too, since by this point
   it's guaranteed dependent-free). **This reuses the exact same machinery already built
   for D3/D7/D9/D12/D13 — zero duplicate implementation in `ZfsSnapshotReconciler`.**
5. Once the backing-clone `ZfsDataset` object is confirmed fully gone,
   **required, not best-effort:** destroy the raw origin snapshot on the source dataset
   (`zfs destroy source@snap-1`, non-recursive) if the source still exists and this
   snapshot wasn't already relocated by an earlier D3 promote (e.g. because the source
   was deleted first). This is a no-op (idempotent success) when it was already
   relocated — it only does real work in the common case (a snapshot created and later
   deleted without the source having been deleted first). **Ordering matters and is not
   optional:** this must run *after* step 4 (the backing clone's own destruction) —
   recall the backing clone's `origin` is `source@snap-1`, so destroying `source@snap-1`
   first would fail with "has dependent clones" while the backing clone still exists.
6. **Only after step 5 succeeds** does the reconciler remove its own `zfssnapshot`
   finalizer, letting the `ZfsSnapshot` object actually disappear. If step 5 fails for any
   reason, `Reconcile` returns the error and the finalizer stays — `ZfsSnapshot` remains
   visibly `Terminating` (debuggable) rather than silently vanishing while leaving an
   orphaned raw snapshot behind on the source. **This is the invariant that makes it safe
   to drop `-r` in `DeleteVolume` (§3.1 `DeleteVolume`, and D3/D7 below): a `ZfsSnapshot`
   can only ever fully disappear once its raw snapshot has been dealt with, so a
   `DeleteVolume` that finds zero live `ZfsSnapshot`s referencing it can trust that zero
   raw snapshots remain too** (raised and closed as a design gap — see D11).

**Always succeeds** for the `ZfsSnapshot` deletion itself (steps 3-4 never block on
dependents, matching Ceph's real, source-confirmed behavior) — the only way `DeleteSnapshot`
can stall is transient failure of the source-side cleanup (step 5), which is retried like
any other reconcile error, not a permanent block.

### 3.2 `integrated` mode (today's model, hardened only with blocking)

- `CreateSnapshot`: plain `zfs snapshot` only — no backing clone, no hidden folder, no
  extra ZFS objects. Exactly today's behavior.
- `DeleteVolume`: **blocks** (`FAILED_PRECONDITION`-style error/requeue) while any
  `integrated`-mode dependent `ZfsSnapshot` still exists — there's nothing to promote in
  this mode, so Option 1 (the democratic-csi-style fix) applies as-is. Once every
  dependent is actually gone (the block only releases then), the eventual destroy is
  **also plain `zfs destroy`, no `-r`** — same D11 invariant, same unified
  `ZfsDatasetReconciler` delete path as `standalone` mode (task list item 4); by that
  point the source is guaranteed to have zero snapshots of its own left either way.
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
| D0 | Core mechanism | `zfs promote` to relocate a snapshot onto a pre-created backing clone — ZFS-native equivalent of Ceph's snap-trash. **Re-examined from scratch and reaffirmed (§2.12)**: Ceph-style trash (rename+defer) was seriously considered as a full replacement, since it would eliminate a large amount of this design's complexity (D1/D5/D8/D12/D13/D15) — rejected because a ZFS dataset with live snapshots cannot have *only* its own private data freed while leaving snapshots attached (no such primitive exists); trash would leave the *entire* deleted volume's storage allocated for as long as any snapshot survives, potentially indefinitely — an unbounded, silent leak on a normal usage pattern (long-retained backup snapshots). `zfs promote` reclaims the deleted volume's own private data immediately and unconditionally instead, which Ceph's RBD cannot do (no reverse-clone-parentage primitive exists there, which is *why* Ceph uses trash — a compensating mechanism for a limitation ZFS doesn't share). |
| D1 | Backing clone naming/location | **Revised (§2.8):** flat sibling `dirname(sourceDataset)/<namePrefix><snapName>` (e.g. `pool/k8s/secure/csi-snap-<name>`) — **not** pool-global (required for `zfs send -R` backup compatibility, §2.5), and **not** a nested subfolder (matches Rook/ceph-csi's own naming convention and this project's existing `@clone-<name>` convention, §2.8) |
| D1a | Name prefix | Configurable via values.yaml, default **`csi-snap-`** |
| D2 | ~~Hidden container creation~~ | **Removed — no longer applicable.** Only existed for the original nested-subfolder design; the flat-prefix revision (§2.8, D1) needs no separate container dataset at all, just a sibling under the already-existing prefix. |
| D3 | In-flight-snapshot vs. concurrent `DeleteVolume` race | `ZfsDatasetReconciler`'s delete path blocks (error+requeue) on any dependent `ZfsSnapshot` not yet `Ready` (`Pending` or `Error` phase) — bounded wait, not a deadlock (verified: both reconcilers run on the same node/manager per pool, underlying ZFS object isn't destroyed yet so the in-flight snapshot can still complete) |
| D4 | `DeleteSnapshot` "in use" detection + policy | **Final: finalizer-based dependency tracking** (`restored-by.<pvcName>` finalizers added at restore time, removed at the dependent's own teardown) **+ reverse-`zfs promote`** of every tracked dependent before destroying the backing clone. Always succeeds, matches Ceph's actual (source-code-confirmed) behavior. Superseded two earlier, less-good candidates: (a) discover "in use" only via the async ZFS destroy error inside the finalizer loop (bad UX, no immediate/standard error signal); (b) synchronous check via `ZfsDataset.Spec.Source.Snapshot` + block with `FAILED_PRECONDITION` (better UX than (a), race-prone via TOCTOU, and doesn't match Ceph's real always-succeeds behavior). **Revised by D15**: since the backing clone is now a real, owned child `ZfsDataset` (not raw exec calls), the `restored-by.*` finalizer lives on *that* `ZfsDataset` object (not on `ZfsSnapshot` itself), and the actual promote/retry execution is delegated to `ZfsDatasetReconciler`'s own delete path — the exact same generalized mechanism as D12's `promoted-onto` tracking. D4 is therefore a special case of D12, not a separate implementation. |
| D5 | Self-snapshot suffix name | Fixed `@restore-source`, distinct from the CSI-visible snapshot name (avoids collision with the relocated raw-origin snapshot after promotion) |
| D6 | Cross-prefix restore (different `datasetPrefix` than the source) | **Reject** (`InvalidArgument`) in the controller, not just documented — applies identically to both modes (the backup-locality problem exists in `integrated` mode too, via the raw snapshot's own location) |
| D7 | Direct PVC-to-PVC clone (`VolumeContentSource_Volume`, no `VolumeSnapshot` involved) — must the *source* PVC's deletion be blocked while a clone exists? | **No — always promote instead, unconditionally** (no mode toggle possible/needed, since there's no `VolumeSnapshotClass` in this path). Same mechanism as D3, applied to ADR-0009's intermediate `<src>@clone-<name>` snapshot. Re-confirmed explicitly as its own question later in the conversation — same answer. |
| D8 | Dual-mode selectability | `VolumeSnapshotClass` parameter `mode: standalone\|integrated`; values.yaml default `csiController.snapshotter.defaultMode: standalone` (chosen over `integrated` because there are no existing snapshots yet in the target cluster — no migration risk) |
| D9 | (alias of D7 — the user re-asked this question in different words; recorded here to make clear it was re-verified, not newly introduced) | Same as D7 |
| D10 | Property/fsType compatibility on clone and restore (§2.7) | **Reject on any mismatch, for both `resolveContentSource` code paths (restore-from-snapshot and clone-from-volume), both modes.** Specifically: (a) `volblocksize` — reject if the target's resolved value differs from the source's actual value (today it's silently ignored instead); (b) any `property.*` override (`recordsize`, `compression`, etc.) — reject if the target's resolved value differs from the source's recorded `Spec.Properties`/`Spec.Volume.Volblocksize`, rather than allowing a technically-valid-but-confusing partial override; (c) `fsType` (block/zvol only) — reject if the target's resolved `fsType` differs from the source's actual formatted filesystem. Chosen over allowing selective, per-property overrides: simpler, matches "clone copies content, not the freedom to redecide structure," and closes a real, confirmed, silently-broken case (Kubernetes explicitly permits cross-StorageClass cloning/restore with no compatibility checking of its own, per §2.7). Comparison for (a)/(b) is Spec-to-Spec (no live ZFS query needed, consistent with the D4 preference for K8s-native checks over ZFS round-trips); (c) requires new state — see task list. Confirmed **unconditional** (no opt-in override, unlike Ceph's `allow-volume-mode-change` annotation for its own analogous, narrower check) — can be loosened later if a real need arises. |
| D11 | Drop `-r` (recursive) from `DeleteVolume`'s `zfs destroy` call, now that D3/D7 promote every tracked dependent first | **Yes — drop it.** Once all tracked dependents (standalone-mode `ZfsSnapshot`s via D3, direct-volume clones via D7) are promoted away before destroy runs, the source dataset is guaranteed to have zero snapshots/children of its own left (destroying a clone — which the source may now be, post-promotion — never needs `-r` regardless of its own origin). Dropping `-r` turns a previously *silent* risk (an untracked/manually-created snapshot or child dataset would have been destroyed without warning) into a *fail-loud* one (plain `zfs destroy` refuses, reconcile errors/requeues, `ZfsDataset` stays visibly `Terminating`) — consistent with ADR-0013's existing "fail loud on unexpected state" philosophy. **Uncovered a real gap while verifying this**: `DeleteSnapshot`'s step 5 (destroying the raw origin snapshot on the source when there were no restore-dependents) was originally worded "best-effort," which would have silently broken this exact invariant — corrected to a **required, finalizer-gated** step (§3.1): a `ZfsSnapshot` cannot fully disappear until its raw snapshot is confirmed dealt with, so by the time `DeleteVolume` finds zero live `ZfsSnapshot`s referencing it, zero raw snapshots can remain either. This correction is what makes D11 safe, not just plausible. |
| D12 | Multiple simultaneous restores from one snapshot: how to keep every dependent independently deletable (§2.9) | **Generalize the finalizer/promote-dependent-tracking mechanism to work between any two `ZfsDataset`s, not just `ZfsSnapshot` → dependent.** When promoting the Nth (N≥2) simultaneous dependent lands it as a clone of dependent #1 instead of fully freeing it — **intentional, documented ZFS design, confirmed directly in the OpenZFS source** (`dsl_dataset_promote_sync`'s explicit "move any clone references" reassignment of sibling clones, §2.9), **not a bug we're relying on** — register a `promoted-onto.<name>` finalizer directly between them; the owning dataset's own `DeleteVolume` path (§3.1 step 5) promotes any such tracked dependents away before it can be destroyed itself — the exact same pattern already built for D3/D4, just made recursive/composable. Chosen over always fully duplicating ("flattening") every extra dependent's data (rejected — real storage cost plus live-mount quiesce/reattach risk for what is a rare case, see §2.9). |
| D13 | `Promote` primitive reliability — is one `zfs promote` call always sufficient, and what should be verified afterward? | **Confirmed real upstream defect exists** ([openzfs/zfs#15587](https://github.com/openzfs/zfs/issues/15587), OpenZFS 2.1.5): promoting a dataset involved in a clone-of-clone chain can require multiple calls; `bemgr` works around this by looping (capped at 100 attempts). **We do not rely on this defect — we defend against it**, but **the verification check must not be "is `origin` empty"** — corrected after live testing (§2.11 test log): a cleanly, successfully promoted dataset does **not** necessarily end up with an empty `origin` at all (multi-hop chains legitimately leave a non-empty `origin` pointing at the previous link — this is normal lineage bookkeeping, not a sign of failure or a block on destroying that dataset). **Corrected check: compare `origin` immediately before and immediately after the call — retry (bounded, e.g. 20 attempts, logged) only if it is unchanged**; a changed value (to anything, including a different non-empty value) means the call made progress. Hard error (fail loud, ADR-0013) only if `origin` never changes across the retry budget. |
| D14 | Is `ZfsSnapshot` still the right CRD name/kind, given `standalone` mode is backed by a raw snapshot + backing clone + self-snapshot, not "just" a plain ZFS snapshot? (§2.10) | **Keep the name and the single CRD kind — add `Mode` as a field (D8), not a new kind.** Matches the precedent already set by `ZfsDataset`: structural kind (`DatasetType`: filesystem/volume) and provenance/mechanism (`DatasetSource`: plain-create vs. clone vs. restore) are already orthogonal, field-level concerns on that CRD, not separate kinds — a cloned `ZfsDataset` isn't renamed `ZfsClone`. `Mode` (standalone/integrated) is the same category of concern for `ZfsSnapshot`: it's about *mechanism*, not *contract*. At the CSI/`VolumeSnapshot` contract level both modes are identical (a point-in-time, read-only, restorable capture of a volume) — which is what "snapshot" means at that layer, regardless of ZFS-level implementation. Reinforced by precedent: Ceph-CSI's own "snapshot" is *also* implemented as a clone-image + its own self-snapshot under the hood (§2.2), and Ceph doesn't rename their concept for it. Renaming the CRD kind itself would also be a large, mostly-cosmetic refactor (touches every reference across controllers/CSI/charts/CRDs) for no functional gain — same category of cost as the naming-consistency item already deferred as future work in [independent-resource-naming-redesign.md](independent-resource-naming-redesign.md). |
| D15 | Backing clone representation: raw exec-managed dataset vs. a real, owned child `ZfsDataset` object — could we just reuse `ZfsDataset` instead of two CRDs? (§2.10 note, raised as a follow-up) | **Keep `ZfsDataset`/`ZfsSnapshot` as two CRDs (D14 stands), but represent the *backing clone itself* as a real, `ZfsSnapshot`-owned child `ZfsDataset` object** (`ownerReference`, `blockOwnerDeletion: true` — appropriate here, unlike the restored-PVC relationship, since this is a genuine strict 1:1 lifecycle-bound parent/child). Provisioning reuses `ZfsDatasetReconciler`'s existing ADR-0009 clone-creation code path instead of a second, parallel raw-exec implementation in `ZfsSnapshotReconciler`. Teardown reuses that same reconciler's own (D3/D7/D9/D12/D13) promote-then-destroy delete-path logic instead of `ZfsSnapshotReconciler` duplicating a bespoke promote/retry/dependent-chaining loop. This **unifies D4 into D12**: the `restored-by.*` finalizer now lives on the backing-clone `ZfsDataset` (where the clone relationship actually is), tracked/promoted by the same generalized mechanism as D12's `promoted-onto` finalizers — one mechanism, one place, rather than two similar-but-separate implementations split across two reconcilers. `ZfsSnapshotReconciler`'s own delete path shrinks to: delete the child object (delegating all promote complexity), wait for it to be gone, run the required raw-origin-snapshot cleanup on the true source, then release its own finalizer. |
| D16 | Does D3's promote-loop iteration order matter when a source has multiple, independent snapshots, each with its own backing clone (§2.11)? | **Confirmed correct, empirically, on a live pool (§2.11 test log) — order-independent, self-correcting, no bug, no design change needed.** Ran the exact scrambled-order scenario (t3, t1, t6, t2, t4, t5) against a real ZFS pool (`spinning-archive`, isolated scratch dataset): `zfs promote`'s history-walk is bounded by the *current* clone-origin chain, not an unconditional walk to genesis as a literal source read first suggested — promoting `csi-snap-t6` stopped exactly at `csi-snap-t3`'s boundary instead of reaching through and stealing `t2`/`t3` from it, and a promoted-and-already-independent `csi-snap-t1` was never touched again by any later promote in the batch. Final state: every backing clone ends up owning exactly and only its own snapshot, `vol1` ends with zero snapshots of its own (`zfs destroy vol1`, no `-r`, succeeds). No fixpoint loop, no reordering, and no `promoted-onto` cross-tracking are needed for this specific case — a single unconditional pass over all tracked dependents (as originally designed) is sufficient. (The live test also surfaced and fixed a real, separate bug in D13's verification check — see D13.) |

## 5. Rejected alternatives (and why)

- **Just implement Option 1 everywhere (block `DeleteVolume` via `FAILED_PRECONDITION`,

- **Just implement Option 1 everywhere (block `DeleteVolume` via `FAILED_PRECONDITION`,
  democratic-csi style) and stop there.** Rejected as the *final* answer (though it's
  still used for `integrated` mode) because the user's real requirement is "deleting a
  volume must always be possible, snapshots must survive" — Option 1 doesn't achieve that,
  it just fails safely instead of corrupting data.
- **Ceph-style "trash" as the core mechanism (rename+defer instead of `zfs promote`).**
  Re-examined in full, from scratch, later in this project (§2.12) at the user's explicit
  request to reconsider every decision here — not just briefly dismissed. Conclusion
  unchanged, now with a precise, quantified reason: a ZFS dataset with live snapshots has
  no operation to free *only* its own private/unique data while leaving its snapshots
  attached (only "refuse" or "cascade-destroy everything including the snapshots" exist)
  — so trash would leave the *entire* deleted volume's storage allocated for as long as
  any snapshot survives, potentially indefinitely (a real, silent, unbounded leak on this
  project's own normal long-retained-backup-snapshot use case, §2.5). `zfs promote`
  reclaims the deleted volume's private data immediately and unconditionally instead —
  something Ceph's RBD structurally cannot do (no reverse-clone-parentage primitive),
  which is *why* Ceph relies on trash, not evidence that trash is inherently more robust.
  Trash would have eliminated a large amount of this design's complexity (D1/D5/D8/D12/
  D13/D15) — seriously weighed, but rejected: given this project's repeated, explicit
  priority on full correctness and no silent leaks, that complexity reduction isn't worth
  the storage-leak risk it would reintroduce.
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
  are the correct tool, not owner references. **Scope note (D15):** this rejection is
  specifically about the *restored-PVC-depends-on-snapshot* relationship. It does not
  apply to the *`ZfsSnapshot`-owns-its-backing-clone* relationship introduced by D15 —
  that one genuinely is a strict 1:1 lifecycle-bound parent/child (the backing clone has
  no independent existence and must not survive its `ZfsSnapshot`), which is exactly what
  `ownerReferences`/`blockOwnerDeletion` are for.
- **`ZfsSnapshotReconciler` managing the `standalone`-mode backing clone directly via raw
  `zpool.ZFS.*` exec calls, with its own bespoke promote/dependent-tracking/destroy loop**
  (the original plan in this document). Superseded by D15: representing the backing
  clone as a real, owned child `ZfsDataset` object lets `ZfsSnapshotReconciler` delegate
  all of that complexity to `ZfsDatasetReconciler`'s already-built (D3/D7/D9/D12/D13)
  create/clone and promote-then-destroy logic instead of maintaining a second, parallel
  implementation of the same rules.
- **Splitting into separate CSI driver names for zvol vs. filesystem** (raised twice: once
  generally, once specifically re: cross-type restore rejection). Rejected both times —
  see ADR-0017 and §2.6 above. Not reopened by anything found in this investigation.
- **Always fully duplicate ("flatten") every simultaneous restore's data via `zfs send`/
  `zfs receive` instead of promoting** (§2.9, D12). Would sidestep ZFS's real
  one-physical-copy-of-shared-history constraint entirely and guarantee unconditional
  mutual independence, but costs a real full data copy per extra dependent and would
  require quiescing/detaching and re-attaching a live NFS export or NVMe-oF target if the
  dataset being replaced is an actively mounted PVC — disproportionate for what is a
  comparatively rare case. Rejected in favor of the generalized, metadata-only
  dependent-tracking mechanism (D12), which reuses the same finalizer+promote machinery
  already built for D3/D4.

## 6. Implementation task list (order of work)

1. `internal/zpool`: add `ZFS.Promote(ctx, dataset) error` (+ CLI impl `zfs promote`, +
   fake/test double). Idempotent: no-op if the dataset has no `origin` property.
   **D13: must verify-and-retry** — after calling `zfs promote`, re-read `origin`; if
   still non-empty, retry (bounded, e.g. 20 attempts, logged each retry) before
   returning a hard error. Do not assume a single call is sufficient (confirmed real
   upstream defect, §2.9/D13, openzfs/zfs#15587).
2. `api/v1alpha1/zfssnapshot_types.go`: add `Spec.Mode` (enum `Standalone`/`Integrated`,
   immutable, default resolved at creation — same pattern as the existing `SourceType`
   field). Regenerate CRDs/deepcopy (`make manifests`).
3. `internal/controller/zfssnapshot_controller.go`:
   - Create path: branch on `Mode`. `standalone` → raw snapshot (`zfs snapshot`) +
     **create a real, owned child `ZfsDataset` object for the backing clone (D15)**
     (`csi-snap-<name>`, `ownerReference`/`blockOwnerDeletion: true` back to this
     `ZfsSnapshot`, `Spec.Source.Snapshot` set to the raw snapshot, `Spec.Type =
     SourceType`) — requeue until it reports `Ready`, then take the `@restore-source`
     self-snapshot directly on it (still a raw op, not itself CRD-tracked). `integrated`
     → unchanged (raw snapshot only).
   - Delete path (D15): **no longer implements its own promote/dependent loop.** Delete
     the backing-clone `ZfsDataset` object and requeue until it's confirmed gone —
     `ZfsDatasetReconciler`'s own delete path (item 4 below) handles all promotion of
     tracked dependents (single or multi-way chain, D12) and the backing clone's own
     destruction. Once gone, required (not best-effort) cleanup of the raw origin
     snapshot on the source if still present, then release own finalizer. `integrated`
     → unchanged (destroy raw snapshot; rely on ZFS's natural "has dependent clones"
     refusal + reconcile retry if something depends on it).
4. `internal/controller/zfsdataset_controller.go` (`ZfsDatasetReconciler` delete path):
   - D3: list dependent `ZfsSnapshot`s (`Spec.SourceVolume == this`); block/requeue if any
     are not yet `Ready`.
   - For `Ready`, `standalone`-mode dependents: `zfs promote` their backing clone, **as a
     bounded fixpoint loop (D16, §2.11), not a single pass** — repeat the full pass over
     all `Ready` dependents until none of their `origin`s flip from empty back to
     non-empty anymore (capped, e.g. 10 rounds, logged), since promoting one can
     transiently re-attach a sibling that was already independent. Only proceed once the
     whole batch is simultaneously confirmed independent. **Verify this against a real
     ZFS pool during implementation** — the exact drag-along boundary isn't fully
     confirmed from source alone (§2.11, open item §8).
   - For `Ready`, `integrated`-mode dependents: block/requeue (`FAILED_PRECONDITION`-style)
     until they're gone.
   - D7/D9: list dependent `ZfsDataset`s (`Spec.Source.Volume == this`, direct clones);
     always `zfs promote` each one, unconditionally.
   - **D12/D15 (unified)**: list any `promoted-onto.<name>` **and** `restored-by.<name>`
     finalizers on this `ZfsDataset` (the latter now generalized here per D15, since
     restored PVCs' finalizers live on whichever `ZfsDataset` physically owns the
     restore-source snapshot — the backing clone included); `zfs promote` each tracked
     dependent away too, same verify-and-retry primitive. Then destroy this dataset's
     own leftover relocated snapshot artifacts (now dependent-free) — required, not
     best-effort.
   - Only then proceed to `zfs destroy` **without `-r`** (D11) — see §3.1's `DeleteVolume`
     walkthrough for why this is safe and what it protects against.
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
   - Restore-side finalizer add (D4/D15): when `CreateVolume` resolves a
     `standalone`-mode snapshot content source, patch the **backing-clone
     `ZfsDataset`** (not the `ZfsSnapshot`) to add
     `storage.simple-zfs-csi.io/restored-by.<newPvcName>` (fetch-check-`deletionTimestamp`
     -patch-with-conflict-retry). Reject the restore if that object is already
     terminating.
   - `ZfsDatasetReconciler`'s own delete path removes that same finalizer from the
     backing-clone `ZfsDataset` (if any) as part of its teardown, regardless of
     clone/restore kind.
7. Chart: `charts/simple-zfs-csi/values.yaml` — add
   `csiController.snapshotter.defaultMode: standalone` and a name-prefix value
   (default `csi-snap-`). Thread both into `cmd/csi-controller` (parameter
   resolution + restore path) via chart-templated flags/args. No separate agent-side
   wiring needed for backing-clone creation itself (D15): it's provisioned as an
   ordinary child `ZfsDataset` object through the existing `ZfsDatasetReconciler`
   path, which the agent already serves.
8. Tests (both `internal/controller/*_test.go` and `internal/csi/*_test.go`):
   - `standalone` create → clone + self-snapshot with correct properties per `SourceType`.
   - `DeleteVolume` promotes all `Ready` `standalone` dependents and always succeeds.
   - `DeleteVolume` with **multiple, independent snapshots of the same source** (D16,
     §2.11): create six snapshots of one volume at different times (each with its own
     backing clone), delete the volume with the dependent list iterated in a
     deliberately scrambled (non-chronological) order, and confirm the fixpoint promote
     loop converges (every backing clone ends up owning exactly its own snapshot, zero
     cross-contamination) — **run against a real ZFS pool/VM, not only a fake/mocked
     `ZFS` double**, since this specific test needs to observe real `zfs promote`
     drag-along behavior, which is not fully characterized from source alone.
   - `DeleteVolume` blocks on a non-`Ready` dependent snapshot (D3) and on any
     `integrated`-mode dependent.
   - `DeleteVolume` promotes direct-clone dependents (D7/D9) unconditionally.
   - Restore clones from `@restore-source` (standalone) / raw snapshot (integrated).
   - Cross-prefix restore rejected (D6), both modes.
   - `DeleteSnapshot` (standalone): finalizer added on restore **to the backing-clone
     `ZfsDataset`** (D15), removed on dependent teardown; deleting the `ZfsSnapshot`
     deletes the backing-clone object and waits for `ZfsDatasetReconciler`'s own delete
     path to promote dependents away and destroy it — always succeeds even with live
     dependents.
   - `ZfsSnapshotReconciler`'s create path correctly waits for the child `ZfsDataset`
     to reach `Ready` before taking `@restore-source`; delete path correctly waits for
     it to be fully gone before the required raw-origin-snapshot cleanup (ordering,
     D15/D11).
   - `DeleteSnapshot` (standalone) with **two or more simultaneous restores from the same
     snapshot** (D12/§2.9): after deletion, every restored PVC remains independently
     deletable in any order; deleting the "chosen" (first-promoted) one correctly
     promotes away any `promoted-onto`-tracked sibling first, never leaves a stuck
     `Terminating` `ZfsDataset`.
   - `Promote` primitive (D13): retries when `origin` doesn't clear on the first call;
     surfaces a hard error after exhausting retries (use a fake ZFS double that
     simulates the confirmed upstream multi-call quirk).
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

None outstanding as of writing except one **explicitly flagged, unresolved item**:

- **D16's promote-order/drag-along behavior needs hands-on ZFS verification before
  implementation** (§2.11). Source-reading (`dsl_dataset.c`'s `snaplist_make`) confirms
  `zfs promote`'s history walk follows each snapshot's intrinsic `ds_prev_snap_obj`
  chain regardless of current directory ownership, which can reach back through
  snapshots already relocated by an earlier promote in the same `DeleteVolume` batch —
  confirmed more aggressive drag-along than first modeled. **Unconfirmed**: whether this
  can also reach back through a snapshot that already belongs to a fully independent,
  previously-promoted clone (which would violate the expected invariant that a dataset's
  own most-recent snapshot's `ds_dir_obj` always matches its own). Mitigated defensively
  regardless (a bounded fixpoint promote loop, D16), but this specific ZFS behavior
  should be verified against a real pool (or the OpenZFS test suite) before the
  implementation task list item for D3/D16 is considered done.

  > **Errata (2026-07-31): resolved.** Verified empirically on a live pool
  > (`spinning-archive`, isolated scratch dataset) — full log in
  > [promote-order-verification-2026-07-31.md](promote-order-verification-2026-07-31.md).
  > No steal-back of an already-independent clone was observed; the drag-along is
  > bounded by the current clone-origin chain, not unconditional. D16's fixpoint-loop
  > mitigation is not required (harmless to keep as defense-in-depth if preferred). This
  > item is no longer open — left in place, unmodified, for the historical record of the
  > reasoning that led to running the test.

Every other decision point raised (D0-D16 aside from the above) has a final answer
recorded above. If new questions come up during implementation, add them here as `D17`,
`D18`, etc., following the same table format, before resolving them.
