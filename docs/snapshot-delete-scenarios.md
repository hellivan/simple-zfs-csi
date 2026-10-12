# Snapshot / restore / clone delete scenarios

Status: **analysis, model-based** (2026-10). Nothing in this file has been confirmed on real ZFS yet.

## Why this exists

A live test of `VolumeGroupSnapshot` on the `joplin` namespace showed that deleting a
snapshot whose restore had been promoted left two `@csi-snap-…` snapshots on the *source*
volumes, with no Kubernetes object tracking them. Reasoning about individual orderings did
not scale, so this file lists the scenarios systematically and records, for each delete
strategy, whether it works.

## The model

`sim.py` models the ZFS rules the delete paths depend on:

- `zfs clone` makes a dataset that depends on a snapshot.
- `zfs promote` moves the origin snapshot **and every older snapshot** onto the clone, makes the
  former parent a clone of it, gives the promoted clone the parent's previous origin, and re-parents sibling
  clones. It fails with *conflicting snapshot* if the clone already owns a snapshot with the same name as one being moved.
- `zfs destroy snap` fails while the snapshot has clones. `zfs destroy -d snap` marks it, and ZFS
  removes it once its last clone is gone.
- `zfs destroy dataset` fails while the dataset owns snapshots.

and the driver's algorithms: backing clone `bk` per snapshot (`S@raw` -> `bk`, `bk@restore-source`), restores
cloned from `bk@restore-source`, direct clones via `V@clone-<name>`, `detachAndCleanSnapshots` for
volume and backing-clone deletes (promote what is cloned, destroy what is not).

## Strategies compared

| Name | What it does when a snapshot is deleted |
|---|---|
| **0.0.44** | Interim fix, deployed. Destroys `bk` (promoting restores off it), then promotes remaining clones of the raw snapshot and destroys it. A destroy that finds nothing counts as success. |
| **A** | Destroys `bk` the same way, then `zfs destroy -d` on the raw snapshot. No promote. |
| **A+cloneclean** | A, plus deleting a direct clone also runs `zfs destroy -d` on the `@clone-<name>` snapshot it was made from. |
| **A+cc+sweeper** | A+cloneclean, plus a sweep that destroys any driver-named snapshot that has no owner object and no clones. |
| **B** | Tombstone design, see the section at the end: deferred destroy, no promote at snapshot delete, backing clone kept until unused, unique `restore-source` name, clone-origin cleanup. No sweeper. |

## Result columns

- **clean**: every operation succeeded and, once everything was deleted, nothing was left in ZFS.
- **N residual**: at some point N snapshots existed on a still-live volume that no object owned (cleaned up
  when that volume is deleted, but they leak space and clutter for as long as it lives).
- **STUCK**: a delete fails on every retry.

## Scenario table

Notation: `P` volume, `S` snapshot, `R` restore from a snapshot, `C` direct clone. Every row ends with all
objects deleted.

