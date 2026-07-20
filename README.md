# simple-zfs-csi

Cloud-native, dynamically-provisioned ZFS storage for Kubernetes on Talos Linux
(no SSH, no systemd-in-a-pod). A PersistentVolumeClaim becomes a ZFS dataset or
zvol, exported over NFS or NVMe-oF and mounted into a pod — driven entirely by
cluster-scoped CRDs, with the physical pool addressed by its **immutable GUID**
so shares survive node takeover, renames and IP changes.

The system is layered: a thin CSI adapter records *intent* as CRDs, per-node
agents realise ZFS *allocation*, a cluster-wide operator compiles *shares* into
node-pinned *exports*, and per-node aggregators execute those exports.

| Component | Image | Kind | Reconciles | Mechanism |
|-----------|-------|------|------------|-----------|
| `csi-controller` | `simple-zfs-csi-controller` | Deployment (cluster-wide) | CSI `CreateVolume`/`DeleteVolume` + `ControllerPublish`/`Unpublish` | thin gRPC adapter: writes `ZfsDataset`; at attach writes one `ZfsShareAttachRequest` per node; returns a routing-only volume context |
| `csi-node` | `simple-zfs-csi-node` | DaemonSet (all nodes) | CSI `NodePublishVolume` | resolves `ZfsPool.status`, `mount -t nfs` / `nvme connect`; refuses `NODE_OFFLINE` |
| `zpool-discovery` (agent) | `simple-zfs-csi-discovery` | DaemonSet (storage nodes) | local pools (Tier 1) + `ZfsDataset` | polls `zpool`/`zfs` into `ZfsPool`; `zfs create/destroy` for datasets/zvols |
| `operator` | `simple-zfs-csi-operator` | Deployment (x1, leader-elected) | `Node` (Tier 2) + `ZfsShareAttachRequest` + `ZfsShare` | offlines dead nodes' `ZfsPool`; aggregates attach requests into a lazy `ZfsShare` → `NetworkExport` |
| `nfs-controller` | `simple-zfs-csi-nfs` | DaemonSet (storage nodes) | `protocol: nfs` `NetworkExport` | writes `/etc/exports`, runs `exportfs -ra`, supervises the NFS server |
| `nvmeof-controller` | `simple-zfs-csi-nvmeof` | DaemonSet (storage nodes) | `protocol: nvmeof` `NetworkExport` | programs the kernel NVMe target via `configfs` (`/sys/kernel/config/nvmet`) |

The CRDs form a compile-down chain — a PVC turns into a `ZfsDataset` at
provisioning; at attach time each consuming node adds a `ZfsShareAttachRequest`,
which the operator aggregates into a lazy `ZfsShare`, and the `ZfsShare` renders a
`NetworkExport`; separately, a `VolumeSnapshot` turns into a `ZfsSnapshot`:

| CRD | Scope | Keyed on | Written by | Purpose |
|-----|-------|----------|------------|---------|
| `ZfsPool` | Cluster | pool GUID (`metadata.name`) | discovery + operator | routing + health of a physical pool |
| `ZfsDataset` | Cluster | `spec.poolGUID` | csi-controller (creates), agent (reconciles) | dataset/zvol allocation intent |
| `ZfsSnapshot` | Cluster | `spec.poolGUID` + source dataset | csi-controller (creates), agent (reconciles) | point-in-time `dataset@snap` for restore/clone |
| `ZfsShareAttachRequest` | Cluster | `spec.volumeName` + `spec.nodeName` | csi-controller (creates/deletes) | per-node "attach intent"; ref-counts the aggregated `ZfsShare` |
| `ZfsShare` | Cluster | `spec.poolGUID` + `spec.dataset` | operator (aggregates from attach requests) | ZFS-centric "intent to share"; renders a `NetworkExport` |
| `NetworkExport` | Cluster | `spec.nodeName` + `spec.path` | operator (owns) or admin (standalone) | generic, ZFS-agnostic node-local export executor contract |

