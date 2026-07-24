# Known Pitfalls & Recurring Bug Classes

A troubleshooting catalogue of bug *classes* that have bitten this driver more
than once, each with the symptom, root cause, the guard that fixes it, and where
that guard lives. Consult this before touching NVMe-oF device handling, CSI
sidecar RBAC, controller client reads, or volume expansion — new code should be
checked against every class below.

Complementary docs: architectural decisions in
[design-decisions.md](design-decisions.md); API rules in
[api-conventions.md](api-conventions.md).

Environment assumptions that make these bugs live:

- Storage node runs **nvme-cli 2.x** (JSON schema differs from 1.x).
- Kernel has **native NVMe multipath enabled** (`CONFIG_NVME_MULTIPATH=y`), so a
  subsystem is exposed as a shared *head* device plus per-controller *path*
  devices.
- The node kernel (Talos) lacks **`CONFIG_NVME_HOST_AUTH`**, so host-side
  DH-CHAP is unsupported. Auth is disabled operationally via
  `values.yaml` `nvmeof.auth.dhchap.enabled=false`; the chart default stays
  **on**. (Target-side `CONFIG_NVME_TARGET_AUTH=y` works.)
- Several controllers run with **namespaced RBAC** (least privilege).

---

## 1. nvme CLI device-type confusion (controller vs namespace device)

**Symptom:** `nvme ns-rescan /dev/nvme0n1: exit status 1: Namespace Rescan:
Block device required`; volume expansion of NVMe-oF (block) volumes stuck at
`NodeExpandVolume` with `NodeResizeError`.

**Root cause:** `nvme ns-rescan` (and `id-ns`, `id-ctrl`, `reset`) require a
**controller char device** (`/dev/nvme0`). The multipath **namespace/head block
device** (`/dev/nvme0n1`) is rejected. A subsystem may also be reachable through
several controllers/paths.

**Guard:** resolve the controllers for the NQN and rescan **each** one. See
`RescanNVMe` and `nvmeControllersForNQN` in
[internal/csi/mount.go](../internal/csi/mount.go). `NVMeDisconnect` (`disconnect
-n <nqn>`) and `nvme list` (no device arg) are safe. `waitNVMeDevice` already
rescans the controller form.

**Guarded by:** the *"resize not working for block devices (nvmeof)"* fix
(and the earlier *"block device not found"* fix).

---

## 2. Multipath sysfs device-name assumptions

**Symptom:** csi-node spins forever after `nvme connect` succeeds; the device is
present (`/dev/nvme0n1`) but never resolved; repeated `nvme ns-rescan` / `nvme
list`.

**Root cause:** with multipath on, a controller under `/sys/class/nvme/nvmeC`
carries only the **path** device `nvme<S>c<C>n<N>` (e.g. `nvme0c0n1`). The usable
**head** block device `nvme<S>n<N>` (e.g. `nvme0n1`) lives under `/sys/block`,
not as a direct child of the controller. Code that assumes `nvmeXnY` is a direct
child of the controller finds nothing.

**Guard:** use `nvmeNamespaceFromSysfs` in
[internal/csi/mount.go](../internal/csi/mount.go), which handles both layouts —
direct head (non-multipath) and derives the head from the path device
(`nvmePathDeviceRe`) under multipath. Never re-derive device names ad hoc.

**Guarded by:** the *"block device not found"* fix (multipath head derivation)
and the *"nvmeof connection still not working"* fix.

---

## 3. nvme-cli JSON schema drift across versions

**Symptom:** device resolution that worked on one host silently returns nothing
on another.

**Root cause:** `nvme list -o json` changed schema between 1.x and 2.x — the 2.x
flat list omits `SubsystemNQN` once a namespace is present, so a parser keyed on
it finds no match. `parseNVMeListDevice` is therefore an effectively-dead
fallback on 2.x.

**Guard:** **sysfs is authoritative** — `nvmeDevice` reads sysfs first and only
falls back to JSON. Do not add new dependencies on `nvme` CLI JSON output. See
`nvmeDevice` / `parseNVMeListDevice` in
[internal/csi/mount.go](../internal/csi/mount.go).

**Guarded by:** the *"nvme-cli 2 not supported"* fix.

