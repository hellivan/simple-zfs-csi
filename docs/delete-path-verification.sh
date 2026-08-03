#!/usr/bin/env bash
# Real-pool verification of the DELETE path, for
# docs/snapshot-lifecycle-redesign.md §9.2 item 10 / D23 (ADR-0020).
#
# WHY THIS EXISTS
# ---------------
# The 2026-07-31 run (docs/promote-order-test.sh) verified that a *batch of
# promotes* converges, and deliberately executed no `zfs destroy` at all. Every
# defect the 2026-08-03 review found lived in the destroy sequences that run
# after those promotes, so that run cannot speak to them. This script exercises
# exactly the missing half.
#
# WHAT IT ACTUALLY TESTS
# ----------------------
# `detach_and_destroy` below is a faithful shell transcription of the driver's
# `detachAndCleanSnapshots` (internal/controller/promote.go), and
# `detach_snapshot` of `detachSnapshotClones` (D19). So this validates two
# things at once: that real ZFS behaves the way the Go test double models it,
# and that the algorithm built on that model actually terminates and succeeds
# against real ZFS. A green run here is what the unit tests cannot give you.
#
# SAFETY
# ------
#   * Everything happens under $POOL/$TESTROOT, created fresh by this script.
#     It refuses to start if that subtree already exists.
#   * Unlike the 2026-07-31 run, this one DOES destroy things — that is the
#     point. Every destroy is non-recursive (`zfs destroy`, never `-r`), and
#     every path is verified to be under $BASE before any destroy is issued.
#   * `-r` appears exactly once, in the optional `cleanup` subcommand.
#   * Nothing outside $BASE is read, modified or destroyed.
#
# USAGE
# -----
#   ./delete-path-verification.sh <pool>            # run all four scenarios
#   ./delete-path-verification.sh <pool> cleanup    # remove the test subtree
#
# Directly on the storage host:
#   sudo ./delete-path-verification.sh spinning-archive
#
# Or through the toolbox pod (no need to copy the file in):
#   kubectl -n simple-zfs-csi exec -i <toolbox-pod> -- bash -s -- spinning-archive \
#     < docs/delete-path-verification.sh
#
# Capture the output — it is the record, same as the 2026-07-31 log:
#   ... 2>&1 | tee /tmp/delete-path-verification-$(date +%F).log

set -euo pipefail