Key rule: **`ZfsShare` compiles down to `NetworkExport`.** Only `NetworkExport`
controllers touch `/etc/exports` / nvmet `configfs` — exactly one aggregator per
node per protocol. `NetworkExport` itself carries no ZFS or sizing parameters, so
it can still be authored by hand for non-ZFS paths.

## Architecture

### Provisioning flow (PVC → mounted pod)

```
kubelet ──► csi-controller ──► ZfsDataset ──► agent: zfs create dataset/zvol
(CreateVolume)   (writes CRD)                  (no export yet — the share is lazy)

kubelet ─────────► csi-controller ──► ZfsShareAttachRequest ──► operator aggregates ──► ZfsShare ──► NetworkExport
(ControllerPublish,  (one per            (ref-counted node allow-list)                     │
 via external-attacher) volume+node)                                                       │
                                                          ┌────────────────────────────────┴───────────┐
                                                          ▼                                            ▼
                                                   nfs-controller                             nvmeof-controller
                                                   (/etc/exports)                             (nvmet configfs)

kubelet ──► csi-node (NodePublishVolume) ──► reads ZfsPool.status (currentIP, baseMountPath)
                                              └► mount -t nfs  /  nvme connect + mount
```

The CSI plane is deliberately **thin**: `csi-controller` performs no ZFS work —
at `CreateVolume` it records a `ZfsDataset` (the export stays lazy), and at
`ControllerPublishVolume` (driven by `external-attacher`) it records one
`ZfsShareAttachRequest` per consuming node, then waits for the export to go live.
It returns only a routing-only volume context (`poolGUID`, `dataset`,
`protocol`). All ZFS allocation happens in the per-node agent; the operator
aggregates attach requests into a ref-counted `ZfsShare` → `NetworkExport` (torn
down at the last detach); `csi-node` only resolves the live `ZfsPool.status` and
mounts.

### Export execution

```
[ NetworkExport CRD (etcd = source of truth) ]
			│  spec.nodeName + spec.protocol
	 ┌──────────────┴───────────────┐
	 ▼                              ▼
[ nfs-controller DaemonSet ]   [ nvmeof-controller DaemonSet ]
 (node-01, node-02, ...)        (node-01, node-02, ...)
	 │ ignores shares for          │ ignores shares for
	 │ other nodes/protocols       │ other nodes/protocols
	 ▼                              ▼
 /etc/exports + exportfs        configfs /sys/kernel/config/nvmet
```

Each `NetworkExport` controller:

- Acts **only** on shares whose `spec.nodeName` matches its own `$NODE_NAME`
  (injected via the downward API) and whose `spec.protocol` matches.
- Is **level-driven**: every event triggers a full rebuild of that node's export
  set from the complete list of owned `NetworkExport` objects, so state is always
  reconstructed from etcd (self-healing across pod/node restarts).
- Runs with `MaxConcurrentReconciles: 1`; no leader election (one owner per node).

### Why configfs instead of `nvmetcli restore`

`nvmetcli restore` tears down and rebuilds the *entire* target on every apply,
dropping all active NVMe connections whenever any single volume changes. The
controller writes `configfs` directly so it can reconcile **incrementally** —
only the changed subsystem is touched; unrelated connected subsystems are left
alone. Access control is zero-trust: each attach derives a per-node host NQN
allow-list and applies a per-attach DH-CHAP in-band key, both written through the
same `configfs` files (see `docs/design-decisions.md` ADR-0011).

### Why the NFS controller supervises the daemons