---

## 4. Cached controller-runtime client vs namespaced RBAC

**Symptom:** a reconciler stalls silently; logs spam
`<resource> is forbidden: User "…" cannot list resource "…" at the cluster
scope`; dependent resources never progress.

**Root cause:** a **cached** read (`r.Get` / `r.List` via `mgr.GetClient()`)
lazily starts a **cluster-wide** informer (LIST+WATCH) for that type. With
namespaced RBAC (a `Role`, not a `ClusterRole`) the cluster-scoped list is
forbidden, the informer never syncs, and the read never returns.

**Guard:**

- For namespaced core reads (Secrets/ConfigMaps) use **`mgr.GetAPIReader()`** — a
  direct, uncached, targeted GET (no informer). The nvmeof controller does this
  via its `SecretReader` field, wired in
  [cmd/nvmeof-controller/main.go](../cmd/nvmeof-controller/main.go); see
  [internal/controller/nvmeof_controller.go](../internal/controller/nvmeof_controller.go).
- Or scope the manager cache with `cache.Options{DefaultNamespaces: …}` (the
  operator does this from `POD_NAMESPACE` in
  [cmd/operator/main.go](../cmd/operator/main.go) — **note:** this fail-safe only
  holds while `POD_NAMESPACE` is set).
- csi-controller / csi-node use a **direct `client.New`** (uncached) client and
  are immune to this class.

**Guarded by:** the *"permission secret error"* fix and ADR-0014 in
[design-decisions.md](design-decisions.md). (Not to be confused with the earlier
fix for the configfs *root path* — see class 8.)

---

## 5. CSI sidecar RBAC gaps

**Symptom:** PVC resize accepted (`spec.resources.requests` bumped) but nothing
happens — no resize conditions, `ZfsDataset` size unchanged; the `csi-resizer`
loops on `pods is forbidden: … cannot list resource "pods" at the cluster
scope`.

**Root cause:** `external-resizer` defaults `--handle-volume-inuse-errors=true`,
which starts a cluster-wide **Pod** informer. Without `pods` `get/list/watch` the
informer never syncs, `WaitForCacheSync` never completes, and **no** resize is
ever processed (`ControllerExpandVolume` is never called).

**Guard:** grant `pods` `get/list/watch` to the controller ClusterRole (external
-resizer section) in
[charts/simple-zfs-csi/templates/rbac.yaml](../charts/simple-zfs-csi/templates/rbac.yaml).

Cross-checks for the other sidecars:

- **external-snapshotter** (co-located sidecar) manages `VolumeSnapshotContents`
  (needs full `create/update/patch/delete` + `/status`), **not**
  `VolumeSnapshots`. Read-only `volumesnapshots get,list` is correct — writing
  `VolumeSnapshots` is the separate cluster-scoped `snapshot-controller`'s job,
  which is **not** part of this chart.
- When adding a sidecar or a sidecar flag, re-derive its RBAC from the
  upstream kubernetes-csi manifests — flags like `--extra-create-metadata` and
  `--handle-volume-inuse-errors` imply extra permissions.

**Guarded by:** the *"volume resize not working"* fix.

### Volume expansion is two-phase for block volumes

- **NFS / filesystem** volumes grow **online** with only the controller-side
  `refquota` bump — no node work.
- **NVMe-oF / zvol** volumes need `ControllerExpandVolume` (bump `volsize`) **and
  then** `NodeExpandVolume` on the node: `nvme ns-rescan <controller>` followed
  by `resize2fs`. If only the capacity of the RWX/NFS PVC moves and the RWO block
  PVCs stay put, suspect the node phase (class 1) or the resizer RBAC (this
  class).

---

## 6. NVMe device-readiness race (post-connect)

**Symptom:** transient `MountVolume.SetUp failed … mkfs.ext4 … Input/output
error` and `The file /dev/nvme0n1 does not exist and no size was specified`;
self-heals on kubelet retry, producing noisy `FailedMount` warnings.

**Root cause:** right after `nvme connect` the sysfs *name* can resolve a moment
before the head block device node is created and its size populated. Returning
the path in that window makes the caller's `mkfs` / mount fail with ENOENT/EIO.

