# Design Decisions

An append-only log of architectural decisions (ADR-lite). Each entry records the
decision, the context, the options weighed, and the consequences. Newest first.

The complementary conventions doc is [api-conventions.md](api-conventions.md);
the build plan is [implementation-strategy.md](implementation-strategy.md).

---

## ADR-0007 — Project identity: renamed to `simple-zfs-csi`

**Status:** Accepted (2026-07-17) · **Scope:** module path, API group, CSI driver name, PVC annotation prefix, Helm chart, image names

### Context

The project began as `zfs-shares`, a name that described only the original plane
(network-sharing pre-provisioned ZFS over NFS/NVMe-oF via the `NetworkExport`
CRD). It has since grown a full CSI plane (dynamic provisioning, expansion, and
soon snapshots/clone). "zfs-shares" no longer describes what it is — a
self-contained **CSI driver** for ZFS — and the "simple" qualifier positions it
against the heavier alternatives it replaces (democratic-csi + TrueNAS, Ceph).

### Decisions

1. **Rename the project to `simple-zfs-csi`,** sweeping every name domain uniformly
   (`zfs-shares` → `simple-zfs-csi`):
   - Go module: `github.com/hellivan/simple-zfs-csi`
   - API group: `storage.simple-zfs-csi.io` (all four CRDs, finalizers,
     `LeaderElectionID`)
   - CSI driver name: `simple-zfs-csi.io` (`CSIDriver` object + StorageClass
     `provisioner` + `--driver-name` default)
   - PVC annotation prefix: `param.simple-zfs-csi.io/`
   - Helm chart: `charts/simple-zfs-csi`
   - Container images: `simple-zfs-csi-<component>`.

2. **Collapse the two CSI image names.** A uniform `<prefix>-<component>` scheme
   would yield `simple-zfs-csi-csi-controller` / `simple-zfs-csi-csi-node` (double
   `csi`), so those two use `simple-zfs-csi-controller` / `simple-zfs-csi-node`
   (the Helm image helper takes an explicit `suffix` of `controller`/`node`). The
   `cmd/` dirs and `build/*.Dockerfile` names stay `csi-controller`/`csi-node`
   (unambiguous internally).

### Consequences

- Breaking: the CRD API group and CSI driver name change, so this is not
  upgrade-compatible with any `zfs-shares` install — acceptable pre-1.0 (no
  production deployments).
- The on-disk repository directory is intentionally **not** renamed here (left to
  the maintainer to avoid breaking the active workspace path); it has no code
  impact.
- CRD manifests were regenerated (`make manifests`) so the group rename is
  authoritative in the generated YAML, not just hand-edited.

---

## ADR-0006 — CRD taxonomy: `ZfsDataset` (fs+zvol) vs a separate `ZfsSnapshot`

**Status:** Accepted (2026-07-17) · **Scope:** API types, CSI controller, agent

### Context

The allocation CRD was originally named `ZfsVolume` with `type: filesystem|volume`.
That overloads "volume": in CSI it means any PV, but in ZFS it means specifically
a *zvol*, so `ZfsVolume{type: volume}` reads as "a ZFS volume of type volume." It
also made the upcoming snapshot object look arbitrary — why does one CRD unify
filesystem+volume while a snapshot is a different CRD?

### Decisions

1. **Rename `ZfsVolume` → `ZfsDataset`** with `type: filesystem | volume`. "Dataset"
   is ZFS's own umbrella term ([api-conventions.md](api-conventions.md) §3), and it
   makes `type: volume` unambiguously a zvol. `shortName` changes `zvol` → `zds`.

2. **Group CRDs by lifecycle, not by ZFS taxonomy.** Taxonomically a snapshot *is*
   a kind of dataset, but `filesystem` and `volume` share one lifecycle (allocate →
   size/quota → share → expand → destroy), while a snapshot has a different one
   (derive-from-source → read-only → restore/clone, never shared/published). So the
   live allocation is `ZfsDataset`; a snapshot is a separate `ZfsSnapshot` (Step 9).

