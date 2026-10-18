# Snapshot delete redesign: plan

Status: **plan complete, nothing implemented** (2026-10-15); model and live ZFS replay done, strategy D chosen. Companion to
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
| P4 | **Restore finds the raw snapshot through the backing clone's `origin` property**: either the origin is the raw snapshot, or the backing clone itself holds it (it was promoted). Always verify the snapshot exists. No pool scan: a promote only moves a snapshot to a clone of it, so it is the backing clone's origin, or a snapshot of the backing clone (when that was promoted; its origin is then unrelated), or, with no backing clone (interrupted create, manual change), still on the source dataset. Restores clone the raw snapshot directly. | approved, origin rule derived from promote semantics, to be checked in W3 |
| P4a | **Promote loop rule:** oldest pinned snapshot first (a promote moves the origin and all older snapshots, so this moves the least and avoids name conflicts); among the clones of that snapshot prefer the backing clone, selected by label, so the raw snapshot stays on the source or its backing clone. Every W4 scenario checks "the raw snapshot is on the source or its backing clone" after every step. | approved |
| P5 | **`clone-origin` cleanup.** Deleting a direct clone marks the `@clone-<name>` snapshot it came from, as the very first step, before any promote: then it is the volume's origin or a snapshot of the volume. (Reading the origin after the promote loop is wrong: found by the lifecycle test, clone of a clone.) No scan. | approved |
| P6 | **No sweeper and no reaper.** Consistent deferred deletes cover every modelled case. Existing objects are migrated instead (P7). | approved |
| P7 | **One-time migration script** (not driver code): labels old objects and runs `destroy -d` on every `restore-source` (W6a). Dry run by default, read-only audit first, explicit approval before it touches the cluster. | approved |
| P8 | **`dataset is busy` is retryable.** A zvol destroyed right after create fails with it about 40 percent of the time and succeeds a second later. | found 2026-10-13 |

## Open ZFS questions

Answered on 2026-10-13, see the results log in [zfs-conformance-checklist.md](zfs-conformance-checklist.md). Still open:
a process holding a mountpoint open during promote or destroy, crash and race behaviour, and deeper #15587 chains.

## Implementation plan (in order)

Rule for every step: tests first or together with the code, one commit per step, nothing touches the production cluster or
pool without explicit approval, live ZFS tests only under `spinning-archive/verify_zfs_functions`, the user pushes and releases.

### Done
- **S0. Model and live verification.** Scenario table, exhaustive model search, live replay of 116 scenarios for D
  (and 96 for C), checklist in [zfs-conformance-checklist.md](zfs-conformance-checklist.md). Session tooling lives outside
  the repo (`sim2.py`, `sim3.py`, `replay*.py`); the Go suite in S2 replaces it.

### S1. Labels (P1) — DONE (`2b8d429`)
No behaviour change; ships first. Verified live: `-o` on create/clone, labels survive `destroy -d` and promote, snapshots
inherit the dataset's user properties when read (see `labels.md`).
- `internal/zpool/zfs.go`: add to the `ZFS` interface and the CLI the calls to set and read user properties on datasets and
  snapshots (`zfs set simple-zfs-csi:role=...`, `zfs get -H -o value`, and a list call that returns chosen properties,
  e.g. `zfs list -o name,simple-zfs-csi:role`). Verified live: user properties work on snapshots, are not inherited by clones,
  and move with a promoted snapshot.
- Label on create, all four places: volume dataset (`volume`), raw snapshot (`raw`), backing clone (`backing`), direct-clone
  origin snapshot (`clone-origin`). `simple-zfs-csi:owner` is the UID of the owning `ZfsDataset`/`ZfsSnapshot`; a
  restore-from-snapshot volume is a `volume`. Properties are set at create time where possible (`zfs create -o`,
  `zfs clone -o`), and for snapshots right after `zfs snapshot` (idempotent; only while the object is being provisioned, see S6).
- Tests on the existing fake first; the same tests move to the in-memory ZFS in S2.
- Files: `internal/zpool/zfs.go`, `zfsdataset_controller.go`, `zfssnapshot_controller.go`, `zfsgroupsnapshot_controller.go`.

### S2. In-memory ZFS and conformance suite (W2, W3) — mostly DONE
Done: `internal/zpool/memzfs` (the model, `Check()` invariants, error injection, `BusyAfterZvolCreate`),
`internal/zpool/zfsconf` (41 cases: 21 each for filesystem and zvol, shared by mock and real pool, includes the P4 origin rule,
label inheritance and promote semantics), `DestroySnapshotDeferred` on the `ZFS` interface and the CLI. The suite passes on the
model (`go test ./internal/zpool/memzfs`) and on the real pool (`-tags zfsreal`, see `real_test.go`), ZFS 2.4.3, 2026-10-17.
Still open: port the 116 whole-scenario replays as driver tests (they need the S4 driver, so they land there), and replace
`fakeZFS` in the controller tests (done with S4, when those tests are rewritten; `fakeZFS` has a stub for the new method).
Original scope:
- Test-support package implementing `zpool.ZFS` as one state machine: datasets (kind, properties), snapshots (properties, defer
  mark, creation order), origin per clone. All reads (`List`, `Get`, `Clones`, `ListSnapshots`, `FindSnapshot`) are views of it.