**Guard:** `waitNVMeDevice` (and the `NVMeConnect` fast-path) only return a
device once **`nvmeDeviceReady`** confirms `/sys/block/<dev>/size` exists and is
`> 0`; otherwise it keeps polling (with an `ns-rescan` nudge) until the bounded
timeout. See [internal/csi/mount.go](../internal/csi/mount.go).

**Guarded by:** the *"race condition in nvme ready function"* fix.

---

## 7. NVMe-oF (zvol) is single-node only — reject multi-node access modes

**Symptom:** an RWX (or `ReadOnlyMany`/`MultiNode`) PVC bound to an NVMe-oF
StorageClass; the same zvol attached to two nodes; ext4/xfs corruption.

**Root cause:** a zvol is a raw block device formatted with a **non-cluster**
filesystem. Exporting it to more than one node at once corrupts data — only NFS
(a real shared filesystem) can back `ReadWriteMany`.

**Guard:** reject multi-node access modes for the `nvmeof` protocol at both
admission points — `CreateVolume` and `ValidateVolumeCapabilities` — via
`hasMultiNodeAccessMode` in
[internal/csi/controller.go](../internal/csi/controller.go). Also reject `Block`
volumeMode with the `nfs` protocol. The only valid RWX path is `nfs`.

**Guarded by:** the *"restrict nvmeof to RWO"* fix.

---

## 8. Distinguish the three NVMe-oF target preconditions

**Symptom:** NVMe-oF exports never come up; opaque errors about a missing
configfs path, or the TCP listener silently never accepts connections.

**Root cause:** three *separate* host prerequisites are easy to conflate:

1. **configfs mounted** — the parent mount (`/sys/kernel/config`) must exist in
   the pod. If missing, configfs isn't mounted on the node or into the pod.
2. **`nvmet` module loaded** — the `nvmet` subtree only appears *under* the
   configfs mount once the module is loaded. The controller must manage
   `<configfsRoot>/nvmet`, **not** the configfs root itself.
3. **`nvmet_tcp` module loaded** — without the transport module the target is
   created but the TCP listener never works. This is best-effort (may be
   built-in), so treat a missing `/sys/module/nvmet_tcp` as a **warning**.

On Talos, load these via `machine.kernel.modules: [nvmet, nvmet_tcp]`.

**Guard:** `Target.Available` reports the configfs-vs-module failure modes
distinctly and `Target.TransportModuleLoaded` warns on a missing transport, in
[internal/nvmet/configfs.go](../internal/nvmet/configfs.go); the configfs root
arg points at the `.../nvmet` subtree.

**Guarded by:** the *"permission error for nvmeof controller"* fix (configfs
`/nvmet` subtree path) and the *"better error handling on missing kernel
modules"* fix.

---

## 9. hostNetwork daemonsets collide on host metric/health ports

**Symptom:** one of the per-node controllers (nfs / nvmeof / discovery)
CrashLoops or fails its health probe when co-scheduled; `bind: address already
in use` on the metrics/health port.

**Root cause:** with `hostNetwork: true`, `--metrics-bind-address` /
`--health-probe-bind-address` bind on the **node**, so two daemonsets sharing one
port set clash on the same node. A single shared `ports:` block is a trap.

**Guard:** give each hostNetwork component its **own** ports under
`<component>.ports` (nfs 8080/8081, nvmeof 8082/8083, discovery 8084/8085) in
[charts/simple-zfs-csi/values.yaml](../charts/simple-zfs-csi/values.yaml). Do not
use `hostNetwork` unless the component actually needs to serve on the node —
`status.hostIP` (downward API) already gives the node address without it.

**Guarded by:** the *"metric ports colliding"* fix and the *"remove obsolete
hostnetwork flag"* fix.

---

## 10. Host ZFS CLI must match the host kernel module

**Symptom:** `zpool`/`zfs` ioctl errors or version-mismatch warnings from the
discovery/scrub agents even though the pool is healthy on the host.

**Root cause:** the ZFS userspace tools speak an ioctl ABI tied to the ZFS
**kernel module** version. A bundled `zfsutils-linux` that drifts from the host
module (Talos `siderolabs/zfs` extension) breaks.

