# Live ZFS Promote-Order Verification — Test Log (2026-07-31)

**Status: completed, empirically verified on a live production pool.** This document
is the permanent, verbatim record of a hands-on test run against a real ZFS pool to
settle an open question raised in
[snapshot-lifecycle-redesign.md §2.11 / D16](snapshot-lifecycle-redesign.md#211-does-promote-iteration-order-matter-when-a-source-has-multiple-independent-snapshots-raised-by-the-user-see-d16---corrected-after-further-review)
(and, as a side effect, D13). Written per explicit request — "I don't want to lose it" —
and is treated as the source of truth for what was actually observed, independent of
whatever the redesign doc's own narrative says at any point in time.

## 1. What this test was verifying

**Question (raised by the user, D16):** if a source volume (`vol1`) has multiple
independent `standalone`-mode snapshots, each with its own backing clone
(`csi-snap-t1`...`csi-snap-t6`), and `DeleteVolume`'s promote loop (D3) iterates over
all of them in an arbitrary (non-chronological) order — does promoting one out of order
cause it to "steal" snapshots away from another, already-claimed or already-independent
backing clone?

Prior to this test, the redesign doc had gone through two rounds of pure source-reading
analysis (fetching fragments of OpenZFS's `dsl_dataset.c`) and arrived at genuine,
stated uncertainty about whether an already-independent, previously-promoted clone's
snapshot could be "stolen back" by a later, unrelated promote in the same batch. This
test was run specifically to resolve that uncertainty empirically instead of continuing
to guess from source fragments.

## 2. Environment

- **Cluster:** `kubectl` context `admin@kube-sl-home`
- **Namespace:** `simple-zfs-csi`
- **Toolbox pod:** `simple-zfs-csi-toolbox-bkmhs` (used to run all `zfs`/`zpool` commands
  via `kubectl exec`)
- **Pool:** `spinning-archive` — **a live, production pool with real user data**
  (confirmed via a read-only `zfs list` before touching anything: `immich`, `backups`,
  `homes`, `BACKUPS_MW/fotos`, etc. are real datasets on this pool)
- **Test dataset:** `spinning-archive/copilot-promote-test` — a disposable subtree
  created specifically and only for this test, verified to not already exist before
  creation. **Nothing outside this subtree was read, modified, or touched at any point.**
- **Safety constraints observed:** no `zfs destroy` (or any other destructive/mutating
  operation outside the test subtree) was run at any point, per explicit instruction.
  The test dataset was **left in place, not cleaned up**, after the test concluded —
  cleanup is deferred to an explicit, separate, future instruction.

## 3. Exact commands run, in order, with full output

### 3.0 Pre-flight: locate the toolbox pod and confirm pool health (read-only)

```
$ kubectl --context admin@kube-sl-home -n simple-zfs-csi get pods -o wide
```
Confirmed `simple-zfs-csi-toolbox-bkmhs` (1/1 Running) as the pod to exec into.

```
$ kubectl --context admin@kube-sl-home -n simple-zfs-csi exec simple-zfs-csi-toolbox-bkmhs -- zpool list
NAME               SIZE  ALLOC   FREE  CKPOINT  EXPANDSZ   FRAG    CAP  DEDUP   HEALTH  ALTROOT
spinning-archive  21.8T  17.7T  4.11T        -         -    19%    81%  1.00x  ONLINE  -
```

```
$ kubectl --context admin@kube-sl-home -n simple-zfs-csi exec simple-zfs-csi-toolbox-bkmhs -- zfs list -o name,used,avail -r spinning-archive | head -30
```
(confirmed real production datasets exist: `spinning-archive/immich`, `.../backups`,
`.../homes`, `.../BACKUPS_MW/fotos`, etc. — this is definitely a live pool, not a test
pool)

```
$ kubectl --context admin@kube-sl-home -n simple-zfs-csi exec simple-zfs-csi-toolbox-bkmhs -- zfs list -r spinning-archive/copilot-promote-test 2>&1 || echo "DOES NOT EXIST YET - safe to create"
cannot open 'spinning-archive/copilot-promote-test': dataset does not exist
command terminated with exit code 1
DOES NOT EXIST YET - safe to create
```

### 3.1 Step 0 — create `vol1` with six sequential snapshots

```bash
TB="simple-zfs-csi-toolbox-bkmhs"; NS="simple-zfs-csi"; CTX="admin@kube-sl-home"
BASE="spinning-archive/copilot-promote-test"

kubectl --context $CTX -n $NS exec $TB -- zfs create -p "$BASE"
kubectl --context $CTX -n $NS exec $TB -- zfs create "$BASE/vol1"
for i in 1 2 3 4 5 6; do
  kubectl --context $CTX -n $NS exec $TB -- sh -c "echo data-t$i > /$BASE/vol1/f\$i 2>/dev/null; zfs snapshot $BASE/vol1@snap_t$i"
done
kubectl --context $CTX -n $NS exec $TB -- zfs list -o name,origin,creation -r -t all "$BASE"
```

Output (the in-container file writes failed harmlessly due to a mountpoint quirk in the
toolbox — irrelevant, since this test is purely about `origin`/promote mechanics, not
file content; all six snapshots were created successfully regardless):

```
NAME                                                ORIGIN  CREATION
spinning-archive/copilot-promote-test               -       1785505298
spinning-archive/copilot-promote-test/vol1          -       1785505299
spinning-archive/copilot-promote-test/vol1@snap_t1  -       1785505300
spinning-archive/copilot-promote-test/vol1@snap_t2  -       1785505300
spinning-archive/copilot-promote-test/vol1@snap_t3  -       1785505301
spinning-archive/copilot-promote-test/vol1@snap_t4  -       1785505301
spinning-archive/copilot-promote-test/vol1@snap_t5  -       1785505301
spinning-archive/copilot-promote-test/vol1@snap_t6  -       1785505301
```

### 3.2 Step 1 — create one backing clone per snapshot

```bash
for i in 1 2 3 4 5 6; do
  kubectl --context $CTX -n $NS exec $TB -- zfs clone -o canmount=off "$BASE/vol1@snap_t$i" "$BASE/csi-snap-t$i"
done
kubectl --context $CTX -n $NS exec $TB -- zfs list -o name,origin,creation -r -t all "$BASE"
```

```
NAME                                                ORIGIN                                              CREATION
spinning-archive/copilot-promote-test               -                                                   1785505298
spinning-archive/copilot-promote-test/csi-snap-t1   spinning-archive/copilot-promote-test/vol1@snap_t1  1785505364
spinning-archive/copilot-promote-test/csi-snap-t2   spinning-archive/copilot-promote-test/vol1@snap_t2  1785505364
spinning-archive/copilot-promote-test/csi-snap-t3   spinning-archive/copilot-promote-test/vol1@snap_t3  1785505365
spinning-archive/copilot-promote-test/csi-snap-t4   spinning-archive/copilot-promote-test/vol1@snap_t4  1785505365
spinning-archive/copilot-promote-test/csi-snap-t5   spinning-archive/copilot-promote-test/vol1@snap_t5  1785505366
spinning-archive/copilot-promote-test/csi-snap-t6   spinning-archive/copilot-promote-test/vol1@snap_t6  1785505366
spinning-archive/copilot-promote-test/vol1          -                                                   1785505299
spinning-archive/copilot-promote-test/vol1@snap_t1  -                                                   1785505300
spinning-archive/copilot-promote-test/vol1@snap_t2  -                                                   1785505300
spinning-archive/copilot-promote-test/vol1@snap_t3  -                                                   1785505301
spinning-archive/copilot-promote-test/vol1@snap_t4  -                                                   1785505301
spinning-archive/copilot-promote-test/vol1@snap_t5  -                                                   1785505301
spinning-archive/copilot-promote-test/vol1@snap_t6  -                                                   1785505301
```

Baseline confirmed: six clones, each cloned directly from its own corresponding
`vol1@snap_tN`, exactly matching the `standalone`-mode design (D0/D15).

### 3.3 Step 2 — promote `csi-snap-t3` first (scrambled order test, step 1 of the sequence: t3, t1, t6, t2, t4, t5)

```bash
kubectl --context $CTX -n $NS exec $TB -- zfs promote "$BASE/csi-snap-t3"
kubectl --context $CTX -n $NS exec $TB -- zfs list -o name,origin,creation -r -t all "$BASE"
```

```
NAME                                                       ORIGIN                                                     CREATION
spinning-archive/copilot-promote-test                      -                                                          1785505298
spinning-archive/copilot-promote-test/csi-snap-t1          spinning-archive/copilot-promote-test/csi-snap-t3@snap_t1  1785505364
spinning-archive/copilot-promote-test/csi-snap-t2          spinning-archive/copilot-promote-test/csi-snap-t3@snap_t2  1785505364
spinning-archive/copilot-promote-test/csi-snap-t3          -                                                          1785505365
spinning-archive/copilot-promote-test/csi-snap-t3@snap_t1  -                                                          1785505300
spinning-archive/copilot-promote-test/csi-snap-t3@snap_t2  -                                                          1785505300
spinning-archive/copilot-promote-test/csi-snap-t3@snap_t3  -                                                          1785505301
spinning-archive/copilot-promote-test/csi-snap-t4          spinning-archive/copilot-promote-test/vol1@snap_t4         1785505365
spinning-archive/copilot-promote-test/csi-snap-t5          spinning-archive/copilot-promote-test/vol1@snap_t5         1785505366
spinning-archive/copilot-promote-test/csi-snap-t6          spinning-archive/copilot-promote-test/vol1@snap_t6         1785505366
spinning-archive/copilot-promote-test/vol1                 spinning-archive/copilot-promote-test/csi-snap-t3@snap_t3  1785505299
spinning-archive/copilot-promote-test/vol1@snap_t4         -                                                          1785505301
spinning-archive/copilot-promote-test/vol1@snap_t5         -                                                          1785505301
spinning-archive/copilot-promote-test/vol1@snap_t6         -                                                          1785505301
```

**Observation:** promoting `csi-snap-t3` pulled `snap_t1` and `snap_t2` along with it
(both now live under `csi-snap-t3`, e.g. `csi-snap-t3@snap_t1`). `csi-snap-t1`'s and
`csi-snap-t2`'s `origin` were automatically reassigned to point at their new location
under `csi-snap-t3`. `vol1` itself now shows `origin = csi-snap-t3@snap_t3` (the
parent/child relationship reversed) and retains only `snap_t4`/`snap_t5`/`snap_t6` of
its own.

### 3.4 Step 3 — promote `csi-snap-t1`

```bash
kubectl --context $CTX -n $NS exec $TB -- zfs promote "$BASE/csi-snap-t1"
kubectl --context $CTX -n $NS exec $TB -- zfs list -o name,origin,creation -r -t all "$BASE"
```

```
NAME                                                       ORIGIN                                                     CREATION
spinning-archive/copilot-promote-test                      -                                                          1785505298
spinning-archive/copilot-promote-test/csi-snap-t1          -                                                          1785505364
spinning-archive/copilot-promote-test/csi-snap-t1@snap_t1  -                                                          1785505300
spinning-archive/copilot-promote-test/csi-snap-t2          spinning-archive/copilot-promote-test/csi-snap-t3@snap_t2  1785505364
spinning-archive/copilot-promote-test/csi-snap-t3          spinning-archive/copilot-promote-test/csi-snap-t1@snap_t1  1785505365
spinning-archive/copilot-promote-test/csi-snap-t3@snap_t2  -                                                          1785505300
spinning-archive/copilot-promote-test/csi-snap-t3@snap_t3  -                                                          1785505301
spinning-archive/copilot-promote-test/csi-snap-t4          spinning-archive/copilot-promote-test/vol1@snap_t4         1785505365
spinning-archive/copilot-promote-test/csi-snap-t5          spinning-archive/copilot-promote-test/vol1@snap_t5         1785505366
spinning-archive/copilot-promote-test/csi-snap-t6          spinning-archive/copilot-promote-test/vol1@snap_t6         1785505366
spinning-archive/copilot-promote-test/vol1                 spinning-archive/copilot-promote-test/csi-snap-t3@snap_t3  1785505299
spinning-archive/copilot-promote-test/vol1@snap_t4         -                                                          1785505301
spinning-archive/copilot-promote-test/vol1@snap_t5         -                                                          1785505301
spinning-archive/copilot-promote-test/vol1@snap_t6         -                                                          1785505301
```

**Observation:** `csi-snap-t1` is now fully independent — `origin = -`, owns
`snap_t1` directly. `csi-snap-t3` (the former holder) correctly reverses: its own
`origin` now shows `csi-snap-t1@snap_t1`. `csi-snap-t3` retains `snap_t2`/`snap_t3`
(unaffected).

### 3.5 Step 4 — promote `csi-snap-t6` (the decisive test: does it steal from `csi-snap-t3` or from the now-independent `csi-snap-t1`?)

```bash
kubectl --context $CTX -n $NS exec $TB -- zfs promote "$BASE/csi-snap-t6"
kubectl --context $CTX -n $NS exec $TB -- zfs list -o name,origin,creation -r -t all "$BASE"
```

```
NAME                                                       ORIGIN                                                     CREATION
spinning-archive/copilot-promote-test                      -                                                          1785505298
spinning-archive/copilot-promote-test/csi-snap-t1          -                                                          1785505364
spinning-archive/copilot-promote-test/csi-snap-t1@snap_t1  -                                                          1785505300
spinning-archive/copilot-promote-test/csi-snap-t2          spinning-archive/copilot-promote-test/csi-snap-t3@snap_t2  1785505364
spinning-archive/copilot-promote-test/csi-snap-t3          spinning-archive/copilot-promote-test/csi-snap-t1@snap_t1  1785505365
spinning-archive/copilot-promote-test/csi-snap-t3@snap_t2  -                                                          1785505300
spinning-archive/copilot-promote-test/csi-snap-t3@snap_t3  -                                                          1785505301
spinning-archive/copilot-promote-test/csi-snap-t4          spinning-archive/copilot-promote-test/csi-snap-t6@snap_t4  1785505365
spinning-archive/copilot-promote-test/csi-snap-t5          spinning-archive/copilot-promote-test/csi-snap-t6@snap_t5  1785505366
spinning-archive/copilot-promote-test/csi-snap-t6          spinning-archive/copilot-promote-test/csi-snap-t3@snap_t3  1785505366
spinning-archive/copilot-promote-test/csi-snap-t6@snap_t4  -                                                          1785505301
spinning-archive/copilot-promote-test/csi-snap-t6@snap_t5  -                                                          1785505301
spinning-archive/copilot-promote-test/csi-snap-t6@snap_t6  -                                                          1785505301
spinning-archive/copilot-promote-test/vol1                 spinning-archive/copilot-promote-test/csi-snap-t6@snap_t6  1785505299
```

**Decisive observations:**
1. **`csi-snap-t1`'s `origin` is still `-`** — completely untouched by this promote.
   No steal-back of an already-independent clone occurred.
2. **`csi-snap-t6` only pulled `snap_t4`/`snap_t5`** (still directly on `vol1` at the
   time) plus its own `snap_t6`. It did **not** reach through and steal `snap_t2`/
   `snap_t3` away from `csi-snap-t3` — instead, `csi-snap-t6`'s own `origin` simply
   became `csi-snap-t3@snap_t3` (a new, reversed bookkeeping link), while `csi-snap-t3`
   keeps physical custody of `snap_t2`/`snap_t3` (both still listed directly under it).
3. This confirms `zfs promote`'s history-walk is bounded by the **current** clone-origin
   chain (it stops at whatever dataset currently owns the boundary snapshot), not an
   unconditional walk all the way back to the very first snapshot ever taken, as a
   literal reading of `snaplist_make`'s `first_obj=0` parameter had suggested was
   possible.

### 3.6 Step 5 — promote the remaining three (`t2`, `t4`, `t5`) to reach final convergence

```bash
kubectl --context $CTX -n $NS exec $TB -- zfs promote "$BASE/csi-snap-t2"
kubectl --context $CTX -n $NS exec $TB -- zfs promote "$BASE/csi-snap-t4"
kubectl --context $CTX -n $NS exec $TB -- zfs promote "$BASE/csi-snap-t5"
kubectl --context $CTX -n $NS exec $TB -- zfs list -o name,origin,creation -r -t all "$BASE"
kubectl --context $CTX -n $NS exec $TB -- zfs list -t snapshot -r "$BASE/vol1" 2>&1
```

```
NAME                                                       ORIGIN                                                     CREATION
spinning-archive/copilot-promote-test                      -                                                          1785505298
spinning-archive/copilot-promote-test/csi-snap-t1          -                                                          1785505364
spinning-archive/copilot-promote-test/csi-snap-t1@snap_t1  -                                                          1785505300
spinning-archive/copilot-promote-test/csi-snap-t2          spinning-archive/copilot-promote-test/csi-snap-t1@snap_t1  1785505364
spinning-archive/copilot-promote-test/csi-snap-t2@snap_t2  -                                                          1785505300
spinning-archive/copilot-promote-test/csi-snap-t3          spinning-archive/copilot-promote-test/csi-snap-t2@snap_t2  1785505365
spinning-archive/copilot-promote-test/csi-snap-t3@snap_t3  -                                                          1785505301
spinning-archive/copilot-promote-test/csi-snap-t4          spinning-archive/copilot-promote-test/csi-snap-t3@snap_t3  1785505365
spinning-archive/copilot-promote-test/csi-snap-t4@snap_t4  -                                                          1785505301
spinning-archive/copilot-promote-test/csi-snap-t5          spinning-archive/copilot-promote-test/csi-snap-t4@snap_t4  1785505366
spinning-archive/copilot-promote-test/csi-snap-t5@snap_t5  -                                                          1785505301
spinning-archive/copilot-promote-test/csi-snap-t6          spinning-archive/copilot-promote-test/csi-snap-t5@snap_t5  1785505366
spinning-archive/copilot-promote-test/csi-snap-t6@snap_t6  -                                                          1785505301
spinning-archive/copilot-promote-test/vol1                 spinning-archive/copilot-promote-test/csi-snap-t6@snap_t6  1785505299
```

```
$ zfs list -t snapshot -r spinning-archive/copilot-promote-test/vol1
no datasets available
```

**Final-state observations:**
- Every one of `csi-snap-t1`...`csi-snap-t6` owns **exactly and only its own**
  `@snap_tN` (confirmed in the listing: each `csi-snap-tN@snap_tN` line exists exactly
  once, under the correspondingly-named dataset).
- `vol1` has **zero snapshots of its own** — `zfs list -t snapshot -r vol1` returns
  "no datasets available". This confirms `zfs destroy vol1` (plain, no `-r`) would
  succeed at this point (not executed, per the no-destroy constraint — but the
  precondition for D11's plain destroy to succeed is directly confirmed).
- Each backing clone's `origin` property, **after being successfully promoted**, is
  **not** necessarily empty — it legitimately shows a link to the *previous* clone in
  the chain (e.g. `csi-snap-t2`'s origin is `csi-snap-t1@snap_t1` even though `t2` was
  itself successfully promoted and independently owns `snap_t2`). This is normal
  lineage bookkeeping, not a sign of an incomplete/failed promote, and does not block
  destroying that dataset.

## 4. Conclusions

### 4.1 D16 (promote iteration order for multiple snapshots of one source): CONFIRMED CORRECT, no bug, no design change needed

The scrambled iteration order (t3, t1, t6, t2, t4, t5) — chosen specifically to
stress-test both "does an earlier promote's claimed history get raided by a later one"
and "can an already-independent clone be stolen back from" — converges to the fully
correct end state: every backing clone independently owns exactly its own snapshot, and
the source ends up with zero snapshots of its own. **No steal-back of an
already-independent, previously-promoted clone was observed.** `zfs promote`'s
history-walk is bounded by the current clone-origin chain (stops at whatever dataset
currently holds the boundary snapshot), not an unconditional walk to the very first
snapshot ever taken, regardless of current ownership — contrary to what a literal
reading of `snaplist_make`'s `first_obj=0` parameter alone had suggested was possible.

**This empirically confirms the original (first) analysis of D16 was correct, and the
subsequent, more pessimistic re-analysis (based on source-reading alone, without a live
test) was overly cautious.** Both readings are preserved in the redesign doc's own
history (see its own §2.11 and the errata appended there) — this document is the
authoritative record of what was actually observed.

### 4.2 D13 (Promote primitive verification check): a real, separate bug FOUND and FIXED as a side effect of this test

The test surfaced an independent, previously undocumented issue: **a cleanly and
successfully promoted dataset does not necessarily end up with an empty `origin`.**
Multi-hop clone chains legitimately leave a non-empty `origin` pointing at whatever
came before them in the chain (e.g. `csi-snap-t2`'s `origin` = `csi-snap-t1@snap_t1`
even after `csi-snap-t2` was itself successfully, fully promoted). This is normal
lineage/accounting bookkeeping — it does **not** indicate an incomplete promote and
does **not** block destroying that dataset.

**Consequence:** the originally-documented D13 verification ("retry until `origin`
becomes empty") would have caused incorrect, needless retries (or an eventual false
hard-error) even on fully successful promotes in any multi-hop scenario — which,
per §4.1, is the normal/common case whenever a source has more than one snapshot.

**Corrected check (already applied to D13 in the redesign doc):** compare `origin`
immediately before and immediately after the `zfs promote` call; retry only if it is
**unchanged**. A changed value (to anything, including a different non-empty value)
means the call made real progress and should not be retried.

## 5. State left behind

- `spinning-archive/copilot-promote-test` (containing `vol1` and `csi-snap-t1`...
  `csi-snap-t6`, all now in the fully-converged state shown in §3.6) is **still present**
  on the live pool. Space usage is negligible (empty/near-empty test files only).
- **No destroy commands were run at any point during this test**, per explicit
  instruction. Cleanup (`zfs destroy -r spinning-archive/copilot-promote-test`) is
  deferred to a future, separate, explicit instruction.

## 6. Errata (2026-07-31): cross-checked against official OpenZFS documentation

Fetched and compared against `zfs-promote.8` and `zfsconcepts.7`
(openzfs.github.io/openzfs-docs) after the fact, at explicit request, to confirm the
findings above aren't just self-consistent but actually match ZFS's documented
behavior.

**Matches confirmed directly, quoting the docs:**
- `zfs-promote.8`: *"The snapshot that was cloned, and any snapshots previous to this
  snapshot, are now owned by the promoted clone."* — matches §3.3's observation exactly
  (`csi-snap-t3`'s promote pulled `snap_t1`/`snap_t2` along with `snap_t3`).
- `zfsconcepts.7`: *"The clone parent-child dependency relationship can be reversed
  [via promote]... This causes the 'origin' filesystem to become a clone of the
  specified filesystem."* — matches §3.3 exactly (`vol1` showed
  `origin = csi-snap-t3@snap_t3` after promoting `csi-snap-t3`).
- The simplest case in this test (promoting `csi-snap-t1`, §3.4, nothing precedes it)
  matched the docs' own single-clone example literally: `origin` became `-`.

**Where the docs don't go far enough to fully explain §3.5/§3.6 (not a contradiction,
an extension):** the official docs' only worked example (`zfs-promote.8` Example 1) is
a single clone, promoted once — they do not walk through repeated/chained promotes on a
dataset that's already on the "losing" side of an *earlier* promote. This is exactly
why `csi-snap-t6`'s own `origin` was non-empty (`csi-snap-t3@snap_t3`) immediately after
being promoted (§3.5), which read as potentially contradicting "no longer depend on
origin snapshot" if taken naively. Reasoned through against the documented single-hop
rule: `vol1` had *already* acquired its own inherited `origin` link (to `csi-snap-t3`,
from the earlier promote of `t3` in §3.3) before `t6` was promoted. When `t6` is
promoted, `vol1` correctly "becomes a clone of `csi-snap-t6`" (per the doc) — but
`vol1`'s own prior inherited link doesn't disappear, it transfers onto `csi-snap-t6`,
which now stands in `vol1`'s place at the front of that reversed sub-chain. So a
non-empty `origin` on a just-promoted dataset, in a chained scenario, represents
**inherited custody of further-back lineage**, not a continuation of the dependency it
just detached from — a logical, repeated application of the documented single-hop rule,
not a deviation from it. This directly explains and further justifies the §4.2 D13
correction: checking for "did `origin` change" rather than "is `origin` empty" is
correct precisely because a non-empty `origin` after a successful promote is expected
and normal once more than one hop is involved.

