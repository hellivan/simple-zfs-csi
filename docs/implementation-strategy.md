# Implementation Strategy

This document is the actionable plan for completing the `simple-zfs-csi` storage system:
the CSI plane (controller + node plugin), the allocation CRD, and the two-layer share
model. It complements the design rationale in [../THOUGHTS.md](../THOUGHTS.md); this file
tracks **what to build, in what order, and how to verify each step.**

## Target end state

```
                         ┌──────────────────────────────────────────┐
 PVC ──> csi-provisioner │ CSI Controller (Deployment, unprivileged) │
                         │  CreateVolume -> writes ZfsDataset(+ZfsShare)│
                         │  waits Ready -> returns volume_context      │
                         └───────────────┬──────────────────────────┘
                                         │ (CRDs in etcd)
        ┌────────────────────────────────┼───────────────────────────────┐
        ▼ storage node (DaemonSet, 1 pod, many containers)               ▼
 ┌─────────────┐  ┌──────────────┐  ┌───────────────┐   ┌────────────────────────────┐
 │ agent       │  │ nfs          │  │ nvmeof        │   │ operator (Deployment x1,   │
 │ zfs create  │  │ exportfs     │  │ configfs      │   │ cluster-wide, leader-elect)│
 │ + discovery │  │ (NetworkExport)  (NetworkExport)│   │  - zpool-watcher:          │
 │ ZfsDataset   │  └──────────────┘  └───────────────┘   │    node death -> NODE_OFFLINE│
 │ (allocation)│                                        │  - ZfsShare -> NetworkExport │
 └─────────────┘                                        └────────────────────────────┘
                                         ▲
                                         │ ZfsPool.status (IP, baseMountPath, health)
 consumer node (DaemonSet, all nodes) ───┘
 ┌───────────────────────────────────────────────┐
 │ csi-node: NodePublish -> mount -t nfs /        │
 │           nvme connect ; refuse NODE_OFFLINE   │
 └───────────────────────────────────────────────┘
```

## CRD model

| CRD | Scope | Keyed on | Written by | Purpose |
|-----|-------|----------|------------|---------|
| `ZfsPool` | Cluster | pool GUID (`metadata.name`) | agent (discovery) + operator (watcher) | routing + health (exists today) |
| `ZfsDataset` | Cluster | `spec.poolGUID` | CSI controller (creates), agent (reconciles) | dataset/zvol allocation intent |
| `ZfsShare` | Cluster | `spec.poolGUID` + `spec.dataset` | CSI controller (creates), operator (ZfsShare reconciler) | ZFS-centric "intent to share"; renders a child `NetworkExport` |
| `NetworkExport` | Cluster | `spec.nodeName` + `spec.path` | operator (ZfsShare reconciler, owns) or admin (standalone) | generic, ZFS-agnostic node-local export executor contract |
| `ZfsSnapshot` | Cluster | `spec.poolGUID` + source dataset | CSI controller (creates), agent (reconciles) | point-in-time `dataset@snap`; source for clone/restore (separate CRD per ADR-0006) |

Key rule: **ZfsShare compiles down to NetworkExport.** Only `NetworkExport` controllers
touch `/etc/exports` / nvmet `configfs` — exactly one aggregator per node per protocol.

## Component placement (planes)

| Component | Kind | Scope | Privilege | Hosts |
|-----------|------|-------|-----------|-------|
| `agent` | DaemonSet | per storage node | privileged (`/dev/zfs`) | discovery + `ZfsDataset` allocation |
| `nfs` / `nvmeof` | containers in the storage DaemonSet | per storage node | privileged | `NetworkExport` aggregators (exportfs / configfs) |
| **`operator`** | Deployment (x1, leader-elected) | cluster-wide | unprivileged | `zpool-watcher` (node death → `ZfsPool` health) **+** `ZfsShare → NetworkExport` translator |
| `csi-controller` | Deployment | cluster-wide | unprivileged | thin gRPC adapter: creates `ZfsDataset`/`ZfsShare`, returns context (no reconcile loops) |
| `csi-node` | DaemonSet | all nodes | privileged (mount) | NodePublish: mount / `nvme connect` |

`operator` is the promoted `zpool-watcher`: the cluster-scoped controller-manager for all
unprivileged, cluster-wide, leader-elected reconcilers. The `ZfsShare` translator lives here
(not in `csi-controller`) so GUID-routed shares work **without** the CSI stack, and so the
CSI controller stays a thin, replaceable adapter.

## Build order

