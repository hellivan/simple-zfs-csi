# Live ZFS Delete-Path Verification — Test Log (2026-08-03)

**Status: completed, empirically verified on a live production pool.** This is the
permanent, verbatim record of the real-pool run required by
[snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md) §9.2 item 10 / D23 and
by [ADR-0020](design-decisions.md).

It is the companion to
[promote-order-verification-2026-07-31.md](promote-order-verification-2026-07-31.md),
which verified the *promote* half. That run deliberately executed **no `zfs destroy` at
all** — and every defect the 2026-08-03 code review later found lived in the destroy
sequences that run after those promotes. This run covers exactly that gap.

## 1. What this verifies

Two things at once:

1. **That real ZFS behaves the way the Go test double models it.** After ADR-0020 the
   delete path's correctness rests entirely on `fakeZFS`'s fidelity, so that fidelity
   needed checking against a real pool, not just against the earlier promote-only log.
2. **That the algorithm built on that model actually terminates and succeeds.** The test
   script is a line-by-line shell transcription of
   `internal/controller/promote.go`'s `detachAndCleanSnapshots` (and `detachSnapshotClones`
   for D19), so a green run exercises the real decision logic, not a simplified stand-in.

The four scenarios are the ones §9.2 item 10 requires, one per review finding F2a–F2d.

## 2. Environment

- **Cluster:** `kubectl` context `admin@kube-sl-home`, namespace `simple-zfs-csi`
- **Toolbox pod:** `simple-zfs-csi-toolbox-bkmhs` (busybox `ash`, no bash — see §3)
- **ZFS:** `zfs-2.4.3-1`, `zfs-kmod-2.4.3-1`
- **Pool:** `spinning-archive` — **a live, production pool with real user data** (21.8T,
  81% full; `immich`, `movies`, `BACKUP`, `k8s`, … are real datasets on it)
- **Test dataset:** `spinning-archive/csi-delete-path-test` — created fresh by this run,
  verified not to exist beforehand. **Nothing outside this subtree was read, modified or
  destroyed.**
- **Safety:** unlike the 2026-07-31 run, this one *does* destroy things — that is the
  point. Every destroy was non-recursive (`zfs destroy`, never `-r`), every path was
  checked to be under the test subtree before any destructive call, and the script
  refuses to start if the subtree already exists. Confirmed afterwards: **0 occurrences
  of `zfs destroy -r` in the entire transcript**, and 837 pre-existing snapshots
  elsewhere on the pool untouched.

## 3. How it was run

The toolbox image has no bash (`/bin/sh -> busybox`), and the script needs bash 4 for
`mapfile`. Rather than degrade the script to POSIX `sh`, it was run **locally** with
`zfs`/`zpool` shims on `PATH` that proxy each call through `kubectl exec`:

```sh
#!/bin/sh
exec kubectl --context admin@kube-sl-home -n simple-zfs-csi \
  exec simple-zfs-csi-toolbox-bkmhs -- zfs "$@"
```

```
$ PATH=/tmp/zfsshim:$PATH ./docs/delete-path-verification.sh spinning-archive
```

The script is committed as [delete-path-verification.sh](delete-path-verification.sh) and
is re-runnable, including directly on a storage host where bash exists.

## 4. Result

```
checks run: 18, failures: 0
PASSED — every object was removed with non-recursive destroys only.
```

### 4.1 Scenario A (F2a) — direct clone, delete source then clone

Promoting the clone relocated `@clone-<hash>` onto it, leaving:

```
.../a/clone                         -
.../a/clone@clone-0123456789abcdef  -
```

Deleting the clone then destroyed that inherited snapshot explicitly before the dataset:

```
$ zfs destroy .../a/clone@clone-0123456789abcdef
$ zfs destroy .../a/clone
```

This is precisely the case that used to fail permanently with "filesystem has children".

### 4.2 Scenario B (F2b) — delete a snapshot while a restore is live

The full sequence, with the state after the snapshot was fully deleted:

