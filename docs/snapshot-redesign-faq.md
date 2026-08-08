# Snapshot/Volume Independence Redesign — FAQ

A running log of questions asked during the snapshot-lifecycle/naming redesign work and
the answers/decisions that came out of them. Meant to be picked up later — by you or
anyone else — without needing to re-derive the reasoning from scratch. Each entry links
to the fuller write-up where one exists.

Related documents:
- [snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md) — the full research/decision log (D0-D16, see §2.12 for the trash-vs-promote re-examination, §2.11 for the promote-ordering proof)
- [independent-resource-naming-redesign.md](independent-resource-naming-redesign.md) — the separate, prerequisite naming redesign

> ### ⚠️ Partly superseded (2026-08-03)
>
> Answers below that describe `promoted-onto.*`/`restored-by.*` finalizer tracking, or a
> bounded fixpoint promote loop, describe a mechanism that **no longer exists**. A code
> review found four critical defects caused by keeping a copy of the ZFS clone graph in
> Kubernetes; dependents are now discovered by querying ZFS directly at delete time.
> The conclusions those answers reach are still right — `zfs promote` over trash, the
> backing clone, the dual modes, cross-prefix rejection — only the bookkeeping changed.
>
> Current behaviour: [ADR-0020](design-decisions.md) and
> [snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md) §4 (D17–D26) / §9.

---

**Q: Do we handle snapshots of zvols and filesystems correctly? What if someone tries to
create a snapshot of a filesystem PVC using a zvol `VolumeSnapshotClass` (or vice
versa)?**

A: This surfaced the real bug that started the whole redesign. Tracing it further
revealed the much bigger issue: `ZfsDatasetReconciler`'s `DeleteVolume` path ran
`zfs destroy -r`, which silently destroys all of a volume's snapshots when the volume
itself is deleted — violating the CSI requirement that snapshots survive their source
volume's deletion. See [snapshot-lifecycle-redesign.md §1](snapshot-lifecycle-redesign.md#1-the-problem-that-started-this).
(The cross-type mismatch itself was already correctly rejected via `SourceType`
checking, added earlier in this work.)

---

**Q: In order to implement the K8s specs correctly, don't we have to allow volume
deletion in any case, with snapshots living on regardless?**

