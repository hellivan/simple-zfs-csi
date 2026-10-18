# ZFS conformance checklist

> **Update (2026-10):** the delete design in this document was replaced by strategy D, see [ADR-0042](design-decisions.md) and [snapshot-delete-redesign-plan.md](snapshot-delete-redesign-plan.md). Mentions of `@restore-source` and `detachSnapshotClones` are historical.


Status: **sections 1 to 3 and most of 4 and 5 verified on `zfs-2.4.3-1` (2026-10-13)**, test dataset `spinning-archive/verify_zfs_functions` (unmounted, removed afterwards), scripts in the session folder. Untested rows are still empty. Every item is an assumption of the model in
[snapshot-delete-scenarios.md](snapshot-delete-scenarios.md) or of the plan in
[snapshot-delete-redesign-plan.md](snapshot-delete-redesign-plan.md). Each one must be checked on a real pool before
implementation and before the in-memory ZFS is trusted.

How to run: a throwaway file-backed pool on a node, never the production pool
(`truncate -s 1G /tmp/t.img; zpool create testpool /tmp/t.img`), with the same ZFS version as the cluster nodes
(record `zfs version`). Filesystems and zvols both where it could differ. Fill in the **Result** column with the exact
command output and the date. A result that differs from **Expected** fixes the model and the mock first.

Legend: S = snapshot, C = clone, `ds` = dataset.

## 1. Deferred destroy (`zfs destroy -d`)

| # | Question | Expected (model) | Result |
|---|---|---|---|
| 1.1 | `destroy -d` on a snapshot with no clones and no holds | destroyed immediately, no mark | OK: removed at once, nothing listed |
| 1.2 | `destroy -d` on a snapshot with a clone | succeeds, snapshot stays, `defer_destroy=on` | OK: stays, `defer_destroy=on`, `clones` lists the clone |
| 1.3 | Does a marked snapshot show in `zfs list -t snapshot` (with and without `-r`, `-d 1`) or need a flag? | visible (man page); doc does not mention a hiding flag | OK: **visible** in plain `zfs list -t snapshot`, with `-d 1` and `-t all -r`; no flag needed. `defer_destroy` column shows `on` |
| 1.4 | Last clone destroyed: is the marked snapshot removed at once or later (txg)? | removed by ZFS itself, check timing with an immediate `zfs list` | OK: removed by ZFS as soon as the last clone is destroyed (already gone on the immediate `zfs list`) |
| 1.5 | Last clone destroyed by promote-and-destroy-parent rather than plain destroy | removed once no clones remain | OK: promote R2 then destroy R, `R2@rs` removed (scenario F, repeated flow) |
| 1.6 | `destroy -d` on a snapshot that does not exist | error text `could not find any snapshots to destroy` (our `isNotExist` already matches it) | OK: `could not find any snapshots to destroy; check snapshot names.` (missing snapshot on existing dataset); `cannot open '<ds>': dataset does not exist` (missing dataset) |
| 1.7 | `destroy -d` twice on a marked snapshot | no error, still marked | OK: rc 0, stays marked |
| 1.8 | New clone from a marked snapshot | allowed (man page) | OK: allowed, the new clone is added to `clones` |
| 1.9 | Snapshot of a dataset whose own origin snapshot is marked | does not change the mark | |
| 1.10 | Snapshot of the clone R does not release R's origin | origin snapshot still has R in `clones` | OK: `e1@o` stays marked with `e1r` in `clones`; `e1r@mine` does not release it |

## 2. Promote

| # | Question | Expected (model) | Result |
|---|---|---|---|
| 2.1 | Which snapshots move: the origin and all older ones, as in the man page | yes | OK: origin and all older moved (`a`,`b`), newer (`c`) stays on the parent |
| 2.2 | Is the `defer_destroy` mark kept on a snapshot that moves | mark moves with it | OK: `defer_destroy=on` moves with the snapshot |
| 2.3 | Are user properties of a moved snapshot kept | yes | OK: `t:role=x` still on the moved snapshot |
| 2.4 | The promoted clone takes over the old parent's previous origin | yes | OK: promoted dataset inherits the parent's previous origin (`D_c2` got `D@s`, `J_bk2` got `J_bk1@rs-1`) |
| 2.5 | Sibling clones of a moved snapshot are re-parented to the promoted clone | yes | OK: siblings (`A_D`, `A_E`, `F_R2`) re-parented to the promoted dataset's snapshot |
| 2.6 | The old parent becomes a clone of the moved snapshot | yes | OK: old parent's origin becomes `<promoted>@<snap>` |
| 2.7 | Promote when the clone already has a snapshot with the same name | fails; record the exact error text (model: `conflicting snapshot`) | OK: `cannot promote 'R/C2_c': conflicting snapshot 'x' from parent 'R/C2@x'` |
| 2.8 | Promote when only the new origin name collides vs. an older moved snapshot collides | both fail, or only one | OK: both fail with the same message (`conflicting snapshot 'dup' from parent 'R/C@x'` for an older moved snapshot) |
| 2.9 | Promote of a clone whose origin is marked for deferred destroy | succeeds, mark moves with the snapshot | OK: promote succeeded, mark moved (`B_C@x on`) |
| 2.10 | Promote of a clone of a clone (chain): is one call enough? | openzfs#15587 says sometimes not; reproduce and record | Partly: two-level chain converged with one promote; a second promote then also worked. #15587 not reproduced |
| 2.11 | Promote on a zvol (not only filesystems) | same behaviour | OK: zvol promote behaves the same |
| 2.12 | Promote while the clone is mounted / the zvol is in use | works or fails; record |  Partly: promote and destroy of mounted filesystems work (`mounted=yes`). A process holding the mountpoint could not be tested, because the Talos host has no shell for `nsenter` |
| 2.13 | Promote when the parent has a snapshot newer than the origin | newer snapshots stay on the parent | OK: see 2.1 |