| # | Steps | 0.0.44 | A | A+cloneclean | A+cc+sweeper | **B** |
|---|---|---|---|---|---|---|
| **Single volume + snapshot** | | | | | | |
| 1 | create P, snapshot S of P, delete S, delete P | clean | clean | clean | clean | clean |
| 2 | create P, snapshot S of P, delete P, delete S | clean | clean | clean | clean | clean |
| **Direct PVC clone** | | | | | | |
| 3 | create P, clone C of P, delete C, delete P | clean, 1 residual | clean, 1 residual | clean | clean | clean |
| 4 | create P, clone C of P, delete P, delete C | clean | clean | clean | clean | clean |
| **Restore from snapshot (3 objects, all 6 orders)** | | | | | | |
| 5 | create P, snapshot S of P, restore R of S, delete R, delete S, delete P | clean | clean | clean | clean | clean |
| 6 | create P, snapshot S of P, restore R of S, delete S, delete R, delete P | clean, 3 residual | clean, 1 residual | clean, 1 residual | clean | clean |
| 7 | create P, snapshot S of P, restore R of S, delete S, delete P, delete R | clean, 2 residual | clean, 1 residual | clean, 1 residual | clean | clean |
| 8 | create P, snapshot S of P, restore R of S, delete P, delete S, delete R | clean, 1 residual | clean, 1 residual | clean, 1 residual | clean | clean |
| 9 | create P, snapshot S of P, restore R of S, delete P, delete R, delete S | clean | clean | clean | clean | clean |
| 9b | create P, snapshot S of P, restore R of S, delete R, delete P, delete S | clean | clean | clean | clean | clean |
| **Several snapshots / restores** | | | | | | |
| 10 | create P, snapshot S1 of P, snapshot S2 of P, delete S1, delete S2, delete P | clean | clean | clean | clean | clean |
| 10b | create P, snapshot S1 of P, snapshot S2 of P, delete P, delete S2, delete S1 | clean | clean | clean | clean | clean |
| 11 | create P, snapshot S1 of P, snapshot S2 of P, restore R1 of S1, restore R2 of S2, delete S1, delete S2, delete R1, delete R2, delete P | clean, 6 residual | clean, 2 residual | clean, 2 residual | clean | clean |
| 12 | create P, snapshot S of P, restore R1 of S, restore R2 of S, delete S, delete R1, delete R2, delete P | clean, 4 residual | clean, 2 residual | clean, 2 residual | clean, 1 residual | clean |
| 12b | create P, snapshot S of P, restore R1 of S, restore R2 of S, delete P, delete S, delete R2, delete R1 | clean, 1 residual | clean, 1 residual | clean, 1 residual | clean, 1 residual | clean |
| **Chains (restore, then snapshot of the restore)** | | | | | | |
| 13 | create P, snapshot S of P, restore R of S, snapshot S2 of R, delete R, delete S, delete S2, delete P | **STUCK** | **STUCK** | **STUCK** | **STUCK** | clean |
| 14 | create P, snapshot S of P, restore R of S, snapshot S2 of R, delete R, delete S2, delete S, delete P | clean | clean | clean | clean | clean |
| 14b | create P, snapshot S of P, restore R of S, snapshot S2 of R, delete S, delete R, delete S2, delete P | clean, 3 residual | clean, 1 residual | clean, 1 residual | clean | clean |
| 15 | create P, clone C of P, snapshot S of C, delete C, delete S, delete P | clean, 1 residual | clean, 1 residual | clean | clean | clean |
| 16 | create P, snapshot S of P, restore R of S, clone C of R, delete R, delete C, delete S, delete P | clean | clean | clean | clean | clean |
| 17 | create P, snapshot S of P, restore R of S, delete P, snapshot S2 of R, delete S, delete R, delete S2 | clean, 1 residual | clean, 1 residual | clean, 1 residual | clean | clean |
| **Restore, source deleted, then a snapshot of the restore** | | | | | | |
| 18 | create P, snapshot S of P, restore R of S, delete P, delete S, snapshot S2 of R, delete S2, delete R | clean, 1 residual | clean, 1 residual | clean, 1 residual | clean | clean |
| 18b | same, but delete R before S2 | clean, 1 residual | clean, 1 residual | clean, 1 residual | clean | clean |
| 19 | create P, snapshot S of P, restore R of S, delete S, delete P, snapshot S2 of R, delete S2, delete R | clean, 2 residual | clean, 1 residual | clean, 1 residual | clean | clean |
| 19b | same, but delete R before S2 | clean, 2 residual | clean, 1 residual | clean, 1 residual | clean | clean |

Rows 18 to 19b: a snapshot of a restore does **not** release a deferred snapshot the restore was cloned from.
In B the tombstone `bk` with its marked `rs-<id>` stays until R is deleted, whether or not R has its own snapshot.

## Exhaustive search

All legal operation sequences with up to 6 creations (volume, snapshot, restore, clone in any order), each
followed by every delete order (about 140 000 distinct states per strategy):

