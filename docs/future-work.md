# Future Work

A running backlog of candidate features that have been discussed but are
**not decided or scheduled** — unlike [design-decisions.md](design-decisions.md),
which is an append-only log of committed decisions, entries here can be
freely edited, reprioritized, or removed as they're picked up or dropped.

---

## Reclaim space (fstrim) for NVMe-oF + filesystem volumes

**Why:** zvols are created thick (`zfs create -V` without `-s`, see
[internal/zpool/zfs.go](../internal/zpool/zfs.go)), so the pool reserves the
full `volsize` upfront. For NVMe-oF volumes with a filesystem on top
(ext4/xfs formatted onto a zvol, see [internal/csi/mount.go](../internal/csi/mount.go)),
deleting files inside that filesystem does **not** free the underlying zvol
blocks unless something issues `discard`/`fstrim` — the same indirection
problem RBD/LVM-thin have. Nothing in the driver does this today: no forced
`discard` mount option, no periodic `fstrim`.

Native ZFS filesystem datasets (NFS shares) do **not** need this — deleting a
file frees dataset blocks immediately, no indirection involved. Raw block-mode
zvols (no filesystem, app manages blocks) are also fine — the app/kernel can
issue discard directly.

**Candidate approach:** a periodic per-node `fstrim <mountpoint>` for mounted
NVMe-oF + filesystem volumes (in `csi-node` or a small sidecar loop) — cheaper
than wiring up the full csi-addons `ReclaimSpace` gRPC service/CRD/sidecar,
which is the alternative, addon-based mechanism for this.

## NetworkFence-style forced node eviction

**Why:** csi-addons' `NetworkFence` forcibly blocks a node's access to a
volume during failover to avoid split-brain. This project already has a
related but weaker mechanism — single-node NVMe-oF export enforcement via
`AllowedHosts` + "oldest attach wins" (see
[internal/controller/zfsshareattachrequest_controller.go](../internal/controller/zfsshareattachrequest_controller.go)) —
but no way to force-evict a node that's gone unresponsive. Relevant to the
node reboot/shutdown hang risk tracked as known-pitfalls.md class 16.

**Candidate approach:** a primitive that drops a node's NQN from every
subsystem's allow-list on demand. Bigger lift than fstrim since it likely
means standing up the separate csi-addons gRPC identity/controller service —
or a project-local equivalent.

## Native Kubernetes VolumeGroupSnapshot support

**Why:** `zfs snapshot` can atomically snapshot multiple datasets in one
command, which is exactly what group-consistent snapshots need (e.g. an app
with separate PVCs for DB data + WAL). `ZfsSnapshot` today is one CR per
dataset with no grouping concept.

**Two possible mechanisms — prefer the native one:**
- **Native CSI + Kubernetes `VolumeGroupSnapshot`** (beta in Kubernetes 1.32):
  an optional CSI service (`GROUP_CONTROLLER_SERVICE` plugin capability,
  `CreateVolumeGroupSnapshot`/`DeleteVolumeGroupSnapshot`/`GetVolumeGroupSnapshot`
  RPCs — confirmed present in the vendored CSI spec, `spec@v1.11.0/spec.md`),
  driven by the `groupsnapshot.storage.k8s.io` CRDs and the same
  `external-snapshotter` sidecar (v7+) already used for regular snapshots.
- **csi-addons' own `VolumeGroupSnapshot`** — an older, addon-based CRD that
  predates native k8s support, requiring the separate csi-addons sidecar/CRDs.

Since native support is now beta and standard, and `external-snapshotter` is
already deployed for per-volume snapshots, extending the existing
`internal/csi` controller with a `GroupControllerServer` (mapping
`CreateVolumeGroupSnapshot` to an atomic multi-dataset
`zfs snapshot pool/ds1@x pool/ds2@x ...`) is the preferred target over
pulling in the csi-addons machinery.

