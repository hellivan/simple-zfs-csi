# Implementation progress: group snapshots (ADR-0038 / 0039 / 0040 / 0041)

Persistent progress file. Tick `[x]` only when the item is done **and** its tests pass.
Details per item live in [TODO.md](TODO.md); decisions live in
[docs/design-decisions.md](docs/design-decisions.md). Nothing here has been run on a real
cluster or ZFS pool until the e2e phase is ticked.

Rules for every item: run the targeted tests, run `go build ./...` and `go vet ./...`,
commit once the item is done (one commit per item or small group), tick it here in the same commit.

## Plan (order matters)

1. Phase A: ADR-0038 freeze rule (existing code), so group children can rely on it.
2. Phase B: ADR-0041 delete path (existing code), independent of the group feature.
3. Phase C: `ZfsGroupSnapshot` feature (TODO.md checklist 1 to 9).
4. Phase D: cleanup and open decisions.
5. Phase E: `FindSnapshot` optimization (E2 together with A3; E3 with C3/C4).

## Phase A: ADR-0038 (`provisionedAt` freezes everything)

- [x] A1. `zfsdataset_controller.go` `setStatusAt`: record `creationTime` once at the first Ready, never re-read.
- [x] A2. `zfssnapshot_controller.go` `reconcileSettled`: same; observes Ready/Lost only, never creates anything.
- [x] A3. Fix the false `Lost` for `<backing clone>@restore-source` after a promote (follow `origin` pointers, see FUTURE_OPTMIZATIONS.md).
- [x] A4. Update/add tests (no `creationTime` re-read on Lost to Ready; no recreate after `provisionedAt`; no false Lost after promote).

## Phase B: ADR-0041 (volume delete looks only at ZFS)

- [x] B1. Remove `checkSnapshotDependents` (D3) and its call in the `ZfsDatasetReconciler` delete path.
- [x] B2. Remove `checkPendingCloneDependents` (D21) and its call (`promote.go`), with its tests.
- [x] B3. Remove the live-CR claim clause from `assertDriverSnapshot` (keep the name allow-list, D18).
- [x] B4. New round order in `detachAndCleanSnapshots`: allow-list check of all snapshots, destroy clone-free driver snapshots, then promote the clones of the rest.
- [x] B5. Verified by reading: a clone whose source vanished fails `zfs snapshot`/`zfs clone` and the dataset reports `Error` `CreateFailed` with the ZFS message on every retry (not silent). Real-ZFS confirmation is part of C9.
- [x] B6. Tests: clone-free `csi-snap-*` destroyed before promote; cloned snapshot promoted not destroyed; non-Ready/Error `ZfsSnapshot` no longer blocks delete; foreign snapshot still refused.
- [x] B7. Update comments citing D3/D21/claim check (`promote.go` ~70, 123-170, 350-373; `zfsdataset_controller.go` ~111).
- [x] B8. Update docs: `lifecycle-protection-matrix.md` (incl. §6.3), `csi-technical-reference.md` tables, `runbooks.md`, `redesign-strategy.md`.

## Phase C: `ZfsGroupSnapshot` (ADR-0039, checklist in TODO.md)

- [x] C1. Variadic `ZFS.Snapshot(ctx, names ...string)`; all exist = no-op, some exist = distinct error; update fakes and call sites.
- [x] C2. `ZfsGroupSnapshot` CRD (cluster-scoped, finalizer, no ownerReference); regenerate CRD and deepcopy.
- [x] C3. `ZfsSnapshot.Spec.GroupSnapshotID` (immutable); child never runs `zfs snapshot` and fails loud (`phase=Error`) if raw snapshot/dataset is missing; `snapshotMessage` sets `group_snapshot_id`; `DeleteSnapshot` on a member = `INVALID_ARGUMENT` (missing CR = OK).
- [x] C4. `ZfsGroupSnapshotReconciler`: three-way raw check, one atomic exec, `creationTime` once, create children, `provisionedAt` once, derived status, finalizer (delete children, wait, destroy orphan raw snapshots before `provisionedAt`), Terminating creates nothing; extract `resolveDatasetPath`.
- [x] C5. `GroupControllerServer` (`internal/csi/groupcontroller.go`): Create/Delete/Get per ADR-0039 tables and error codes.
- [x] C6. Wiring: `GROUP_CONTROLLER_SERVICE` plugin capability, `GroupControllerGetCapabilities`, optional group server in `csi.Serve`, controller entrypoint only.
- [x] C7. Helm/RBAC, csi-snapshotter `--feature-gates=CSIVolumeGroupSnapshot=true` (verified in external-snapshotter v8.2.0 main.go; opt-in via `csiController.snapshotter.groupSnapshots.enabled`), docs for the `groupsnapshot.storage.k8s.io` CRDs and flags, CRD install note.
- [x] C8. Unit tests per TODO.md item 8 (variadic exec, reconciler, child fail-loud, RPC tables).
- [ ] C9. End-to-end per TODO.md item 9 (single exec with both datasets, restore both, clean delete, cross-pool `FAILED_PRECONDITION`, member restore after another member deleted).
- [ ] C10. Verify Helm chart's csi-snapshotter version supports the group snapshot flag.
- [ ] C11. Check upstream behavior when a group Create never succeeds (does it clean up the group CR?).

## Phase D: cleanup and open decisions