3. **The consumer model mirrors the `zfs` verbs:** `zfs create` a **dataset** →
   `zfs snapshot` it → `zfs clone` a snapshot into a new **dataset**. A snapshot
   never carries a filesystem/volume arm, an export, or an expand path, so folding
   it into `ZfsDataset` would mean an inert third `type` and `if type==snapshot`
   guards in every consumer — a separate CRD keeps each type's invariants clean.

### Consequences

- Breaking API rename (type, CRD `zfsdatasets`, finalizer
  `storage.simple-zfs-csi.io/zfsdataset`, CSI code, tests) — acceptable pre-1.0.
- `ZfsPool` stays as-is: Kubernetes names observed-infrastructure objects with
  plain nouns (`Node`, `CSINode`, `CSIStorageCapacity`); its empty spec already
  signals "discovered, not authored," so no rename is warranted.

---

## ADR-0005 — Access control and the CSI attach stage (direction)

**Status:** Accepted direction (2026-07-17), not yet implemented · **Scope:** CSIDriver, csi-controller, csi-node, `NetworkExport`

### Context

Today every share is effectively **public**: an NFS `NetworkExport` is exported
to the whole reachable network, and an NVMe-oF subsystem accepts any host NQN.
That is acceptable for the initial single-tenant bring-up but is **not** the end
state — we do not want any pod on any node able to mount any volume.

The driver currently sets `attachRequired: false` (ADR-0001/0003): there is no
controller-mediated attach step, because the node plugin does all reachability
work itself (`mount -t nfs` for NFS, `nvme connect` for NVMe-oF) and node-death
fencing comes from `ZfsPool.status.health == NODE_OFFLINE`, not from
`VolumeAttachment`. Ceph-RBD sets `attachRequired: true` for a genuine
map/lock/fence reason; our reason to (eventually) enable it is different but
compatible: **per-node access programming**.

The CSI attach stage — `ControllerPublishVolume(volume_id, node_id)` /
`ControllerUnpublishVolume`, tracked by `VolumeAttachment` objects and driven by
the `external-attacher` sidecar — is *controller-issued and node-parameterized*.
That is exactly the shape of "grant this specific node access to this volume,"
which is what access restriction needs.

### Decisions

1. **Keep `attachRequired: false` now.** No `ControllerPublishVolume`, no
   attacher, no `VolumeAttachment`. Simplicity while shares are trusted.

2. **NVMe-oF host allow-listing will move to the attach stage.** When we restrict
   NVMe-oF to specific consumers, we flip `attachRequired: true` and implement
   `ControllerPublishVolume` to add the consumer node's host NQN to the target
   subsystem's `allowed_hosts` (and `ControllerUnpublishVolume` to remove it),
   gated *before* `NodePublish`. This is the idiomatic CSI location for
   node-scoped access and gives us serialization + clean revoke for free.

3. **NFS allowed-clients live in the `NetworkExport` contract.** `NetworkExport`
   gains an allowed-clients field (NFS: CIDRs/IPs rendered into `/etc/exports`;
   NVMe-oF: host NQNs). Two ways to populate it, from coarse to fine:
   (a) **static policy** — allow the cluster node/pod CIDR, sourced from
   StorageClass/PVC params (simple, ship first); (b) **attach-driven** — the
   attach stage adds the specific consumer node's IP/NQN per publish (tightest,
   layered later). The executor (`nfs`/`nvmeof` controllers) stays generic; it
   only renders whatever allow-list the contract carries.

### Consequences

- Current simplicity is retained; access control is purely additive.
- Enabling attach later requires the `external-attacher` sidecar,
  `VolumeAttachment` RBAC, and `ControllerPublish/Unpublish` implementations —
  none of which exist today.