**Guard:** by default run the **host's own** binaries via `chroot /host` (or
`nsenter`) so the CLI can never drift; the in-image tools are only a fallback and
should track the host version. Configurable via `discovery.hostExec.*`. See
`internal/zpool/hostexec.go`, the discovery DaemonSet, and
[docs/zfs-utils-version.md](zfs-utils-version.md).

**Guarded by:** the *"improved version for binary borrowing of the host"* fix.

---

## 11. ZfsPool is discovery-only — keep observed state out of `spec`

**Symptom:** wanting to "set" a pool's name or routing in `spec`; confusion when
a host-side `zpool rename` diverges from the CRD.

**Root cause:** a `ZfsPool` is fully **discovered** — the per-node agent creates
it and reports everything into `status`. Its `metadata.name` is the immutable
pool **GUID**. The human-readable pool name is volatile (renamable on the host),
so it belongs in `status`, not `spec` (which is intentionally empty).

**Guard:** route by immutable GUID (`metadata.name`) + `status.baseMountPath`,
never by the renamable `status.poolName`. See
[api/v1alpha1/zfspool_types.go](../api/v1alpha1/zfspool_types.go).

**Guarded by:** the *"move poolname into status as it is not a desired spec"*
fix.

---

## 12. Chart StorageClass / VolumeSnapshotClass names are verbatim

**Symptom:** PVCs fail to bind because `spec.storageClassName` doesn't match —
the installed StorageClass got an unexpected `<release>-` prefix (or vice versa).

**Root cause:** unlike most templated resource names, the chart uses each
`storageClasses[].name` / `volumeSnapshotClasses[].name` **verbatim** (no
fullname prefix), so the `name` is exactly what PVCs must reference.

**Guard:** reference the bare `name` in PVCs; see
[charts/simple-zfs-csi/templates/storageclasses.yaml](../charts/simple-zfs-csi/templates/storageclasses.yaml).

**Guarded by:** the *"non verbatim storage class names"* fix.

---

## 13. Single-node (RWO) volume double-attached across nodes (attach race)

**Symptom:** during a forced pod move / node failure an NVMe-oF (RWO) volume's
attach request appears for a **new** node while the old node's attachment is
still being torn down; the losing node's `ControllerPublish` "succeeds" but its
mount never completes — or, unguarded, the same zvol is exported to two nodes.

**Root cause:** distinct from class 7, which rejects multi-node *access modes* at
admission. Here every attach carries a valid single-node cap, but the Kubernetes
attach-detach controller can create a `VolumeAttachment` for node B before node
A's detach completes (force-deleted / unreachable node). Left unchecked the
operator aggregates both nodes, and — even though the export is capped to one
host — the *losing* attach request is still marked `Ready` by share-level
readiness, so its publish returns success and the subsequent mount then fails.

**Guard (two layers):**

- **CSI controller:** `ControllerPublishVolume` rejects a single-node volume
  already published to a *different* node with **`FailedPrecondition`**, so
  external-attacher retries once the prior attachment is released. See
  `attachedNode` in [internal/csi/controller.go](../internal/csi/controller.go).
- **Operator aggregator (defense in depth):** for a zvol it exports to **exactly
  one** node — the **oldest** attach request wins, so an established export is
  never stolen by a racing newcomer — and readiness is **node-level**: a request
  whose node is not the exported one stays `Waiting`, so its publish times out and
  retries. See `oldestAttachNode` / `reconcileVolume` in
  [internal/controller/zfsshareattachrequest_controller.go](../internal/controller/zfsshareattachrequest_controller.go).
  This is a deterministic, concurrency-safe selection (see the `SetupWithManager`
  note), not a reliance on single-threaded reconciliation.

**Guarded by:** the *"handle repeated attach of an RWO volume"* fix.

---

## 14. Empty PV `fsType` silently disables fsGroup on block volumes

**Symptom:** a non-root pod (`securityContext.fsGroup` / `runAsNonRoot`) gets
**permission denied** writing to a freshly provisioned NVMe-oF (RWO) volume; the
mount succeeds and the filesystem is fine, but its root stays `root:root` — no
recursive chown ever runs. NFS (RWX) volumes are unaffected.

