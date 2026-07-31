# Snapshot/Volume Independence Redesign — FAQ

A running log of questions asked during the snapshot-lifecycle/naming redesign work and
the answers/decisions that came out of them. Meant to be picked up later — by you or
anyone else — without needing to re-derive the reasoning from scratch. Each entry links
to the fuller write-up where one exists.

Related documents:
- [snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md) — the full research/decision log (D0-D16, see §2.12 for the trash-vs-promote re-examination, §2.11 for the promote-ordering proof)
- [independent-resource-naming-redesign.md](independent-resource-naming-redesign.md) — the separate, prerequisite naming redesign

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
