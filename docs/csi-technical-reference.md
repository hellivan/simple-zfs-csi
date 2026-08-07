# CSI technical reference (runtime flow + protection mechanisms)

This is the short operational reference for how this CSI driver behaves at
runtime, with a strict distinction between:

- **finalizers** (API deletion gates),
- **guards** (runtime checks in reconcile code),
- **upstream protections** (Kubernetes/sidecar finalizers outside this repo).

---

## 1) Runtime model in one screen

- `csi-controller` writes intent objects (`ZfsDataset`, `ZfsSnapshot`,
  `ZfsShareAttachRequest`).
- node agents execute ZFS/export work from those intents.
- `operator` aggregates attach requests into one `ZfsShare`, then `ZfsShare`
  compiles to `NetworkExport`.

Delete semantics are always two-step:

1. CSI/controller deletes CR object.
2. Reconciler finalizer performs host-side cleanup, then removes finalizer.

---

## 2) Finalizers used by this project (ours)

| Finalizer | Object | Purpose | Why needed |
| --- | --- | --- | --- |
| `storage.simple-zfs-csi.io/zfsdataset` | `ZfsDataset` | Run dataset/zvol teardown before object disappears | Without it, API deletion can orphan on-disk ZFS state |
| `storage.simple-zfs-csi.io/zfssnapshot` | `ZfsSnapshot` | Run snapshot teardown before object disappears | Without it, API deletion can orphan on-disk snapshots |
| `storage.simple-zfs-csi.io/zfsshareattachrequest` | `ZfsShareAttachRequest` | Force one last volume-level recompute before request disappears | Without it, last-request removal can be missed and stale exports/allow-lists can remain |

### Why the attach-request finalizer exists

Yes, the reconciler recomputes from the full attach-request set (`activeNodes`)
and applies the share from that set each pass.  
But this still requires a reconcile pass *while the deleting request is still
observable*. The finalizer guarantees that pass (the object is terminating, not
gone), so recompute/teardown happens deterministically before deletion finishes.

---

## 3) Guards used by this project (not finalizers)

These are plain function calls in delete preparation logic, not metadata keys.

| Guard | Type | Where used | Purpose |
| --- | --- | --- | --- |
| `checkSnapshotDependents` | function | `beforeDestroy` (`ZfsDataset`) | Block volume teardown while dependent snapshots are not yet `Ready` |
| `checkPendingCloneDependents` | function | `beforeDestroy` (`ZfsDataset`) | Block volume teardown while a clone dependent is declared but not yet materialized in ZFS |
| `assertDriverSnapshot` | function | promote/detach path | Refuse to destroy non-driver snapshots |
| `assertKnownDatasets` | function | promote/detach path | Refuse to promote foreign/unmanaged clone datasets |

Important: `checkSnapshotDependents` and `checkPendingCloneDependents` are
**not finalizers**; they are runtime guard checks that return an error (requeue)
when unsafe conditions are present.

---

## 4) Upstream protections (not implemented here)

These are provided by Kubernetes controllers/sidecars:

- `kubernetes.io/pvc-protection`
- `kubernetes.io/pv-protection`
- `provisioner.storage.kubernetes.io/cloning-protection`
- `snapshot.storage.kubernetes.io/pvc-as-source-protection`
- `snapshot.storage.kubernetes.io/volumesnapshot-as-source-protection`
- `provisioner.storage.kubernetes.io/volumesnapshot-as-source-protection`

See:

- [kubernetes-volume-lifecycle-facts.md](kubernetes-volume-lifecycle-facts.md)
- [lifecycle-protection-matrix.md](lifecycle-protection-matrix.md)

---

## 5) Quick answers to common confusion

- **“Is this a finalizer?”**  
  Only if it is a string in `metadata.finalizers` on an object.

- **“Is this protection a finalizer or a guard?”**  
  `checkSnapshotDependents` and `checkPendingCloneDependents` are guards
  (functions), not finalizers.

- **“Do we regenerate share config from all attach requests each time?”**  
  Yes. The finalizer is still used to guarantee a safe last recompute on delete.