**Root cause:** kubelet's default fsGroup policy is
**`ReadWriteOnceWithFSType`** — it applies `fsGroup` ownership **only** to RWO
volumes whose **`pv.spec.csi.fsType` is non-empty**. external-provisioner records
`fsType` from the StorageClass `csi.storage.k8s.io/fstype` parameter (or its
`--default-fstype` flag). With neither set, the node plugin still formats ext4
(its own `FormatAndMount` fallback), so the volume *works* — but the PV's
`fsType` is `""`, so kubelet **silently skips** the chown. The failure is
per-workload (only non-root pods hit it) and easy to misread as an app bug.

**Guard:** give external-provisioner **`--default-fstype`** (matching the node
plugin's ext4 fallback so the PV's recorded type is truthful), via
`csiController.provisioner.defaultFsType` in
[charts/simple-zfs-csi/values.yaml](../charts/simple-zfs-csi/values.yaml) →
[csi-controller-deployment.yaml](../charts/simple-zfs-csi/templates/csi-controller-deployment.yaml).
Keep `fsGroupPolicy: ReadWriteOnceWithFSType` explicit on the CSIDriver
([csidriver.yaml](../charts/simple-zfs-csi/templates/csidriver.yaml),
`csiDriver.fsGroupPolicy`). This is the standard **block**-driver posture; NFS is
left to server-side ownership like every other NFS driver.

**NFS is out of scope by design, not by accident:** `ReadWriteOnceWithFSType`
never touches RWX volumes, so shared NFS exports are never recursively chowned
(which would be slow and fails under `root_squash`). The `--default-fstype` also
stamps `fsType` on NFS PVs cosmetically, but the node plugin's `publishNFS`
ignores `fsType` and the policy excludes RWX, so it is inert. The one case that
*would* invite a chown is an **RWO NFS** PVC — an unusual configuration this
driver doesn't restrict; treat NFS as RWX.

**Two-side consistency:** the provisioner default and the node's
`FormatAndMount` fallback (both ext4) must stay in sync — if you change one,
change the other, or the PV will advertise a filesystem type the node didn't
create.

**Guarded by:** the *"fsgroup does not work"* fix.

---

## 15. chroot host-exec cannot create host mounts (no-go on Talos) — use `nsenter`

**TL;DR:** `hostExec.mode` defaults to **`nsenter`** (ADR-0016). `chroot` is
fine for **read-only** host commands (`zpool status`, `zfs list`, `zpool
scrub`) but **cannot correctly create a mount that the host sees on Talos** —
use it only where you understand the propagation requirements below.

**Symptom:** with `hostExec.mode: chroot`, dynamically provisioned datasets are
invisible outside the pod that created them. Concretely: a newly provisioned
NFS PVC never exports (`exportfs` on the NFS DaemonSet can't find the path,
because the dataset the **discovery** agent just `zfs create`d is not in the
host mount table); or, if `csiNode.hostExec` is enabled, a consumer pod's volume
comes up **empty**. `nsenter` mode does not exhibit this.

**Field observation (Talos, 2026-07):** with `discovery.hostExec.mode: chroot`,
**every NFS (filesystem) PVC dataset was left `mounted no` on the host.**
`zfs get -H -o value mounted <ds>` returned `no`; the dataset was absent from
`/proc/1/mountinfo`; and files written to the "mounted" path (via the NFS export
or the toolbox `/host` path) landed in the **parent** dataset — child `USED`
stayed at 96K while the parent grew. zvols (NVMe-oF) were unaffected (block
export, no filesystem mount).

**Root cause:** `chroot /host <tool>` changes only **path resolution**, not the
**mount namespace**. So any mount the tool creates (`zfs create`/`zfs clone`
auto-mount; `mount -t nfs`; `mkfs`+`mount`; bind mounts) is born **inside the
pod's** mount namespace, materialising under the `/host` bind at
`/host/<mountpoint>`. To escape to the host it needs the `host-root` volume to
be `Bidirectional` (rshared) **and** the host source to itself be a shared
mount. Neither the trapped-in-rslave case nor the missing-shared-source case
reaches the host.

