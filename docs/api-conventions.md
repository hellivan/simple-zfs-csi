# API & CRD Conventions

Conventions for the `storage.zfs-shares.io` CRD types. These are the rules we
settled on while shaping `ZfsVolume`, `ZfsShare` and `NetworkExport`; follow them
for any new type or field.

## 1. Type-discriminated specs use a nested discriminated union

When a resource has a `type`/`protocol`-style discriminator and the valid options
depend on that discriminator, **group the type-specific options into a nested
"arm" struct per discriminator value** — do not scatter them as flat, optional
top-level fields.

**Avoid** (flat optionals — unclear which field applies to which type):

```go
type ZfsVolumeSpec struct {
    Type         VolumeType         `json:"type"`
    Quota        *resource.Quantity `json:"quota,omitempty"`         // only type=filesystem
    Size         *resource.Quantity `json:"size,omitempty"`          // only type=volume
    Volblocksize string             `json:"volblocksize,omitempty"`  // only type=volume
}
```

**Prefer** (nested arms — each block is self-evidently scoped to one type):

```go
type ZfsVolumeSpec struct {
    Type       VolumeType        `json:"type"`
    Filesystem *FilesystemConfig `json:"filesystem,omitempty"` // honoured iff type=filesystem
    Volume     *VolumeConfig     `json:"volume,omitempty"`     // honoured iff type=volume
}
```

```yaml
spec:
  type: filesystem
  filesystem:
    quota: 1Gi
  volume:
    size: 10Gi
    volblocksize: 16k
```

Guard the union with CEL `XValidation` on the spec: the matching arm required,
the other arm(s) forbidden. Example:

```go
// +kubebuilder:validation:XValidation:rule="self.type != 'volume' || has(self.volume)",message="spec.volume is required when type is volume"
// +kubebuilder:validation:XValidation:rule="self.type != 'volume' || !has(self.filesystem)",message="spec.filesystem is only valid when type is filesystem"
// +kubebuilder:validation:XValidation:rule="self.type != 'filesystem' || !has(self.volume)",message="spec.volume is only valid when type is volume"
```

This is the same shape Kubernetes core uses (`VolumeSource`, `PersistentVolumeSource`,
Ingress backends). The arms are necessarily optional pointers gated by CEL — you
cannot make an arm structurally required when only one applies at a time.

`NetworkExport` and `ZfsShare` already follow this via their `nfs` / `nvmeof`
arms under the `protocol` discriminator; `ZfsVolume` was migrated to match.

## 2. Shared identity stays OUTSIDE the union

Fields that are present and mean the same thing regardless of the discriminator
(identity, routing, name/path) belong at the **top level of the spec**, not
inside an arm.

Rationale: a discriminated-union arm should hold only the fields that actually
*diverge* by type. A universal field placed inside an arm forces every consumer
to `switch` on the discriminator (with nil-checks) just to read it, and
duplicates the field across arms with no structural guarantee they agree.

Example — the logical dataset path is universal, so it stays top-level as
`spec.dataset`, never inside `spec.filesystem` / `spec.volume`:

```go
// Read the name unconditionally:
full := pool.Status.PoolName + "/" + vol.Spec.Dataset

// NOT: switch vol.Spec.Type { case ...: vol.Spec.Filesystem.Name; case ...: vol.Spec.Volume.Name }
```

## 3. Use ZFS's own vocabulary

In ZFS, **"dataset" is the umbrella term** for any object in a pool's namespace.
The `zfs(8)` man page lists the kinds as: **file system, volume (zvol),
snapshot, bookmark**. `zfs list -t filesystem,volume` shows both; `zfs create`
makes a filesystem, `zfs create -V` makes a volume — same namespace, different
`type` property.

```
dataset (generic ZFS object)
├── filesystem   → mountable POSIX fs   (exported over NFS)
├── volume/zvol  → block device         (exported over NVMe-oF)
├── snapshot
└── bookmark
```

Consequences for our API:

- **`dataset` is the correct name for the shared path field** in both
  `ZfsVolume` and `ZfsShare` — a zvol *is* a dataset, so it is accurate for the
  NVMe-oF/zvol case too, not just filesystems.
- **The `type` discriminator uses ZFS's real type names**: `filesystem` and
  `volume` (not the earlier `dataset`/`zvol`, where `dataset` was imprecise since
  the whole object is a dataset). This also matches `DatasetKind{filesystem,
  volume}` already used in `internal/zpool`.

## 4. Naming consistency across sibling CRDs

The same concept must use the same field name across related types. The logical
ZFS object path is `spec.dataset` in **both** `ZfsVolume` and `ZfsShare`. When a
nested arm would collide with a shared field name, rename the *arm/discriminator*
(per §3), not the shared field — keep the shared field's name stable across the
family.

## Current CRD shapes

| CRD | Discriminator | Arms (only the matching one is honoured) | Shared identity fields |
|-----|---------------|------------------------------------------|------------------------|
| `ZfsVolume` | `type: filesystem\|volume` | `filesystem{quota}` / `volume{size, volblocksize}` | `poolGUID`, `dataset`, `properties` |
| `ZfsShare` | `protocol: nfs\|nvmeof` | `nfs{clients}` / `nvmeof{nqn, allowedHosts}` | `poolGUID`, `dataset` |
| `NetworkExport` | `protocol: nfs\|nvmeof` | `nfs{clients}` / `nvmeof{nqn, allowedHosts}` | `nodeName`, `path` |
