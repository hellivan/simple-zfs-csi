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

- **A dataset the driver has provisioned is never re-created (ADR-0037).**
  Whichever order you rename and edit in, a reconcile that lands between the
  `zfs rename` and the `spec.dataset` edit finds nothing at the path the CR
  names and marks the `ZfsDataset` `Lost` — it does not create an empty dataset.
  Once both steps are done the object returns to `Ready` (re-checked every
  minute). While it is `Lost`, publishes of the volume fail fast. Do not delete
  the object to "fix" it.

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

---

## Migrating to the raw-only snapshot design (strategy D) and upgrading the driver

One-time procedure for the upgrade from `0.0.44` (or any earlier version) to `0.0.45`, the first
release containing ADR-0042. Executed on 2026-10-19, see
[migration-0.0.45-report-2026-10-19.md](migration-0.0.45-report-2026-10-19.md). Run it from any machine; it needs nothing from the
chat session. Background: [snapshot-delete-redesign-plan.md](snapshot-delete-redesign-plan.md).

**What it does.** `zfs-migrate` stamps ownership labels on every driver-created
dataset and snapshot, and marks every `@restore-source` snapshot with
`zfs destroy -d`. The new driver contains no legacy handling, so the migration
must run before it starts and must have finished completely.

**It is one-way.** `@restore-source` is gone after the migration. The *old*
driver restores from it, so after the migration a restore from an
existing snapshot only works with the new driver. There is no automatic
rollback. Everything else (existing volumes, snapshots, normal use) keeps
working with the old driver.

### Prerequisites (the other machine)

- `git`, `kubectl`, and Go matching `go` in `go.mod` (1.26.4 at the time of writing).
- A kube context with rights to: list pods and `exec` in namespace `simple-zfs-csi`;
  get/list/update `zfsdatasets`, `zfssnapshots` and their `/status`; get/list `zfspools`.
  Cluster-admin is sufficient.
- The toolbox must be enabled (`toolbox.enabled: true`, it is in the gitops values).
  The tool runs every `zfs` command through `kubectl exec` in the pod labelled
  `app.kubernetes.io/component=toolbox`.
- The release exists: tag `vX.Y.Z` pushed, the GitHub `release` workflow finished,
  chart `oci://ghcr.io/hellivan/charts/simple-zfs-csi` version `X.Y.Z` is available.
  The bump of `targetRevision` in
  `kube-sl-home-gitops/apps/csi/simple-zfs-csi.application.yaml` is prepared but
  **not merged yet**.

```bash
git clone https://github.com/hellivan/simple-zfs-csi && cd simple-zfs-csi
git checkout vX.Y.Z
go build -o zfs-migrate ./cmd/zfs-migrate
export CTX=admin@kube-sl-home        # your kube context
kubectl --context "$CTX" -n simple-zfs-csi get pods -l app.kubernetes.io/component=toolbox
```

The last command must show one `Running` toolbox pod per pool node.

### 1. Quiet window

Between the migration (step 4) and the new driver being Ready (step 5) nothing
may snapshot, restore or delete. Pick a window of about 15 minutes and:

- Avoid the Velero schedule (`0 2 * * *`), or suspend it:
  `kubectl --context "$CTX" -n velero get schedules` then
  `kubectl --context "$CTX" -n velero patch schedule <name> --type merge -p '{"spec":{"paused":true}}'`.
- Pause any other snapshot or restore producer (CronJobs, other backup tools).
- Check nothing is in flight; every list must be empty or fully `READYTOUSE=true`
  and without a deletion timestamp:

```bash
kubectl --context "$CTX" get volumesnapshot,volumegroupsnapshot -A
kubectl --context "$CTX" get zfssnapshots -o wide
kubectl --context "$CTX" get zfssnapshots,zfsdatasets -o json \
  | jq -r '.items[]|select(.metadata.deletionTimestamp)|.kind+"/"+.metadata.name'   # must print nothing
```

### 2. Safety copies (read-only)

```bash
mkdir -p mig-$(date +%F) && cd mig-$(date +%F)
for k in zfspools zfsdatasets zfssnapshots; do kubectl --context "$CTX" get $k -o yaml > $k.yaml; done
T=$(kubectl --context "$CTX" -n simple-zfs-csi get pod -l app.kubernetes.io/component=toolbox -o name | head -1)
kubectl --context "$CTX" -n simple-zfs-csi exec "$T" -- zfs list -t all -r -o name,origin,clones,defer_destroy <POOL>/<CSI-ROOT> > zfs-before.txt
cd ..
```

(Replace `<POOL>/<CSI-ROOT>` with the dataset prefix the driver uses, the same
roots as in the `ZfsPool` CRs.) These do not make `@restore-source` restorable;
they are for diagnosis.

### 3. Dry run (changes nothing)

