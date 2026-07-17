# Design Decisions

An append-only log of architectural decisions (ADR-lite). Each entry records the
decision, the context, the options weighed, and the consequences. Newest first.

The complementary conventions doc is [api-conventions.md](api-conventions.md);
the build plan is [implementation-strategy.md](implementation-strategy.md).

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
(`ZfsVolume` + `ZfsShare`) and never returns an absolute path — only a
`volume_context`. Several forks needed pinning before implementation.

### Decisions

#### 1. Pool selection — fixed per StorageClass

`spec.poolGUID` is taken from a StorageClass parameter (resolvable via the
inheritance chain below); one StorageClass targets one pool. No scheduler /
free-space picking and no CSI topology awareness in this step.

- Rationale: deterministic, no placement logic, matches the GUID-keyed routing
  model already in place. Scheduling across a pool set can be layered later
  without changing the CRD contract.

#### 2. `CreateVolume` creates **both** `ZfsVolume` and `ZfsShare` (provision-time share)

`CreateVolume` writes the `ZfsVolume`, waits for it to reach `Ready`, writes the
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
   overlays annotations prefixed `param.zfs-shares.io/<key>`.

Resolved keys (all optional except `poolGUID` and `protocol`, which must resolve
from some layer). `poolGUID` and `datasetPrefix` are **StorageClass-only** — see
[ADR-0002](#adr-0002--poolguid-and-datasetprefix-are-storageclass-only):

| Key | Applies to | Notes |
|-----|-----------|-------|
| `poolGUID` | ZfsVolume/ZfsShare | required; **StorageClass-only**; fixed per StorageClass |
| `protocol` | both | `nfs`\|`nvmeof` → derives ZFS `type` |
| `datasetPrefix` | ZfsVolume | **StorageClass-only**; final `dataset = <prefix>/<pv-name>` |
| `volblocksize` | zvol only | |
| `nfsClients` | ZfsShare | comma list, e.g. `10.0.0.0/8:rw` |
| `nvmeofAllowedHosts` | ZfsShare | comma list of host NQNs (empty = allow-all) |
| `property.<zfsprop>` | ZfsVolume | pass-through to `spec.properties` |

Capacity: `CreateVolumeRequest.capacity_range` maps to the zvol `spec.volume.size`
and to the filesystem `spec.filesystem.quota`.

#### 5. `DeleteVolume`

Deletes the `ZfsShare` and `ZfsVolume` CRDs; finalizers on the agent/operator
drive the actual teardown (`zfs destroy`, export removal). The controller does no
direct ZFS or export work.

### Consequences

- The node plugin (Step 7) only needs `ZfsPool.status` + the `volume_context`; it
  never writes CRDs and never learns an absolute path from the controller.
- `ControllerExpandVolume` and snapshots remain optional, layered later.
- The controller stays a replaceable adapter: all reconciliation lives in the
  agent (`ZfsVolume`) and operator (`ZfsShare → NetworkExport`).
