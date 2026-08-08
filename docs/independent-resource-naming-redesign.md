# Independent ZFS-Facing Naming for `ZfsDataset`/`ZfsSnapshot` — Research & Decision Log

**Status: implemented (2026-08-02), see [ADR-0018](design-decisions.md).** Was previously
prioritized ahead of [snapshot-lifecycle-redesign.md](snapshot-lifecycle-redesign.md)
since it's more fundamental (touches the volume path, not just the new snapshot path)
and landing it first means less rework for that plan (its backing-clone naming already
assumed an independent `Spec.SnapshotName`, which this change provides for free).

Like the sibling document, this is a full working record (findings, rejected ideas,
accepted ideas) meant to survive independently of chat history, kept as a standalone doc
(not folded into `snapshot-lifecycle-redesign.md`) because it's a separate, more
foundational concern with its own scope and priority.

---

## 1. The question that started this

Today, `CreateVolume`'s incoming CSI `Name` is used directly for **two different
things**:
1. `ZfsDataset.ObjectMeta.Name` — the Kubernetes object identity.
2. `ZfsDataset.Spec.Dataset` — the actual ZFS on-disk dataset path (via
   `rp.Dataset(name)` = `datasetPrefix/name`).

Same pattern for `ZfsSnapshot`: `CreateSnapshot`'s `Name` becomes both
`ObjectMeta.Name` and `Spec.SnapshotName` (the literal `@`-suffix used in `zfs
snapshot`/`zfs clone` calls) — see ADR-0008: *"the snapshot id is the object name."*

Question raised: is it always safe to feed the CO-provided name directly into ZFS
commands, or could a differently-named PVC ever produce ZFS-invalid characters — and if
we're at all unsure, should we decouple entirely and generate our own opaque identifiers,
Ceph-style, rather than trust an external naming convention?

## 2. Investigation

### 2.1 The premise was subtly wrong — Kubernetes never passes raw user text to `CreateVolume`/`CreateSnapshot`

Confirmed by reading the actual sidecar source (fetched live from GitHub):

- `external-provisioner`, `pkg/controller/controller.go`:
  ```go
  func makeVolumeName(prefix, pvcUID string, volumeNameUUIDLength int) (string, error) {
      if volumeNameUUIDLength == -1 {
          return fmt.Sprintf("%s-%s", prefix, pvcUID), nil // default: no truncation
      }
      return fmt.Sprintf("%s-%s", prefix, strings.Replace(pvcUID, "-", "", -1)[0:volumeNameUUIDLength]), nil
  }
  ...
  pvName, err := makeVolumeName(p.volumeNamePrefix, string(claim.ObjectMeta.UID), p.volumeNameUUIDLength)
  req := csi.CreateVolumeRequest{ Name: pvName, ... }
  ```
  `CreateVolumeRequest.Name` is **always** `<prefix>-<PVC's Kubernetes UID>` (default
  prefix `pvc`) — **never** the user's own PVC `metadata.name`. A PVC named
  `my-app!!!💾weird///name` produces exactly the same shape of CSI `Name` as any other:
  `pvc-550e8400-e29b-41d4-a716-446655440000`. Lowercase hex + hyphens, always, by
  construction.

- `external-snapshotter`, `pkg/utils/util.go`:
  ```go
  func GetDynamicSnapshotContentNameForSnapshot(snapshot *crdv1.VolumeSnapshot) string {
      return "snapcontent-" + string(snapshot.UID)
  }
  ```
  This becomes `CreateSnapshotRequest.Name` for **dynamically** provisioned snapshots.
  Checked `pkg/sidecar-controller/snapshot_controller.go`'s `shouldDelete`/`syncContent`:
  pre-provisioned snapshots (`Spec.Source.SnapshotHandle != nil`) never call
  `CreateSnapshot` at all — only dynamic provisioning does, where the name is always
  `snapcontent-<VolumeSnapshotContent UID>`.

Both are Kubernetes-generated UUIDs (RFC 4122: lowercase hex digits + hyphens, fixed
length) — always within ZFS's dataset-name charset (`[a-zA-Z0-9_.:-]`) and length limits.
**Today's direct-naming approach is safe by construction, not by luck.**

### 2.2 So is a `csi-snap-`/`csi-vol-` prefix even needed for collision-avoidance?

No, not anymore, once traced through: `pvc-<uuid>` (volumes) and `snapcontent-<uuid>`
(snapshots) are structurally distinct, Kubernetes-enforced prefixes that can never
collide with each other regardless of what we do. A prefix we add ourselves
(`csi-snap-`, discussed for the backing-clone naming in the sibling doc) is no longer
required for *that* purpose — but is still worth keeping as cheap defense-in-depth
against relying on `external-snapshotter`'s specific internal naming convention (an
implementation detail, not a documented CSI-spec guarantee) staying unchanged forever.

### 2.3 Why not go further and decouple entirely (full Ceph-style opaque IDs)?

Investigated seriously, since "what if Kubernetes changes its naming convention" is a
reasonable long-term concern even if today's names are safe. Two designs were
considered, with very different costs:

