# zfs-shares

Cloud-native network sharing of pre-provisioned ZFS storage on Talos Linux
(no SSH, no systemd-in-a-pod). A generic, cluster-scoped CRD, `NetworkExport`, expresses
the intent to export a node-local path from a specific storage node. Two lightweight,
per-node controllers turn that intent into live exports:

| Controller | Image | Reconciles | Mechanism |
|------------|-------|------------|-----------|
| `nfs-controller` | `zfs-shares-nfs` | `protocol: nfs` exports | writes `/etc/exports`, runs `exportfs -ra`, supervises the in-container NFS server |
| `nvmeof-controller` | `zfs-shares-nvmeof` | `protocol: nvmeof` exports | programs the kernel NVMe target via `configfs` (`/sys/kernel/config/nvmet`) |
| `zpool-discovery` | `zfs-shares-discovery` | local ZFS pools (Tier 1) | polls `zpool`/`zfs`, publishes each pool's identity, routing and health into `ZfsPool` objects |
| `operator` | `zfs-shares-operator` | core `Node` objects (Tier 2) | cluster-wide control plane: detects node death and forces stale `ZfsPool` status to `NODE_OFFLINE` |

Storage *allocation* (creating datasets/zvols, quotas, snapshots) is intentionally
**out of scope** here — that is owned by the CSI/storage plane. `NetworkExport` carries
no sizing parameters; it is strictly an "intent to share" over the network.

## Architecture

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

Each controller:

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
alone. Host NQN allow-lists are supported today; DH-CHAP auth can be layered on
later via the same `attr_*` files.

### Why the NFS controller supervises the daemons

Rather than an entrypoint script that `exec`s away and leaves `rpcbind`/`rpc.mountd`
dangling and unobserved, the Go binary is the single container process. It mounts
the required kernel filesystems, starts `rpcbind` and `rpc.mountd` as **supervised
children** (their logs are piped to the controller's stdout), starts the `nfsd`
kernel threads, and treats the death of any critical daemon as fatal — the
container exits non-zero and Kubernetes restarts the pod.

## The `NetworkExport` resource

```yaml
apiVersion: storage.zfs-shares.io/v1alpha1
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
apiVersion: storage.zfs-shares.io/v1alpha1
kind: NetworkExport
metadata:
  name: pvc-postgres-data
spec:
  nodeName: talos-node-01
  protocol: nvmeof
  path: /dev/zvol/tank/pvc-postgres-data
  nvmeof:
    # nqn omitted -> derived: nqn.2025-01.io.zfs-shares:talos-node-01:pvc-postgres-data
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
apiVersion: storage.zfs-shares.io/v1alpha1
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

## Build

```sh
make build          # compile both binaries
make vet            # static checks
make docker         # build both container images
make manifests      # regenerate deepcopy + CRD/RBAC from Go types (needs network)
make helm-lint      # lint the chart
make helm-template  # render the chart
```

## Deploy

The chart and both images are published to GHCR by the release pipeline:

- `oci://ghcr.io/hellivan/charts/zfs-shares` (chart)
- `ghcr.io/hellivan/zfs-shares-nfs` / `ghcr.io/hellivan/zfs-shares-nvmeof` (images)

```sh
# Label your storage node(s) first:
kubectl label node talos-node-01 zfs-shares.io/storage=true

# Install a published version:
helm install zfs-shares oci://ghcr.io/hellivan/charts/zfs-shares \
  --version 0.1.0 \
  --namespace zfs-shares --create-namespace \
  --set nfs.pool.hostPath=/tank --set nfs.pool.mountPath=/tank
```

Or from the local checkout:

```sh
make helm-install    # helm upgrade --install into namespace zfs-shares
```

Common values (see [charts/zfs-shares/values.yaml](charts/zfs-shares/values.yaml)):

| Value | Default | Purpose |
|-------|---------|---------|
| `nfs.enabled` / `nvmeof.enabled` | `true` | toggle each controller |
| `nfs.pool.hostPath` / `mountPath` | `/tank` | ZFS pool root visible to the NFS pod |
| `nfs.v4Only` | `false` | NFSv4-only mode (drops rpcbind) |
| `nvmeof.transport.serviceId` | `"4420"` | NVMe/TCP port |
| `nvmeof.nqnPrefix` | `nqn.2025-01.io.zfs-shares` | derived subsystem NQN prefix |
| `nodeSelector` | `zfs-shares.io/storage: "true"` | where the DaemonSets run |
| `image.tag` | chart `appVersion` | image tag override |

### CRD management & upgrades

The `NetworkExport` CRD is generated from the Go types (`make manifests`) and shipped
in the chart's Helm-native [charts/zfs-shares/crds](charts/zfs-shares/crds)
directory. Helm treats `crds/` specially:

- **Install:** `helm install` applies the CRD automatically. Skip it (e.g. when
  CRDs are managed separately) with `helm install --skip-crds`.
- **Upgrade:** Helm **never** updates (or deletes) CRDs in `crds/` on `helm
  upgrade`, and there is **no flag** that changes this. When a release changes
  the CRD schema you must apply it yourself, **before** upgrading the workload:

  ```sh
  # from a local checkout:
  make install-crd            # kubectl apply -f charts/zfs-shares/crds/

  # or from a published chart version:
  helm pull oci://ghcr.io/hellivan/charts/zfs-shares --version <v> --untar
  kubectl apply -f zfs-shares/crds/

  # then upgrade the workload:
  helm upgrade zfs-shares oci://ghcr.io/hellivan/charts/zfs-shares --version <v> ...
  ```

  `kubectl apply` on a CRD is additive and safe for the schema changes this
  project makes (adding fields/validations). Never `kubectl delete` the CRD to
  "refresh" it — that cascade-deletes every `NetworkExport` object.
- **Uninstall:** Helm does not remove `crds/` CRDs, so the CRD and any `NetworkExport`
  objects are retained by design.

### Host prerequisites (Talos system extensions)

- **NFS:** `nfsd` + `sunrpc` kernel modules available on the host.
- **NVMe-oF:** `nvmet` + `nvmet-tcp` kernel modules loaded; `configfs` mounted at
  `/sys/kernel/config`.
- Both DaemonSets run `privileged` with `hostNetwork` so exports are reachable on
  the node IP (NFS `2049/111/20048`, NVMe/TCP `4420` by default).

## CI/CD

Two GitHub Actions workflows:

- [.github/workflows/ci.yaml](.github/workflows/ci.yaml) — on push/PR to `main`:
  `gofmt`/`go vet`/`build`/`test`, `helm lint` + `helm template`, and a no-push
  Docker build of both images.
- [.github/workflows/release.yaml](.github/workflows/release.yaml) — on a
  `v*.*.*` tag (or manual dispatch): builds and pushes both multi-arch
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
api/v1alpha1/            NetworkExport types + generated deepcopy
cmd/nfs-controller/      NFS controller entrypoint
cmd/nvmeof-controller/   NVMe-oF controller entrypoint
internal/controller/     reconcilers (nfs, nvmeof) + shared helpers
internal/nfsserver/      /etc/exports rendering + NFS daemon supervisor
internal/nvmet/          nvmet configfs backend
config/samples/          example NetworkExport objects
charts/zfs-shares/       Helm chart (canonical deploy path; CRD lives here)
build/                   Dockerfiles (multi-arch)
.github/workflows/       CI + release pipelines
```
