# Node-Local Passthrough Redesign — Research & Implementation Plan

**Status: Phase 1 partially implemented (2026-08-07).** See [ADR-0031](design-decisions.md).

## Implementation status

- **Done:** `NodePublishVolume` routes a zvol straight to its local device path
  (`ZfsDataset.Status.Path`) when `isLocalToPool` reports this node hosts the
  pool — no `nvme connect`, no `NetworkExport` dependency at all for that call.
  See `publishLocalZvol`/`isLocalToPool` in
  [internal/csi/node.go](../internal/csi/node.go) and the
  `TestNodePublish_NVMeoF_Local*` tests in
  [internal/csi/node_test.go](../internal/csi/node_test.go).
- **Deliberately deferred (task 3 below):** `ControllerPublishVolume` still
  creates a `ZfsShareAttachRequest` and `nvmeof-controller` still programs the
  nvmet configfs export unconditionally, even for a local attach. The export
  is simply never used by a local publish. This is safe (no functional harm,
  some wasted target-side setup) and keeps this first slice small; skipping
  the nvmet programming step for local-only attaches is the next increment.
- **Known gap, not yet fixed:** `NodeExpandVolume` does not yet know about
  local zvols. It still resolves the device purely through `NetworkExport`/
  `NVMeDevice`, so expanding a *local* zvol's filesystem currently fails with a
  clear `FailedPrecondition: nvme device ... is not connected` — a safe, loud
  failure, not silent data loss, but online expansion of a local zvol does not
  work yet. Needs a locality branch mirroring `NodePublishVolume`'s, using
  `ZfsDataset.Status.Path` directly (no rescan needed — the block device
  already reflects the grown zvol without any NVMe-oF hop involved).
- **Not started:** Phase 2 (dataset/NFS passthrough).

## Motivation

Every CSI-provisioned volume today routes through a network protocol —
NVMe-oF (zvols) or NFS (filesystem datasets) — even when the pod consuming the
volume runs on the **same node** that hosts the ZFS pool. On a single-node
deployment this is unconditionally true for every volume, every time; on a
multi-node deployment it is still frequently true. Stacking NVMe-over-TCP, ZFS,
and (for datasets) NFS on top of each other for what is, physically, local disk
I/O produces avoidable overhead and an avoidable failure surface: see
[known-pitfalls.md](known-pitfalls.md) class 21 and its "loopback stacking"
adjacent note for the concrete symptoms this has produced (elevated load
average, and — separately — a boot-time race between the NVMe-oF initiator and
target sides).

The idea: keep volume **creation** unchanged, but make **attach/mount routing**
locality-aware.

- **Zvols are always RWO** (single-node only, class 7), so if the attach request
  comes from the node that already hosts the pool, the node plugin can access
  the zvol's block device directly — no `nvme connect`, no nvmet export.
- **Filesystem datasets** are exported over NFS today regardless of access mode.
  A **RWX** dataset must keep using NFS (multiple nodes may need concurrent
  access). A **RWO** dataset's access mode is fixed for the life of the PV (PVC
  access modes are immutable), so if the one node ever allowed to mount it is
  the pool's own node, the node plugin can bind-mount the dataset's existing
  host mountpoint directly instead of looping back through NFS.

## Current architecture (baseline)

- **Zvols:** `CreateVolume` writes a `ZfsDataset` (zvol). `ControllerPublishVolume`
  creates a `ZfsShareAttachRequest{volume, node}` (ADR-0010 zero-trust); once at
  least one attach request exists, `nvmeof-controller` programs the nvmet
  configfs export. `NodeStageVolume` runs `nvme connect` against
  `ZfsPool.status.currentIP` and formats/mounts the resulting device.
- **Filesystem datasets:** same attach lifecycle, but the node plugin mounts via
  NFS (`mount -t nfs <currentIP>:<path> <target>`).

Both paths already resolve pool/protocol/dataset **live** at Stage/Publish time
(ADR-0021, ADR-0022) rather than trusting a cached `volume_context` — the
locality check this redesign adds fits the same pattern.

## Design: locality-aware routing

The routing decision is a single comparison: is the requesting node
(`req.GetNodeId()`, already available at `ControllerPublishVolume` and
implicitly at `NodeStageVolume`/`NodePublishVolume` since they run *on* that
node) equal to `ZfsPool.status.currentNode` for the pool backing this volume?

This data already exists on `ZfsPool.status` — no new CRD field is required.

- If equal (and the pool is healthy — see open questions): use the local path.
- Otherwise: today's NVMe-oF/NFS path, unchanged.

This must be **re-evaluated on every Stage/Publish call**, never cached or
decided once at `CreateVolume` time — a pod can be rescheduled to a different
node, and (in a future multi-pool topology) a pool could move. This mirrors the
existing "resolve live, don't trust a stale value" precedent from ADR-0018/
ADR-0021.

## Phase 1 — zvol / NVMe-oF local passthrough