- Enforces with exact ZFS error texts: destroy with clones or snapshots, `-d` semantics and auto-removal when the last clone
  goes, promote (moves origin and older snapshots, mark and properties move, conflict error), rename, injectable
  `dataset is busy` (P8), user properties.
- Invariants checked after every step: no dangling origin, unique snapshot names per dataset, clone origins exist, the raw
  snapshot is on its source or its backing clone.
- Conformance suite: scenario scripts run on the mock always and on a real pool behind a build tag, only under the test dataset.
  The 116 scenarios already verified live are ported as the first cases. Also checks the origin rule (P4) against real ZFS.
- Replaces `fakeZFS` in the controller tests.

### S3. Source references (API change) — DONE
Implemented: `DatasetSource` gained `zfsSnapshotRef` / `zfsDatasetRef` (`ObjectRef{name}`), CEL "exactly one of the four", CRD
regenerated. The CSI controller now sets the references (no backing-clone path, no `restore-source` name). The `ZfsDataset`
reconciler resolves them: snapshot ref via `locateRawSnapshot` (backing clone's `origin`, or on the backing clone, or on the source; verified, no pool scan),
dataset ref via `@clone-<name>`; a missing, terminating or not-Ready source gives `SourceNotReady` and a 10 s retry, never another
source; a pool mismatch fails hard. RBAC needed no change (the agent role already reads `zfssnapshots` and `zfsdatasets`).
Verified on the cluster's API server with a throwaway CRD (deleted again): the rule accepts each single field and rejects two
fields, none and an empty name. Tests: `internal/controller/source_ref_test.go` (on the in-memory ZFS, including "the origin was
enough, no scan" and "no scan when the backing clone was promoted"), `internal/csi/clone_test.go`.
`spec.source` is now enforced immutable by a CEL rule on `ZfsDatasetSpec` (cannot be changed, set or cleared; verified on the API server with a throwaway CRD, unrelated spec updates still pass); see api-conventions §5.
Original scope:
Needed because under D the raw snapshot has no fixed path (see P4). The CSI layer must pass intent, not a path.
- `DatasetSource` keeps the two path fields (`snapshot`, `volume`: create from a given ZFS path, still supported and useful
  for manual or tooling use) and gains two references: `zfsSnapshotRef: {name}` (a `ZfsSnapshot`) and
  `zfsDatasetRef: {name}` (a `ZfsDataset`). The names avoid `volumeRef`, which would clash with `spec.volume` (zvol options).
- The three options are mutually exclusive: path (`snapshot` or `volume`), `zfsSnapshotRef`, `zfsDatasetRef`. A CEL rule on
  `DatasetSource` allows exactly one of the four fields; `source` stays immutable.
- CSI (`internal/csi/clone.go`): fills the references, keeps the existing checks (same pool, type compatible, same prefix,
  snapshot not terminating and Ready). Stops deriving the backing-clone path.
- `ZfsDataset` reconciler: resolves a reference on the node. Snapshot ref: read the `ZfsSnapshot`, find the raw snapshot
  (P4, origin lookup, no scan), `zfs clone` it. Dataset ref: read the `ZfsDataset`, snapshot it as `@clone-<name>`
  and clone. A path source behaves as today. A missing or terminating referenced object means wait or report, never clone
  something else.
- RBAC: check that the role running the `ZfsDataset` reconciler can `get` `zfssnapshots` and `zfsdatasets`; update chart roles.
- CRD, generated code and chart CRDs regenerated. Existing objects with path sources keep working unchanged.
- Old backing clones (with `restore-source`) restore fine through the new path: the raw snapshot is still pinned by the backing clone.

### S4. Strategy D in the driver (W4, W5) — DONE
Implemented: the snapshot delete is `destroy -d` on the raw snapshot (found via the backing clone's origin or the backing clone itself),
then the shared promote loop empties the backing clone, then the clone is destroyed. The loop picks the oldest cloned snapshot
first and prefers a clone labelled `backing`; every snapshot destroy is `-d`. A direct clone's delete marks its `@clone-<name>`
snapshot (first step, before any promote). Nothing scans the pool. No `restore-source` is created or read, and the driver contains no logic for the old layout: the cluster is migrated (S6) before the new driver runs, and the delete allow-list is only `clone-*` and `csi-snap-*`, so a leftover `restore-source` would block a delete loudly. A settled snapshot is Lost when its raw snapshot cannot be found.
Tests, driving the real reconcilers on `memzfs` (`internal/controller/lifecycle_test.go`): every sequence of up to 3 creations
(131; `LIFECYCLE_MAX_CREATES=4` gives 4705, about 80 s, run once green), 150 random walks of up to 7 creations, named scenarios
(restore then delete snapshot pins nothing, delete source first, fan-out, chains, direct clones), and a single failing ZFS call (`destroy`, `destroy -d`, `promote`, including P8 "dataset is
busy") at every dataset and snapshot of a populated pool. After every step: model invariants, no live volume without dataset, every
live snapshot restorable, no unowned dataset or unmarked unowned snapshot; at the end the pool is empty. Mutation-checked
(dropping the deferred destroy or the clone-origin cleanup fails them). `fakeZFS` is kept for the narrow unit tests only.
Failure policy: a delete must be resumable. Not finding the origin or clone-origin snapshot of a clone means an earlier attempt
already got past that step (the promote loop moved or destroyed it), so the delete goes on. The loud failure is the destroy of the
clone itself, which ZFS refuses while the clone still has snapshots or dependent clones (after a clean cleanup that never happens;
if it does, something went wrong and the object stays Terminating with `DeleteBlocked`). Residual risk, accepted: if someone changes
the clone graph from outside the driver before the first attempt, the snapshot is not found and stays behind unmarked.
Deliberately not done (future improvement, benefit unproven): label-based selection in the delete path. It still uses the name allow-list and positional lookup; labels select only the promote preference.
Original scope:
- Scenario tests (W4): one test per row of the scenario table (25 rows plus 18 to 19b), fan-out, group snapshots, and a
  bounded property test, driving the real reconcilers on the in-memory ZFS. Assertions: no stuck delete, nothing left at the
  end, live volumes and snapshots intact, no unowned snapshot on a live volume, invariants after every step.
- Snapshot create: raw snapshot, backing clone (labelled), no `restore-source`.
- Restore: resolve the raw snapshot via the backing clone's `origin` (P4), verify it; no scan.
- Snapshot delete (P3): `destroy -d` the raw, promote clones of it off whichever dataset holds it, destroy the empty backing clone.
- Promote loop `detachAndCleanSnapshots` (P4a): oldest pinned snapshot first; among its clones prefer the one labelled
  `backing`; bounded rounds, re-check after each promote. Every snapshot destroy is `-d`.
- Volume delete: same loop; a direct clone marks its `@clone-<name>` snapshot (P5, found by label or name anywhere).
- `dataset is busy` is retryable (P8).
- The uncommitted prototype (`DestroySnapshotDeferred`, `detachSnapshotClones` removed) is the starting point and is
  re-checked against these tests before it is committed.
- Delete logic selects by label. The allow-list (`driverSnapshotSuffix`) has no `restore-source` arm: the migration (S6) must have removed every `restore-source` before the new driver runs.
- Remove the fixed `restore-source` creation and every code path that reads it (`restoreSourcePresent`, the self-snapshot in `reconcileBackingClone`).

### S5. Documentation (DONE: ADR-0042, supersession notes on ADR-0020/0028/0030/0041 and on the historical docs, progress log)
- Revise ADR-0028, ADR-0030, ADR-0041, ADR-0009 (source references) and D0/D5/D15/D19 text that mentions `detachSnapshotClones`
  or `restore-source`; add an ADR for strategy D. Update `lifecycle-protection-matrix.md`,
  `delete-path-verification-2026-08-03.md`, `IMPLEMENTATION_PROGRESS.md`, and the scenario doc (D column).

### S6. One-time migration script (P7)
**Status:** implemented as `cmd/zfs-migrate` (`internal/migrate`). Dry run by default, `--apply` to change; it uses the current kube context and runs zfs in the toolbox pod. Verified: unit tests on memzfs; a read-only dry run on the real cluster (617 actions: 265 objects labelled, 87 `restore-source` marked, no findings); `--apply` on an old-style layout built under `verify_zfs_functions` (`go test -tags zfsreal ./internal/migrate`, guarded to that root, never contacts Kubernetes). Not yet run with `--apply` on the cluster: needs the user's approval.

Why: from S4 on the driver selects objects by label (see `labels.md`). Objects created before S1 (and any created by a
pre-S1 driver while the new one rolls out) carry no labels. Labels are not optional for correctness: an unlabelled raw
snapshot or backing clone must never be misclassified.

The driver does not repair anything (ADR-0038): once an object is provisioned (`provisionedAt`) its reconciler creates and
repairs nothing and never touches ZFS again. Labels are written only at creation (and while an object is still being
provisioned). **The migration script therefore labels every pre-S1 object: volumes, raw snapshots, backing clones and
clone-origin snapshots.** S4's label-based delete may only be released once its audit is clean.

The script also covers orphans (driver-named ZFS objects without a CR), `restore-source` snapshots and `@clone-*` snapshots
whose `ZfsDataset` is gone.

Properties:
- Outside the driver, dry run by default (`--apply` to act), idempotent, only touches objects under the configured prefix.
- Never guesses: an object is labelled only when ownership is derivable from a CR (below). Everything else is *reported*, not changed.

Derivation of labels (CR to object):

| Object | Found via | role | owner |
|---|---|---|---|
| volume dataset/zvol | `ZfsDataset` status/spec dataset path | `volume` | CR UID |
| raw snapshot | `ZfsSnapshot` `spec.dataset@spec.snapshotName` (if promoted: the backing clone's origin, or a snapshot of the backing clone; no scan) | `raw` | CR UID |
| backing clone | `ZfsSnapshot` name to `<parent>/csi-snap-<uuid>` of the raw snapshot | `backing` | CR UID |
| `@clone-<name>` snapshot | `ZfsDataset <name>` exists | `clone-origin` | CR UID |
| `@restore-source` | (old layout) | not labelled; `zfs destroy -d` (removed at once, or when the last old restore depending on it is gone; the mark survives promotes) | |

Steps:
1. Audit (read-only): list all driver datasets and snapshots with `zfs list -t all -o name,origin,clones,simple-zfs-csi:role,simple-zfs-csi:owner`
   and all CRs; produce: labelled OK, to-label (CR derivable), orphans (no CR), `restore-source` list, conflicts (existing label
   differs from derived one; inherited snapshot values must be told from local ones with `zfs get -s local`).
2. Dry-run prints the exact `zfs set` / `zfs destroy -d` commands.
3. `--apply` runs them; re-audit must report zero to-label, zero `restore-source`. Orphans stay a human decision.
4. Verified live first on a layout built the old way (volume, snapshot, restore, clone, group) in `verify_zfs_functions`; run on the
   cluster only after the user approves the audit output.
5. Ordering with the release: deploy the S1 driver first (new objects get labels at creation), run the script, audit clean, then the S4 driver.
   **Rollback is not trivial.** Labels are inert, but `destroy -d` on a `restore-source` that has no clone removes it at once (verified:
   ZFS destroys a deferred snapshot immediately when nothing clones it), and the old driver restores from `restore-source`. After the
   migration the old driver cannot restore from those snapshots. The migration is therefore one-way unless the `restore-source` step is
   postponed. **Decision (user): keep it in the migration, accept that it is one-way.**

Tests: the derivation and audit logic is Go or testable shell against the in-memory ZFS (S2), covering: unlabelled legacy layout,
partially labelled, promoted raw (raw lives on a restore), orphan, wrong existing label, snapshot-inherited false positive.

The driver also has no backfill for objects that predate `status.provisionedAt` (a Ready or Lost phase alone no longer counts as
provisioned), no read of a missing `creationTime`, and no default for an empty `spec.sourceType` (the snapshot goes to Error). The
migration must therefore stamp `status.provisionedAt` on every Ready or Lost ZfsDataset and ZfsSnapshot, and must reject or fix a
ZfsSnapshot without `sourceType` before the new driver runs.

### S7. Release and verify
- The user releases. Live test on a throwaway namespace: the joplin group test, a restore, deletes in the orders of scenarios 6, 12 and 13.
- The audit must find no unlabelled object and no `restore-source` before the new driver is deployed (the driver has no code for them).

### Future improvements
- Select delete targets by label instead of name: destroy a snapshot only if its `role` is `raw` or `clone-origin` and its `owner` equals the
  UID of the object being deleted, replacing the `driverSnapshotSuffix` regex. Safe only after the S6 migration labelled everything.
  Not done because the current name allow-list works and the gain is unproven.

### Open items outside this redesign
Finish C9 (cross-pool `FAILED_PRECONDITION`, member restore after a member is deleted), C10, C11, D1/D3/D4/D5, E1/E2/E4 to E6,
F3 to F5, Phase G, the optional chart template `volumeGroupSnapshotClasses`, the stale gitops comment on `snapshotter.tag`.
Remove the test dataset `spinning-archive/verify_zfs_functions` at the end, only with the user's OK.


## Risks

- The model and the mock may both be wrong about real ZFS. Mitigation: the conformance suite (W3) runs the same scenarios on the
  mock and on a real pool; a full replay of 96 scenarios already matched.
- Crashes between steps and create-vs-delete races are not modeled. Mitigation: every step idempotent and retried by the
  reconciler (the finalizer stays until the flow completes); add fault-injection tests in the mock (fail after step N).
- A process holding a mountpoint open during promote/destroy is untested.

## Strategy D (raw-only): verified 2026-10-14

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