```bash
./zfs-migrate --context "$CTX" | tee mig-$(date +%F)/dryrun.txt; echo "exit=${PIPESTATUS[0]}"
```

Exit codes: `0` nothing to do, `3` changes pending (expected now), `1` error or
blockers. Without `--apply` the tool only reads.

Continue only if the output shows **zero blockers and zero unexpected
findings**. The previous run on this cluster showed 617 actions: label `set`
actions on about 265 objects (87 raw snapshots, 87 backing clones, 91 volumes)
and 87 `destroy-deferred` for `@restore-source`, with no `provisionedAt` or
`sourceType` stamps. Counts will have drifted; a different *kind* of action, or
any blocker, means stop and investigate (the message names the object).

### 4. Apply

```bash
./zfs-migrate --context "$CTX" --apply | tee mig-$(date +%F)/apply.txt; echo "exit=${PIPESTATUS[0]}"
```

It re-audits at the end and exits `1` unless the audit is clean. Safe to re-run
(idempotent) if it is interrupted. From here on do not delete snapshots or volumes
until step 6 is done.

Every `zfs` command is one `kubectl exec`, so a network or DNS blip aborts the run midway
(seen on 2026-10-19: `lookup kube.sl.home: no such host`). The `restore-source` marks run
before the labels, so an interrupted apply has usually already passed the one-way point:
fix the connection, re-run `--apply`, and upgrade the driver without further delay.

### 5. Upgrade the driver

1. Merge the `targetRevision: X.Y.Z` bump in the gitops repo and sync the `simple-zfs-csi` Application.
2. Check the CRDs were updated (Helm does not upgrade `crds/`; Argo CD applies them on sync, but verify):
   ```bash
   kubectl --context "$CTX" get crd zfsdatasets.storage.simple-zfs-csi.io -o yaml | grep -c 'self.source\|oldSelf'   # > 0
   ```
   If the new rules are missing, apply them by hand from the release checkout:
   `kubectl --context "$CTX" apply --server-side --force-conflicts -f charts/simple-zfs-csi/crds/`.
3. Wait for rollout and read the logs:
   ```bash
   kubectl --context "$CTX" -n simple-zfs-csi get pods
   kubectl --context "$CTX" -n simple-zfs-csi logs -l app.kubernetes.io/component=controller --all-containers --tail=100
   ```
   Every pod must be Ready, with no `Error`/`panic` lines and no `InvalidSnapshot` errors on `ZfsSnapshot`s
   (`kubectl --context "$CTX" get zfssnapshots`).

### 6. Straggler pass

Anything the old driver created between steps 4 and 5 is unlabelled:

```bash
./zfs-migrate --context "$CTX"          # expect exit 0
./zfs-migrate --context "$CTX" --apply  # only if the dry run reported pending changes
```

Then un-pause Velero / the other jobs.

### 7. Live test (throwaway namespace)

```bash
kubectl --context "$CTX" create ns mig-test
cat <<'Y' | kubectl --context "$CTX" -n mig-test apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: src}
spec: {accessModes: [ReadWriteOnce], storageClassName: generic-scratch, resources: {requests: {storage: 1Gi}}}
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: s1}
spec: {volumeSnapshotClassName: <YOUR-SNAPSHOT-CLASS>, source: {persistentVolumeClaimName: src}}
Y
kubectl --context "$CTX" -n mig-test get volumesnapshot s1 -w     # READYTOUSE true
cat <<'Y' | kubectl --context "$CTX" -n mig-test apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: restored}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: generic-scratch
  resources: {requests: {storage: 1Gi}}
  dataSource: {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: s1}
Y
```

(Get `<YOUR-SNAPSHOT-CLASS>` from `kubectl get volumesnapshotclass`; `generic-scratch` is `Delete`-reclaim.)
Then delete in the orders that were the failure cases, one run each (recreate between runs):
snapshot first (`delete volumesnapshot s1`), then `restored`, then `src`; and
`src` first, then `restored`, then the snapshot. After each run,
`zfs list -t all -r <POOL>/<CSI-ROOT>` through the toolbox must show no leftover
`csi-snap-*`, `clone-*` or `restore-source` for the test objects, and
`kubectl get zfsdatasets,zfssnapshots` no remaining test objects. Finally `kubectl delete ns mig-test`.
Optionally repeat with a `VolumeGroupSnapshot` of two PVCs using class `zfs-group-snapshot`.

### If something goes wrong

- `zfs-migrate` stops with a blocker or error: nothing further has been changed beyond what the output lists; fix the named object and re-run.
- A restore from an old snapshot fails *before* step 5 completes: expected (see above); finish the upgrade.
- New-driver delete refuses an object (`not a driver-owned snapshot/dataset`): it is unlabelled; run the straggler pass.
- Returning to the old driver is possible for volumes and new snapshots but not for restores from snapshots that existed at migration time.