**(a) Full Ceph-style: a separate volume/snapshot journal.** Ceph-csi never reuses the
CO-provided name as its backend object name — it generates a fresh opaque UUID and
maintains a persistent journal (RADOS omap) mapping CSI name ⟷ backend name, so a
*retried* `CreateVolume` with the same CO-provided name reliably finds the same
previously-created backend object instead of duplicating it. Estimated cost if copied
literally: **very large.** Grepped the codebase: **80 matches across 16 files** assume
`ObjectMeta.Name == CSI VolumeId/SnapshotId` directly (`ZfsDataset`, `ZfsShare`,
`ZfsSnapshot`, `ZfsShareAttachRequest`, all their controllers and tests, `resolveContentSource`,
`ListSnapshots`, the node plugin, ...). Decoupling the *Kubernetes object identity* from
the CSI ID would require rebuilding a Name↔ID reservation journal (with proper
create/retry concurrency-safety — the actual hard part of Ceph's design) and refactoring
every one of those 80 call sites, plus every *derived* naming scheme downstream
(`ZfsShare`, `NetworkExport`, `ZfsShareAttachRequest` are all currently keyed by simple
string derivation from the volume ID). Rejected: comparable in size to, or larger than,
the entire snapshot-lifecycle redesign, to solve a problem (naming-convention
independence) that doesn't need that scope.

**(b) Decouple only the ZFS-facing name, keep the Kubernetes object identity as-is
(accepted).** The key realization (raised by the user, and correct): `ObjectMeta.Name`
and `Spec.Dataset`/`Spec.SnapshotName` are **already separate fields** in the data model
— they just happen to be *computed from the same source* today, as a convenience, not
because they must be. Keeping `ObjectMeta.Name == VolumeId/SnapshotId` unchanged means:
- **Zero changes** to all 80 existing call sites, or to any derived naming
  (`ZfsShare`, `ZfsShareAttachRequest`, `NetworkExport`) — they all keep keying off the
  CSI ID exactly as today.
- **Free, atomic idempotency is preserved.** Kubernetes' own etcd-level name uniqueness
  already guarantees that two concurrent `Create()` calls with the same object name
  resolve to exactly one winner — this is *why* today's scheme needs no journal at all.
  A journal is only necessary when the CO-facing identity and the backend identity are
  two different objects that need to be atomically correlated; if they stay the same
  object, there's nothing to correlate.
- `resolveContentSource`, `ListSnapshots`, status reporting, etc. already read
  `Spec.Dataset`/`Spec.SnapshotName` directly off the fetched object rather than
  recomputing a path from the ID — so they need no changes either.
- Full independence from whatever Kubernetes' naming convention does or doesn't do,
  for the one thing that actually touches ZFS commands.

**Decision: (b).** Much smaller than (a), and delivers genuine decoupling (not just a
defensive patch), matching the user's stated preference for "the Ceph way" without its
associated cost.

### 2.4 Does a snapshot need its own dedicated CRD for this (separate from `ZfsSnapshot`)?

No — `ZfsSnapshot` already has exactly the same two-field split as `ZfsDataset`
(`ObjectMeta.Name` vs. `Spec.SnapshotName`; see ADR-0008), so it already plays the role
being asked about. No new CRD needed for the independent identifier itself, and no new
CRD needed for the snapshot-lifecycle redesign's "backing clone" either (§6 of the
sibling doc) — that stays a computed path derived from `ZfsSnapshot`'s own fields, not an
independently-tracked object, since it has no lifecycle of its own beyond what
`ZfsSnapshotReconciler` already manages.

### 2.5 One more place doing the same thing, found while auditing for consistency

`internal/controller/zfsdataset_controller.go`'s `clone()` path (ADR-0009, direct
PVC-to-PVC cloning, no `VolumeSnapshot` involved):
```go
snapFull := srcFull + "@clone-" + vol.Name  // vol.Name = the destination PVC's CSI volume id
```
This embeds the CO-provided destination volume's name directly into a ZFS snapshot
suffix on the **source** dataset. Same category of exposure as `Spec.Dataset`/
`Spec.SnapshotName` — included in scope below for full consistency (this one is a purely
internal, ephemeral snapshot suffix, so a simpler independent identifier — not
necessarily a new persisted field, since it's derived fresh each reconcile from
`vol.Name` today — is enough; needs a small design call during implementation on whether
to persist it or keep deriving it from `vol.Name` but hashed/sanitized instead of used
raw).

## 3. Decision

Generate independent, opaque identifiers for the two ZFS-facing fields, persisted once
at first creation, immutable after:
- `ZfsDataset.Spec.Dataset`'s leaf component: `csi-vol-` + `uuid.New().String()`.
- `ZfsSnapshot.Spec.SnapshotName`: `csi-snap-` + `uuid.New().String()`.

`ObjectMeta.Name` on both CRDs remains exactly the CSI-provided `VolumeId`/`SnapshotId`,
unchanged. No new dependency — `github.com/google/uuid v1.6.0` is already vendored
(confirmed in `go.mod`).

