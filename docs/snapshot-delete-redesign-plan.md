# Snapshot delete redesign: plan

Status: **plan, nothing implemented** (2026-10); model and live ZFS replay done, strategy C chosen. Companion to
[snapshot-delete-scenarios.md](snapshot-delete-scenarios.md), which holds the scenario table and the model results.
No code is written until this plan is complete and approved.

## Goals

1. No stuck deletes (scenario 13).
2. No unowned snapshot on a live volume, except snapshots that are the origin of a live clone (inherent to ZFS).
3. Live data is never damaged.
4. Everything is removed once everything is deleted.
5. Selection by labels (ZFS user properties), not by name patterns (phase 2).

Out of scope (postponed): `zfs hold` as protection against manual destroys.

## Decisions

Target strategy: **C** (see [snapshot-delete-scenarios.md](snapshot-delete-scenarios.md)). B (tombstone backing clone with
a reaper) is only a fallback if C fails; C has been verified on a real pool (2026-10-13, see
[zfs-conformance-checklist.md](zfs-conformance-checklist.md)).

| # | Decision | Status |
|---|---|---|
| P1 | **Labels** (phase 2, after deferred deletes). Every dataset and snapshot the driver creates carries `simple-zfs-csi:role` (`volume`, `raw`, `backing`, `restore-source`, `clone-origin`) and `simple-zfs-csi:owner` (UID of the owning `ZfsDataset`/`ZfsSnapshot`). Delete logic selects by these. The `driverSnapshotSuffix` allow-list stays only as a fallback for objects created before labels existed. | approved |
| P2 | **Rule: whoever creates a snapshot defers its destruction, always with `zfs destroy -d`.** The snapshot, the direct clone and the volume delete only mark what they created. A restore creates nothing and never touches `rs-<id>`. | approved |
| P3 | **Snapshot delete flow (C):** mark `P@raw` and `bk@rs-<id>` with `destroy -d`, promote every restore off `bk` (the loop in `detachAndCleanSnapshots`), destroy the now empty `bk`, mark the `rs-<id>` that moved onto the restore again. No tombstone datasets, no reaper. | approved, verified on ZFS 2.4.3 |
| P4 | **Unique `rs-<id>` name** per backing clone for new snapshots, fixing the scenario-13 collision. | approved |
| P5 | **`clone-origin` cleanup.** Deleting a direct clone marks the `@clone-<name>` snapshot it came from (found by name anywhere, since a promote may have moved it). | approved |
| P6 | **No sweeper and no reaper.** Consistent deferred deletes cover every modelled case. Existing objects are migrated instead (P7). | approved |
| P7 | **One-time migration script** (not driver code) renames the fixed `restore-source` of existing backing clones to `rs-<id>` (`zfs rename`, verified to work while restores depend on it). Dry run by default, read-only audit first, explicit approval before it touches the cluster. Likely few or no such snapshots. Labelling of old objects is part of phase 2. | approved |
| P8 | **`dataset is busy` is retryable.** A zvol destroyed right after create fails with it about 40 percent of the time and succeeds a second later. | found 2026-10-13 |

## Open ZFS questions

Answered on 2026-10-13, see the results log in [zfs-conformance-checklist.md](zfs-conformance-checklist.md). Still open:
a process holding a mountpoint open during promote or destroy, crash and race behaviour, and deeper #15587 chains.

## Work packages (in order)

### W1. Extend the model
- Add the strategies B and C to the model; keep it outside the repo until ported.
- Re-run the 25 scenarios and the exhaustive search with a larger restore fan-out (at least 5 restores of one snapshot).
- Exit: B has no STUCK and no free residual, or the remaining cases are understood.
- **Done (2026-10):** B has no stuck delete, leak, damage or residual up to 6 creations and in 5/6-restore fan-out; see
  [snapshot-delete-scenarios.md](snapshot-delete-scenarios.md). Still depends on the unverified defer-mark-moves-on-promote behaviour (W3).

### W2. In-memory ZFS (test-support package)
- One state machine implementing `zpool.ZFS`. State: datasets (properties, kind), snapshots (properties, defer mark,
  creation order), origin per clone.
- Every method reads or mutates this one state: `List`, `Get` (`origin`, `clones`, user properties), `ListSnapshots`,
  `Clones`, `FindSnapshot` are views of it.