A: Yes — confirmed against the CSI spec directly (`spec.md`): `DeleteVolume` explicitly
allows two valid strategies — true independence (delete the volume, keep snapshots fully
usable), or refuse the delete with `FAILED_PRECONDITION` and let the orchestrator retry.
Blocking is spec-legal but doesn't meet the actual requirement ("must be possible to
delete and not influence others"), so the design targets true independence, modeled on
how Ceph-CSI achieves it via `zfs promote` (the ZFS-native equivalent of RBD's
snap-trash). See [§2.1-2.4](snapshot-lifecycle-redesign.md#2-what-the-specs-and-other-drivers-actually-requiredo).

---

**Q: Should we error if the ZFS properties (volblocksize, recordsize, etc.) or fsType
differ between a clone/restore source and its target?**

A: Yes — **unconditional reject** on any mismatch (D10), for both `resolveContentSource`
paths (restore-from-snapshot and clone-from-volume), both modes. Confirmed via
Kubernetes' own docs that cross-StorageClass cloning is explicitly permitted with *no*
built-in property/fsType compatibility checking — that responsibility falls entirely on
the driver. Chosen over Ceph's opt-in-override pattern; can be loosened later if a real
need arises. See [§2.7](snapshot-lifecycle-redesign.md#27-propertyfstype-compatibility-on-clone-and-restore-raised-by-the-user-separately-from-the-snapshot-lifecycle-work-above-but-folded-into-the-same-redesign) and D10.

---

**Q: I'd prefer a prefix for the backing dataset (like Ceph's `csi-snap-<uuid>`) instead
of a nested hidden folder. How much effort would that be?**

A: Revised the design from a nested subfolder (with a separate hidden container dataset)
to a flat sibling name (`dirname(source)/csi-snap-<name>`) — matches how Rook/ceph-csi
actually names things (a structural necessity for RBD, adopted here for consistency) and
this project's own existing `@clone-<name>` convention. Also eliminates a whole
container-dataset-creation step. Crucially, this also turned out to be *required*, not
just nicer: a pool-global hidden namespace would have broken the user's real
`zfs send -R` backup workflow (an `-R` stream can't preserve a clone unless its origin
snapshot is inside the same replicated subtree). See
[§2.5](snapshot-lifecycle-redesign.md#25-the-zfs-send--r-backup-replication-constraint-raised-by-the-user-this-is-their-production-workflow-not-a-hypothetical)
and [§2.8](snapshot-lifecycle-redesign.md#28-backing-clone-naming-flat-name-prefix-not-a-nested-subfolder-revised-from-the), D1/D1a.

---

**Q: Why do we need a translation journal (Ceph-style volume-journal mapping) at all? We
already have the `ZfsDataset`/`ZfsSnapshot` CRDs — isn't changing the naming scheme
straightforward?**

A: Correct catch — the original effort estimate (an 80-call-site refactor to fully
decouple Kubernetes object identity from ZFS dataset naming, Ceph-journal style) was
overestimated. Because `ObjectMeta.Name` (Kubernetes/CSI identity) and
`Spec.Dataset`/`Spec.SnapshotName` (the actual ZFS-facing name) are *already* separate
fields in this data model, the cheap fix is to just generate independent, opaque
`csi-vol-<uuid>`/`csi-snap-<uuid>` values for the latter, once, at creation — zero
changes needed at the ~80 call sites that assume `ObjectMeta.Name` is the CSI ID. Written
up as its own, separate, independent redesign — see
[independent-resource-naming-redesign.md](independent-resource-naming-redesign.md).

---

**Q: Please document the naming redesign independently, and I want it implemented
*before* the snapshot-lifecycle redesign — it's more fundamental, and the sooner we
change it the better.**

A: Done — split into its own document
([independent-resource-naming-redesign.md](independent-resource-naming-redesign.md)),
with its own task list, and reordered the todo list so that work is explicitly a
prerequisite ahead of any snapshot-lifecycle implementation.

---

**Q: Does it really work? What if I create lots of snapshots and then delete them again
— if nothing ever got promoted (no restores happened), do the raw snapshots still exist
under the volume, and would deleting the volume without `-r` then fail?**

A: This caught a real, genuine gap. The original wording of `DeleteSnapshot`'s
"clean up the raw origin snapshot on the source" step said "best-effort" and didn't
pin down its ordering relative to destroying the backing clone. Fixed: that cleanup is
now a **required, finalizer-gated** step, and it must run **after** the backing clone is
destroyed (the backing clone is itself a dependent clone of the raw snapshot, so the raw
snapshot can't be destroyed while the backing clone still exists). A `ZfsSnapshot` object
can now only ever fully disappear once its raw snapshot has been dealt with — which is
exactly the invariant that makes it safe to also drop `-r` from `DeleteVolume` (D11). See
[§3.1 `DeleteSnapshot` steps 3-6](snapshot-lifecycle-redesign.md#31-standalone-mode-ceph-style-via-zfs-promote--new-default) and D11.

---

**Q: Is it safe to remove the backing clone using `-r`? Do we ever create other
snapshots on that backing clone, or is it always just `@restore-source`?**

A: Yes, safe, and yes — at most two: `@restore-source` (always) plus, if the *source*
volume was deleted first, the relocated raw origin snapshot (still under its original
CSI-visible name, e.g. `@snap-1`). Nothing else, because the backing clone is never
CSI-visible/mountable (`canmount=off`/`volmode=none`, no `ZfsDataset` object of its own),
so no CSI client can ever point a `CreateSnapshot`/`CreateVolume` call at it directly.
`-r` here only ever recurses over snapshots our own code created and fully accounts for
— unlike D11's reasoning for the *source* dataset, which can't make that same guarantee
about untracked/manually-created state.

---

**Q: Does this all fit the common pattern — create a volume, snapshot it, create a
volume *from* that snapshot, then delete the snapshot? Does cleanup complete, or does
this result in an error?**

A: For exactly **one** restore from the snapshot: yes, fully — traced step by step, ends
with the source volume, the backing clone gone, and the restored volume fully
independent, owning its own copy of `@restore-source`.

But tracing it surfaced two real gaps for the more general case:
1. **Orphaned relocated snapshot**: after promoting a restored PVC, the relocated
   `@restore-source` snapshot physically lands *on that PVC's own dataset*, untracked by
   any `ZfsSnapshot` CRD. Left alone, it would later make that PVC's own (non-recursive,
   per D11) `DeleteVolume` fail. Fixed: destroy it immediately after promotion, once
   confirmed dependent-free.
2. **Multiple simultaneous restores from the same snapshot**: `zfs promote` can only
   physically relocate history onto *one* dataset; promoting one of several sibling
   clones **reparents** the others onto it instead of freeing them. This part is
   verified as **intentional, documented ZFS design** (OpenZFS source,
   `dsl_dataset_promote_sync`'s explicit sibling-clone reassignment) — not a bug we rely
   on. Separately, a **real, confirmed upstream defect**
   ([openzfs/zfs#15587](https://github.com/openzfs/zfs/issues/15587)) means even a single
   promote call isn't always reliably sufficient to fully detach a dataset (a real
   downstream project loops the call, capped at 100 attempts, to work around it) — we do
   **not** rely on this defect either, we defend against it (D13: verify `origin`
   cleared, retry, hard error otherwise). Fixed the chaining case by generalizing the
   finalizer/promote-tracking mechanism itself (D12) so any dataset-to-dataset
   dependency created by this is tracked and promoted-away recursively at `DeleteVolume`
   time. Rejected the alternative of always fully duplicating ("flattening") extra
   dependents' data — real storage cost, plus live-remount risk for an actively-mounted
   PVC, for what's a comparatively rare case.

See [§2.9](snapshot-lifecycle-redesign.md#29-multiple-simultaneous-restores-from-one-snapshot--chained-dependency-problem-found-while-re-verifying-does-this-really-fit-the-pattern), D12, D13.

---

**Q: The aim is full independence — deleting anything must be possible and must never
influence others, whatever it takes. Elaborate/harden the design further for this.**

A: This is what drove the D12/D13 additions above (multi-restore chaining +
verify-and-retry promote). The `Promote`/finalizer-tracking mechanism is now generalized
to compose recursively between *any* two datasets our code creates a dependency between
(`ZfsSnapshot` → dependent, or dependent-#1 → dependent-#2 via a promote chain), so
deleting any one object — a `ZfsSnapshot`, a source volume, or any restored/cloned
volume, in any order — always either succeeds outright or promotes away exactly the
tracked dependents that are actually in the way first, never leaving one object's
deletability silently dependent on another's continued existence.

---

**Q: Does D12 rely on the sibling-clone-reparenting behavior or the multi-promote defect
being an actual ZFS bug? Never rely on a bug!**

A: No — verified this precisely by reading the actual OpenZFS source, not just the bug
tracker. Two separate things are involved, and only one is a defect:
1. **Sibling-clone reparenting** (D12 depends on this): confirmed as **intentional,
   maintained ZFS design** directly in `dsl_dataset.c`'s `dsl_dataset_promote_sync` — an
   explicit block (commented `"move any clone references"`) deliberately reassigns every
   other sibling clone to the newly-promoted dataset. Not a bug, not something we're
   exploiting — this is ZFS working as designed.
2. **Multi-call reliability** (D13 defends against this, does *not* rely on it): a real,
   confirmed upstream defect ([openzfs/zfs#15587](https://github.com/openzfs/zfs/issues/15587))
   where a single `zfs promote` call can leave a dataset still partially attached in
   complex clone-of-clone chains. `Promote` verifies `origin` actually cleared and
   retries (bounded) rather than trusting one call — if OpenZFS fixes this outright, our
   retry loop just becomes a harmless single-pass no-op, zero behavior change either way.

---

**Q: I don't know if `ZfsSnapshot` is the appropriate CRD name — it isn't an actual
snapshot in `standalone` mode. Could it be a typed CRD, like `ZfsDataset`?**

A: Keep the name and the single CRD kind (D14) — add `Mode` as a **field**, which is
already the `ZfsDataset` pattern, just under different terminology. `ZfsDataset` already
separates *structural* kind (`DatasetType`: filesystem/volume) from *provenance/mechanism*
(`DatasetSource`: plain-create vs. clone vs. restore) as orthogonal fields, not separate
CRD kinds — a cloned `ZfsDataset` isn't renamed `ZfsClone`. `ZfsSnapshot`'s planned `Mode`
(standalone/integrated) is the same category: it's about *mechanism*, not the CSI/K8s
*contract*, which is identical in both modes (a point-in-time, read-only, restorable
capture of a volume). Reinforced by Ceph-CSI's own precedent: its "snapshot" is *also* a
clone-image + self-snapshot under the hood, and Ceph doesn't rename their concept over
it. Splitting into a second CRD kind was considered and rejected (CSI code would need to
branch on kind instead of a field, listing would need merging two kinds, more RBAC/
controller duplication, for no functional gain). See
[§2.10](snapshot-lifecycle-redesign.md#210-is-zfssnapshot-still-the-right-crd-name-given-standalone-modes-implementation-raised-by-the-user-see-d14)
and D14.

---

**Q: Why do we need two separate CRDs for snapshots and datasets at all — couldn't we
just use `ZfsDataset` instead?**

A: Still no, for the same reasons as D14/ADR-0006: lifecycle mismatch (size/quota,
UID/GID/mode, `ZfsShare`/`NetworkExport` attachment, mount/publish, expand — none of
which apply to a snapshot), and CSI's own distinct `VolumeId`/`SnapshotId` RPC namespaces
(`CreateVolume`/`DeleteVolume`/`ListVolumes` vs. `CreateSnapshot`/`DeleteSnapshot`/
`ListSnapshots`, watched by two different sidecars). Merging would just reintroduce the
"inert third `type`" problem ADR-0006 already rejected once.

---

**Q: If it simplifies the architecture and is applicable, we should consider [representing
the backing clone as a real `ZfsDataset` object].**

A: Adopted as **D15**. The `standalone`-mode backing clone is now a real,
`ZfsSnapshot`-owned child `ZfsDataset` object (`ownerReference`, `blockOwnerDeletion:
true` — appropriate here, since unlike the restored-PVC relationship this *is* a strict
1:1 lifecycle-bound parent/child), rather than something `ZfsSnapshotReconciler` manages
via raw `zpool.ZFS.*` calls with its own bespoke promote/dependent-tracking loop.
Concretely this reuses `ZfsDatasetReconciler`'s existing ADR-0009 clone-creation logic for
provisioning and its own (D3/D7/D9/D12/D13) promote-then-destroy delete-path logic for
teardown — and, as a direct consequence, **unifies D4 into D12**: the `restored-by.*`
finalizer now lives on the backing-clone `ZfsDataset` (where the clone relationship
actually is) and is tracked/promoted by the exact same generalized mechanism as D12's
`promoted-onto` finalizers, instead of two similar-but-separate implementations split
across two reconcilers. `ZfsSnapshotReconciler`'s own delete path shrinks to: delete the
child object, wait for it to be gone, run the required raw-origin-snapshot cleanup, then
release its own finalizer — all the promote/retry/multi-restore-chaining complexity now
lives in exactly one place. See
[§3.1](snapshot-lifecycle-redesign.md#31-standalone-mode-ceph-style-via-zfs-promote--new-default)
and D15.

---

**Q: Would Ceph's trash approach be more robust/reliable than promote? I suspect it
leaks storage (snapshots/drifted data never cleaned up if you just rename-to-trash) —
but let's seriously reconsider every decision here, and discard promote if trash is
better.**

A: Your suspicion is correct, and it turns out to be the decisive reason to keep
`zfs promote` (D0 reaffirmed), not a minor tradeoff. A ZFS dataset with live snapshots
has no operation to free *only* its own private/unique data while leaving its snapshots
attached to it — only "refuse" or "cascade-destroy everything including the snapshots"
exist. So a literal Ceph-style trash (rename+hide, defer the real `zfs destroy -r`
until every dependent is gone) would leave the **entire** deleted volume's storage
allocated for as long as any snapshot survives — potentially indefinitely, for a
completely normal usage pattern (long-retained backup snapshots, this project's own
stated use case). `DeleteVolume` would report success while the pool silently keeps all
of that space allocated — a real, unbounded, *silent* leak, arguably worse than just
blocking the delete (at least an honest block doesn't claim space was reclaimed when it
wasn't). Ceph relies on trash because RBD has no reverse-clone-parentage primitive — not
because trash is inherently more robust; ZFS *does* have that primitive (`zfs promote`),
so we get a strictly stronger guarantee Ceph structurally can't offer: the deleted
volume's own private data is freed immediately and unconditionally, regardless of how
long its snapshots are kept. Trash would have eliminated a lot of this design's
complexity (D1/D5/D8/D12/D13/D15 all become unnecessary) — a real, quantified trade-off,
not dismissed lightly — but rejected given this project's repeated priority on full
correctness and no silent leaks. See
[§2.12](snapshot-lifecycle-redesign.md#212-re-examined-from-scratch-is-cephs-trash-approach-actually-more-robustreliable-than-zfs-promote-raised-by-the-user-re-affirms-d0)
and D0.

---

**Q: If `vol1` has six `standalone`-mode snapshots (`snap_t1`...`snap_t6`, each with its
own backing clone) and I delete `vol1`, what's the exact flow of the promotes? Doesn't
iterating over all six and promoting them mean they'd steal each other's snapshots?**

A: Yes, genuinely — and worse than my first answer said (caught and corrected after you
checked the trace by hand, D16). `zfs promote`'s history walk (`snaplist_make` in
`dsl_dataset.c`) follows each snapshot's own `ds_prev_snap_obj` chain regardless of which
dataset currently dir-owns it — so promoting `csi-snap-t6` *after* `csi-snap-t3` already
claimed `t1`/`t2`/`t3` doesn't stop at `csi-snap-t3`'s boundary: it reaches straight
through and pulls all six (`1,2,3` from wherever `t3` currently holds them, `4,5,6` from
`vol1`) — exactly the correction you made. It's not even bounded to "different clone
already claimed it" — the same source suggests even a clean, oldest-first order could
reach back through an already-independent, previously-promoted clone's snapshot later in
the same batch, which I can't fully confirm or rule out from source fragments alone.
**Mitigation, correct regardless of how that uncertainty resolves:** the promote loop
must be a bounded **fixpoint** (repeat the full pass until no dependent's `origin` flips
back to non-empty), not a single pass — costs nothing extra if the deeper concern turns
out not to apply, and correctly repairs it if it does. **Flagged as needing hands-on ZFS
verification before implementation**, not treated as fully closed. See
[§2.11](snapshot-lifecycle-redesign.md#211-does-promote-iteration-order-matter-when-a-source-has-multiple-independent-snapshots-raised-by-the-user-see-d16---corrected-after-further-review)
and D16.

---

## 2026-08-05 — re-opening the backing-clone design

> These entries follow the 2026-08-03 review and ADR-0020. They lead to the **proposal in
> [§10](snapshot-lifecycle-redesign.md)**, which is not yet decided or implemented.

**Q: I heard the current implementation is an architecture smell. Could we add a new
`ZfsSnapshotSource` CRD, reconciled by the controller, that creates the actual
`ZfsDataset`? Then the DaemonSet wouldn't need write access, we wouldn't duplicate code,
and it would match patterns we already have — or am I missing something?**

A: The smell is real, and precisely nameable: **a layering violation — a node-scoped
executor authoring control-plane objects**. The project has a strict two-tier split (the
control plane declares what should exist, node agents make it so) and
`ZfsSnapshotReconciler` authoring `ZfsDataset` objects is its only exception. Three
symptoms confirm it rather than it being aesthetic: it forced `create`/`delete` on
`zfsdatasets` into the discovery role (letting a node author objects carrying an arbitrary
`poolGUID`, and so reach another node's pool); it produced the review's only Blocker (F1);
and D26 had to be written to explain why the exception exists, which is usually the tell.

The instinct about matching existing patterns is right too — the operator already does
exactly this shape twice (`ZfsShareAttachRequest` → `ZfsShare`, `ZfsShare` →
`NetworkExport`). D26 had rejected moving authoring to the *CSI controller* and then
treated the question as closed without considering that third home. That gap became D27.

**But the proposed CRD would trade one smell for another.** `ZfsSnapshotSource` would be a
*middle man*: 1:1 with `ZfsSnapshot`, carrying the same fields, no independent lifecycle,
nothing to aggregate — unlike `ZfsShareAttachRequest`, which earns its existence by
collapsing N per-node requests into one `ZfsShare`. And it would not even fix the
privilege problem: if the *agent* creates it, a node still authors an object carrying a
`poolGUID`; if the CSI controller creates it, it is redundant with `ZfsSnapshot`, which
that controller already authors with every field needed. A misplaced responsibility is
fixed by moving the responsibility, not by inserting an indirection in front of it.

---

**Q: This all sounds very complicated and I don't get it. Isn't there a simpler solution?**

A: Yes, and the question was the right one — every hard sub-problem (who authors the
object, RBAC widening, owner references, GC, readiness signals, delete handshakes, all of
D26/D27) exists **only because the backing clone is a Kubernetes object at all**.

It became one in D15, for exactly one reason: to reuse `ZfsDatasetReconciler`'s
clone-creation and promote-then-destroy logic rather than write a second copy. That was
correct when the logic *was* methods on that reconciler.

**ADR-0020 dissolved that premise without anyone noticing.** The delete logic is now free
functions (`detachAndCleanSnapshots`, `detachSnapshotClones`, `assertKnownDatasets`,
`assertDriverSnapshot`) taking a `client.Reader` and a `zpool.ZFS` — and
`ZfsSnapshotReconciler` *already calls one of them* for D19. The shared behaviour is shared
by being a shared function, so the object is no longer what delivers the reuse, and
dropping it duplicates nothing. (The clone-creation "reuse" was always one line:
`r.ZFS.Clone(...)`.)

So the simpler answer is to stop making it an object: one reconciler, one finalizer, four
`zfs` calls, and the layering violation has nothing left to violate. Written up as §10.

Worth recording that this was the *third* design proposed in two days (D26 keep-in-agent →
D27 move-to-operator → §10 remove-the-object). The first two were not preferences that
changed; a fact changed, and neither of us spotted it until the "isn't there something
simpler" question forced a step back.

---

**Q: Scanning all known usages of `Spec.Source` is bad design — it will rot, and no other
developer will know about it. Aren't finalizers the default Kubernetes concept for this?**

A: Yes, and there are three in-tree precedents: `kubernetes.io/pvc-protection`,
`kubernetes.io/pv-protection`, and
`snapshot.storage.kubernetes.io/volumesnapshot-as-source-protection` — the last being this
exact problem one layer up.

Two clarifications worth keeping, because they cut in opposite directions:

1. **A finalizer does not remove the need to know who depends on you.** `pvc-protection`
   is itself computed by scanning Pods. What changes is the *direction*: the referencer
   declares itself at reference time, instead of the deleter guessing at delete time. That
   inversion is the real win, and it is what stops the rot.
2. **The scan is worse than "fragile" — it is incomplete by construction.** It can only
   see objects that already exist, but the dangerous window opens when
   `resolveContentSource` decides to restore, *before* the dependent object is created. A
   lease taken at that moment closes the window; a scan structurally cannot.

The distinction from the `restored-by.*` finalizers ADR-0020 removed must be stated
loudly, or someone will reintroduce the bug: those stored a **graph** — multi-edge derived
state that ZFS rewrote behind our back — and were read back to decide *what to promote*. A
lease is a **binary flag** that gates object deletion and never informs a ZFS decision.
Same mechanism, categorically different use.

---

**Q: Why should the protected resource manage the finalizers placed on it? Shouldn't that
be done by the resources that reference it? I'd expect: I want to create something from a
source, so I put a finalizer on the source, create the thing, then remove the finalizer.
Everything cleaned up — and idempotent?**

A: Correct, and this replaced an earlier, worse answer of mine ("the protected object walks
its own finalizers and reaps stale ones"). That was copying `pvc-protection`, which is a
*long-lived in-use marker*; what this actually needs is a **short-lived lease around a
critical section**.

It works because `CreateVolume` already blocks until Ready (`waitVolumeReady`), so the
whole lease lifetime fits inside one call:

```
add    in-use-by.<pvc>     ← before anything exists
create the ZfsDataset
wait   until Ready         ← the zfs clone now exists
remove in-use-by.<pvc>     ← ZFS truth covers it from here
```

Idempotent at every step — `AddFinalizer`/`RemoveFinalizer` are no-ops when already
present/absent, `ensureVolume` tolerates `AlreadyExists`, `waitVolumeReady` re-polls — so a
retried `CreateVolume` resumes wherever it died, and `external-provisioner` retries until
success.

One residual case, and it is not new: if `CreateVolume` never completes and is never
retried (PVC deleted while pending), the lease persists — but in that same scenario the
`ZfsDataset` created mid-flight also leaks, since no PV was created and `DeleteVolume` is
never called. That orphan is a pre-existing condition, and the lease is *correct* while it
exists. Cheap safety net: have `DeleteVolume` also release any lease its volume holds.

**Caveat that must not be forgotten:** extra finalizers do *not* gate our ZFS work — the
delete paths run on `deletionTimestamp` regardless of which finalizers remain. So
`beforeDestroy` and `reconcileDelete` must explicitly refuse while any `in-use-by.*` is
present, or the protection is cosmetic.

---

**Q: What is the general strategy for this across CSI drivers — how do others handle it?**

A: The CSI spec defines the contract, and it is the same for everyone. From the vendored
`spec@v1.11.0`, both `DeleteSnapshot` and `DeleteVolume` list:

> **Snapshot in use** — `9 FAILED_PRECONDITION` — "…could not be deleted because it is in
> use by another resource." Recovery: *"Caller SHOULD ensure that there are no other
> resources using the snapshot, and then retry with exponential back off."*

So: the driver detects in-use and returns `FAILED_PRECONDITION`; the CO retries with
backoff. *How* in-use is detected is left entirely to the driver — Ceph-CSI uses its OMAP
journal, democratic-csi a live check, and we would use the lease.

Three things fell out of reading that text:

- `DeleteVolume`'s condition reads "…or **has snapshots and the plugin doesn't treat them
  as independent entities**". The spec explicitly contemplates both of D8's modes, and
  `standalone` is the "independent entities" branch. Direct validation of D8.
- The spec *would* permit `DeleteSnapshot` to return `FAILED_PRECONDITION` while a lease is
  held, instead of accepting the delete and stalling internally. **Deliberately not doing
  that** — it contradicts D4's "always succeeds, never blocks", and the window is
  sub-second. Recorded so it isn't rediscovered as a novelty.
- **Asymmetry worth knowing:** a `VolumeSnapshot` can be protected by the
  snapshot-controller when installed, but a PVC used as a *clone dataSource* has no in-tree
  equivalent — `kubernetes.io/pvc-protection` only guards deletion while mounted by a pod.
  So for direct PVC-to-PVC clones our lease is the only protection that exists, which is
  why §10.5 covers both paths rather than restores alone.

---

**Q: Can we rely on the cluster's snapshot-controller to close this window for us?**

A: No. Checked against the live cluster: there are **no `snapshot.storage.k8s.io` CRDs and
no snapshot-controller deployment**, so `volumesnapshot-as-source-protection` is not
available — and in fact the entire `VolumeSnapshot` path is unusable there today, which is
also why the review's F1 RBAC blocker was never observed in practice: nothing could
exercise it.

More generally the snapshot-controller is an **optional, admin-installed** cluster
component that a CSI driver cannot assume is present. The lease is therefore the primary
protection, not a redundant second layer. (The chart is safe either way: the
`VolumeSnapshotClass` template is gated behind `range .Values.volumeSnapshotClasses` and
the sidecar behind `csiController.snapshotter.enabled`, so a missing CRD does not break
`helm install`. The prerequisite is documented in the README's host/cluster requirements.)

---

**Q: I dug into the Kubernetes specs. (1) Finalizers are meant for cleanup on deletion,
so they're the wrong tool for blocking deletion. (2) Kubernetes already protects PVCs when
you snapshot or clone from them, so we don't need to do this at all. Verify — don't
assume.**

A: Verified against primary sources. **(1) is contradicted. (2) is confirmed for the case
that matters, and it removes the need for our own protection on the restore path.**

**(1) Finalizers do block deletion, and Kubernetes uses them for exactly that.** The
official finalizers page states it directly:

> Finalizers are namespaced keys that tell Kubernetes to **wait until specific conditions
> are met before it fully deletes** resources that are marked for deletion.

> **You can also use finalizers to prevent deletion of unmanaged resources.**

> A common example of a finalizer is `kubernetes.io/pv-protection`, which **prevents
> accidental deletion** of `PersistentVolume` objects. […] If you try to delete the
> `PersistentVolume`, it enters a `Terminating` status, **but the controller can't delete
> it because the finalizer exists.**

Cleanup is the common use, not the definition. In-use protection is the *named canonical
example*.

**(2) Confirmed — and it closes the restore case entirely.** Ground truth from
external-snapshotter `pkg/utils/util.go`:

```go
// Name of finalizer on VolumeSnapshot that is used as a source to create a PVC
VolumeSnapshotAsSourceFinalizer = "snapshot.storage.kubernetes.io/volumesnapshot-as-source-protection"

// Name of finalizer on PVCs that is being used as a source to create VolumeSnapshots
PVCFinalizer = "snapshot.storage.kubernetes.io/pvc-as-source-protection"
```

```go
func NeedToAddSnapshotAsSourceFinalizer(snapshot *crdv1.VolumeSnapshot) bool {
	return snapshot.ObjectMeta.DeletionTimestamp == nil &&
		!slices.Contains(snapshot.ObjectMeta.Finalizers, VolumeSnapshotAsSourceFinalizer)
}
```

The snapshot-controller holds a finalizer on a `VolumeSnapshot` **while a PVC is being
created from it** — precisely the window §10.5's lease was designed to cover. The
Kubernetes docs describe the mirror-image protection for the other direction ("Persistent
Volume Claim as Snapshot Source Protection": deleting a PVC in active use as a snapshot
source is "postponed until the snapshot is readyToUse or aborted").

**Why the earlier caveat ("but this cluster has no snapshot-controller") does not apply.**
The snapshot CRDs and the snapshot-controller are installed together as one unit. Without
them no `VolumeSnapshot` object can exist, so `CreateSnapshot`/`DeleteSnapshot` are never
called and the driver's snapshot code never runs. **Whenever our restore path can execute
at all, the protection is necessarily present.** That is structural, not an assumption
about cluster conformance.

**Consequence:** the in-use lease and D21's scan are both unnecessary for
restore-from-snapshot. The CO closes that window before the CSI layer ever sees it.

**Still unverified: the direct PVC-to-PVC clone case.** `pvc-as-source-protection` covers
*snapshot* creation, not cloning — cloning is external-provisioner's territory, which was
not checked. The CSI Volume Cloning docs say only that the source "must be bound and
available (not in use)", and that afterwards the source "may also be modified or deleted
without affecting the newly created clone" — a statement about the completed clone, not
the in-flight window. Open question, tracked in §10.8.

> **Resolved 2026-08-06 — see the next section.** The answer is yes: clone sources *are*
> protected, by `provisioner.storage.kubernetes.io/cloning-protection`.

---

## 2026-08-06 — the full upstream verification pass

> Outcome: [lifecycle-protection-matrix.md](lifecycle-protection-matrix.md) (our flows,
> our verdicts) and [kubernetes-volume-lifecycle-facts.md](kubernetes-volume-lifecycle-facts.md)
> (upstream fact sheet). §10.5 withdrawn; §10.8 partially refuted; §10.10 added.

**Q: The previous entry left "does anything protect a PVC used as a clone source?" open.
What is the answer?**

A: **Yes, and the earlier "no" was wrong.** The protection is
`provisioner.storage.kubernetes.io/cloning-protection`, added to the source PVC by
external-provisioner during `ProvisionExt` and released by a worker pool sized by
`--cloning-protection-threads`.

The way the earlier answer was wrong is the useful part: the search stopped at in-tree
controllers. `kubernetes.io/pvc-protection` genuinely does only cover Pod mounts, so "no
protection exists" followed *correctly from an incomplete search*. **For CSI, "does
Kubernetes protect this?" is never answerable from `kubernetes/kubernetes` alone** — half
the lifecycle logic lives in sidecars, under `provisioner.storage.kubernetes.io/` and
`snapshot.storage.kubernetes.io/` prefixes that never appear in the main tree.

**Q: How much of the §10.5 lease design survived?**

A: None of it. Both windows it was built to cover are gated upstream before our code runs
— `cloning-protection` for clones, `volumesnapshot-as-source-protection` for restores.
§10.5 is withdrawn. D21's `checkPendingCloneDependents` stays *unchanged*, with its
justification narrowed to what Kubernetes genuinely cannot see: direct
`kubectl delete zfsdataset`, and the crash window where an upstream finalizer has leaked.

**Q: The lease was also justified by "the snapshot-controller is optional, so we can't
rely on it". Isn't that still true?**

A: True, and irrelevant — which is a distinction worth keeping. The snapshot CRDs and the
controller install as one unit, and external-provisioner resolves a snapshot data source by
fetching the object (`getSnapshotSource`). With no CRDs that fetch fails, so our
restore path is never invoked. **Absence of the protection and absence of the risk are the
same condition.** We do not need to assume the controller is present; we only need to
notice that when it is not, there is nothing to protect.

**Q: Did this turn up anything we did not already know?**

A: Three things, one of them a live defect.

1. **`checkSnapshotDependents` (D3) is spec-mandated, not merely prudent.** CSI v1.11.0
   `DeleteVolume`: a plugin that cannot delete a volume without affecting its snapshots
   "MUST NOT" alter the volume and "must return the `FAILED_PRECONDITION` error code".
   That is integrated mode exactly. Stronger justification than the one recorded for D3.

2. **We violate that clause today.** `DeleteVolume` is fire-and-forget — it issues the
   `Delete` and returns `OK` without reading back the outcome. The finalizer still
   protects the data, but the PV is deleted while the `ZfsDataset` blocks indefinitely,
   nothing retries (the CO was told it succeeded), and the user gets no diagnostic. Filed
   as §10.10 and §6.2 of the matrix. **Unfixed, and independent of whether §10 is
   accepted** — it is a defect in current code, not a design question.

3. **Upstream's own finalizers leak, and they shipped a reaper for it**
   (`--snapshot-orphan-sweep-interval`, default 5m, because these finalizers "can become
   stuck if the provisioner crashes"). Direct evidence that "just add a finalizer" is a
   partial solution — and our draft had only a single opportunistic release as its safety
   net. Also the first thing to check when a PVC or VolumeSnapshot hangs in `Terminating`.

**Q: Why two documents instead of one?**

A: They answer different questions and rot at different rates.
`kubernetes-volume-lifecycle-facts.md` is upstream-only and driver-agnostic — it stays
true regardless of what we build, and it is what to re-verify when bumping sidecar
versions. `lifecycle-protection-matrix.md` is the analysis: our flows, our gaps, our
verdicts, changing whenever our design does. Merged, we would have to re-audit upstream
facts every time our own code moved.

**Q: What is still unverified?**

A: Three items, all marked `[?]` in the documents and none affecting a decision taken
today: exact version boundaries for the newer external-provisioner finalizers;
cross-namespace data sources (`AnyVolumeDataSource` / `ReferenceGrant`); and whether both
snapshot-as-source finalizers are simultaneously active in the sidecar versions we deploy.

---

## 2026-08-06 — should `integrated` mode be dropped?

> Context: integrated (native) mode came first; standalone was added for CSI compliance and
> usability. Integrated was kept on the condition that it causes no problems. Full analysis
> in [§11](snapshot-lifecycle-redesign.md); the separate defect it uncovered is §12.

**Q: We have two snapshot modes. Would it be simpler to support only standalone? And if
someone used two VolumeSnapshotClasses with different modes on the same volume, would that
break everything?**

A: Mixing is constructible — `mode` is a per-`VolumeSnapshotClass` parameter and nothing
binds a volume to one mode — but it does **not** corrupt anything. What it produces is
incoherent semantics on a single volume: some snapshots make the volume undeletable
(`checkSnapshotDependents` refuses while a live integrated snapshot exists), others do not;
some restores survive a `zfs promote`, others break. And because `DeleteVolume` is
fire-and-forget (§10.10), the user is told none of it.

The stronger argument against integrated mode is not mixing, though — it is that
**integrated mode is the only reason we are on the non-compliant side of the CSI spec.**
CSI v1.11.0 requires a plugin that cannot delete a volume without affecting its snapshots
to return `FAILED_PRECONDITION`; standalone mode makes that clause inapplicable. Dropping
integrated makes `checkSnapshotDependents`' second clause dead code and closes the only gap
in the protection matrix that is genuinely ours.

**Recommendation: drop it.** It fails the condition under which it was kept, and D8 already
records that there is nothing to migrate.

**Q: Was the "mixing corrupts data" theory right?**

A: No — and it is worth recording that it was tested rather than argued. The hypothesis was
that an integrated snapshot could be silently destroyed by a promote triggered elsewhere. A
probe against the pool-verified fake showed the opposite: `assertDriverSnapshot` matches on
the snapshot **suffix** regardless of which dataset now holds it, so a live `ZfsSnapshot`
still blocks the destroy. No data loss.

The probe did, however, surface a real defect that the hypothesis had not predicted, and it
affects **standalone too** — see the next question. Had this been reasoned about instead of
executed, the wrong problem would have been fixed.

**Q: What is the defect the probe found?**

A: `ZfsSnapshot.Spec.Dataset` records where the raw snapshot was taken, but a promote can
relocate that snapshot to another dataset and nothing updates the record. The delete path
then computes a stale address, `Destroy` is `NotExist`-tolerant, so it no-ops, the finalizer
is released, and the object disappears while the real ZFS snapshot survives — holding space
with nothing referencing it.

It is bounded, not unbounded: the orphan is destroyed when whichever dataset currently holds
it is eventually deleted, since no live `ZfsSnapshot` claims that suffix any more. So the
cost is retained space and a confusing `zfs list`, not data loss or a permanent leak.
Tracked as §12, needing its own decision independent of the mode question.

**Q: Is there anything integrated mode does better?**

A: One thing, and it is cosmetic: it creates no extra datasets. Standalone carries one
`csi-snap-*` sibling per live snapshot, which is more `zfs list` noise and more surface
inside a `zfs send -R` subtree. §2.5 established the flat-sibling placement keeps `send -R`
correct, and a clone consumes no space until it diverges, so this is presentation rather
than cost — but it is the one honest argument in integrated mode's favour.

## 2026-08-08 — why does `ZfsDataset.status.fsType` exist, and does it belong there?

**Q: Why do we need `fsType` on the `ZfsDataset` level? Can't this live a level higher up —
on the source (a PV or a snapshot), since a clone always has one? And is it a status or a
spec field — if it's spec, why does nothing seem to consider it?**

A: It is **status-only** — there is no `spec.fsType` anywhere; nothing is silently ignored.
It's set once, in [`recordFSType`](../internal/csi/node.go), the first time the node plugin
actually formats/mounts a zvol, and never rewritten afterwards. It can't live any higher or
earlier, because at `CreateVolume` time (when the `ZfsDataset` and any PV/PVC are created)
the fs type simply isn't known yet — it's discovered later, at first mount, so there is no
earlier object in the chain that could hold it.

The "ask the source, one level up" instinct is already exactly how both clone paths work,
just via two different objects depending on whether the source can be expected to still
exist:

- **Direct volume→volume clone** (`spec.source.volume`): `checkCloneCompatibility`
  ([`clone.go`](../internal/csi/clone.go)) reads the **live source `ZfsDataset`'s own
  `Status.FSType`** directly — no copy needed, the source object still exists.
- **Snapshot-based restore** (`spec.source.snapshot`): a `ZfsSnapshot` is designed to
  outlive its source volume, so `CreateSnapshot` captures a copy onto
  `ZfsSnapshot.spec.sourceFSType` at snapshot-creation time (D25) as the fallback for when
  the live source is already gone by restore time — the *headline* case for a snapshot, not
  an edge case.

So the field is required (D10 has nothing to check a restore's requested `fsType` against
without it — a mismatch would otherwise pass `CreateVolume` silently and fail much later at
`NodeStageVolume`, exactly the bug `TestCreateVolume_RestoreChecksCapturedSourceFSType`
guards against) and it must stay where it is: `ZfsDataset.status.fsType` as the live
per-instance record, `ZfsSnapshot.spec.sourceFSType` as its captured-at-snapshot-time
fallback. Neither is redundant with the other. See D10/D25 above and
[api-conventions.md §5](api-conventions.md) for why `sourceFSType` is a *captured fact*
field rather than a mutable pointer.
