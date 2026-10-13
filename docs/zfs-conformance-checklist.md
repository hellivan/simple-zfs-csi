# ZFS conformance checklist

Status: **nothing verified yet** (2026-10). Every item is an assumption of the model in
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
| 1.1 | `destroy -d` on a snapshot with no clones and no holds | destroyed immediately, no mark | |
| 1.2 | `destroy -d` on a snapshot with a clone | succeeds, snapshot stays, `defer_destroy=on` | |
| 1.3 | Does a marked snapshot show in `zfs list -t snapshot` (with and without `-r`, `-d 1`) or need a flag? | visible (man page); doc does not mention a hiding flag | |
| 1.4 | Last clone destroyed: is the marked snapshot removed at once or later (txg)? | removed by ZFS itself, check timing with an immediate `zfs list` | |
| 1.5 | Last clone destroyed by promote-and-destroy-parent rather than plain destroy | removed once no clones remain | |
| 1.6 | `destroy -d` on a snapshot that does not exist | error text `could not find any snapshots to destroy` (our `isNotExist` already matches it) | |
| 1.7 | `destroy -d` twice on a marked snapshot | no error, still marked | |
| 1.8 | New clone from a marked snapshot | allowed (man page) | |
| 1.9 | Snapshot of a dataset whose own origin snapshot is marked | does not change the mark | |
| 1.10 | Snapshot of the clone R does not release R's origin | origin snapshot still has R in `clones` | |

## 2. Promote

| # | Question | Expected (model) | Result |
|---|---|---|---|
| 2.1 | Which snapshots move: the origin and all older ones, as in the man page | yes | |
| 2.2 | Is the `defer_destroy` mark kept on a snapshot that moves | mark moves with it | |
| 2.3 | Are user properties of a moved snapshot kept | yes | |
| 2.4 | The promoted clone takes over the old parent's previous origin | yes | |
| 2.5 | Sibling clones of a moved snapshot are re-parented to the promoted clone | yes | |
| 2.6 | The old parent becomes a clone of the moved snapshot | yes | |
| 2.7 | Promote when the clone already has a snapshot with the same name | fails; record the exact error text (model: `conflicting snapshot`) | |
| 2.8 | Promote when only the new origin name collides vs. an older moved snapshot collides | both fail, or only one | |
| 2.9 | Promote of a clone whose origin is marked for deferred destroy | succeeds, mark moves with the snapshot | |
| 2.10 | Promote of a clone of a clone (chain): is one call enough? | openzfs#15587 says sometimes not; reproduce and record | |
| 2.11 | Promote on a zvol (not only filesystems) | same behaviour | |
| 2.12 | Promote while the clone is mounted / the zvol is in use | works or fails; record | |
| 2.13 | Promote when the parent has a snapshot newer than the origin | newer snapshots stay on the parent | |

## 3. Destroy of datasets

| # | Question | Expected (model) | Result |
|---|---|---|---|
| 3.1 | `zfs destroy ds` while it has a plain snapshot | refused, error text recorded | |
| 3.2 | Same while the snapshot is marked for deferred destroy | refused (marked snapshots still exist) | |
| 3.3 | `zfs destroy -r ds` when its snapshot has a clone outside the tree | refused, error text recorded | |
| 3.4 | `zfs destroy ds` of a clone whose origin snapshot is marked and has no other clones | succeeds and the origin snapshot goes away | |
| 3.5 | Destroying the clone that held the last reference to a marked snapshot on a different dataset | snapshot removed | |
| 3.6 | `zfs destroy -d` on a filesystem or volume | rejected (`-d` is snapshot-only) | |
| 3.7 | `zfs destroy` of a non-existent dataset | error text matches `isNotExist` | |
| 3.8 | Destroy of a dataset that is the origin of nothing but owns marked snapshots with no clones | succeeds only if the marked snapshots are already gone | |

## 4. Clone, snapshot, list and property reads

| # | Question | Expected (model) | Result |
|---|---|---|---|
| 4.1 | `clones` property of a snapshot after clone, promote and clone destroy | lists direct clones only, updated at once | |
| 4.2 | `origin` property after each of the above | `-` for independent datasets | |
| 4.3 | `zfs list -t snapshot -d 1 -s creation ds` ordering after a promote | oldest first, includes moved snapshots | |
| 4.4 | `zfs list -t snapshot -r pool` finds a snapshot by short name after a promote (our `FindSnapshot`) | only one match | |
| 4.5 | Multi-snapshot call `zfs snapshot a@x b@y`: same `createtxg` for both | yes (confirmed live on 2026-10-10, repeat on the scratch pool) | |
| 4.6 | Clone with `-o` properties | applied | |
| 4.7 | Snapshot name rules: length, characters (`rs-<uuid>`, `csi-snap-<uuid>`) | accepted | |
| 4.8 | `used`, `usedbysnapshots`, `referenced` after promote and after the clone destroy (space freed as expected) | space follows the promoted dataset | |

## 5. Labels (ZFS user properties), phase 2

| # | Question | Expected (model) | Result |
|---|---|---|---|
| 5.1 | `zfs set simple-zfs-csi:role=raw ds@snap` on a snapshot | accepted | |
| 5.2 | Property survives promote of the snapshot | yes | |
| 5.3 | Property is not inherited by clones; clone starts with its own values | check `zfs get -s local,inherited` | |
| 5.4 | Property on a marked snapshot | readable | |
| 5.5 | `zfs list -o name,simple-zfs-csi:owner` works for selecting by label | yes | |
| 5.6 | `zfs rename` of a snapshot (for migrating the fixed `restore-source` name) and its effect on clones that depend on it | works; clones keep depending on it | |
| 5.7 | `zfs rename` of a snapshot while a clone is mounted | works or fails; record | |

## 6. Whole-scenario replays

Run every row of the scenario table (and the generated sequences, as many as is practical) as shell against the scratch pool,
compare the state after **every** step with the model.

| # | What | Result |
|---|---|---|
| 6.1 | All 25 rows plus 18 to 19b, for strategy C (and B if C fails) | |
| 6.2 | Fan-out: one snapshot, 5 restores, several delete orders (12, 12b, and at least 10 random orders) | |
| 6.3 | The exact joplin sequence: restore, promote, delete snapshot, delete restore (the ghost snapshot case) | |
| 6.4 | Scenario 13 with unique `rs-<id>` names (stuck before) | |
| 6.5 | Same replay with zvols instead of filesystems | |

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