- Rules enforced in the mutation with ZFS-style errors: destroy with clones, destroy dataset with snapshots, promote
  conflict, `-d` auto-removal, the multi-call promote quirk (openzfs#15587).
- Invariants checked after every step: no dangling origin, unique snapshot names per dataset, clone origins exist.
- Replaces `fakeZFS` in the controller tests.

### W3. Conformance suite
- Scenario scripts written once, run against the in-memory ZFS (always) and the real `zfs` CLI on a file-backed scratch
  pool (build tag or env var, never the production pool).
- Compare state after every step. A difference means the mock is wrong; fix the mock and the doc.
- Answers the open ZFS questions above.

### W4. Scenario tests
- One test per row of the scenario table, driving the real reconcilers on the in-memory ZFS.
- Assertions: no stuck delete, nothing left at the end, live volumes and snapshots intact, no unowned snapshot on a live volume.
- Bounded property test generating operation sequences as the model does.
- Include fan-out (several restores of one snapshot) and group snapshots (a group delete is a series of single deletes).

### W5. Implementation, phase 1: deferred deletes (no labels)
- Ships first and works with the existing names and the `driverSnapshotSuffix` allow-list.
- Flow C for snapshot delete (P3), deferred destroy everywhere for snapshots (P2), unique `rs-<id>` name for new snapshots (P4),
  clone-origin cleanup (P5), retryable `dataset is busy` (P8).
- Revise ADR-0028, ADR-0030, ADR-0041 and D0/D19 text that mentions `detachSnapshotClones`; update
  `lifecycle-protection-matrix.md` and `delete-path-verification-2026-08-03.md`.
- The uncommitted prototype (`DestroySnapshotDeferred`, promote removal) is the starting point and is re-checked against W4.
- The delete path keeps recognising the old fixed `restore-source` name, so old objects keep working until the script (W6a) ran.

### W6a. One-time migration script: rename old `restore-source`
- Script outside the driver (`hack/`), dry run by default.
- Audit: list every `@restore-source` snapshot on the pool, the backing clone it belongs to and the `ZfsSnapshot` that owns it.
- Rename each to `rs-<snapshot id>` with `zfs rename`; abort on `dataset already exists`. Idempotent.
- Run once after phase 1 is deployed and after the user approves the audit. Likely few or no objects.

### W6b. Implementation, phase 2: labels and migration
- Starts only after phase 1 is released and stable.
- Labels on create (P1). Delete logic prefers labels and falls back to the allow-list for unlabelled objects.
- Objects from 0.0.44 and earlier have no labels. One-time labelling pass: read-only audit first (list unlabelled driver
  snapshots and datasets, compare with `ZfsSnapshot`/`ZfsDataset` objects), then label with approval.
- Once everything is labelled, the allow-list fallback can be removed in a later release.
- Nothing touches the production cluster without explicit approval.

### W7. Release and verify
- Chart and image release by the user, then a live test on a throwaway namespace repeating the joplin group test, a restore, and deletes in the orders of scenarios 6, 12 and 13.

## Risks

- The model and the mock may both be wrong about real ZFS. Mitigation: the conformance suite (W3) runs the same scenarios on the
  mock and on a real pool; a full replay of 96 scenarios already matched.
- Crashes between steps and create-vs-delete races are not modeled. Mitigation: every step idempotent and retried by the
  reconciler (the finalizer stays until the flow completes); add fault-injection tests in the mock (fail after step N).
- A process holding a mountpoint open during promote/destroy is untested.

## Strategy D (raw-only): candidate, verified 2026-10-14

Idea (user): drop `rs-<id>`/`restore-source`. The backing clone `csi-snap-<uuid>` stays, but only as a snapshot-less vessel for a
promote. Restores clone the raw snapshot `csi-snap-<uuid>` directly, wherever it currently lives (the source dataset or the
backing clone; one lookup by the unique name). Flow on delete: `destroy -d` the raw, promote the restores off whichever dataset
holds it (the loop in `detachAndCleanSnapshots`), destroy the empty vessel.

- Model: exhaustive search, up to 6 creations (about 62k states), no STUCK, LEAK, INTEGRITY, residual or ghost-dataset result,
  same as C. In every reachable state the raw is either in the source or in its own vessel, never elsewhere.
- Live (ZFS 2.4.3, `spinning-archive/verify_zfs_functions`): replay of 116 scenarios (29 table rows on filesystems and zvols,
  fan-out, 60 random sequences), 740 steps, matched the model after every step. The only differences are the known transient
  zvol `dataset is busy` on destroy (P8).
- Gains over C: no `rs-*` name at all (scenario 13 cannot collide), no re-marking step, no rename migration (W6a disappears),
  one snapshot fewer per CSI snapshot.
- Status: proposal, not yet adopted.