## Gate re-provisioning on `Status.Phase` (fail loud when a dataset disappears)

**Why:** `ZfsDatasetReconciler` calls `create()` from the `ErrNotExist` branch of its
idempotent-create check, with no memory of whether the dataset ever existed. So a
`ZfsDataset` whose ZFS object has gone missing — destroyed out of band, pool restored
from a backup predating it, or `spec.dataset` repointed by hand to a path that doesn't
exist yet — is silently **re-provisioned**. For a clone-sourced dataset that means
re-cloning from `spec.source`, producing a fresh copy at the *original* vintage and
silently discarding everything written since; for an empty-source dataset, an empty
dataset where data used to be.

This matters most for the supported emergency workflow of repointing `spec.dataset` by
hand. Repointing to a dataset that **already exists** is safe — `Get(type)` succeeds and
the driver simply adopts it. Repointing to a path that doesn't exist yet triggers the
silent re-create instead of an error.

Surfaced during the 2026-08-03 code-review follow-up discussion while confirming that
`spec.source` is read *only* from that branch (which is what makes a stale
`spec.source.volume` harmless for a live dataset — see
[snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md) §9.3).

**Candidate approach:** refuse to `create()` when the object has previously reported
`Ready`, surfacing a hard error instead. Checked against the cases that could plausibly
break it and none do: pool failover and pool re-import both leave the dataset present, so
`Get` succeeds and `create()` is never reached. Deliberately kept out of the review
fix-up work — it is a behaviour change unrelated to the snapshot lifecycle.

## Generate RBAC from the kubebuilder markers

**Why:** `make manifests` runs `controller-gen` with only the `object` and `crd`
generators — there is **no `rbac:` generator**. So every
`+kubebuilder:rbac:...` marker in `internal/controller` is decorative: it never
becomes YAML, and the ClusterRoles in
[charts/simple-zfs-csi/templates/rbac.yaml](../charts/simple-zfs-csi/templates/rbac.yaml)
are maintained entirely by hand. Nothing checks that the two agree.

That is the structural reason the 2026-08-03 review's blocker finding went
unnoticed: D15 gave `ZfsSnapshotReconciler` a new job (authoring backing-clone
`ZfsDataset` objects, ADR-0020/D26), the discovery ClusterRole was never given
`create`/`delete` to match, and because the reconciler also carried no
`zfsdatasets` marker at all, regenerating manifests would not have surfaced the
gap either. The result was that the backing-clone snapshot mechanism — then the
default `standalone` mode, now the only one (ADR-0027) — could neither create nor
delete a snapshot on a fresh install.

**Candidate approach, cheapest first:**
- A test that renders the chart and asserts each reconciler's declared marker
  verbs are a subset of what its ServiceAccount's ClusterRole grants. This was
  the code review's own suggestion, and it catches the drift without changing
  how the chart is produced.
- Or wire up `rbac:roleName=...` in the `manifests` target properly. Bigger:
  the chart's roles also carry hand-written rules for the CSI sidecars
  (provisioner, resizer, snapshotter) and for core resources that no marker
  describes, so the generated output would have to be merged with those rather
  than replacing them — probably as a separate generated role per component,
  aggregated alongside the hand-written one.

Either way the markers should stop being advisory, since they are currently the
only place a reconciler's real permission needs are written down.

## Mixed local/NFS mounts for RWX volumes (rejected/postponed)

**Why considered:** ADR-0031 Phase 2 gives RWO NFS datasets a local bind-mount
when the pod and the pool share a node. The natural next question is whether
RWX (multi-node) datasets could get the same optimization per-pod — pods that
land on the pool's own node get a bind-mount, pods on other nodes get NFS, same
volume, mixed transport. Investigated and **rejected as unsafe by default**;
documenting the findings here so it isn't re-attempted without re-deriving why.