- `NetworkExport` grows an `allowedClients` field; it remains a ZFS-agnostic,
  node-local executor contract (an admin can still author one directly).
- Fencing semantics are unchanged: `NODE_OFFLINE` remains the availability gate;
  the attach stage would add an *authorization* gate, not replace fencing.

---

## ADR-0004 — Volume expansion: spec-driven size convergence, online grow

**Status:** Accepted (2026-07-17) · **Scope:** CSI controller + node, agent reconciler, Helm chart

### Context

democratic-csi-class parity starts with online volume expansion. A PVC edit that
requests more capacity flows through `external-resizer` →
`ControllerExpandVolume` → (for block) `NodeExpandVolume`. The backing size lives
in the `ZfsDataset` spec (`filesystem.quota` → ZFS `refquota`, `volume.size` → ZFS
`volsize`), which the per-node agent already owns. Expansion should reuse that
ownership rather than have the CSI plane touch ZFS directly.

### Decisions

1. **Expansion is spec convergence, not a special path.** `ControllerExpandVolume`
   only bumps the `ZfsDataset` spec size (retrying on conflict with the agent's
   status writes) and waits for the agent to observe it
   (`status.observedGeneration >= target`). The agent's reconciler gained an
   `ensureSize` step that runs on every reconcile: filesystem → `zfs set refquota`
   (grows or shrinks the cap), zvol → `zfs set volsize` (**grow only**, never
   shrink — shrinking a zvol under a live filesystem is unsafe). This also makes
   quota drift self-heal, not just explicit expands.

2. **`NodeExpansionRequired` follows the protocol.** NFS/filesystem quotas take
   effect the instant `refquota` is set, so no node work is needed
   (`NodeExpansionRequired: false`). A zvol grow only changes the target; the
   initiator must rescan the namespace and grow the on-device filesystem, so
   `NodeExpansionRequired: true`. `NodeExpandVolume` runs `nvme ns-rescan` then
   `resize2fs`/`xfs_growfs`; raw-block volumes stop after the rescan (no fs), and
   an NFS volume (no `NetworkExport` NQN) is a no-op.

3. **`volsize` alignment.** ZFS requires `volsize` to be a multiple of
   `volblocksize`, so the agent rounds the requested bytes up to the volume's
   block size (default 16 KiB) before `zfs set`.

4. **Online capability.** The controller Identity advertises
   `VOLUME_EXPANSION: ONLINE`; the controller service advertises `EXPAND_VOLUME`;
   the node service advertises `EXPAND_VOLUME`. Helm gains the `external-resizer`
   sidecar (`csiController.resizer.*`, on by default) plus RBAC for
   `persistentvolumeclaims/status` and `persistentvolumes` update. StorageClasses
   opt in per class with `allowVolumeExpansion: true`.

### Consequences

- No new CRD; expansion rides the existing `ZfsDataset` ownership boundary — the
  CSI plane stays a thin CRD adapter and only the agent runs ZFS.
- Shrinking is intentionally unsupported for zvols (and Kubernetes forbids PVC
  shrink anyway); filesystem `refquota` can still be lowered by editing the spec.
- Live `resize2fs`/`xfs_growfs` over NVMe-oF is the manual verification step (not
  unit-tested); unit tests cover the controller size-bump + node rescan/resize
  dispatch and the agent's `ensureSize` for both types.

---

## ADR-0003 — CSI node plugin: routing-only publish, NODE_OFFLINE fencing, protocol dispatch

**Status:** Accepted (2026-07-17) · **Scope:** Step 7 (`cmd/csi-node`), Helm chart

### Context

The node plugin is a privileged DaemonSet on every node. The controller
(ADR-0001) returns only a routing `volume_context = { poolGUID, dataset,
protocol }` — never an absolute path — so the node must resolve the real mount
target itself, at publish time, from live cluster state. It writes no CRDs.

### Decisions