Rather than an entrypoint script that `exec`s away and leaves `rpcbind`/`rpc.mountd`
dangling and unobserved, the Go binary is the single container process. It mounts
the required kernel filesystems, starts `rpcbind` and `rpc.mountd` as **supervised
children** (their logs are piped to the controller's stdout), starts the `nfsd`
kernel threads, and treats the death of any critical daemon as fatal — the
container exits non-zero and Kubernetes restarts the pod.

## The `NetworkExport` resource

```yaml
apiVersion: storage.simple-zfs-csi.io/v1alpha1
kind: NetworkExport
metadata:
  name: pvc-media-movies
spec:
  nodeName: talos-node-01          # pins the export to the storage node
  protocol: nfs                    # nfs | nvmeof
  path: /tank/media/movies         # dataset mountpoint (nfs) or zvol dev (nvmeof)
  nfs:
    clients:
      - client: "10.0.0.0/24"
        options: ["rw", "sync", "no_subtree_check", "no_root_squash"]
```

```yaml
apiVersion: storage.simple-zfs-csi.io/v1alpha1
kind: NetworkExport
metadata:
  name: pvc-postgres-data
spec:
  nodeName: talos-node-01
  protocol: nvmeof
  path: /dev/zvol/tank/pvc-postgres-data
  nvmeof:
    # nqn omitted -> derived: nqn.2025-01.io.simple-zfs-csi:talos-node-01:pvc-postgres-data
    allowedHosts:
      - "nqn.2014-08.org.nvmexpress:uuid:worker-01"
```

See `config/samples/` for full examples.

## The `ZfsPool` resource & node-health monitoring

`ZfsPool` is a cluster-scoped, node-agnostic handle to a physical ZFS pool. Its
`metadata.name` is the **immutable ZFS pool GUID** (`zpool-<GUID>`), so the same
pool maps to exactly one object no matter which node imports it or how it is
renamed. Nobody authors `ZfsPool` objects by hand — a two-tier monitor keeps them
in sync so CSI clients never route to a dead target:

- **Tier 1 — `zpool-discovery` (per-node DaemonSet):** polls the local `zpool`
  view and writes `status.poolName`, `status.currentNode`, `status.currentIP`,
  `status.baseMountPath` (`zfs get mountpoint`) and `status.health`
  (`ONLINE` / `DEGRADED` / `FAULTED` / `SUSPENDED`). Importing a pool on a new
  node automatically takes over its object.
  By default it runs the **host's own** `zpool`/`zfs` (via `chroot /host`, which
  Talos documents for the `siderolabs/zfs` extension) so the CLI can never drift
  from the host ZFS kernel module. Switch to `nsenter` or the in-image tools via
  `discovery.hostExec.*` in values.
- **Tier 2 — `operator` (single Deployment):** watches core `Node` objects
  and, when a node goes `NotReady` (or vanishes), forcibly sets every `ZfsPool` it
  last served to `status.health: NODE_OFFLINE`. A completely dead node can't
  self-report, so this override is what prevents a stale `ONLINE` at a dead IP.

```yaml
apiVersion: storage.simple-zfs-csi.io/v1alpha1
kind: ZfsPool
metadata:
  name: zpool-12140134988506841113   # immutable ZFS pool GUID
spec: {}                             # intentionally empty; ZfsPool is fully discovered
status:                              # written by the operator, not the user
  poolName: tank                     # observed name; may be renamed safely
  currentNode: talos-node-01
  currentIP: 192.168.10.15
  baseMountPath: /tank
  health: ONLINE
```

A CSI node plugin resolves the real mount at attach time by reading
`status.currentIP` + `status.baseMountPath` from the live object and joining the
logical dataset name — so node deaths, IP changes, pool renames and mountpoint
shifts never require editing a PersistentVolume.

## Dynamic provisioning (CSI)

The CSI driver (`simple-zfs-csi.io`) turns a PVC into a ZFS dataset (NFS) or zvol
(NVMe-oF). Provisioning is driven by **StorageClass parameters**, merged in three
flat layers (later wins):

```
chart defaultParameters  <  StorageClass.parameters  <  PVC annotations
                                                        (param.simple-zfs-csi.io/<key>)
```

| Parameter | Where | Meaning |
|-----------|-------|---------|
| `poolGUID` | **StorageClass only** | target `ZfsPool` GUID; no default — required per class |
| `datasetPrefix` | **StorageClass only** | dataset namespace prepended to the volume name |
| `protocol` | any layer | `nfs` (→ filesystem) or `nvmeof` (→ zvol) |
| `volblocksize` | any layer | zvol block size (NVMe-oF only) |
| `property.<name>` | any layer | passed through as a ZFS `-o` property |

