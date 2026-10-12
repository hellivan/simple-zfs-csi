# Snapshot delete redesign: plan

Status: **plan, nothing implemented** (2026-10). Companion to
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

| # | Decision | Status |
|---|---|---|
| P1 | **Labels** (phase 2, after deferred deletes). Every dataset and snapshot the driver creates carries `simple-zfs-csi:role` (`volume`, `raw`, `backing`, `restore-source`, `clone-origin`) and `simple-zfs-csi:owner` (UID of the owning `ZfsDataset`/`ZfsSnapshot`). Delete and sweep logic selects by these. The `driverSnapshotSuffix` allow-list stays only as a fallback for objects created before labels existed. | approved |
| P2 | **Deferred destroy everywhere.** Snapshots are removed with `zfs destroy -d`. Deleting a snapshot never promotes a restore. | approved, to be verified in the model (strategy B) |
| P3 | **Tombstone backing clone.** When a snapshot is deleted while restores still clone its `restore-source`, the backing clone `bk` stays (labelled, owner gone) and a reaper destroys it once nothing depends on it. Promote remains only when a volume or backing clone that owns snapshots must itself be destroyed (D0). | to be verified in the model |
| P4 | **Unique `restore-source` name** per backing clone for new snapshots (`restore-source-<uuid>`), fixing the scenario-13 collision. Old names remain valid. | approved |
| P5 | **`clone-origin` cleanup.** Deleting a direct clone runs `destroy -d` on the `@clone-<name>` snapshot it came from. | approved |
| P6 | **Sweeper.** Destroys labelled snapshots whose owner no longer exists and that have no clones; destroys tombstone backing clones with no snapshots. Runs periodically (reuse the Phase G resource-check interval). | approved |

## Open ZFS questions (answered only by the conformance suite on a real pool)

1. Does a promote move snapshot user properties and the defer mark with the snapshot?
2. Exact sibling re-parenting and origin inheritance on promote.
3. Exact *conflicting snapshot* error text and when ZFS raises it.
4. When ZFS auto-removes a `-d`-marked snapshot (synchronously on the last clone destroy or later).
5. Does promoting a clone whose origin is `-d`-marked behave sensibly?

## Work packages (in order)

### W1. Extend the model
- Add strategy **B** (P2 + P3 + P4 + P5 + P6) to `sim.py`; keep it outside the repo until ported.
- Re-run the 25 scenarios and the exhaustive search with a larger restore fan-out (at least 5 restores of one snapshot).
- Exit: B has no STUCK and no free residual, or the remaining cases are understood.

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
- Deferred destroy and tombstone backing clone (P2, P3), unique `restore-source` name (P4), clone-origin cleanup (P5).
- Sweeper (P6) in its name-based form, restricted to the allow-list, report-only for anything it cannot attribute.
- Revise ADR-0028, ADR-0030, ADR-0041 and D0/D19 text that mentions `detachSnapshotClones`; update
  `lifecycle-protection-matrix.md` and `delete-path-verification-2026-08-03.md`.
- The uncommitted prototype (`DestroySnapshotDeferred`, promote removal) is the starting point and is re-checked against W4.
- Needs no migration: old objects keep working because the allow-list and the old fixed `restore-source` name stay valid.

### W6. Implementation, phase 2: labels and migration
- Starts only after phase 1 is released and stable.
- Labels on create (P1). Delete and sweep logic prefers labels and falls back to the allow-list for unlabelled objects.
- Objects from 0.0.44 and earlier have no labels. One-time labelling pass: read-only audit first (list unlabelled driver
  snapshots and datasets, compare with `ZfsSnapshot`/`ZfsDataset` objects), then label with approval.
- Once everything is labelled, the allow-list fallback can be removed in a later release.
- Nothing touches the production cluster without explicit approval.

### W7. Release and verify
- Chart and image release by the user, then a live test on a throwaway namespace repeating the joplin group test, a restore, and deletes in the orders of scenarios 6, 12 and 13.

## Risks

- The model and the mock may both be wrong about real ZFS. Mitigation: W3 before W5.
- Tombstone datasets are visible in `zfs list`. Mitigation: label them and document them.
- Crashes between steps and create-vs-delete races are not modeled. Mitigation: every step idempotent and retried by the reconciler; add targeted fault-injection tests in the mock (fail after step N).

## Not decided

- Whether the sweeper deletes unlabelled legacy leftovers automatically or only reports them.
- Reaper timing for tombstones (periodic only, or also triggered on deleting the last restore).