This also **simplifies** the sibling snapshot-lifecycle redesign slightly: its backing-clone
naming (`csi-snap-<name>`) can now just reuse `Spec.SnapshotName` directly (already
independent/opaque) instead of the raw CSI snapshot name — one less thing that plan needs
to derive itself.

## 4. Implementation task list

1. `internal/csi/controller.go` (`CreateVolume`): on the not-found branch (new
   `ZfsDataset`), generate `csi-vol-` + `uuid.New().String()` for the dataset leaf name
   instead of using `name` directly; persist as `Spec.Dataset`. On the found branch
   (idempotent retry), do not recompute — reuse the existing object's `Spec.Dataset`
   as-is.
2. `ensureVolume`/`volumeSpecCompatible`: adjust the idempotency comparison to stop
   requiring an exact `Spec.Dataset` match between "desired" and "existing" (since it's
   no longer deterministically recomputable by the caller) — compare `PoolGUID`, `Type`,
   `Source`, size instead, and treat an already-existing object's `Spec.Dataset` as
   authoritative.
3. `internal/csi/snapshot.go` (`CreateSnapshot`): identical pattern for
   `Spec.SnapshotName` (`csi-snap-` + `uuid.New().String()`), and the equivalent
   `ensureSnapshot` idempotency-comparison adjustment (drop the `SnapshotName` equality
   check; compare `PoolGUID`/`Dataset`/`SourceVolume`/`SourceType`).
4. `internal/controller/zfsdataset_controller.go`'s `clone()`: replace the raw
   `"@clone-" + vol.Name` intermediate snapshot suffix with a sanitized/independent
   identifier (design call during implementation: derive-and-hash `vol.Name`, or persist
   a small new field — lean toward the cheaper hash-of-`vol.Name` option since this
   suffix is purely internal/ephemeral and doesn't need the same persisted-identity
   treatment as `Spec.Dataset`/`Spec.SnapshotName`).
5. Regenerate CRDs/deepcopy if any new field is added (`make manifests`) — likely
   unnecessary for steps 1-3 (no new fields, just how existing fields get populated);
   only needed if step 4 ends up persisting something new.
6. Tests:
   - Idempotent retry of `CreateVolume`/`CreateSnapshot` with the same CSI name reuses
     the already-persisted `Spec.Dataset`/`Spec.SnapshotName`, doesn't regenerate.
   - Two different `CreateVolume`/`CreateSnapshot` calls never collide on the ZFS-facing
     name even with adversarial/unusual CSI-provided names (since those names are no
     longer used for the ZFS path at all).
   - Restore/clone/list/status paths unaffected (already read `Spec.Dataset`/
     `Spec.SnapshotName` directly, not by recomputing — verify with existing test suite,
     should require no test changes there).
   - `zfsdataset_controller.go`'s `clone()` intermediate-snapshot naming change (step 4)
     covered by its existing clone tests.
7. Once implemented and tested: fold a brief note into `design-decisions.md` as its own
   ADR (this doc stays as the detailed backing record), and update
   `snapshot-lifecycle-redesign.md`'s references to `csi-snap-<name>` to reflect that
   `<name>` is now already `Spec.SnapshotName` (an independent identifier), not the raw
   CSI snapshot name.

## 5. Rejected alternatives (and why)

- **Full Ceph-style volume/snapshot journal, decoupling `ObjectMeta.Name` from the CSI
  ID too.** Rejected: ~80 call sites and multiple derived-naming schemes would need
  rework, plus reimplementing Ceph's atomic reservation logic, to solve a problem that
  doesn't require touching the Kubernetes-object-identity layer at all (§2.3).
- **Just validate-and-fallback (sanitize/hash the CSI name only if it doesn't fit ZFS's
  charset/length).** Considered as a cheaper middle ground before landing on full
  decoupling. Superseded once it became clear that decoupling `Spec.Dataset`/
  `Spec.SnapshotName` (keeping `ObjectMeta.Name` untouched) costs about the same as this
  validation-only approach but delivers genuine, permanent independence rather than a
  conditional fallback.
- **A dedicated CRD for the independent snapshot identifier, or for the snapshot-lifecycle
  redesign's backing clone.** Rejected — `ZfsSnapshot` already has the field separation
  needed (§2.4); a new CRD would just add a second reconcile loop/RBAC surface/object set
  for something fully derivable from data the existing CRD already holds.

## 6. Open items / not yet decided

- ~~Step 4's exact mechanism (hash `vol.Name` vs. persist a new field) — lean toward
  hashing, final call during implementation.~~ **Resolved during implementation
  (2026-08-02): hashing was used** (`cloneSnapshotSuffix` in
  `internal/controller/zfsdataset_controller.go`, SHA-256 of the destination object
  name, truncated to 16 hex chars) — no new persisted field was added. **Superseded
  (later revision): the hash was dropped in favor of using `vol.Name` raw** — the
  API server already guarantees `ZfsDataset` object names are DNS-1123-safe (and
  thus ZFS-safe), so the hash added no safety, only made `zfs list -t snapshot`
  output opaque. This also unifies the pattern with the backing-clone snapshot,
  which is likewise named directly after its owning object's name
  (`Spec.SnapshotName`) rather than a derived hash. None outstanding.