**Scope:** `internal/csi/{node,mount,controller}.go`,
`internal/controller/zfsshareattachrequest_controller.go`, `internal/nvmet`,
chart RBAC/hostPath.

1. Add a shared routing helper, e.g. `isLocalToPool(nodeID string, pool
   *v1alpha1.ZfsPool) bool`, used by both the controller and node plugin.
2. `NodeStageVolume` (block/zvol path): when local, resolve the zvol's device
   path directly (needs to match whatever naming `internal/zpool` already uses
   for zvols, e.g. `/dev/zvol/<pool>/<dataset>`) instead of calling `nvme
   connect`; skip `RescanNVMe`/multipath handling for this volume entirely.
3. Decide whether `ControllerPublishVolume` still creates a
   `ZfsShareAttachRequest` for local volumes:
   - **(a) Keep creating it**, but make `nvmeof-controller`'s nvmet configfs
     programming step itself locality-aware (skip programming the export when
     every current attach request for that volume is local-only). Reuses the
     existing zero-trust audit trail and attach/detach lifecycle unchanged.
   - **(b) Skip the attach-request machinery entirely** for local volumes.
     Leaner, but needs a new mechanism to track "is anything still using this
     local volume" for safe teardown, duplicating what the attach-request
     already gives us for free.
   - **Recommendation: (a).** Don't invent a second bookkeeping mechanism for
     what is otherwise an internal implementation detail of how the mount is
     served.
4. `NodeUnstageVolume`: mirror the routing decision; a local volume needs no
   `nvme disconnect`.
5. Handle both attach-mode **transitions**: a volume that was local and becomes
   remote (pod rescheduled elsewhere, or the pool's `currentNode` changes), and
   the reverse. Both must fall back correctly on the *next* Stage/Publish call.
   Add explicit tests for both directions — this is the highest-risk part of
   the whole change.
6. Confirm fsGroup ownership behaves identically for a local zvol block device
   vs. an NVMe-oF-attached one (kubelet's fsGroup handling is device-based, not
   protocol-based, so this should be a non-issue — verify rather than assume).
7. Tests: extend the fake mounter in
   [internal/csi/node_test.go](../internal/csi/node_test.go)/
   [mount_test.go](../internal/csi/mount_test.go) with a "local" branch; add a
   controller test asserting the chosen behavior from step 3 for a same-node
   attach.
8. Manual verification on a real cluster: confirm zero `nvme connect`/nvmet
   configfs activity for a same-node PVC attach (dmesg should show nothing),
   and that cross-node attach is unchanged.

## Phase 2 — dataset / NFS local passthrough

**Scope:** `internal/csi/{node,mount,controller}.go`,
`internal/controller/nfs_controller.go`, chart.

1. Reuse the Phase 1 routing helper.
2. `NodeStageVolume`/`NodePublishVolume` (filesystem/dataset path): if the
   access mode is RWO **and** local, bind-mount
   `ZfsPool.status.baseMountPath/<dataset>` directly instead of `mount -t nfs`.
   RWX always uses NFS regardless of locality.
3. **SELinux is the biggest unknown.** Pods run under `pod_t` on Talos
   (`selinux=1` on the kernel cmdline). NFS never triggers relabeling because
   it's a network filesystem; a direct bind-mount of a host ZFS mountpoint may
   need an explicit mount `context=` option or an equivalent relabel step to
   avoid permission-denied. **Prototype this first**, before writing any other
   Phase 2 code — if it doesn't work cleanly, the rest of Phase 2 may not be
   worth doing.
4. Audit `uid`/`gid`/`mode` (ADR-0015) and fsGroup interaction: confirm nothing
   the current NFS export options do (e.g. any implicit squash behavior) is
   silently relied upon and would change under a bind mount.
5. Document (README/known-pitfalls) that local-bind datasets use native POSIX
   locks, not NFS `lockd`, for any workload that depends on lock semantics.
6. `NodeUnstageVolume` + transition handling, mirroring Phase 1 step 5.
7. Tests + manual verification, mirroring Phase 1 steps 7–8.

## Open questions

- Should `ZfsShareAttachRequest`/the CRD model surface "local" vs "network"
  attach as a distinct, observable state, or stay purely an internal
  implementation detail of how the node plugin serves the mount?
- Exact zvol device path resolution — must match existing zvol naming in
  `internal/zpool`, not invent a second convention.
- Should a `DEGRADED`/`SUSPENDED` pool (class re: `ZfsPoolHealth`) force the
  network path even when local, as defense in depth, or is local access
  strictly safer (one fewer moving part) in a degraded state?

## Rejected alternatives

- **Drop the network path entirely; require pod/pool colocation always.**
  Rejected — breaks true multi-node scheduling and RWX volumes, both of which
  the project explicitly supports today.
- **Decide locality once at `CreateVolume`/first `ControllerPublishVolume` and
  freeze it on the PV forever.** Rejected — pods (and, in a future multi-pool
  topology, pools) can move; routing must be re-resolved live on every
  Stage/Publish call, exactly like protocol/poolGUID/dataset already are
  (ADR-0021).
