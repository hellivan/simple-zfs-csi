# Snapshot delete redesign: plan

Status: **plan, nothing implemented** (2026-10); model and live ZFS replay done, strategy C chosen. Companion to
[snapshot-delete-scenarios.md](snapshot-delete-scenarios.md), which holds the scenario table and the model results.
No code is written until this plan is complete and approved.

## Goals

1. No stuck deletes (scenario 13).
2. No unowned snapshot on a live volume, except snapshots that are the origin of a live clone (inherent to ZFS).
3. Live data is never damaged.
4. Everything is removed once everything is deleted.
5. Selection by labels (ZFS user properties), not by name patterns.

Out of scope (postponed): `zfs hold` as protection against manual destroys.

## Decisions

Target strategy: **D (raw-only)**, see [snapshot-delete-scenarios.md](snapshot-delete-scenarios.md) and the section at the end. C (a
`restore-source` snapshot on the backing clone) and B (tombstone backing clone with a reaper) are superseded. D was verified on a real pool
(2026-10-14, see [zfs-conformance-checklist.md](zfs-conformance-checklist.md)).

| # | Decision | Status |
|---|---|---|
| P1 | **Labels, in phase 1.** Every dataset and snapshot the driver creates carries `simple-zfs-csi:role` (`volume`, `raw`, `backing`, `clone-origin`) and `simple-zfs-csi:owner` (UID of the owning `ZfsDataset`/`ZfsSnapshot`). Delete and promote selection use these instead of name matching. Old objects are labelled by the one-time script (P7). | approved |
| P2 | **Rule: whoever creates a snapshot defers its destruction, always with `zfs destroy -d`.** The snapshot, the direct clone and the volume delete only mark what they created. A restore creates nothing and never touches the raw snapshot. | approved |
| P3 | **Snapshot delete flow (D, raw-only):** `destroy -d` the raw snapshot, promote every clone of it off whichever dataset holds it (the loop in `detachAndCleanSnapshots`), then destroy the empty backing clone. No `restore-source`/`rs-*` snapshot exists any more. No tombstones, no reaper. Verified in the model (up to 6 creations) and live on ZFS 2.4.3 (116 scenarios, 740 steps). | approved |
| P4 | **Restore finds the raw snapshot through the backing clone's `origin` property**: either the origin is the raw snapshot, or the backing clone itself holds it (it was promoted). Always verify the snapshot exists; if the backing clone is missing (creation or delete interrupted, manual change), fall back to the pool-wide scan by name. Restores clone the raw snapshot directly. | approved, origin rule derived from promote semantics, to be checked in W3 |
| P4a | **Promote loop rule:** oldest pinned snapshot first (a promote moves the origin and all older snapshots, so this moves the least and avoids name conflicts); among the clones of that snapshot prefer the backing clone, selected by label, so the raw snapshot stays on the source or its backing clone. Every W4 scenario checks "the raw snapshot is on the source or its backing clone" after every step. | approved |
| P5 | **`clone-origin` cleanup.** Deleting a direct clone marks the `@clone-<name>` snapshot it came from (found by name anywhere, since a promote may have moved it). | approved |
| P6 | **No sweeper and no reaper.** Consistent deferred deletes cover every modelled case. Existing objects are migrated instead (P7). | approved |
| P7 | **One-time migration script** (not driver code): labels old objects and runs `destroy -d` on every `restore-source` (W6a). Dry run by default, read-only audit first, explicit approval before it touches the cluster. | approved |
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

### W5. Implementation: strategy D with labels
- Flow D for snapshot delete (P3), deferred destroy everywhere for snapshots (P2), clone-origin cleanup (P5), retryable `dataset is busy` (P8), labels on create (P1).
- Restore: locate the raw snapshot via the backing clone's `origin` (P4), verify it exists, fall back to the pool-wide scan only if the backing clone is missing.
- API consequence: today the CSI layer (no ZFS access) builds `DatasetSource.Snapshot = <backing clone>@restore-source` ([clone.go](../internal/csi/clone.go)). Under D the raw snapshot's location is only known from ZFS, so the CSI layer passes a reference to the snapshot (its `ZfsSnapshot`) and the `ZfsDataset` reconciler resolves the raw location (P4) when it clones. Needs a `DatasetSource` field and CRD change.
- Group snapshots are unaffected: the group reconciler only takes the atomic `zfs snapshot` of the raws; each member then gets its own backing clone from the normal `ZfsSnapshot` path. Under D a member restore clones its raw directly, so it carries exactly the group instant (the old `restore-source` was taken a moment later).
- Promote loop (`detachAndCleanSnapshots`): oldest pinned snapshot first; among its clones prefer the one labelled `backing` for that snapshot, otherwise any; bounded rounds, re-checked after each promote.
- Revise ADR-0028, ADR-0030, ADR-0041 and D0/D19 text that mentions `detachSnapshotClones`; update `lifecycle-protection-matrix.md` and `delete-path-verification-2026-08-03.md`.
- The uncommitted prototype (`DestroySnapshotDeferred`, promote removal) is the starting point and is re-checked against W4.
- Delete logic selects by label. The allow-list only keeps its `restore-source` arm until the migration ran (W6a).

### W6a. One-time migration script (labels and `restore-source`)
- Script outside the driver (`hack/`), dry run by default. Idempotent.
- Audit (read-only): list every driver dataset and snapshot, the `ZfsDataset`/`ZfsSnapshot` that owns it, and every `@restore-source`.
- Labels each old backing clone and volume with the P1 properties (`zfs set`), which is what lets the driver drop name matching.
- Runs `zfs destroy -d` on every `@restore-source`: ZFS removes it at once, or when the last old restore that depends on it is gone (the mark survives promotes).
- No rename, and no restore-from-`restore-source` fallback in the driver.
- Run once after phase 1 is deployed and after the user approves the audit. Likely few or no objects.
- The driver keeps a single `restore-source` arm in its allow-list for one release, so an old restore that still holds a marked `restore-source` can be deleted. Removed once an audit finds none.
- The migration is verified live on a layout built the old way (a few scenarios) before it is run anywhere real. Mixed-state simulation is not needed.
- Nothing touches the production cluster without explicit approval.

### W6b. Later: remove the allow-list
- Once an audit shows no unlabelled objects and no `restore-source`, remove `driverSnapshotSuffix` and its `restore-source` arm.

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
- Gains over C: no `rs-*` name at all (scenario 13 cannot collide), no re-marking step, no rename migration (W6a only labels and marks),
  one snapshot fewer per CSI snapshot.
- Status: adopted (see Decisions).
