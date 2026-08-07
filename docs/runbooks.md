# Operational Runbooks

Step-by-step procedures for operations that touch on-disk ZFS state directly.
Each step says *why* it exists, because the ordering is what makes these safe.
The rationale behind the behaviour they rely on lives in
[design-decisions.md](design-decisions.md); the failure modes they avoid are
catalogued in [known-pitfalls.md](known-pitfalls.md).

---

## Renaming a dataset (repointing a `ZfsDataset` at a different path)

This is supported. `ZfsDataset.spec.dataset` names a *location*, and location
fields are deliberately left mutable so they can be repointed by hand
([api-conventions.md](api-conventions.md) §5). The `PersistentVolume` is never
touched: its `volumeHandle` is the `ZfsDataset`'s object name, which never
changes.

### Preconditions

- **No consumer pods running against the volume.** Renaming under a live mount
  breaks it at the ZFS level — the exported path stops existing and NFS clients
  are left with stale handles. No amount of reconciliation repairs an
  already-broken mount, so the scale-down is not a formality.
- The pool is imported, and `ZfsPool.status.currentNode` names the node you will
  run `zfs rename` on.

### Procedure

1. Scale consumers to zero and confirm the export is gone:

   ```sh
   kubectl scale deploy/<app> --replicas=0
   kubectl get zfsshare <volume-id>       # expect NotFound
   kubectl get networkexport <volume-id>  # expect NotFound
   ```

   The `ZfsShare` is rebuilt from a live read of the `ZfsDataset` at the *next
   attach* — it is not recomputed continuously. Its absence here is exactly what
   guarantees the new path is picked up on scale-up.

2. Rename on the storage node, in the host mount namespace:

   ```sh
   nsenter -t 1 -m -- zfs rename tank/k8s/<old> tank/k8s/<new>
   ```

   Snapshots move with their dataset; there is nothing else to rename.

3. Repoint the CR, immediately after step 2 (see Pitfalls for why the order and
   the gap both matter):

   ```sh
   kubectl patch zfsdataset <volume-id> --type=merge \
     -p '{"spec":{"dataset":"k8s/<new>"}}'
   ```

4. Scale back up and verify:

   ```sh
   kubectl scale deploy/<app> --replicas=1
   kubectl get zfsdataset <volume-id> -o jsonpath='{.status.path}{"\n"}'
   kubectl get networkexport <volume-id> -o jsonpath='{.spec.path}{"\n"}'
   ```

### What follows automatically

| Concern | Why there is nothing to do |
|---|---|
| Mount path on the node | `NodePublishVolume` re-resolves pool, path and protocol from the `ZfsDataset` on every publish (ADR-0022) |
| NFS export path / nvmet device path | `ZfsShare` → `NetworkExport` are re-rendered from the live CR at the next attach |
| Existing snapshots | resolved through `spec.sourceVolume`, not the path recorded when they were taken (ADR-0025) |
| The `PersistentVolume` | untouched — `volumeHandle` is the object name, not a path |

### Pitfalls

- **Do not edit the CR before renaming.** The reconciler creates the dataset
  whenever `spec.dataset` names an object that does not exist, so a reconcile in
  that window provisions an *empty* dataset at the new path — and the `zfs
  rename` in step 2 then fails with "dataset already exists".

- **Do not leave a long gap after renaming either.** In that order, a reconcile
  landing between steps 2 and 3 recreates an empty dataset at the *old* path —
  intended behaviour, not a defect: the CR still declares that a dataset must
  exist there (ADR-0026). It is orphaned — the finalizer only ever destroys the
  path currently in `spec.dataset` — so remove it by hand once you have
  confirmed it is empty:

  ```sh
  nsenter -t 1 -m -- zfs list -o name,used tank/k8s/<old>
  nsenter -t 1 -m -- zfs destroy tank/k8s/<old>
  ```

  Keeping steps 2 and 3 back to back shrinks this window to nothing in practice.

- **Changing the parent prefix affects future clones and restores.** Both reject
  a source whose parent differs from the target StorageClass's `datasetPrefix`
  (D6, `checkSamePrefix` in [../internal/csi/clone.go](../internal/csi/clone.go)).
  Renaming `k8s/a` → `k8s/b` is unaffected; renaming `k8s/a` → `archive/a` means
  the volume can no longer be cloned or restored from through a StorageClass
  with `datasetPrefix: k8s`.

---

## Keeping periodic-snapshot tools off the driver's datasets

A `zfs-auto-snapshot`-style tool, a TrueNAS periodic task, or anything feeding a
`zfs send -R` replication schedule takes snapshots the driver did not create. If
such a schedule covers a dataset the driver manages, **that PVC can no longer be
deleted**: `assertDriverSnapshot` (D18) refuses to destroy a snapshot the driver
does not own, which is correct — it will not destroy state it did not create —
and `DeleteVolume` reports it as `FAILED_PRECONDITION` ([ADR-0029](design-decisions.md)),
which surfaces on the PVC as:

```
snapshot "<pool>/<prefix>/csi-vol-<uuid>@auto-2026-08-06_00-00" was not created
by this driver; refusing to destroy it — remove it manually to continue
```

### Checking the current state

List every snapshot on the pool and see which parent datasets carry them:

```sh
zfs list -H -o name -t snapshot -r <pool> | sed 's/@.*//' | sort -u
```

Compare that against the prefixes the driver provisions into (the
`datasetPrefix` StorageClass parameter — e.g. `k8s/backup-tier`,
`k8s/scratch-tier`). **No dataset under a driver prefix should appear.**

Verified on `spinning-archive` (2026-08-07): 837 snapshots, all `auto-*`, and
every one of them on a non-`k8s/` dataset — `BACKUP`, `immich`, `paperless-ngx`,
`vaultwarden` and friends. No overlap today.

### If they do overlap

Either narrow the snapshot schedule so it excludes the driver's prefix, or accept
that those PVCs need the foreign snapshots removed by hand before they can be
deleted. Narrowing is strongly preferred: the driver deliberately will not delete
what it did not create, and that guard is what stops it from destroying a
replication source.

### Why the driver cannot simply skip them

A snapshot on the dataset blocks a non-recursive `zfs destroy`, so the dataset
cannot be removed while it exists. The alternatives are to destroy it (unsafe —
it may be a replication source) or to refuse (current behaviour). There is no
third option that both deletes the volume and preserves the snapshot, because the
snapshot lives *on* the volume's dataset.