> NFS client allow-lists and NVMe-oF host NQNs are **not** parameters — they are
> derived automatically at attach time from the consuming node(s) under a
> zero-trust model (see `docs/design-decisions.md` ADR-0010/0011).

`poolGUID` and `datasetPrefix` are **StorageClass-only** on purpose (ADR-0002):
they define tenancy/placement, so a PVC author cannot redirect a claim to another
pool or dataset namespace via annotations — those keys are stripped from the
`defaultParameters` and PVC-annotation layers.

`protocol` also fixes the ZFS object type (`nfs` → filesystem, `nvmeof` →
zvol); `volumeMode` (`Filesystem` vs `Block`) is orthogonal and resolved at the
node. The only rejected combination is `Block` + `nfs`.

StorageClasses are declared in the chart under `storageClasses:` (a list; none
are created by default). Each entry's `name` is used **verbatim** as the
StorageClass name (it is not prefixed with the release name), so it is exactly
what PVCs reference in `spec.storageClassName`:

```yaml
# values.yaml
storageClasses:
  - name: nfs
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
    allowVolumeExpansion: true
    parameters:
      poolGUID: "12140134988506841113"   # required
      datasetPrefix: k8s/nfs
      protocol: nfs
  - name: nvmeof
    parameters:
      poolGUID: "12140134988506841113"
      datasetPrefix: k8s/block
      protocol: nvmeof
      volblocksize: "16k"
```

```yaml
# A PVC that overrides a ZFS property for just this claim:
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: media-movies
  annotations:
    param.simple-zfs-csi.io/property.compression: "zstd"
spec:
  accessModes: ["ReadWriteMany"]
  storageClassName: nfs
  resources:
    requests:
      storage: 100Gi
```

### Volume expansion

Set `allowVolumeExpansion: true` on the StorageClass and grow a PVC by editing its
`spec.resources.requests.storage`. The `external-resizer` sidecar
(`csiController.resizer`, on by default) drives it:

- **NFS (filesystem):** the agent raises the dataset's `refquota` — effective
  immediately, no node action.
- **NVMe-oF (zvol):** the agent grows `volsize`, then the node rescans the NVMe
  namespace and grows the on-device filesystem (`resize2fs`/`xfs_growfs`).

Shrinking is not supported for zvols (and Kubernetes forbids PVC shrink).

## Build

```sh
make build          # compile all binaries
make vet            # static checks
make docker         # build all container images
make manifests      # regenerate deepcopy + CRD/RBAC from Go types (needs network)
make helm-lint      # lint the chart
make helm-template  # render the chart
```

## Deploy

The chart and all images are published to GHCR by the release pipeline:

- `oci://ghcr.io/hellivan/charts/simple-zfs-csi` (chart)
- `ghcr.io/hellivan/simple-zfs-csi-{controller,node,discovery,operator,nfs,nvmeof}` (images)

```sh
# Label your storage node(s) first:
kubectl label node talos-node-01 simple-zfs-csi.io/storage=true

# Install a published version:
helm install simple-zfs-csi oci://ghcr.io/hellivan/charts/simple-zfs-csi \
  --version 0.1.0 \
  --namespace simple-zfs-csi --create-namespace \
  --set nfs.pool.hostPath=/tank --set nfs.pool.mountPath=/tank
```

Or from the local checkout:

```sh
make helm-install    # helm upgrade --install into namespace simple-zfs-csi
```

Common values (see [charts/simple-zfs-csi/values.yaml](charts/simple-zfs-csi/values.yaml)):

