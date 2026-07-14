# zfs-shares

Cloud-native network sharing of pre-provisioned ZFS storage on Talos Linux
(no SSH, no systemd-in-a-pod). A single cluster-scoped CRD, `ZfsShare`, expresses
the intent to export a ZFS path from a specific storage node. Two lightweight,
per-node controllers turn that intent into live exports:

| Controller | Image | Reconciles | Mechanism |
|------------|-------|------------|-----------|
| `nfs-controller` | `zfs-shares-nfs` | `protocol: nfs` shares | writes `/etc/exports`, runs `exportfs -ra`, supervises the in-container NFS server |
| `nvmeof-controller` | `zfs-shares-nvmeof` | `protocol: nvmeof` shares | programs the kernel NVMe target via `configfs` (`/sys/kernel/config/nvmet`) |

Storage *allocation* (creating datasets/zvols, quotas, snapshots) is intentionally
**out of scope** here — that is owned by the CSI/storage plane. `ZfsShare` carries
no sizing parameters; it is strictly an "intent to share" over the network.

## Architecture

```
[ ZfsShare CRD (etcd = source of truth) ]
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
  set from the complete list of owned `ZfsShare` objects, so state is always
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

## The `ZfsShare` resource

```yaml
apiVersion: storage.zfs-shares.io/v1alpha1
kind: ZfsShare
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
kind: ZfsShare
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

The `ZfsShare` CRD is generated from the Go types (`make manifests`) and shipped
in the chart's Helm-native [charts/zfs-shares/crds](charts/zfs-shares/crds)
directory: `helm install` creates it automatically, but `helm upgrade` does not
update it — apply schema changes with `make install-crd`.

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
api/v1alpha1/            ZfsShare types + generated deepcopy
cmd/nfs-controller/      NFS controller entrypoint
cmd/nvmeof-controller/   NVMe-oF controller entrypoint
internal/controller/     reconcilers (nfs, nvmeof) + shared helpers
internal/nfsserver/      /etc/exports rendering + NFS daemon supervisor
internal/nvmet/          nvmet configfs backend
config/samples/          example ZfsShare objects
charts/zfs-shares/       Helm chart (canonical deploy path; CRD lives here)
build/                   Dockerfiles (multi-arch)
.github/workflows/       CI + release pipelines
```
