# Node-Local Passthrough Redesign — Research & Implementation Plan

**Status: Phase 1 implemented (2026-08-08).** See [ADR-0031](design-decisions.md).

## Implementation status

- **Done — node plugin:** `NodePublishVolume`/`NodeExpandVolume` route a zvol
  straight to its local device path (`ZfsDataset.status.path`) when
  `isLocalToPool` reports this node hosts the pool — no `nvme connect`, no
  `NetworkExport` dependency, no rescan. See `publishLocalZvol`/
  `isLocalToPool` in [internal/csi/node.go](../internal/csi/node.go) and the
  `TestNodePublish_NVMeoF_Local*`/`TestNodeExpand_NVMeoFLocal` tests in
  [internal/csi/node_test.go](../internal/csi/node_test.go).
- **Done — no network export for a local attach, with no new CRD field:**
  `ZfsShareAttachRequestReconciler.reconcileVolume` (the aggregator) already
  resolves the single winning attach node for a zvol (`oldestAttachNode`); it
  now also resolves that volume's `ZfsPool` and compares `CurrentNode` against
  the winner. When they match, it **does not create a `ZfsShare` at all**
  (deletes a stale one plus any DH-CHAP secret if present) and marks the
  attach request `Ready` directly — see `TestAttachRequest_NVMeoFLocalSkipsShare`
  in [internal/controller/zfsshareattachrequest_controller_test.go](../internal/controller/zfsshareattachrequest_controller_test.go).
  A new `Watches(&ZfsPool{}, ...)` (`requestsForPool`) re-triggers this
  decision on pool migration — see `TestAttachRequest_NVMeoFPoolMovesAwayCreatesShare`.
  The `ZfsShare`→`NetworkExport` translator (`ZfsShareReconciler`) is
  completely unchanged: it never needs to know about locality at all, because
  the decision not to export is made *before* a `ZfsShare` would ever be
  written, not downstream of one existing. See ADR-0031's "Rejected
  alternatives" for two earlier designs (a `ZfsShareSpec.AttachedNode` field,
  then a field-free derivation in the translator) that worked but were backed
  out for violating "a `ZfsShare`'s existence means it's exported."
- **Done — Phase 2 (dataset/NFS passthrough):** see below.

## Motivation

Every CSI-provisioned volume today routes through a network protocol —
NVMe-oF (zvols) or NFS (filesystem datasets) — even when the pod consuming the
volume runs on the **same node** that hosts the ZFS pool. On a single-node
deployment this is unconditionally true for every volume, every time; on a
multi-node deployment it is still frequently true. Stacking NVMe-over-TCP and
ZFS on top of each other for what is, physically, local disk I/O produces
avoidable overhead and an avoidable failure surface (a boot-time race between
the NVMe-oF initiator and target sides was observed on a live cluster and is
recorded separately in known-pitfalls.md).

The idea: keep volume **creation** unchanged, but make **attach/mount
routing** locality-aware.

- **Zvols are always RWO** (single-node only), so if the attach request comes
  from the node that already hosts the pool, the node plugin can access the
  zvol's block device directly — no `nvme connect`, no nvmet export, and (new
  in this design) no `ZfsShare`/`NetworkExport` object at all.
- **Filesystem datasets** (Phase 2, not started) are exported over NFS today
  regardless of access mode. A **RWX** dataset must keep using NFS (multiple
  nodes may need concurrent access). A **RWO** dataset's access mode is fixed
  for the life of the PV, so if the one node ever allowed to mount it is the
  pool's own node, the node plugin could in principle bind-mount the
  dataset's existing host mountpoint directly instead of looping back through
  NFS.

## Design: locality-aware routing, decided once, as early as possible

The routing decision is a single comparison: is the requesting node equal to
`ZfsPool.status.currentNode` for the pool backing this volume? This data
already exists — no new CRD field was needed, in either the node plugin or
the attach-request pipeline.