- [ ] D1. Remove the `provisionedAt` migration fallback after verifying all objects (TODO.md). **Blocked:** on 2026-10-08 the live cluster had `provisionedAt` on 0 of 91 datasets and 0 of 57 snapshots (the deployed build predates it); deploy, let agents backfill, re-check, then remove.
- [x] D2. Removed the legacy snapshot `mode` parameter check (live VolumeSnapshotClass `zfs-snapshot` has no parameters).
- [ ] D3. Decide: act on a `provisionedAt` vs `creationTime` mismatch.
- [ ] D4. Decide: adoption of an already-existing dataset.
- [ ] D5. Optional: record `createtxg` and `snapshotTakenAt` on group snapshots.

## Phase E: `FindSnapshot` optimization (details: [FUTURE_OPTMIZATIONS.md](FUTURE_OPTMIZATIONS.md))

Today `FindSnapshot` runs `zfs list -t snapshot -r <pool>` and suffix-matches; it follows no `origin` pointers.
Callers: `reconcileSettled` (every ~30s per object, observation only) and `reconcileDelete`.

- [ ] E1. (not done: needs the lineage prefix passed in; E3 already removes the scan from the common case) Narrow the listing to `-r <pool>/<prefix>` (one-line change, already blessed by ADR-0028).
- [ ] E2. Follow `origin` pointers (backing clone `origin` -> raw snapshot; promoted clone: `<clone>@<raw>`); pool scan only as fallback. This is also the A3 fix, so do A3 and E2 together.
- [x] E3. Delete and Lost-recovery read the recorded raw path directly (`locateRaw`); `FindSnapshot` only on a miss. (Group children already use direct reads.)
- [ ] E4. Optional: one snapshot scan per pool per cycle shared by all objects.
- [ ] E5. Optional: user-property tag on snapshots (verify it moves with a promote first).
- [ ] E6. Decide: drop or slow down the periodic settled check (leaning: drop; FUTURE_OPTMIZATIONS.md item 5).

## Phase F: API-server load (measured 2026-10-08 on `admin@kube-sl-home`, one-minute sample)

The driver issues about 855 of about 2870 API requests per minute. Cause: every 30s `ZfsPool.status.lastUpdated`
changes and each controller re-reconciles every object (56 attach requests, 91 datasets, 54 snapshots, 37 shares).

- [x] F1. Add a predicate to the `ZfsPool` watches (dataset, snapshot, share, attach request) that ignores updates where only `lastUpdated` changed.
- [x] F2. Attach request local-only path (`zfsshareattachrequest_controller.go` ~245, ~288): stop the uncached `Delete` of a non-existent `ZfsShare` on every reconcile (about 108 DELETE 404/min); read from cache first, delete only if it exists.
- [ ] F3. (deferred: these reads are deliberate safety gates; re-evaluate after F5) Stop the uncached `ZfsPool` GET (`gateReader()`, about 112/min) and the live attach-request LIST (about 154/min) on every settled reconcile; use them only where a stale read is dangerous (teardown).
- [ ] F4. (deferred: most patches were driven by the 30s churn removed in F1; re-evaluate after F5) Skip status patches when nothing changed (about 180 dataset, 112 attach request, 108 snapshot, 74 share patches/min).
- [ ] F5. Re-measure afterwards (`apiserver_request_total`, 60s sample).

## Phase G: periodic resource check (follow-up to F1)

F1 stopped the 30s `ZfsPool.status.lastUpdated` heartbeat from re-reconciling every object, which also removed the only periodic "is it still there?" check for datasets and snapshots (Lost detection now happens only on an event or an agent restart). Replace it with an explicit, configurable one.

- [ ] G1. Add a configurable interval (flag + Helm value, e.g. `agent.resourceCheckInterval`, sane default such as 5m, `0` disables) for the periodic check of `ZfsDataset` and `ZfsSnapshot` (and `ZfsGroupSnapshot` Lost derivation); implement as `RequeueAfter` on settled reconciles with jitter, so it needs no pool heartbeat.
- [ ] G2. The check must also fire immediately when the pool status changes (already true via the F1 predicate: only `lastUpdated`-only updates are ignored); add a test that a node/health change still enqueues.
- [ ] G3. The periodic check must stay read-only and cheap (one `zfs get` per object, as `reconcileSettled` does today); verify with F5 measurement that API load stays low.
- [ ] G4. Decide with E6 (drop or slow down the periodic settled check) so both agree; document the interval in the chart values and docs.

## Log

| Date | Item | Commit | Note |
|------|------|--------|------|
| 2026-10-08 | design docs | `96efe13` and earlier | all ADRs written, no code yet |
| 2026-10-08 | A1-A4 | `293823b` | creationTime recorded once; restore-source located via origin (A3 done; E2 only for FindSnapshot remains) |
| 2026-10-08 | B1-B8 | `f0da7c4` | ADR-0041 delete path: D3, D21, claim clause removed; new round order |
| 2026-10-08 | C1 | `f2f8beb` | variadic atomic `ZFS.Snapshot` |
| 2026-10-08 | C2-C6, C8 | (this commit) | group CRD, reconciler, CSI group server, wiring, unit tests |
| 2026-10-08 | C7 | `dfc1690` | chart: agent/controller RBAC, opt-in `groupSnapshots.enabled` (feature gate + RBAC); flag name corrected from upstream source |
| 2026-10-08 | D2, E3, F1, F2 | (this commit) | pool-watch predicate ignoring `lastUpdated`; cache-first ZfsShare delete; direct raw lookup; mode param removed. Note: the 30s pool heartbeat no longer triggers periodic re-reconciles (relevant to E6) |
| 2026-10-08 | review fixes | (this commit) | cache audit: group provision re-reads the group directly (no re-provision/creation from a stale view), delete reads `provisionedAt` directly, child confirms the source path directly before a decisive snapshot/GroupRawSnapshotMissing; transient ZFS errors retry with backoff instead of setting `Error` |