| Value | Default | Purpose |
|-------|---------|---------|
| `driverName` | `simple-zfs-csi.io` | CSI driver / StorageClass provisioner name |
| `logLevel` | `""` (info) | set to `debug` to log every host `zfs`/`zpool`/`mount`/`nvme` command |
| `storageClasses` | `[]` | list of StorageClasses to create (see above; none by default) |
| `csiController.enabled` | `true` | provisioner Deployment (+ external-provisioner sidecar) |
| `csiController.defaultParameters` | `{}` | lowest-priority parameter layer (`poolGUID`/`datasetPrefix` ignored here) |
| `csiNode.enabled` | `true` | node-publish DaemonSet (+ node-driver-registrar sidecar) |
| `csiNode.hostExec.*` | `chroot` | how the node plugin runs `nvme`/`mount` against the host |
| `nfs.enabled` / `nvmeof.enabled` | `true` | toggle each export controller |
| `nfs.pool.hostPath` / `mountPath` | `/tank` | ZFS pool root visible to the NFS pod |
| `nfs.v4Only` | `false` | NFSv4-only mode (drops rpcbind) |
| `nvmeof.transport.serviceId` | `"4420"` | NVMe/TCP port |
| `nvmeof.nqnPrefix` | `nqn.2025-01.io.simple-zfs-csi` | derived subsystem NQN prefix |
| `nodeSelector` | `simple-zfs-csi.io/storage: "true"` | where the storage-node DaemonSets run |
| `image.tag` | chart `appVersion` | image tag override |

### Debug logging

Every `zfs`/`zpool`/`mount`/`nvme` command the agents run against the host goes
through a single runner, which logs each invocation — and its outcome (duration,
trimmed output or error) — at debug verbosity. It logs the **fully resolved**
command, including the `chroot /host` / `nsenter` prefix and version-matched host
binary, e.g. `chroot /host /usr/sbin/zfs create tank/k8s/pvc-abc123`.

Debug logging is off by default. Turn it on for all components:

```sh
helm upgrade simple-zfs-csi ... --set logLevel=debug
```

Or for a single component without touching the rest (handy for the provisioning
agent that runs `zfs create`):

```sh
# the discovery DaemonSet is the agent that creates datasets
helm upgrade simple-zfs-csi ... --set 'discovery.extraArgs={--zap-log-level=debug}'
```

Then watch the agent on the node hosting the pool:

```sh
kubectl -n simple-zfs-csi logs -l app.kubernetes.io/component=discovery -f | grep hostcmd
```

> A `zfs create ... parent does not exist` error means the parent dataset (the
> StorageClass `datasetPrefix`, e.g. `tank/k8s`) does not exist — `zfs create`
> is not run with `-p`, so intermediate datasets are not auto-created. Create
> the prefix dataset once (`zfs create tank/k8s`) or drop the prefix.

### CRD management & upgrades

The six CRDs (`ZfsPool`, `ZfsDataset`, `ZfsSnapshot`, `ZfsShareAttachRequest`, `ZfsShare`, `NetworkExport`) are generated
from the Go types (`make manifests`) and shipped in the chart's Helm-native
[charts/simple-zfs-csi/crds](charts/simple-zfs-csi/crds) directory. Helm treats `crds/`
specially:

- **Install:** `helm install` applies the CRD automatically. Skip it (e.g. when
  CRDs are managed separately) with `helm install --skip-crds`.
- **Upgrade:** Helm **never** updates (or deletes) CRDs in `crds/` on `helm
  upgrade`, and there is **no flag** that changes this. When a release changes
  the CRD schema you must apply it yourself, **before** upgrading the workload:

  ```sh
  # from a local checkout:
  make install-crd            # kubectl apply -f charts/simple-zfs-csi/crds/

  # or from a published chart version:
  helm pull oci://ghcr.io/hellivan/charts/simple-zfs-csi --version <v> --untar
  kubectl apply -f simple-zfs-csi/crds/

  # then upgrade the workload:
  helm upgrade simple-zfs-csi oci://ghcr.io/hellivan/charts/simple-zfs-csi --version <v> ...
  ```

  `kubectl apply` on a CRD is additive and safe for the schema changes this
  project makes (adding fields/validations). Never `kubectl delete` the CRD to
  "refresh" it — that cascade-deletes every object of that kind.