**Why this is a no-go on Talos specifically:** the `/host` volume is
`hostPath: /`. The Talos kubelet only propagates paths declared in
`machine.kubelet.extraMounts` — here only `/var/mnt/spinning-archive`
(`rshared`). `/` is **not** in `extraMounts` and rsharing the whole root into
the kubelet is impractical. So `Bidirectional` on `hostPath: /` either fails pod
startup (non-shared source) or, as observed, creates the mount but leaves it
trapped. There is no clean chroot fix.

**Fix / guard — default to `nsenter`:** `nsenter -t 1 -m` enters the **host's**
mount namespace (via `hostPID`), so `zfs create`/`zfs mount` create the mount
**directly on the host**, under the pool path Talos already shares via
`extraMounts`. It propagates to the NFS server pod and consumers with no `/host`
volume, no Bidirectional plumbing, and no whole-root rshare. All host-exec
components (`discovery`, `csiNode`, `toolbox`; scrub follows `discovery.hostExec`)
default to `nsenter`.

**If you deliberately use `chroot`** (non-Talos host, or read-only tools):
mount-*creating* components must mount `host-root` **`Bidirectional`** (rshared)
and the host path must itself be `rshared` (verify `findmnt -o
TARGET,PROPAGATION /` → `shared`; a Bidirectional mount of a non-shared path
fails pod startup — fail-loud, better than the silent data bug). Pure
receive/browse volumes (toolbox `datasetMountRoot`, nfs `pool` mount) stay
`HostToContainer`. The Bidirectional `host-root` blocks in
[discovery-daemonset.yaml](../charts/simple-zfs-csi/templates/discovery-daemonset.yaml),
[csi-node-daemonset.yaml](../charts/simple-zfs-csi/templates/csi-node-daemonset.yaml)
and [toolbox.yaml](../charts/simple-zfs-csi/templates/toolbox.yaml) exist for
exactly this chroot-only path.

**Not retroactive:** the ZfsDataset reconciler is idempotent-create, so switching
an existing cluster to `nsenter` fixes **future** provisioning only. Datasets
already created under `chroot` stay unmounted until explicitly `zfs mount`-ed in
the host namespace, and any data written into a parent while the child was
unmounted is **shadowed** once the child mounts and must be reconciled by hand.

**Rule of thumb for new components:** *use `nsenter`.* Reach for `chroot` only
for read-only host tools, and then only with the Bidirectional/rshared caveats
above. See [design-decisions.md](design-decisions.md) ADR-0016.

---

## 16. Node shutdown/reboot can hang on unmount when the NFS/NVMe-oF server dies first (no guard yet — open risk)

**Status:** identified 2026-07-24, **not yet fixed**. No code guard exists for
this class; it is documented here so the risk is tracked even though the
`Guarded by:` slot below is empty.

**Symptom:** a `talos reboot`/shutdown (or plain `kubectl drain`) of a storage
node takes a very long time to complete, sometimes appearing to hang
indefinitely, when that node hosts both a pool's NFS/NVMe-oF server DaemonSet
pod *and* a CSI-node-mounted consumer of that same export.

**Root cause:** the NFS and NVMe-oF target DaemonSets are `hostNetwork` and
node-pinned to the pool's owner node (`pool.Status.currentNode`) — see
[nfs-daemonset.yaml](../charts/simple-zfs-csi/templates/nfs-daemonset.yaml) and
[nvmeof-daemonset.yaml](../charts/simple-zfs-csi/templates/nvmeof-daemonset.yaml)
— while the CSI node plugin that mounts those exports runs on **every** node
([csi-node-daemonset.yaml](../charts/simple-zfs-csi/templates/csi-node-daemonset.yaml)).
On a single-node cluster, or whenever a workload happens to land on the node
that also owns the pool, the NFS/NVMe-oF server and its own client are
co-located. If the server pod is torn down (or the whole node goes offline)
before the client unmounts, the client is in the same position as any NFS
client whose server vanished: `NodeUnpublishVolume`'s `unmount()` shells out to
a plain `umount <target>` with **no `-f`/`-l`, no timeout, and
`context.Background()`** — see `unmount` in
[internal/csi/mount.go](../internal/csi/mount.go). A hard NFS mount to a
now-unreachable server blocks in `umount` (and often in any process with an
open fd on it) indefinitely, which in turn blocks kubelet's volume teardown and
therefore pod/node termination. NVMe-oF's disconnect-by-NQN step in
`NodeUnpublishVolume` ([internal/csi/node.go](../internal/csi/node.go)) runs
*after* this same blocking `umount` call, so it does not help once the umount
itself is stuck.