### Step 0 — Rename `zpool-watcher` -> `operator`
Promote the cluster-wide watcher into the operator/controller-manager that will also host the
`ZfsShare` reconciler (Step 3).
- `cmd/zpool-watcher` -> `cmd/operator`; keep the existing watcher reconciler registered.
- Image `simple-zfs-csi-watcher` -> `simple-zfs-csi-operator`; update `build/watcher.Dockerfile`,
  Makefile targets, chart (`watcher-deployment.yaml` -> `operator-deployment.yaml`, values,
  serviceaccounts, RBAC), `README.md`.
- Enable leader election on the manager (single active; safe to scale to 2 for HA).
- Verify: `make vet`, `make build`, `make helm-lint`, `make helm-template`.

### Step 1 — Rename `ZfsShare` -> `NetworkExport`
Mechanical refactor to free the `ZfsShare` name for the ZFS-centric type and preserve the
generic executor.
- `api/v1alpha1/zfsshare_types.go` -> `networkexport_types.go`; rename `ZfsShare*` -> `NetworkExport*`.
- Update `internal/controller/nfs_controller.go`, `nvmeof_controller.go`, `common.go`.
- Update `config/samples/`, chart CRDs/templates, `README.md` references.
- Verify: `make manifests` (regenerate deepcopy + CRDs), `make vet`, `make build`.

### Step 2 — `ZfsDataset` allocation CRD
- New `api/v1alpha1/zfsdataset_types.go`.
- Spec: `{ poolGUID, dataset, type: dataset|zvol, quota (dataset), size (zvol), volblocksize? (zvol), properties? map }`.
- Status: `{ phase, path, observedGeneration, conditions }`.
- Verify: `make manifests`, `make vet`.

### Step 3 — New GUID-based `ZfsShare` (reconciler in `operator`)
- New `api/v1alpha1/zfsshare_types.go` (ZFS-centric).
- Spec: `{ poolGUID, dataset, protocol, nfs|nvmeof }`.
- Reconciler `internal/controller/zfsshare_controller.go`, **registered in the `operator`
  manager** (cluster-wide, leader-elected — not per-node):
  - resolve `poolGUID` -> `ZfsPool.status` (currentNode, baseMountPath, poolName);
  - derive path (NFS: `baseMountPath/dataset`; NVMe-oF: `/dev/zvol/poolName/dataset`);
  - create/update an owned `NetworkExport` (owner ref for GC);
  - watch `ZfsPool` and requeue referencing shares on takeover.
- RBAC: operator gains `get/list/watch zfsshares` + `create/update/delete/watch networkexports`.
- Verify: `make manifests`, `make vet`; unit test path derivation + child render.

### Step 4 — `ZFS` interface + hostexec impl
- Define `type ZFS interface { CreateDataset; CreateZvol; Destroy; Get; List }` in `internal/zpool` (or `internal/zfs`).
- Implement over the existing `internal/zpool/hostexec.go` (chroot `/host` / nsenter).
- Verify: `make vet`, unit tests with a fake exec.

### Step 5 — Agent allocation reconciler (fold into discovery = `agent`)
- Reconcile `ZfsDataset` where the pool GUID is currently hosted by this node.
- Create: `zfs create [-V]` idempotently; set `status.path` + `Ready`.
- Delete: finalizer -> `zfs destroy`.
- Merge with the existing discovery loop so one binary/container = `agent`.
- Verify: `make build`; manual `kubectl apply` of a `ZfsDataset` on a test node.

### Step 6 — CSI controller (`cmd/csi-controller`)
- Identity + Controller gRPC services (grpc + csi spec proto).
- `CreateVolume`: write `ZfsDataset` (+ `ZfsShare`), wait for Ready, return volume_context
  `{ pool_guid, dataset, protocol }` (never an absolute path).
- `DeleteVolume`: delete the CRDs (finalizers drive teardown).
- Optional: `ControllerExpandVolume`, snapshots.
- Deploy: Deployment + `csi-provisioner` (+ resizer/snapshotter if enabled).
- Verify: `csi-sanity` against the controller socket; e2e PVC create.

### Step 7 — CSI node plugin (`cmd/csi-node`)
- Identity + Node gRPC services.
- `NodePublishVolume`: read `ZfsPool.status` (currentIP + baseMountPath), join `dataset`,
  `mount -t nfs` or `nvme connect`; **refuse when `health == NODE_OFFLINE`** (clean error).
- `NodeUnpublishVolume`: unmount / `nvme disconnect`.
- Deploy: DaemonSet on all nodes + `node-driver-registrar`; ship a `CSIDriver` object.
- Verify: `csi-sanity` node tests; e2e pod mounts PVC over NFS and NVMe-oF.

## Phase 2 — parity capabilities (layered on the core path)

These match the democratic-csi feature surface (expansion, snapshots, clone). Each is an
optional CSI capability advertised only when its sidecar/RBAC is deployed.