- **Uninstall:** Helm does not remove `crds/` CRDs, so the CRDs and any custom
  objects are retained by design.

### Host prerequisites (Talos system extensions)

- **Storage nodes (exports + discovery):** must carry the
  `simple-zfs-csi.io/storage=true` label (see [Deploy](#deploy)).
  - **ZFS userland:** host `zpool`/`zfs` binaries (Talos `siderolabs/zfs`
    extension). The discovery agent runs them via `chroot /host` and ships **no**
    ZFS tools of its own, so the CLI can never drift from the host kernel module.
  - **NFS:** `nfsd` + `sunrpc` kernel modules available on the host.
  - **NVMe-oF:** `nvmet` + `nvmet-tcp` kernel modules loaded; `configfs` mounted
    at `/sys/kernel/config`.
  - The export DaemonSets run `privileged` with `hostNetwork` so exports are
    reachable on the node IP (NFS `2049/111/20048`, NVMe/TCP `4420` by default).
- **All nodes (csi-node client mounts):**
  - **NFS client:** `nfs`/`nfsd` client support to `mount -t nfs`.
  - **NVMe-oF initiator:** `nvme_tcp` (+ `nvme_fabrics`) modules loaded; the
    plugin shells `nvme connect`/`nvme disconnect` against the host via
    `csiNode.hostExec` (`chroot /host` by default).
  - **Mount propagation:** the kubelet must allow rshared bind-mount propagation
    (the default on Talos) so the node plugin's mounts reach consuming pods.
- **Cluster-wide (optional):**
  - **Snapshots:** the external snapshot CRDs (`VolumeSnapshot`,
    `VolumeSnapshotClass`, `VolumeSnapshotContent`) and the `snapshot-controller`
    must be installed **before** using `volumeSnapshotClasses` / snapshotting;
    this chart does not ship them.

## CI/CD

Two GitHub Actions workflows:

- [.github/workflows/ci.yaml](.github/workflows/ci.yaml) — on push/PR to `main`:
  `gofmt`/`go vet`/`build`/`test`, `helm lint` + `helm template`, and a no-push
  Docker build of all images.
- [.github/workflows/release.yaml](.github/workflows/release.yaml) — on a
  `v*.*.*` tag (or manual dispatch): builds and pushes the multi-arch
  (`amd64`+`arm64`) images to GHCR via Buildx, then packages the Helm chart with
  the tag version and `helm push`es it as an OCI artifact. Uses the built-in
  `GITHUB_TOKEN` (`packages: write`) — no extra secrets required.

Cut a release:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The chart's `appVersion` (and thus the default image tag) is set to the tag
version at package time, so `helm install --version 0.1.0` pulls the matching
`:0.1.0` images.

## Repository layout

```
api/v1alpha1/            CRD types (ZfsPool, ZfsDataset, ZfsShare, NetworkExport) + deepcopy
cmd/csi-controller/      CSI controller (provisioner) entrypoint
cmd/csi-node/            CSI node plugin entrypoint
cmd/nfs-controller/      NFS export controller entrypoint
cmd/nvmeof-controller/   NVMe-oF export controller entrypoint
cmd/operator/            cluster operator (node health + ZfsShare) entrypoint
cmd/zpool-discovery/     per-node agent (ZfsPool discovery + ZfsDataset) entrypoint
internal/csi/            CSI identity/controller/node servers + params + mount backend
internal/controller/     reconcilers (nfs, nvmeof, zfsshare, zfsdataset, discovery) + helpers
internal/nfsserver/      /etc/exports rendering + NFS daemon supervisor
internal/nvmet/          nvmet configfs backend
internal/zpool/          zpool/zfs CLI wrappers + host-exec
config/samples/          example CRD objects
charts/simple-zfs-csi/       Helm chart (canonical deploy path; CRDs live here)
build/                   Dockerfiles (multi-arch)
.github/workflows/       CI + release pipelines
```
