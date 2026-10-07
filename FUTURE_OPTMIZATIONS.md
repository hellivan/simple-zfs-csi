# Future optimizations

> **Update 2026-10-07 (ADR-0038):** after provisioning nothing is rebuilt and no recorded
> fact is re-read, so the only remaining need for a lookup is the Ready/Lost *observation*
> (status only). Item 1 below (follow `origin` pointers) is the right fix for the false
> `Lost` after a promote, and "drop the periodic settled check" is now mainly about cost.
> Rebuild-from-raw and the `creationTime` re-read are no longer planned.


## Locating snapshot primitives after a promote

Context: `reconcileSettled` (`internal/controller/zfssnapshot_controller.go`) checks
`<backing clone>@restore-source`. A promote relocates the origin snapshot and every
older snapshot, so that fixed path can be wrong (false `Lost`). For example, promoting a
restored PVC takes `@restore-source` (and a relocated raw snapshot) onto the restored
dataset. `FindSnapshot` (`internal/zpool/zfs.go`) matches by suffix over `zfs list -r`
of the whole pool, so it is expensive and ambiguous for the non-unique `restore-source`
name (only the raw name `csi-snap-<uuid>` is unique).

Nothing below is verified on a live pool; it is derived from the docs and code.

### Why `FindSnapshot` is pool-wide today (ADR-0028)
- Introduced for `reconcileDelete`: a promote moves snapshots, so the recorded address
  can be stale. Pool-wide `zfs list -r <pool>` was chosen as unconditionally correct.
- The ADR priced it as running once per snapshot deletion, after the backing-clone
  wait. A settled-state check every ~30s per object is a different workload, so that
  premise no longer holds.
- The ADR already records the cheap narrowing: `-r <pool>/<prefix>` is provably enough
  (a relocated snapshot stays in its clone lineage; cross-prefix restores are rejected,
  D6). It is the first step and keeps unrelated `auto-*` trees out of the listing, but
  the cost still grows with the number of objects.
- Live check in redesign doc §12.5: 837 snapshots on `spinning-archive`, trivial then.

### Alternative: drop the periodic settled check entirely (leaning option)
- The check only drives the informational `Ready`/`Lost` flip; the controller never acts
  on it under the never-recreate rules. Places where it matters already fail loudly:
  delete (`reconcileDelete` calls `FindSnapshot`), restore (clone from
  `@restore-source` fails), CSI create/publish waits.
- Settled objects would do zero ZFS calls per reconcile, and the false-`Lost` after a
  promote disappears because no fixed path is checked.
- Cost: a snapshot whose ZFS object vanished keeps showing `Ready` until used or
  deleted. Acceptable under the trust boundary (nobody touches the ZFS objects).
- Rebuild-from-raw (if wanted) can also be lazy: at restore time, when the clone or
  `@restore-source` is missing but the raw snapshot still exists.
- Middle ground: keep `Lost` detection only on real events (spec change, deletion, CSI
  request), not on every `ZfsPool` poll.
- Independent of the group snapshot feature: a group child adopts the raw snapshot at
  its recorded path (direct `zfs get`, `FindSnapshot` only on a miss), and deletion keeps
  the once-per-deletion `FindSnapshot`.

### 0. Narrow the listing to `<pool>/<prefix>`
- One-line change, already blessed by ADR-0028.

### 1. Follow `origin` pointers instead of searching (preferred primary check)
- Every clone has an `origin` property that ZFS keeps correct across promotes.
- Backing clone `origin` leads to the raw snapshot; a restored PVC's `origin` leads to
  its `@restore-source`. One `zfs get origin <dataset>` per hop.
- If the backing clone was promoted it has no origin, and the raw snapshot is
  `<backing clone>@<raw>`.
- Fall back to a pool scan only when the chain is genuinely missing.

### 2. One snapshot scan per pool, shared by all objects
- `zfs list -t snapshot -r <pool> -o name,<tag>` once per pool per cycle, not once per
  `ZfsSnapshot`.
- The discovery agent already polls every 30s; it could keep a name to path map in
  memory or in `ZfsPool` status for the controllers to read.
- Cost becomes independent of the number of `ZfsSnapshot` objects.

### 3. Tag snapshots with a ZFS user property
- Set e.g. `simple-zfs-csi:snapshot=<uid>` on the raw snapshot and on `@restore-source`
  (`SetProperty` already exists). The property is expected to move with the snapshot on
  promote (to be verified).
- Removes the `restore-source` name ambiguity. It does not make a search cheaper: ZFS
  has no property index, so filtering is client-side.
- Existing untagged snapshots fall back to name matching until re-tagged.

### 4. Record the last known path in status
- e.g. `status.rawSnapshotPath`: common case is one `zfs get` on that path; refresh it
  when found elsewhere; scan only on a miss.

### 5. Check settled objects less often (behaviour change, undecided)
- Every `ZfsPool` status patch (~30s) currently re-reconciles every object, including
  long-settled ones.
- Settled objects could ignore pool events and recheck on their own timer
  (e.g. 5-10 min with jitter). Trade-off: `Lost` is detected later.
- Decide only if the cost is still visible after 1 and 2.

### Suggested order
1 as the primary check, 2 as the fallback, 3 to disambiguate `@restore-source` during a
scan, 4 and 5 only if needed.

## Recording `createtxg` (future)

- Record the raw snapshot's `createtxg` in status as proof that group members were taken
  in the same instant (members of one atomic `zfs snapshot` exec share it; `creationTime`
  has only one-second resolution and is set per dataset, so members can differ).
- It has the same lookup problem as `creationTime`: it must be read from the raw
  snapshot, wherever a promote left it. Read it only on the transition into Ready (like
  `creationTime`) via the recorded path, with `FindSnapshot` only on a miss, so it adds
  no periodic search. It fits the optimizations above (origin pointers, shared scan,
  tag, last known path).
