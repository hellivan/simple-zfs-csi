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

## Not pursuing (for now)

- **`VolumeReplication`** (csi-addons) — would mean building `zfs
  send/receive`-based async mirroring from scratch; a large new subsystem,
  not a small addition.
- **`EncryptionKeyRotation`** (csi-addons) — doesn't apply because there's no
  ZFS-native-encryption support in the driver at all yet; nothing to rotate.
