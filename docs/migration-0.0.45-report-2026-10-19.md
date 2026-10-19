# Migration and upgrade to 0.0.45: report (2026-10-19)

Cluster `admin@kube-sl-home`, pool `spinning-archive`, driver `0.0.44` to `0.0.45`. Procedure:
[runbooks.md](runbooks.md#migrating-to-the-raw-only-snapshot-design-strategy-d-and-upgrading-the-driver).
Design: [ADR-0042](design-decisions.md), [snapshot-delete-redesign-plan.md](snapshot-delete-redesign-plan.md).

## Result

The migration and the upgrade are complete and verified. The new delete path was exercised live in both
orders that used to fail (snapshot first, source first) and with a two-member group snapshot. Nothing was
left on the pool or in the CRs after any test, and the controller logs showed no errors.

## Why the migration was needed

0.0.45 replaces the old snapshot layout (raw snapshot, backing clone and a fixed-name
`backing@restore-source` that restores cloned) with strategy D (raw snapshot plus an empty backing clone;
restores clone the raw snapshot; every driver destroy is `zfs destroy -d`). The reason is in
[snapshot-delete-scenarios.md](snapshot-delete-scenarios.md): promotes move snapshots between datasets,
so name-based cleanup left orphans after group snapshot deletes, and the fixed `restore-source` name made
one order of deletes fail forever (scenario 13).

The new driver contains no handling of the old layout, so the existing objects had to be converted first:

1. Every driver-created dataset and snapshot gets the labels `simple-zfs-csi:role` and `simple-zfs-csi:owner`.
   The new delete and promote logic selects by label, and the driver never repairs provisioned objects.
2. Every `@restore-source` snapshot is marked with `zfs destroy -d`. The new driver does not read or create it,
   and a leftover one would block a delete.

The migration is one-way: after it, the old driver cannot restore from existing snapshots.

## Timeline

| Step | What | Result |
|---|---|---|
| Review | `go build`, `go vet`, `go test ./internal/... ./api/...` | all green |
| Preconditions | pods, in-flight snapshots, terminating objects, Velero | all on 0.0.44; no VolumeSnapshots or group snapshots; nothing terminating; Velero schedule `0 2 * * *`, no backup running, so it was not paused |
| Dry run | `zfs-migrate` without `--apply` | exit 3: 632 actions (542 `zfs set` for 271 objects: 90 `raw`, 90 `backing`, 91 `volume`; 90 `destroy -d`), 0 blockers, 0 findings, no `provisionedAt`/`sourceType` stamps, no `clone-origin` objects |
| Apply, run 1 | `--apply` | aborted mid-way: `lookup kube.sl.home: no such host` (see below) |
| Resume check | dry run | 104 objects labelled, 334 `zfs set` left, 0 `destroy -d` left, 0 findings |
| Apply, run 2 | `--apply` | exit 0, audit after apply: 271 labelled, 0 actions, 0 findings |
| Upgrade | gitops `targetRevision: 0.0.45`, pushed, Argo CD sync | all pods Ready on 0.0.45, CRD has the new `spec.source` rule, no `Error`/`panic`/`InvalidSnapshot` in logs, all 90 `ZfsSnapshot` Ready |
| Straggler pass | dry run after the upgrade | exit 0, nothing created unlabelled during the gap |

### Incident: apply interrupted by DNS

Each `zfs` command of the tool is one `kubectl exec`, so a name resolution failure for `kube.sl.home` aborted
run 1 after some labels were set. `/etc/hosts` then resolved it (`10.0.2.20`). Because the migration is idempotent,
re-running `--apply` finished the work.

Consequence worth knowing: the `destroy -d` actions are planned before the label actions, so the first run had
already marked or destroyed all 90 `restore-source` snapshots. The one-way point was therefore passed in run 1, and
the driver upgrade followed right after run 2. The runbook now says this.

### Two small gotchas

- The gitops bump was a local commit (`ahead 1`) and had to be pushed before Argo CD could see it.
- The shell's current kube context was a different cluster (`oidc-kube-bru-solunio-com`); every command used
  `--context admin@kube-sl-home`.

## Live tests (namespace `mig-test`, StorageClass `generic-scratch`, zvols)

All objects were created and deleted by hand by the user. Verification between steps was read-only
(`zfs list` in the toolbox pod, CRs, controller logs).

### Snapshot and restore layout (after create)

Source volume `volume`; raw snapshot `raw` on the source; `backing` clone whose origin is the raw snapshot;
a restore is a `volume` whose origin is the raw snapshot itself. No `restore-source` anywhere.

### Run A: delete snapshot, then restore, then source (scenario 6)

| After | State |
|---|---|
| delete snapshot | raw snapshot marked `defer_destroy=on`, kept only by the restore; backing clone destroyed; `ZfsSnapshot` and `VolumeSnapshotContent` gone |
| delete restore | restore zvol gone, deferred raw snapshot removed by ZFS itself; only the source remained, no snapshots |
| delete source | pool and CRs empty of test objects |

### Run B: delete source, then restore, then snapshot (scenario 13 class)

| After | State |
|---|---|
| delete source | source destroyed; raw snapshot promoted onto its own backing clone; snapshot and restore still Ready/Bound |
| delete restore | restore gone; backing clone and raw snapshot remained (snapshot still live) |
| delete snapshot | backing clone and raw snapshot removed; pool and CRs empty of test objects |

### Group snapshot (`VolumeGroupSnapshot` of two PVCs, class `zfs-group-snapshot`)

- Both raw snapshots had the same `createtxg` (7006224), so they were taken atomically. Each source had its own
  `raw` snapshot and `backing` clone; the `ZfsGroupSnapshot` was Ready.
- Both members were restored to new PVCs (Bound, clones of the raw snapshots, no `restore-source`).
- Deleted in the order that originally left orphans: group, restores, sources.
  - After the group delete: both raw snapshots marked `defer_destroy=on` on the sources, backing clones destroyed,
    group and member CRs and the Kubernetes group/snapshot contents gone.
  - After the restore deletes: the deferred snapshots disappeared on their own.
  - After the source deletes: pool and CRs empty of test objects.

### Final audit

After every test `zfs-migrate` (dry run) still reported 271 labelled objects, 0 actions, 0 findings.

## Not covered

- Direct PVC clones (`clone-origin` snapshots): none existed at migration time and the live tests did not create one.
  They are covered by the memzfs lifecycle tests only.
- PVCs mounted by running pods during snapshot, restore or delete (the test PVCs were never consumed).
- Filesystem-type datasets (all tests used zvols; the conformance suite covers both on the model and on a real pool).
- Deep clone chains, a process holding a mountpoint during promote or destroy, other ZFS versions
  (listed as unverified in ADR-0042).
- The first nightly Velero run on 0.0.45 (schedule `0 2 * * *`). Check it read-only the next morning:
  `kubectl --context admin@kube-sl-home -n velero get backups`.

## Cleanup status

- Namespace `mig-test`: all resources deleted; the empty namespace itself may still exist.
- Temporary files on the machine that ran the migration: `/tmp/mig-2026-10-19/` (dry runs, apply output, safety
  copies) and `/tmp/zfs-migrate`.