if [ -z "${BASH_VERSINFO:-}" ] || [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  echo "FATAL: bash >= 4 required (uses mapfile)." >&2
  exit 1
fi

POOL="${1:-}"
if [ -z "$POOL" ]; then
  echo "usage: $0 <pool> [cleanup]" >&2
  exit 1
fi
TESTROOT="csi-delete-path-test"
BASE="$POOL/$TESTROOT"

FAILURES=0
CHECKS=0

log()  { printf '%s\n' "$*"; }
step() { printf '\n=== %s ===\n' "$*"; }

# run echoes each ZFS command before executing it, so the captured output is a
# complete, replayable transcript.
run() {
  printf '  $ %s\n' "$*"
  "$@"
}

# guard refuses any destructive operation on a path outside the test subtree.
guard() {
  case "$1" in
    "$BASE"|"$BASE"/*|"$BASE"@*) ;;
    *) printf 'FATAL: refusing to touch %s outside %s\n' "$1" "$BASE" >&2; exit 1 ;;
  esac
}

check() {
  local desc="$1" cond="$2"
  CHECKS=$((CHECKS + 1))
  if [ "$cond" = "ok" ]; then
    log "  PASS: $desc"
  else
    log "  FAIL: $desc"
    FAILURES=$((FAILURES + 1))
  fi
}

exists()      { zfs list -H -o name "$1" >/dev/null 2>&1; }
check_gone()  { if exists "$1"; then check "$1 is gone" bad; else check "$1 is gone" ok; fi; }
check_alive() { if exists "$1"; then check "$1 still exists" ok; else check "$1 still exists" bad; fi; }

snapshots_of() { zfs list -H -o name -t snapshot -d 1 -s creation "$1" 2>/dev/null || true; }

clones_of() {
  local v
  v="$(zfs get -H -o value clones "$1" 2>/dev/null || echo -)"
  if [ -z "$v" ] || [ "$v" = "-" ]; then
    return 0
  fi
  printf '%s\n' "$v" | tr ',' '\n' | sed '/^$/d'
}

# detach_and_destroy mirrors internal/controller/promote.go's
# detachAndCleanSnapshots + the plain destroy that follows it.
detach_and_destroy() {
  local ds="$1" round snaps snap clones promoted
  guard "$ds"
  log "-> detach_and_destroy $ds"
  for ((round = 0; round < 100; round++)); do
    mapfile -t snaps < <(snapshots_of "$ds")
    [ "${#snaps[@]}" -eq 0 ] && break
    promoted=0
    for snap in "${snaps[@]}"; do
      mapfile -t clones < <(clones_of "$snap")
      [ "${#clones[@]}" -eq 0 ] && continue
      guard "${clones[0]}"
      run zfs promote "${clones[0]}"
      promoted=1
      break
    done
    [ "$promoted" -eq 1 ] && continue
    # Nothing clones the remainder: leftover driver artifacts, destroy directly.
    for snap in "${snaps[@]}"; do
      guard "$snap"
      run zfs destroy "$snap"
    done
    break
  done
  run zfs destroy "$ds"   # non-recursive, deliberately: D11/D22
}

# detach_snapshot mirrors detachSnapshotClones (D19): promote away everything
# that clones a single snapshot so it can be destroyed on its own.
detach_snapshot() {
  local snap="$1" round clones
  guard "$snap"
  log "-> detach_snapshot $snap"
  for ((round = 0; round < 100; round++)); do
    mapfile -t clones < <(clones_of "$snap")
    [ "${#clones[@]}" -eq 0 ] && break
    guard "${clones[0]}"
    run zfs promote "${clones[0]}"
  done
  if zfs list -H -o name -t snapshot "$snap" >/dev/null 2>&1; then
    run zfs destroy "$snap"
  else
    log "  (already relocated by the promote above -> destroy is a no-op)"
  fi
}

dump() {
  log "--- state: $1 ---"
  zfs list -o name,origin -r -t all "$BASE" 2>&1 || true
  log "--- end ---"
}

# --------------------------------------------------------------------------
# Scenario A (F2a): direct PVC-to-PVC clone; delete source, then delete clone.
# The clone inherits the "@clone-<hash>" snapshot and must stay deletable.
# --------------------------------------------------------------------------
scenario_a() {
  step "A (F2a): direct clone -> delete source -> delete clone"
  local a="$BASE/a"
  run zfs create -p "$a"
  run zfs create "$a/src"
  run zfs snapshot "$a/src@clone-0123456789abcdef"
  run zfs clone "$a/src@clone-0123456789abcdef" "$a/clone"
  dump "A: initial"

  detach_and_destroy "$a/src"
  check_gone "$a/src"
  dump "A: after deleting the source"

  detach_and_destroy "$a/clone"
  check_gone "$a/clone"
}

# --------------------------------------------------------------------------
# Scenario B (F2b): restore from a standalone snapshot, then delete the
# snapshot while the restored volume is live, then delete the restored volume,
# then the original source. This is the sequence that used to wedge a
# ZfsSnapshot in Terminating forever.
# --------------------------------------------------------------------------
scenario_b() {
  step "B (F2b): restore -> delete snapshot (live restore) -> delete restore -> delete source"
  local b="$BASE/b"
  run zfs create -p "$b"
  run zfs create "$b/vol"
  run zfs snapshot "$b/vol@csi-snap-x"
  run zfs clone "$b/vol@csi-snap-x" "$b/bc"
  run zfs snapshot "$b/bc@restore-source"
  run zfs clone "$b/bc@restore-source" "$b/restored"
  dump "B: initial"

  log "DeleteSnapshot step 1: tear down the backing clone"
  detach_and_destroy "$b/bc"
  check_gone "$b/bc"

  log "DeleteSnapshot step 2: required raw-origin cleanup (D19)"
  detach_snapshot "$b/vol@csi-snap-x"
  dump "B: after the snapshot is fully deleted"

  detach_and_destroy "$b/restored"
  check_gone "$b/restored"

  detach_and_destroy "$b/vol"
  check_gone "$b/vol"
}

# --------------------------------------------------------------------------
# Scenario C (F2c): two simultaneous restores from one snapshot, then delete
# the snapshot, then delete both restores in either order. Promoting one
# re-parents the other onto it, which is what the old tracking got wrong.
# --------------------------------------------------------------------------
scenario_c() {
  step "C (F2c): two restores -> delete snapshot -> delete both"
  local c="$BASE/c"
  run zfs create -p "$c"
  run zfs create "$c/vol"
  run zfs snapshot "$c/vol@csi-snap-y"
  run zfs clone "$c/vol@csi-snap-y" "$c/bc"
  run zfs snapshot "$c/bc@restore-source"
  run zfs clone "$c/bc@restore-source" "$c/r1"
  run zfs clone "$c/bc@restore-source" "$c/r2"
  dump "C: initial"

  detach_and_destroy "$c/bc"
  check_gone "$c/bc"
  detach_snapshot "$c/vol@csi-snap-y"
  dump "C: after the snapshot is fully deleted"

  check_alive "$c/r1"
  check_alive "$c/r2"

  # Deliberately delete the one that ended up owning the shared history first.
  detach_and_destroy "$c/r1"
  check_gone "$c/r1"
  detach_and_destroy "$c/r2"
  check_gone "$c/r2"
  detach_and_destroy "$c/vol"
  check_gone "$c/vol"
}

# --------------------------------------------------------------------------
# Scenario D (F2d): six standalone snapshots of one volume; delete the volume,
# then delete each snapshot in scrambled order. After the volume goes, the
# backing clones are chained to one another — the state the 2026-07-31 run
# recorded but never tried to delete.
# --------------------------------------------------------------------------
scenario_d() {
  step "D (F2d): 6 snapshots -> delete volume -> delete each snapshot, scrambled"
  local d="$BASE/d" i
  run zfs create -p "$d"
  run zfs create "$d/vol"
  for i in 1 2 3 4 5 6; do
    run zfs snapshot "$d/vol@csi-snap-t$i"
    run zfs clone "$d/vol@csi-snap-t$i" "$d/bc$i"
    run zfs snapshot "$d/bc$i@restore-source"
  done
  dump "D: initial"

  log "DeleteVolume on the source"
  detach_and_destroy "$d/vol"
  check_gone "$d/vol"
  dump "D: after DeleteVolume (note the chained origins)"

  for i in 3 1 6 2 4 5; do
    log "DeleteSnapshot t$i"
    detach_and_destroy "$d/bc$i"
    check_gone "$d/bc$i"
  done
  dump "D: final (should be empty apart from the container datasets)"
}

# --------------------------------------------------------------------------

case "${2:-run}" in
  cleanup)
    guard "$BASE"
    echo "Destroying the whole test subtree $BASE (this is the only use of -r)."
    zfs destroy -r "$BASE"
    echo "done."
    exit 0
    ;;
  run) ;;
  *) echo "usage: $0 <pool> [cleanup]" >&2; exit 1 ;;
esac

if ! zpool list -H -o name "$POOL" >/dev/null 2>&1; then
  echo "FATAL: pool $POOL not found." >&2
  exit 1
fi
if exists "$BASE"; then
  echo "FATAL: $BASE already exists. Inspect it, then run: $0 $POOL cleanup" >&2
  exit 1
fi

log "pool:      $POOL"
log "test root: $BASE (created now, nothing else is touched)"
run zfs create "$BASE"

scenario_a
scenario_b
scenario_c
scenario_d

step "RESULT"
log "checks run: $CHECKS, failures: $FAILURES"
if [ "$FAILURES" -ne 0 ]; then
  log "FAILED — the delete path does not behave as designed on this pool."
  log "Leave the subtree in place for inspection; do not run cleanup yet."
  exit 1
fi
log "PASSED — every object was removed with non-recursive destroys only."
log "Clean up with: $0 $POOL cleanup"