### Step 8 — Volume expansion  ✅ done (ADR-0004)
Spec-driven size convergence, online grow.
- Agent: `ZfsDataset` reconcile calls `ensureSize` — filesystem grows `refquota`, zvol grows
  `volsize` (align up to `volblocksize`, never shrink).
- CSI controller: `ControllerExpandVolume` bumps `ZfsDataset` spec size (retry-on-conflict),
  waits for `observedGeneration`, returns `NodeExpansionRequired` (false for NFS/filesystem,
  true for zvol). Advertises `EXPAND_VOLUME`.
- CSI node: `NodeExpandVolume` — NFS no-op; NVMe-oF `ns-rescan` + `resize2fs`/`xfs_growfs`
  (skipped for block volumeMode). Advertises node `EXPAND_VOLUME`; plugin cap `ONLINE`.
- Deploy: `csi-resizer` sidecar; RBAC for `persistentvolumeclaims/status`;
  `allowVolumeExpansion: true` on StorageClasses.
- Verify: unit tests (controller expand fs/zvol, node expand nvme/nfs); `helm template`.

### Step 9 — Snapshots  ✅ done (ADR-0008)
Dedicated `ZfsSnapshot` CRD (grouped by lifecycle, not ZFS taxonomy — ADR-0006).
- Agent: `zfs snapshot pool/ds@snap` (create), `zfs destroy pool/ds@snap` (finalizer);
  status `readyToUse`, `creationTime` (from the ZFS `creation` property),
  `restoreSize` (referenced bytes). Only the node currently hosting the pool acts.
- CSI controller: `CreateSnapshot` (looks up the source `ZfsDataset` for pool +
  dataset, writes a `ZfsSnapshot`, waits `readyToUse`), `DeleteSnapshot`,
  `ListSnapshots` (id/source filters + offset pagination); advertises
  `CREATE_DELETE_SNAPSHOT` + `LIST_SNAPSHOTS`.
- Deploy: `csi-snapshotter` sidecar (`csiController.snapshotter.*`, on by default) +
  RBAC for `volumesnapshotcontents`/`volumesnapshotclasses`; optional
  `volumeSnapshotClasses` chart values render `VolumeSnapshotClass` objects. The
  snapshot CRDs + snapshot-controller are a cluster prerequisite (not shipped).
- Verify: `make manifests` (new CRD), unit tests (zfs `Snapshot`, agent reconcile,
  controller CreateSnapshot/DeleteSnapshot/ListSnapshots); `helm template`.

### Step 10 — Volume from snapshot / clone
- CSI controller: `CreateVolume` honours `VolumeContentSource` (snapshot or volume) →
  writes a `ZfsDataset` whose spec references the source; advertise `CLONE_VOLUME`.
- Agent: `zfs clone pool/ds@snap pool/newds` (from snapshot) or
  `zfs snapshot`+`zfs clone` (from volume) instead of `zfs create`.
- Verify: `make manifests` if spec grows a source ref; unit tests; e2e restore + clone.

## Verification matrix

| Step | Build | Unit | Cluster/e2e |
|------|-------|------|-------------|
| 0 operator rename | `vet`+`build`+`helm-lint`+`helm-template` | existing watcher tests pass | watcher still sets NODE_OFFLINE |
| 1 rename | `make manifests`+`vet`+`build` | existing tests pass | — |
| 2 ZfsDataset | `make manifests`+`vet` | — | — |
| 3 ZfsShare | `make manifests`+`vet` | path derivation, child render | — |
| 4 ZFS iface | `vet` | fake-exec unit tests | — |
| 5 agent | `build` | reconcile idempotency | `kubectl apply` ZfsDataset creates dataset |
| 6 controller | `build` | — | csi-sanity + PVC provisions |
| 7 node | `build` | — | csi-sanity + pod mounts (NFS + NVMe-oF) |
| 8 expansion ✅ | `vet`+`build`+`helm-template` | controller/node expand unit tests | PVC resize grows fs/zvol |
| 9 snapshots ✅ | `make manifests`+`build` | snapshot reconcile + CreateSnapshot | `VolumeSnapshot` create/delete |
| 10 clone/restore | `make manifests`+`build` | clone spec + CreateVolume source | PVC from snapshot; PVC clone |

## Out of scope (tracked, not now)
- Backup pod (`ssh-daemon` + `cron-puller`) — separate pod, later; shares the host-exec helper.
- gRPC ZFS daemon — deferred; only if a second in-pod control-plane ZFS consumer appears.

## Cross-references
- Design rationale, container inventory, invariants: [../THOUGHTS.md](../THOUGHTS.md)
  sections 1–9 of the appended architecture notes.
- Existing controllers: `internal/controller/`. Host exec seam: `internal/zpool/hostexec.go`.