```
DeleteSnapshot step 1: tear down the backing clone
  $ zfs promote .../b/restored
  $ zfs destroy .../b/bc
DeleteSnapshot step 2: required raw-origin cleanup (D19)
  $ zfs promote .../b/restored
  (already relocated by the promote above -> destroy is a no-op)

NAME                          ORIGIN
.../b/restored                -
.../b/restored@csi-snap-x     -
.../b/restored@restore-source -
.../b/vol                     .../b/restored@csi-snap-x
```

**Two observations worth recording.** First, D19 works as designed: promoting the
restored volume away relocated the raw snapshot onto it, so the required destroy became a
genuine no-op instead of failing with "snapshot has dependent clones" — which is what used
to wedge the `ZfsSnapshot` in `Terminating` forever.

Second, and more interesting: the restored volume ends up owning **both** relocated
snapshots, and the original source volume becomes a *clone of the restore*. This inverted
final shape is exactly what
`TestZfsSnapshotReconcile_StandaloneDeleteWithLiveRestore` asserts
(`snapshots[pvc-r] == [csi-snap-x, restore-source]`) — an assertion that was initially
written wrong and corrected against the Go model. The live pool independently confirms the
corrected version.

Both the restore and the original source then deleted cleanly, in that order.

### 4.3 Scenario C (F2c) — two restores from one snapshot

Both restores survived the snapshot's deletion and both deleted cleanly afterwards,
starting with the one that had ended up owning the shared history. Consistent with §2.9:
promoting one restore re-parents its sibling onto it rather than freeing both.

### 4.4 Scenario D (F2d) — six snapshots, delete volume, then delete each in scrambled order

After `DeleteVolume`, the backing clones converged to the chained state — reproducing the
2026-07-31 run's final listing exactly, and matching the Go double's
`TestFakeZFSPromote_MatchesLivePoolVerification`:

```
.../d/bc1  -
.../d/bc2  .../d/bc1@csi-snap-t1
.../d/bc3  .../d/bc2@csi-snap-t2
.../d/bc4  .../d/bc3@csi-snap-t3
.../d/bc5  .../d/bc4@csi-snap-t4
.../d/bc6  .../d/bc5@csi-snap-t5
```

Deleting them in scrambled order (t3, t1, t6, t2, t4, t5) succeeded at every step. The
last one deleted had accumulated the whole relocated history and destroyed all of it
non-recursively, one snapshot at a time:

```
DeleteSnapshot t5
  $ zfs destroy .../d/bc5@csi-snap-t1
  $ zfs destroy .../d/bc5@csi-snap-t2
  $ zfs destroy .../d/bc5@csi-snap-t3
  $ zfs destroy .../d/bc5@csi-snap-t4
  $ zfs destroy .../d/bc5@csi-snap-t5
  $ zfs destroy .../d/bc5@restore-source
  $ zfs destroy .../d/bc5
```

Deleting `t1` — whose backing clone owned the snapshot `bc2` depended on — is the case
that used to fail permanently with "snapshot has dependent clones". It promoted `bc2` away
and succeeded.

Final state: nothing left but the empty container datasets.

## 5. Conclusions

- **D23 is closed.** The delete path is now verified against a real pool, not only against
  a model. The 2026-07-31 run's conclusion stays correctly scoped to the promote batch.
- **The Go test double is faithful** for every behaviour the delete path depends on:
  snapshot relocation and ordering, sibling re-parenting, lineage inheritance, the
  parent-becomes-a-clone inversion, and both `zfs destroy` refusal modes. Where the model
  and an initial hand-written expectation disagreed (§4.2), the pool sided with the model.
- **No `zfs destroy -r` is needed anywhere**, under any of the four orderings — which is
  the empirical form of D11/D22's invariant.
- Every object in all four scenarios was removed; nothing was left stuck.

## 6. Cleanup

The test subtree was left containing only four empty container datasets (504K total). It
can be removed with:

```
$ ./docs/delete-path-verification.sh spinning-archive cleanup
```

That subcommand contains the script's only `zfs destroy -r`, and it is guarded to the test
subtree.
