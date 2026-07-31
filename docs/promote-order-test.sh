#!/usr/bin/env bash
# Empirical test for docs/snapshot-lifecycle-redesign.md §2.11 / D16.
#
# Verifies exactly what `zfs promote`'s history-walk does when a source dataset
# has multiple, independent snapshots, each with its own backing clone, and
# whether promoting one out of order can "steal" a snapshot away from an
# already-independent, previously-promoted clone.
#
# SAFETY: everything happens under $POOL/$TESTROOT, a disposable subtree.
# Nothing else on the pool is touched. Run `zfs destroy -r $POOL/$TESTROOT`
# at the end (or after any step) to clean up.
#
# Usage: edit POOL below, then run each STEP function in order, pasting the
# `zfs list` output back after each one.

set -euo pipefail

POOL="yourpool"          # <-- EDIT THIS to your real pool name
TESTROOT="promotetest"   # disposable subtree, safe to destroy anytime
BASE="$POOL/$TESTROOT"

show() {
  echo "--- zfs list ($1) ---"
  zfs list -o name,origin,creation -r -t all "$BASE" 2>&1 || true
  echo "--- end ---"
}

step0_setup() {
  echo "### step0: create vol1 + 6 sequential snapshots"
  zfs create -p "$BASE"
  zfs create "$BASE/vol1"
  for i in 1 2 3 4 5 6; do
    echo "data at t$i" > "/$BASE/vol1/f$i" 2>/dev/null || true
    zfs snapshot "$BASE/vol1@snap_t$i"
  done
  show "after step0"
}

step1_clone_all() {
  echo "### step1: create one clone per snapshot (csi-snap-t1..t6)"
  for i in 1 2 3 4 5 6; do
    zfs clone "$BASE/vol1@snap_t$i" "$BASE/csi-snap-t$i"
  done
  show "after step1"
}

step2_promote_t3() {
  echo "### step2: promote csi-snap-t3 (expect it to also pull t1,t2)"
  zfs promote "$BASE/csi-snap-t3"
  show "after promoting csi-snap-t3"
  echo ">>> CHECK: does csi-snap-t3 now list snap_t1, snap_t2, snap_t3 under it?"
  echo ">>> CHECK: does vol1 still show snap_t1/snap_t2, or are they gone from vol1?"
}

step3_promote_t1() {
  echo "### step3: promote csi-snap-t1 (expect to detach cleanly from wherever t1 now lives)"
  zfs promote "$BASE/csi-snap-t1"
  show "after promoting csi-snap-t1"
  echo ">>> CHECK: csi-snap-t1 origin should now be '-' (fully independent)"
  echo ">>> CHECK: does csi-snap-t1 now own snap_t1 directly?"
}

step4_promote_t6() {
  echo "### step4: promote csi-snap-t6 -- THE KEY TEST"
  echo "    (does this reach through csi-snap-t3's remaining t2 AND steal t1 back"
  echo "     from the now-independent csi-snap-t1?)"
  zfs promote "$BASE/csi-snap-t6"
  show "after promoting csi-snap-t6"
  echo ">>> CHECK #1: is csi-snap-t1's origin STILL '-' (still independent), or did"
  echo "              it become non-empty again (i.e. did it get re-attached/stolen)?"
  echo ">>> CHECK #2: does csi-snap-t6 now list snap_t2 (stolen from csi-snap-t3)?"
  echo ">>> CHECK #3: does csi-snap-t6 list snap_t1 at all (would mean steal-back happened)?"
}

step5_promote_rest() {
  echo "### step5: promote the remaining ones (t2, t4, t5) to reach the final state"
  zfs promote "$BASE/csi-snap-t2"
  zfs promote "$BASE/csi-snap-t4"
  zfs promote "$BASE/csi-snap-t5"
  show "after promoting t2, t4, t5"
  echo ">>> CHECK: does EVERY csi-snap-tN now show origin '-' (all independent)?"
  echo ">>> CHECK: does EVERY csi-snap-tN own exactly its own snap_tN and nothing else?"
  echo ">>> CHECK: does vol1 have zero remaining snapshots (zfs list -t snapshot vol1)?"
}

cleanup() {
  echo "### cleanup: destroying $BASE recursively"
  zfs destroy -r "$BASE"
}

# Run one step at a time, e.g.:
#   source promote-order-test.sh
#   step0_setup
#   step1_clone_all
#   step2_promote_t3
#   step3_promote_t1
#   step4_promote_t6     <-- the decisive step
#   step5_promote_rest
#   cleanup