**The core problem: lock-domain collision between `flock()` and NFS.**
Linux has two independent locking domains, `flock()` (BSD, whole-file) and
`fcntl()`/OFD (POSIX, byte-range), and the kernel does not cross-check between
them. NFS has no native `flock()` support: the Linux NFS client silently
translates a client's `flock()` call into an `fcntl()` byte-range lock over the
wire. So under a mixed mount:

- A pod using the **local bind-mount** calls `flock()` → gets a native BSD lock
  directly against the ZFS dataset.
- A pod using the **NFS mount** calls `flock()` → the NFS client turns this
  into an `fcntl()` lock, which the NFS server (`nfsd`) then places on the same
  underlying dataset.
- The kernel holds both a `flock()` and an `fcntl()` lock on the same file
  *simultaneously*, from two different "views" of the same storage, and treats
  them as non-conflicting because they're different domains. **Both pods get
  write access at once.** Silent corruption, not a crash — the worst kind of
  failure for a storage layer to introduce.

Any application replica scaled across nodes that calls `flock()` (a very common
pattern for `.pid` files, simple daemons, and plenty of "simple" locking code)
would be exposed to this the moment one replica landed on the pool's node and
another didn't — which, on a cluster where pods get rescheduled routinely, is
not a rare edge case but an eventual certainty.

**Two more problems on top of that, even for `fcntl()`-only / lock-free apps:**

- **Cache coherency (NFS close-to-open):** NFS clients only guarantee
  consistency at open/close time and otherwise trust their attribute cache
  (typically several seconds to a minute). A local bind-mount pod's write hits
  ZFS immediately; a same-volume NFS pod reading without an intervening
  open/close (e.g. tailing a file) can serve stale data from its own cache
  indefinitely, because the local writer never went through anything the NFS
  server's revalidation would notice in time.
- **NFSv4 delegations:** `nfsd` can hand a client a delegation (full
  read/write control, cached locally, no round-trip). Recalling it when a
  *different path* (the local bind-mount) modifies the file depends on the
  kernel's VFS lease-break mechanism noticing the change — a known
  race-prone path. A missed or delayed lease break lets the delegation holder
  keep writing its stale cached copy while the local bind-mount pod writes
  concurrently to disk.

**Why this differs from the RWO fix:** the RWO restriction (see ADR-0031 above)
exists precisely because a *single* volume must never be visible through two
lock domains from two different nodes at once. RWX by definition allows
multiple nodes to hold the volume simultaneously, so "mixed" isn't a transient
race to close (like the RWO migration case) — it would be the **steady state**
for as long as pods are scheduled across nodes, which is indefinite for any
long-running RWX deployment.

**Candidate approach if ever revisited:** do **not** make this the default.
Expose it, if at all, as an explicit, scarily-named opt-in StorageClass
parameter (e.g. `unsafeLocalBypassRWX: true` / `experimentalMixedRWX: true`),
document loudly that it requires every workload sharing the volume to either
use only `fcntl()`/OFD byte-range locks (never `flock()`), avoid relying on
timely cross-node read-after-write visibility, or coordinate externally (e.g.
a lock service), and treat it as a niche "hub-and-spoke" optimization (one
heavy local writer, several remote read-mostly consumers) rather than a
general-purpose RWX speedup.

**Decision: not pursued.** The blast radius (silent corruption for any RWX
workload using `flock()`, which the driver cannot detect or prevent) outweighs
the loopback-avoidance benefit for the common case. RWX volumes continue to
always go through NFS regardless of locality (current, safe behavior — see
ADR-0031 Phase 2). Revisit only if a concrete workload needs it badly enough to
justify a guarded, explicitly unsafe opt-in.

## Not pursuing (for now)

- **`VolumeReplication`** (csi-addons) — would mean building `zfs
  send/receive`-based async mirroring from scratch; a large new subsystem,
  not a small addition.
- **`EncryptionKeyRotation`** (csi-addons) — doesn't apply because there's no
  ZFS-native-encryption support in the driver at all yet; nothing to rotate.