| Check | 0.0.44 | A | A+cloneclean | A+cc+sweeper |
|---|---|---|---|---|
| Objects left after **everything** is deleted | none | none | none | none |
| Live volume or snapshot damaged | never | never | never | never |
| States with a residual on a live volume (free) | 495 666 | 414 730 | 103 169 | 0 |
| States with a residual on a live volume (pinned by a clone) | 185 720 | 122 416 | 24 404 | 22 245 |
| States where a delete is **STUCK** | 2 153 | 1 750 | 1 524 | 1 332 |

## Findings

1. **No permanent leak.** Every residual disappears when the volume that carries it is deleted. The problem
   is volumes that live long (databases), which carry them for as long as they live.
2. **The ghosts seen in the live test were caused by the promote at snapshot delete.** The original volume
   became a clone of the restore and the raw snapshot ended up on whichever dataset held it.
3. **Deferred destroy (A) is not enough by itself.** It cuts the residuals but two sources remain:
   `@clone-<name>` left on a source after a direct clone is deleted (scenario 3), and `@restore-source` left on a
   restore after its backing clone was destroyed (scenarios 6 to 8, 11, 12).
4. **Stuck delete independent of the above (scenario 13).** The fixed name `restore-source` collides when
   promoting a backing clone that already owns one: restore R from S, snapshot R (as S2), delete R, delete S.
   The promote fails with *conflicting snapshot restore-source* on every retry, under every strategy,
   including the deployed one. Needs its own fix (for example a unique name per backing clone).

## Not covered by the model

- Crash between the steps of a promote or between the driver's steps.
- Creating a snapshot while its source is being deleted.
- `Lost` snapshots and snapshots whose objects were deleted by hand.
- Group snapshots as a unit (a group delete is a series of single snapshot deletes, so it is covered by
  the single-snapshot rows).
- Real-ZFS behaviour of: the exact promote semantics (sibling re-parenting, origin inheritance), automatic removal of a
  `-d`-marked snapshot, whether the mark moves with a promote, and the *conflicting snapshot* error.

## Plan to turn this into tests

1. **Replay on real ZFS** (file-backed scratch pool on a node, never the production pool): run the same
   scenarios as shell, compare the result of every step with the model. Any difference fixes the model first.
2. **Port the scenarios to Go tests.** `fakeZFS` in `internal/controller` already models clone and promote;
   extend it with `destroy -d`, the *conflicting snapshot* rule and deferred auto-removal, then drive the real
   reconcilers through each scenario row and assert: no stuck delete, nothing left when everything is deleted,
   live volumes and snapshots intact, and no unowned snapshot on a live volume.
3. **Property test.** Generate the operation sequences as the model does (bounded, small) so new orderings
   are covered without writing rows by hand.

## Strategy B (tombstone backing clone + unique `restore-source` names)

Added after the first review (2026-10). Model only, same caveats as above.

- **B** = deferred destroy everywhere, no promote when a snapshot is deleted, the backing clone stays as a hidden
  tombstone until nothing depends on it (a reaper destroys it), a unique `rs-<snapshot id>` name per backing clone, and
  cleanup of the `@clone-<name>` snapshot when a direct clone is deleted (found by name anywhere, because a promote may
  have moved it). The reaper repeats until nothing more can be removed.
- Exhaustive search, all legal sequences with up to **6 creations** (about 75 000 states):
  no stuck delete (so scenario 13 is fixed), no object left after everything is deleted, no live volume or snapshot damaged,
  **no residual snapshot on a live volume at all**, including scenarios 12 and 12b. B needs no sweeper in the model.
- Fan-out: one volume and one snapshot with 5 and with 6 restores, every delete order (5 040 and 40 320 orders):
  B has no stuck delete, no leak and no residual. The deferred-only variant with sweeper (A+cc+sweeper+unique name)
  has no stuck delete either but shows the pinned residual from scenario 12 in many orders.
- The unique name is required: without it the model loses restorability (a delete removes the wrong `restore-source`).
- The cost of B is the hidden tombstone dataset, which stays while restores of a deleted snapshot are alive.
- B relies on one unverified ZFS behaviour: that the `-d` mark moves together with the snapshot on a promote. The
  conformance suite (plan W3) must confirm it before implementation.