## 3. Destroy of datasets

| # | Question | Expected (model) | Result |
|---|---|---|---|
| 3.1 | `zfs destroy ds` while it has a plain snapshot | refused, error text recorded | OK: `filesystem has children` / `use '-r' to destroy the following datasets: <ds>@plain` |
| 3.2 | Same while the snapshot is marked for deferred destroy | refused (marked snapshots still exist) | OK: refused, same text, marked snapshot listed as child |
| 3.3 | `zfs destroy -r ds` when its snapshot has a clone outside the tree | refused, error text recorded | OK: `filesystem has dependent clones` / `use '-R'` listing the clones |
| 3.4 | `zfs destroy ds` of a clone whose origin snapshot is marked and has no other clones | succeeds and the origin snapshot goes away | OK: see 1.4 and the strategy-C flow |
| 3.5 | Destroying the clone that held the last reference to a marked snapshot on a different dataset | snapshot removed | OK: see 1.4 |
| 3.6 | `zfs destroy -d` on a filesystem or volume | rejected (`-d` is snapshot-only) | OK: `cannot open '<ds>': missing '@' delimiter in snapshot name` |
| 3.7 | `zfs destroy` of a non-existent dataset | error text matches `isNotExist` | OK: `cannot open '<ds>': dataset does not exist` |
| 3.8 | Destroy of a dataset that is the origin of nothing but owns marked snapshots with no clones | succeeds only if the marked snapshots are already gone |  Not tested separately; covered by the strategy-C flow, where `bk` is destroyed only after the promote removed its snapshots |

## 4. Clone, snapshot, list and property reads

| # | Question | Expected (model) | Result |
|---|---|---|---|
| 4.1 | `clones` property of a snapshot after clone, promote and clone destroy | lists direct clones only, updated at once | OK for direct clones, updated at once and re-parented by promote |
| 4.2 | `origin` property after each of the above | `-` for independent datasets | OK: shown as the dataset's origin; `-` when independent |
| 4.3 | `zfs list -t snapshot -d 1 -s creation ds` ordering after a promote | oldest first, includes moved snapshots | |
| 4.4 | `zfs list -t snapshot -r pool` finds a snapshot by short name after a promote (our `FindSnapshot`) | only one match | |
| 4.5 | Multi-snapshot call `zfs snapshot a@x b@y`: same `createtxg` for both | yes (confirmed live on 2026-10-10, repeat on the scratch pool) | OK: both snapshots `createtxg` 6898551 |
| 4.6 | Clone with `-o` properties | applied | |
| 4.7 | Snapshot name rules: length, characters (`rs-<uuid>`, `csi-snap-<uuid>`) | accepted | |
| 4.8 | `used`, `usedbysnapshots`, `referenced` after promote and after the clone destroy (space freed as expected) | space follows the promoted dataset |  OK: used space moves to the promoted dataset (`zv` 56K -> 0B, `zvc` 0B -> 56K); destroying the old parent leaves the child's usage unchanged |

## 5. Labels (ZFS user properties), phase 2

| # | Question | Expected (model) | Result |
|---|---|---|---|
| 5.1 | `zfs set simple-zfs-csi:role=raw ds@snap` on a snapshot | accepted | OK |
| 5.2 | Property survives promote of the snapshot | yes | OK: moved with the snapshot (2.3) |
| 5.3 | Property is not inherited by clones; clone starts with its own values | check `zfs get -s local,inherited` | OK: clone shows `-` (not inherited) |
| 5.4 | Property on a marked snapshot | readable |  OK: property readable and settable on a marked snapshot; `destroy -n -v -d` prints `would destroy` |
| 5.5 | `zfs list -o name,simple-zfs-csi:owner` works for selecting by label | yes | OK: `zfs list -t snapshot -o name,t:owner` works |
| 5.6 | `zfs rename` of a snapshot (for migrating the fixed `restore-source` name) and its effect on clones that depend on it | works; clones keep depending on it | OK: rename works with a clone attached; the clone's `origin` and the snapshot's `clones` follow, property kept |
| 5.7 | `zfs rename` of a snapshot while a clone is mounted | works or fails; record |  Not tested with a mounted clone; rename with an attached clone works (5.6) |