The important design lesson from this implementation (see ADR-0031's
"Rejected alternatives" for the full history): **make this decision as early
as possible in the pipeline, before anything that implies "this is exported"
gets created** — not later, by having a downstream component silently produce
nothing. Two earlier attempts put the decision in the `ZfsShare`→
`NetworkExport` translator instead (once via an explicit field, once via a
derived recomputation), and both had the translator receive a `ZfsShare`
object and then decide not to act on it. That works, but it means a
`ZfsShare`'s mere existence stops reliably meaning "this is shared over the
network" — a `kubectl get zfsshare` for a healthy, working local attach would
show `Bound` with no backing `NetworkExport`, indistinguishable at a glance
from a broken one. The fix was to move the decision to the **aggregator**,
which is the only component with the authority to decide "does this attach
need a `ZfsShare` at all" — mirroring a decision the aggregator already makes
("no attach requests left → delete the `ZfsShare`").

This must be **re-evaluated on every relevant reconcile**, never cached or
decided once: a pod can be rescheduled to a different node, and a pool can
migrate to a different node. The node plugin re-resolves live on every
Stage/Publish call (ADR-0018/ADR-0021's precedent); the aggregator gets the
same property via a `ZfsPool` watch (`requestsForPool`), mirroring the
translator's own pre-existing `sharesForPool` watch.

## Phase 1 — zvol / NVMe-oF local passthrough (done)

**Scope:** `internal/csi/{node,mount,controller}.go`,
`internal/controller/zfsshareattachrequest_controller.go`.

1. ~~Add a shared routing helper~~ `isLocalToPool(nodeID, pool)` in
   `internal/csi/node.go`, used by `NodePublishVolume` and `NodeExpandVolume`.
2. ~~`NodeStageVolume` (block/zvol path)~~ `NodePublishVolume`: when local,
   resolve `ZfsDataset.status.path` directly instead of calling `nvme
   connect`.
3. Decide whether the attach-request aggregator still creates a `ZfsShare` for
   local volumes: **no** — it now checks locality itself (resolving the
   volume's `ZfsPool`) and skips creating one entirely, deleting a stale one
   if the volume was previously remote.
4. `NodeExpandVolume`: mirrors the routing decision; a local zvol resizes
   straight from `Status.Path`, no rescan.
5. Handle the "used to be local, now remote" transition (pool migrates away
   from the attached node) and the reverse — both verified with tests
   (`TestAttachRequest_NVMeoFPoolMovesAwayCreatesShare`,
   `TestZfsShareReconcile`-equivalent coverage no longer needed since the
   translator isn't involved).
6. fsGroup/ownership: unaffected — kubelet's fsGroup handling is device-based,
   not protocol-based.
7. Tests: `internal/csi/node_test.go` (`TestNodePublish_NVMeoF_Local*`,
   `TestNodeExpand_NVMeoFLocal`), `internal/controller/zfsshareattachrequest_controller_test.go`
   (`TestAttachRequest_NVMeoFLocalSkipsShare`,
   `TestAttachRequest_NVMeoFPoolMovesAwayCreatesShare`).
8. Manual verification on a real cluster (still pending): confirm zero `nvme
   connect`/nvmet configfs activity and zero `ZfsShare`/`NetworkExport`
   objects for a same-node PVC attach, and that cross-node attach is
   unchanged.

## Phase 2 — dataset / NFS local passthrough (done, RWO only)

**Scope:** `internal/csi/{node,mount}.go`,
`internal/controller/zfsshareattachrequest_controller.go`,
`api/v1alpha1/zfsshareattachrequest_types.go`.

1. ~~`isLocalToPool` reused from Phase 1~~ — no new routing helper needed.
2. ~~`NodePublishVolume` (filesystem/dataset path)~~: **RWO only.** If the
   VolumeCapability is `SINGLE_NODE_WRITER` or `SINGLE_NODE_READER_ONLY` AND the
   pool is local, calls `publishLocalDataset` which bind-mounts
   `ZfsDataset.Status.Path` directly. **RWX always uses NFS regardless of
   locality** — mixing bind-mounts and NFS mounts from different nodes puts POSIX
   locks and NFS lockd locks in separate domains and breaks file-locking semantics
   (e.g. database journalling).
3. SELinux: `BindMountDir` applies `context=system_u:object_r:container_file_t:s0`
   **conditionally** — it checks whether `/sys/fs/selinux/enforce` exists at
   runtime. If the kernel has `CONFIG_SECURITY_SELINUX` compiled in, the option
   is added (providing the same single-label behaviour NFS always provided); if not
   (e.g. an embedded or minimal kernel), it is omitted to avoid `EINVAL`. See
   `selinuxActive()` in `mount.go`.
4. **`ZfsShareAttachRequestSpec.SingleNode bool`** added (true = RWO): the CSI
   controller sets this at `ControllerPublishVolume` time from the request's
   VolumeCapability, so the aggregator doesn't have to re-derive it.
5. Aggregator (NFS): `reconcileVolume` checks `isSingleNodeVolume` via
   `gateReader()`.
   - **Single-node (RWO):** uses `oldestAttachNode` ("oldest wins," same as
     NVMe-oF), then checks locality. If the winning node IS the pool's node → no
     `ZfsShare` (bind-mount path). If remote → NFS export to the single winning
     node only. Multiple requests here are a race the CSI controller already
     rejects; the aggregator handles them defensively by exporting only to the
     oldest.
   - **Multi-node (RWX):** all requesting nodes get NFS. Never local, never
     skips `ZfsShare`.
6. Pool migration (NFS): existing `requestsForPool` watch re-triggers
   reconciliation; if the pool moves away, the aggregator creates a `ZfsShare`
   on the next reconcile. Tested with
   `TestAttachRequest_NFSPoolMovesAwayCreatesShare`.
7. `NodeUnpublishVolume`: unchanged — `Unmount` + `RemovePath` works for bind
   mounts and NFS mounts identically.
8. `NodeExpandVolume`: NFS early-return is unchanged — local datasets need no
   node-side resize work (ZFS handles capacity via dataset quota/reservation).
9. Tests: `TestNodePublish_NFS_Local`, `TestNodePublish_NFS_LocalRequiresStatusPath`,
   `TestNodePublish_NFS_RWX_NeverLocal` (node_test.go);
   `TestAttachRequest_NFSLocalSkipsShare`, `TestAttachRequest_NFSRWXAlwaysNFS`,
   `TestAttachRequest_NFSMixedNodesCreatesShare`,
   `TestAttachRequest_NFSPoolMovesAwayCreatesShare`,
   `TestAttachRequest_NFSRWORaceExportsOldestNodeOnly`
   (zfsshareattachrequest_controller_test.go).

## SELinux future work (not implemented)

The `context=container_file_t:s0` option applies the same **single label per
mount** that NFS always provided. A more precise future improvement is to pass the
pod's exact SELinux MCS context through via the CSI
`NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP` capability: kubelet would then
supply the pod's `fsGroup`/SELinux context at `NodePublishVolume` time, letting
the driver apply per-pod `context=` labels instead of a fixed shared type. That
requires implementing `NODE_SERVICE_CAPABILITY_VOLUME_MOUNT_GROUP` and is
currently noted as future work in [known-pitfalls.md](known-pitfalls.md).

## Open questions

- Should the "no `ZfsShare` for a local attach" state be surfaced more
  visibly than just "the object doesn't exist" (e.g. an event on the
  `ZfsShareAttachRequest`) for observability, or is `status.message` (`"volume
  %q is local to its own node; no network export needed"`) enough?
- Should a `DEGRADED`/`SUSPENDED` pool health force the network path even when
  local, as defense in depth, or is local access strictly safer (one fewer
  moving part) in a degraded state?

## Rejected alternatives

See ADR-0031's own "Rejected alternatives" section for the two earlier
translator-based designs and why both were backed out in favor of the
aggregator deciding up front. At the routing-scope level (not the "who decides
locally" level):

- **Drop the network path entirely; require pod/pool colocation always.**
  Rejected — breaks true multi-node scheduling and RWX volumes, both of which
  the project explicitly supports today.
- **Decide locality once at `CreateVolume`/first `ControllerPublishVolume` and
  freeze it on the PV forever.** Rejected — pods and pools can both move;
  routing must be re-resolved live, exactly like protocol/poolGUID/dataset
  already are (ADR-0021).