1. **Routing resolved from `ZfsPool.status` at publish time.** `NodePublishVolume`
   loads the `ZfsPool` by `zpool.ResourceName(poolGUID)` (the same GUID→object
   mapping the operator uses) and reads `CurrentIP`, `BaseMountPath`, `PoolName`
   and `Health`. Resolving per-publish (not from a cached path) means pool
   takeover to a new node is picked up automatically on the next mount.

2. **NODE_OFFLINE fencing.** If `status.health == NODE_OFFLINE` (or there is no
   `CurrentIP`), publish fails `FailedPrecondition` with a clear message rather
   than mounting a stale/dead target. This is the node-side half of the watcher's
   fencing: the watcher marks the pool offline, the node refuses to mount it.

3. **`protocol` dispatches the publish mechanism; `volumeMode` is orthogonal.**
   - `nfs` → `mount -t nfs <CurrentIP>:<baseMountPath>/<dataset>` (filesystem
     only; block mode is rejected — mirrors the controller's rule).
   - `nvmeof` → `nvme connect` to `<CurrentIP>:<nvmePort>` for the export's NQN,
     then: filesystem mode → `mkfs` if unformatted + mount (fs-on-zvol); block
     mode → bind-mount the raw device node.
   The NVMe-oF subsystem NQN is read from the child `NetworkExport.status.NQN`
   (falling back to `spec.nvmeof.nqn`); an absent/empty NQN yields
   `FailedPrecondition` ("export not ready"), which naturally gates publish on the
   operator having rendered and the aggregator having configured the export.

4. **Privileged host operations behind a `NodeMounter` interface.** All mounts,
   `mkfs`, and `nvme connect/disconnect` go through
   [internal/csi/mount.go](../internal/csi/mount.go) `NodeMounter`, with a
   host-exec-aware command runner (`chroot`/`nsenter`, reusing the discovery
   plane's `zpool.HostExec`). The interface lets the routing logic be unit-tested
   with a fake (no real host). The node image bundles `nfs-common` + `nvme-cli` +
   `util-linux` + mkfs helpers so in-container mounting works by default;
   `--host-exec-mode` switches to the host's binaries (e.g. Talos).

5. **Publish-only (no stage/unstage).** The plugin advertises no optional node
   capabilities and does all work in `NodePublishVolume`/`NodeUnpublishVolume`.
   Publish is idempotent (an already-mounted target returns success). Unpublish
   unmounts, removes the target, and best-effort `nvme disconnect`s.

6. **Deployment.** DaemonSet (plugin + `node-driver-registrar` sidecar) with
   `hostNetwork` (to reach the storage node's NFS/NVMe endpoints), a
   `Bidirectional`-propagated `<kubeletDir>/pods` mount, the plugin/registration
   socket dirs, and `/dev`. The shared `CSIDriver` object (ADR-0001 render,
   `attachRequired: false`) covers both planes; the same driver name ties the
   registrar registration to the controller's provisioner.

### Consequences

- The node never learns an absolute path from the controller and never writes
  CRDs; its only inputs are the `volume_context` and read-only `ZfsPool` /
  `NetworkExport` status.
- A pool that has moved or died is fenced cleanly at mount time.
- `csi-sanity` node tests and live NFS + NVMe-oF pod mounts are the manual
  verification steps (not unit-tested); the fake-mounter unit tests cover routing,
  fencing, protocol dispatch, idempotency and unpublish.

---

## ADR-0002 — `poolGUID` and `datasetPrefix` are StorageClass-only

**Status:** Accepted (2026-07-17) · **Scope:** Step 6 (`cmd/csi-controller`), Helm chart

### Context

ADR-0001 defined a three-layer parameter inheritance chain (provisioner defaults
< StorageClass parameters < PVC annotations). Two of those keys select *where*
data lands: `poolGUID` picks the physical ZFS pool, and `datasetPrefix` scopes
the dataset namespace within it. If either could be set from the defaults layer
or, worse, from a PVC annotation, then:

- a cluster-wide default could silently route volumes to the wrong pool; and
- a namespace tenant authoring a PVC could redirect provisioning onto another
  pool or escape its dataset prefix — a tenancy/isolation hole.

### Decisions

1. **`poolGUID` and `datasetPrefix` are StorageClass-only.** They are honoured
   *only* from `CreateVolumeRequest.Parameters` (the StorageClass layer). If they
   appear in the provisioner-defaults layer or in the PVC-annotation layer they
   are dropped during resolution. Implemented as `storageClassOnlyParams` in
   [internal/csi/params.go](../internal/csi/params.go); other keys (`protocol`,
   `volblocksize`, `nfsClients`, `nvmeofAllowedHosts`, `property.*`) keep the full
   inheritance chain.

2. **No default `poolGUID`.** There is no cluster-wide default pool. Every
   StorageClass must name its pool explicitly; `poolGUID` remains required, so a
   StorageClass that omits it fails `CreateVolume` with `InvalidArgument`. The
   Helm `csiController.defaultParameters` value therefore must not carry
   `poolGUID`/`datasetPrefix` (documented inline in `values.yaml`).

3. **StorageClasses are declared in the Helm chart.** `values.yaml` exposes a
   `storageClasses` map (empty by default — the chart installs none), rendered by
   `templates/storageclasses.yaml`, mirroring the Ceph CSI chart. Each entry sets
   its own `parameters` (including the required `poolGUID` and optional
   `datasetPrefix`), `reclaimPolicy`, `volumeBindingMode`, etc.

### Consequences

- Pool routing and dataset scoping are fixed by cluster administrators at
  StorageClass-authoring time and cannot be overridden by PVC authors.
- `defaultParameters` stays useful for genuinely global, safe defaults
  (`protocol`, ZFS `property.*`), not placement.
- Tests cover the restriction: `TestResolveParameters_StorageClassOnly` and the
  updated `TestCreateVolume_PVCAnnotationsOverride` assert the SC-only keys ignore
  the defaults/annotation layers while non-restricted keys still inherit.

---

## ADR-0001 — CSI controller: provisioning model, protocol/type/volumeMode axes, parameter inheritance

**Status:** Accepted (2026-07-16) · **Scope:** Step 6 (`cmd/csi-controller`)

### Context

The CSI controller is a thin, unprivileged gRPC adapter driven by
`external-provisioner`. It must turn a PVC into the ZFS-centric CRDs
(`ZfsDataset` + `ZfsShare`) and never returns an absolute path — only a
`volume_context`. Several forks needed pinning before implementation.

### Decisions

#### 1. Pool selection — fixed per StorageClass

`spec.poolGUID` is taken from a StorageClass parameter (resolvable via the
inheritance chain below); one StorageClass targets one pool. No scheduler /
free-space picking and no CSI topology awareness in this step.

- Rationale: deterministic, no placement logic, matches the GUID-keyed routing
  model already in place. Scheduling across a pool set can be layered later
  without changing the CRD contract.

#### 2. `CreateVolume` creates **both** `ZfsDataset` and `ZfsShare` (provision-time share)

`CreateVolume` writes the `ZfsDataset`, waits for it to reach `Ready`, writes the
`ZfsShare`, and returns `volume_context = { poolGUID, dataset, protocol }`.

- CSI does **not** require creating an export in `CreateVolume`; its only hard
  contract is "provision storage, return `volume_id` (+ optional
  `volume_context`)." Two patterns were considered:
  - **Provision-time share (chosen):** export exists for the volume's whole
    lifetime; `NodePublish` just mounts. Keeps every CRD write in the
    unprivileged, cluster-scoped controller; the node plugin stays "dumb";
    GUID-routed shares work even with no consuming pod.
  - **Publish-time share (rejected for now):** export created per consuming node
    at `NodeStage`/`NodePublish`, torn down on unstage. Tighter security but the
    node plugin needs RBAC to write `ZfsShare`/`NetworkExport`, and shares stop
    working without a pod.
- NVMe-oF host allow-listing starts permissive on the storage network and can be
  tightened later — a security refinement, not an architecture change.

#### 3. ZFS `type` and Kubernetes `volumeMode` are independent axes

- **`protocol` fixes the ZFS `type`** (hard technical constraint):
  - `nfs` ⟹ `filesystem` dataset (only a filesystem can be NFS-exported);
  - `nvmeof` ⟹ `volume`/zvol (only a block device can be NVMe-oF-exported).
- **`volumeMode` is orthogonal and resolved by the node plugin (Step 7):**
  - `nfs` → always a mounted filesystem.
  - `nvmeof` + `volumeMode=Filesystem` → node connects the zvol, `mkfs` if empty,
    mounts it → **filesystem PVC on a zvol** (e.g. databases).
  - `nvmeof` + `volumeMode=Block` → node exposes the raw connected block device.
- Only rejected combination: `volumeMode=Block` + `protocol=nfs`.
- Consequence: the controller derives `spec.type` from `protocol` alone; it does
  **not** read `volumeMode` to pick the ZFS type. This supports both "media on an
  NFS filesystem" and "database filesystem on a zvol" from the same driver.

#### 4. Parameter inheritance — three flat layers, no templating

Parameters resolve into a single `map[string]string` (later layer wins), then
parse into the CRD specs. Deliberately simpler than democratic-csi templating.

1. **Provisioner defaults** — a YAML map mounted into the controller
   (`--default-parameters-file`, sourced from Helm values).
2. **StorageClass `parameters`** — arrive in `CreateVolumeRequest.Parameters`.
3. **PVC annotations** — `external-provisioner` runs with
   `--extra-create-metadata`, which injects
   `csi.storage.k8s.io/pvc/{name,namespace}`; the controller fetches that PVC and
   overlays annotations prefixed `param.simple-zfs-csi.io/<key>`.

Resolved keys (all optional except `poolGUID` and `protocol`, which must resolve
from some layer). `poolGUID` and `datasetPrefix` are **StorageClass-only** — see
[ADR-0002](#adr-0002--poolguid-and-datasetprefix-are-storageclass-only):

| Key | Applies to | Notes |
|-----|-----------|-------|
| `poolGUID` | ZfsDataset/ZfsShare | required; **StorageClass-only**; fixed per StorageClass |
| `protocol` | both | `nfs`\|`nvmeof` → derives ZFS `type` |
| `datasetPrefix` | ZfsDataset | **StorageClass-only**; final `dataset = <prefix>/<pv-name>` |
| `volblocksize` | zvol only | |
| `nfsClients` | ZfsShare | comma list, e.g. `10.0.0.0/8:rw` |
| `nvmeofAllowedHosts` | ZfsShare | comma list of host NQNs (empty = allow-all) |
| `property.<zfsprop>` | ZfsDataset | pass-through to `spec.properties` |

Capacity: `CreateVolumeRequest.capacity_range` maps to the zvol `spec.volume.size`
and to the filesystem `spec.filesystem.quota`.

#### 5. `DeleteVolume`

Deletes the `ZfsShare` and `ZfsDataset` CRDs; finalizers on the agent/operator
drive the actual teardown (`zfs destroy`, export removal). The controller does no
direct ZFS or export work.

### Consequences

- The node plugin (Step 7) only needs `ZfsPool.status` + the `volume_context`; it
  never writes CRDs and never learns an absolute path from the controller.
- `ControllerExpandVolume` and snapshots remain optional, layered later.
- The controller stays a replaceable adapter: all reconciliation lives in the
  agent (`ZfsDataset`) and operator (`ZfsShare → NetworkExport`).