**Why it's not just a multi-node problem:** there is no ordering guarantee
between the NFS/NVMe-oF server DaemonSets and the CSI node plugin today — no
`priorityClassName`, no `preStop` hook, no `PodDisruptionBudget`, no
terminationGracePeriod tuning in any of the chart templates. Kubernetes gives
no cross-DaemonSet shutdown ordering by default, so "server disappears first"
is not an edge case to defend against, it's the coin-flip default on a
single-node (or storage+compute-colocated) deployment.

**Guarded by:** nothing yet — open risk. Candidate fixes (soft/`timeo` NFS
mount options, bounded-timeout unmount with lazy fallback in
[internal/csi/mount.go](../internal/csi/mount.go), chart-level
`priorityClassName`/`preStop` ordering) are being evaluated; update this entry
with the `Guarded by:` fix once one lands.

---

## Adjacent operational gotchas (not bugs, but frequently confusing)

### ZVOL vs filesystem sizing

`quota` / `refquota` are **filesystem-only** properties — they do not apply to
zvols. A zvol is capped by **`volsize`** (plus a **`refreservation`** equal to
volsize, because the driver creates non-sparse zvols with `zfs create -V`
without `-s`). Consequently `zfs list` shows a zvol's `AVAIL` as **pool free
space, not a per-volume cap**, and its `USED` ≈ `volsize` immediately (reserved)
while `REFER` stays tiny until data is written. This is expected. See
`CreateZvol` in [internal/zpool/zfs.go](../internal/zpool/zfs.go) and `ensureSize`
in [internal/controller/zfsdataset_controller.go](../internal/controller/zfsdataset_controller.go).

### `Retain` reclaim leaves work behind

With `reclaimPolicy: Retain`, deleting the PVC leaves the PV `Released` **and**
the `ZfsDataset` CR **and** the on-disk zvol/filesystem in place. Deleting the PV
does **not** cascade to them: there is no `ownerReference`, and CSI
`DeleteVolume` is never called for `Retain`. To reclaim a retained volume,
delete the **`ZfsDataset` CR** (its finalizer destroys the on-disk object), then
delete the `Released` PV. `Delete`-policy volumes are fully reclaimed
automatically.

### `uid`/`gid`/`mode` are seeded once, not reconciled

The `uid`/`gid`/`mode` parameters set an NFS/filesystem dataset's root ownership
and permissions **once, right after the dataset is created** (via host
`chown`/`chmod`). They are an initial seed, not an enforced invariant, so:

- **Changing a StorageClass's `uid`/`gid`/`mode` does not re-`chown` existing
  datasets** — only datasets provisioned *after* the change pick up the new
  values. To retro-fit an existing share, `chown`/`chmod` its mountpoint on the
  host manually.
- A workload (or admin) is free to re-`chown` files inside the share afterwards;
  the operator never fights those changes.
- They are the RWX/NFS analogue of a pod's `securityContext.fsGroup` (which
  kubelet applies only to single-node block volumes, never to shared NFS
  exports). They are **silently ignored for NVMe-oF** (block) volumes, whose
  ownership is a fsGroup concern (see class 14). Unset leaves the ZFS default
  `root:root 0755`. See `ApplyOwnership` in
  [internal/zpool/zfs.go](../internal/zpool/zfs.go), `applyRootOwnership` in
  [internal/controller/zfsdataset_controller.go](../internal/controller/zfsdataset_controller.go),
  and ADR-0015 in [design-decisions.md](design-decisions.md).

---

## Pre-commit verification recipe

```sh
gofmt -l internal/ cmd/ api/ \
  && go build ./... \
  && go vet ./... \
  && go test ./... \
  && helm lint charts/simple-zfs-csi \
  && helm template rel charts/simple-zfs-csi >/dev/null
```