## 6. Whole-scenario replays

Run every row of the scenario table (and the generated sequences, as many as is practical) as shell against the scratch pool,
compare the state after **every** step with the model.

| # | What | Result |
|---|---|---|
| 6.1 | All 25 rows plus 18 to 19b, for strategy C (and B if C fails) |  OK: all 25 rows plus 18 to 19b, as filesystems and as zvols, state compared after every step, no difference (see results log) |
| 6.2 | Fan-out: one snapshot, 5 restores, several delete orders (12, 12b, and at least 10 random orders) |  OK: fan-out with 5 restores in 5 delete orders and the 2-restore case (12/12b), no difference |
| 6.3 | The exact joplin sequence: restore, promote, delete snapshot, delete restore (the ghost snapshot case) | |
| 6.4 | Scenario 13 with unique `rs-<id>` names (stuck before) | |
| 6.5 | Same replay with zvols instead of filesystems |  OK: every table row also replayed on zvols, no difference apart from the transient busy error (results log) |

## Results log

### 2026-10-13, `zfs-2.4.3-1`, node talos-1, dataset `spinning-archive/verify_zfs_functions` (unmounted, empty afterwards)

Method: the model (strategy C, with the deferred destroy before the promote) produced every ZFS command of every scenario
step. The same commands ran on the test dataset, and the live state (datasets, origins, snapshots, `defer_destroy`) was
compared with the model after **every** step.

- 96 scenarios, 686 steps: the 29 table rows each as filesystems and as zvols, 5 fan-out orders with 5 restores, the 2-restore
  case, and 40 random sequences (up to 6 creations, then deleted in random order, alternating filesystem and zvol).
  Scenarios 13 (with unique `rs-<id>` names) and 12/12b are included.
- Result: **no difference** in state, **no stuck delete**, nothing left over at the end, with one exception below.
- **Finding (zvols): `zfs destroy` of a zvol right after `zfs create` fails with `cannot destroy '<zvol>': dataset is busy`**
  in about 40 percent of tries (8 of 20 in a tight loop) and succeeds when repeated one second later. It is unrelated to clones
  and snapshots (probably the device node being probed after creation). The driver propagates the error and the reconciler
  retries (`internal/zpool/zfs_test.go` covers the propagation), so it is harmless, but tests and the delete path must treat
  `dataset is busy` as retryable and the mock should be able to inject it.
- Other results from the same run:
  - Deleting the source volume while a restore of a deleted snapshot is alive fails with `filesystem has children` because
    `P@raw` is still pinned (marked). The volume delete has to promote the restore first, as the delete path already does.
  - Renaming `restore-source` to `rs-<id>` works while a restore depends on it. Renaming onto an existing name fails with
    `cannot rename '<snap>': dataset already exists`.
  - Promote of a five-level clone chain: one call moves only the first level; the loop in `detachAndCleanSnapshots` is needed.
  - Promoting and destroying mounted filesystems works.
- Not covered: a process holding a mountpoint open (the host has no shell for `nsenter`), crash and race behaviour (section 7),
  deeper chains of #15587.

### 2026-10-17, `zfs-2.4.3-1`, conformance suite as code

`go test ./...` runs the suite against the in-memory model only (no ZFS or kernel module needed). The real-pool run is a
manual script, `hack/run-zfs-conformance.sh`, which builds the suite and runs it in the toolbox pod on the cluster. Run it after
changing `memzfs` or the suite and when the node ZFS version changes. The model is only as good as the last real run.

The checklist rows for deferred destroy, promote, dataset destroy, listing, atomic snapshots, user properties and the P4 origin
rule are now Go test cases (`internal/zpool/zfsconf`), run against the in-memory model on every `go test` and against the real
pool with `-tags zfsreal` (`ZFS_CONF_ROOT` must contain `verify_zfs_functions`). All 41 cases passed on the real pool and on
the model. New facts confirmed live: user properties are **inherited from the parent dataset** (a child reads `inherit-me`),
a snapshot reads its dataset's value until it has a local one, the backing clone's `origin` follows the raw snapshot through
promotes of other clones and becomes `-` only when the backing clone itself is promoted (the pool scan then finds the
snapshot on it), and a non-user property cannot be set on a snapshot.

## 7. Crash and race behaviour (optional, later)

| # | Question | Result |
|---|---|---|
| 7.1 | Kill the driver between mark, promote and destroy of `bk`; the retry converges | |
| 7.2 | Snapshot create while the source volume is being deleted | |
| 7.3 | Two deletes of the same snapshot in parallel | |

## 8. Postponed

- `zfs hold` as protection against manual destroys (and how holds interact with promote and `destroy -d`). Not needed for the redesign.

## Sign-off

Each section 1 to 6 needs every row filled in, with the ZFS version and the date. Then the in-memory ZFS is written to match the
results (not the model), and the same scripts run against both.
