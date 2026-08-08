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
| `storage.simple-zfs-csi.io/zfsshareattachrequest` | `ZfsShareAttachRequest` | Force one last volume-level recompute (and explicit teardown of the aggregated `ZfsShare` + DH-CHAP `Secret`) before request disappears | Without it, last-request removal can be missed and stale exports/allow-lists can remain |
| `storage.simple-zfs-csi.io/networkexport` | `NetworkExport` | Force one last host-side unexport (`/etc/exports` line or nvmet configfs subsystem) by the owning node's `NFSReconciler`/`NVMeoFReconciler` before the object disappears | Without it, deleting the last export for a node while that node's exporter pod is down leaves nothing in a later List to trigger the unexport — the host-side artifact can persist indefinitely |

### Why the attach-request finalizer exists

Yes, the reconciler recomputes from the full attach-request set (`activeNodes`)
and applies the share from that set each pass.  
But this still requires a reconcile pass *while the deleting request is still
observable*. The finalizer guarantees that pass (the object is terminating, not
gone), so recompute/teardown happens deterministically before deletion finishes.

### Why the `NetworkExport` finalizer exists

`NFSReconciler`/`NVMeoFReconciler` each fully re-render their node's host state
from every currently-live `NetworkExport` on every reconcile
(`listOwnedExports`, which already excludes anything with a
`DeletionTimestamp`). That is correct only if *some* reconcile actually fires
after deletion. Before this finalizer, deleting a `NetworkExport` removed it
from etcd immediately; if the owning node's exporter pod happened to be down
at that instant, its next startup List would never see the object again —
there would be nothing left to generate the event that tells it to drop the
stale export. The finalizer keeps the object present (Terminating) until its
own exporter has explicitly re-rendered around its removal, so even a pod that
restarts after being down still sees it once and reacts correctly. See
ADR-0032 in [design-decisions.md](design-decisions.md).

### How the three finalizers fit together (attach → share → export)

The full per-volume cleanup chain, in order:

1. **`ZfsShareAttachRequest`** (one per node/volume attach) carries its own
   finalizer. On delete, it recomputes the aggregated `ZfsShare` from the
   remaining attach requests and explicitly `Delete()`s the `ZfsShare` and its
   DH-CHAP `Secret` once no attach requests reference them, *before* releasing
   its own finalizer.
2. **`ZfsShare`** has **no finalizer of its own**. Its child `NetworkExport`
   is created with `SetControllerReference`, so deleting the `ZfsShare`
   triggers Kubernetes' built-in owner-reference garbage collection to delete
   the `NetworkExport` — no explicit cleanup code is needed in
   `zfsshare_controller.go`.
3. **`NetworkExport`** carries the finalizer described above. The GC-issued
   delete only marks it Terminating; `NFSReconciler`/`NVMeoFReconciler` must
   still unexport it host-side and remove the finalizer before it actually
   disappears. This means the owner-reference cascade from step 2 is not
   instantaneous — it completes only once the owning node's exporter has
   converged — which is the intended trade-off (a brief Terminating window in
   exchange for guaranteed convergence, even across exporter-pod downtime).

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

- **“Does `ZfsShare` have its own finalizer to clean up its `NetworkExport`?”**  
  No — `ZfsShare` relies on owner-reference garbage collection to delete its
  child `NetworkExport`. The `NetworkExport` itself carries the finalizer, so
  the GC-triggered delete is honored but not completed until the node
  exporter converges host state (see "How the three finalizers fit together"
  above).

